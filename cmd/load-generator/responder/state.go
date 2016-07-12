package responder

import (
	"bytes"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"github.com/letsencrypt/boulder/cmd/load-generator/latency"
	"github.com/letsencrypt/boulder/core"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// State holds all the good stuff
type State struct {
	requests    [][]byte
	numRequests int
	maxRequests int
	ocspBase    string
	getRate     float64
	postRate    float64
	dbURI       string
	runtime     time.Duration
	client      *http.Client
	callLatency *latency.File
	wg          *sync.WaitGroup
}

// New returns a pointer to a new State struct, or an error
func New(ocspBase string, getRate, postRate float64, issuerPath, latencyPath string, runtime time.Duration, serials []string) (*State, error) {
	issuer, err := core.LoadCert(issuerPath)
	if err != nil {
		return nil, err
	}
	latencyFile, err := latency.New(latencyPath)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(ocspBase, "/") {
		ocspBase += "/"
	}
	s := &State{
		ocspBase:    ocspBase,
		getRate:     getRate,
		postRate:    postRate,
		runtime:     runtime,
		client:      new(http.Client),
		callLatency: latencyFile,
		wg:          new(sync.WaitGroup),
	}

	fmt.Println("warming up")
	err = s.warmup(serials, issuer)
	if err != nil {
		return nil, err
	}
	fmt.Println("finished warm up")

	return s, nil
}

// Run runs the OCSP-Responder load generator for the configured runtime/rate
func (s *State) Run() {
	stop := make(chan bool, 2)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	if s.getRate > 0 {
		go func() {
			for {
				up := unsafe.Pointer(&s.getRate)
				gr := (*float64)(atomic.LoadPointer(&up))
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(float64(time.Second.Nanoseconds()) / *gr)):
					s.wg.Add(1)
					go s.sendGET()
				}
			}
		}()
	}
	if s.postRate > 0 {
		go func() {
			for {
				up := unsafe.Pointer(&s.postRate)
				pr := (*float64)(atomic.LoadPointer(&up))
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(float64(time.Second.Nanoseconds()) / *pr)):
					s.wg.Add(1)
					go s.sendPOST()
				}
			}
		}()
	}

	select {
	case <-time.After(s.runtime):
		fmt.Println("SLEEP END")
	case sig := <-sigs:
		fmt.Printf("SIG CAUGHT [%s], ENDING\n", sig.String())
	}
	stop <- true
	stop <- true
	fmt.Println("sent stop signals, waiting")
	s.wg.Wait()
	fmt.Println("all calls finished")
}

func (s *State) warmup(serials []string, issuer *x509.Certificate) error {
	issuerKeyHash, err := hashIssuerKey(issuer)
	if err != nil {
		return err
	}
	var requests [][]byte
	for _, s := range serials {
		serial, err := core.StringToSerial(s)
		if err != nil {
			continue
		}
		req, err := minimalCreateRequest(serial, issuerKeyHash)
		if err != nil {
			continue
		}
		requests = append(requests, req)
	}

	s.numRequests = len(requests)
	if s.numRequests == 0 {
		return fmt.Errorf("No requests to send!")
	}
	s.requests = requests
	return nil
}

func (s *State) sendGET() {
	defer s.wg.Done()
	started := time.Now()
	resp, err := s.client.Get(s.ocspBase + base64.StdEncoding.EncodeToString(s.requests[rand.Intn(s.numRequests)]))
	finished := time.Now()
	state := "good"
	defer func() { s.callLatency.Add("GET", started, finished, state) }()
	if err != nil {
		fmt.Printf("[FAILED] GET: %s\n", err)
		state = "error"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("[FAILED] GET: incorrect status code %d\n", resp.StatusCode)
		state = "unexpected status"
		return
	}
	if _, err := ioutil.ReadAll(resp.Body); err != nil {
		fmt.Printf("[FAILED] GET: bad body, %s\n", err)
		state = "read error"
		return
	}
}

func (s *State) sendPOST() {
	defer s.wg.Done()
	started := time.Now()
	resp, err := s.client.Post(s.ocspBase, "application/ocsp-request", bytes.NewBuffer(s.requests[rand.Intn(s.numRequests)]))
	// doing this here seems to ignore the time it takes to read the response...
	// should it be replace with a time.Now() in the defer?
	finished := time.Now()
	state := "good"
	defer func() { s.callLatency.Add("POST", started, finished, state) }()
	if err != nil {
		fmt.Printf("[FAILED] POST: %s\n", err)
		state = "error"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("[FAILED] POST: incorrect status code %d\n", resp.StatusCode)
		state = "unexpected status"
		return
	}
	if _, err := ioutil.ReadAll(resp.Body); err != nil {
		fmt.Printf("[FAILED] POST: bad body, %s\n", err)
		state = "read error"
		return
	}
}

// Extremely hacky minimal version of https://github.com/golang/crypto/blob/master/ocsp/ocsp.go#L445
// that allows us to just input a serial number and issuer key hash! (includes the private structs
// required for marshaling the ASN.1 properly).

type certID struct {
	HashAlgorithm pkix.AlgorithmIdentifier
	NameHash      []byte
	IssuerKeyHash []byte
	SerialNumber  *big.Int
}

// https://tools.ietf.org/html/rfc2560#section-4.1.1
type ocspRequest struct {
	TBSRequest tbsRequest
}

type tbsRequest struct {
	Version       int              `asn1:"explicit,tag:0,default:0,optional"`
	RequestorName pkix.RDNSequence `asn1:"explicit,tag:1,optional"`
	RequestList   []request
}

type request struct {
	Cert certID
}

func hashIssuerKey(issuer *x509.Certificate) ([]byte, error) {
	var publicKeyInfo struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &publicKeyInfo); err != nil {
		return nil, err
	}

	h := sha1.New()
	h.Write(publicKeyInfo.PublicKey.RightAlign())
	return h.Sum(nil), nil
}

func minimalCreateRequest(serial *big.Int, issuerKeyHash []byte) ([]byte, error) {
	return asn1.Marshal(ocspRequest{
		tbsRequest{
			Version: 0,
			RequestList: []request{
				{
					Cert: certID{
						HashAlgorithm: pkix.AlgorithmIdentifier{
							Algorithm:  asn1.ObjectIdentifier([]int{1, 3, 14, 3, 2, 26}), // SHA1
							Parameters: asn1.RawValue{Tag: 5 /* ASN.1 NULL */},
						},
						IssuerKeyHash: issuerKeyHash,
						SerialNumber:  serial,
					},
				},
			},
		},
	})
}