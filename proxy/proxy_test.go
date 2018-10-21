package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/nccgroup/tracy/api/store"
	"github.com/nccgroup/tracy/api/types"
	"github.com/nccgroup/tracy/configure"
	"github.com/nccgroup/tracy/log"
)

var requestDataNoTags = `GET /api/v1/action/ HTTP/1.1
Host: normandy.cdn.mozilla.net
User-Agent: Mozilla/5.0 (Windows NT 6.1; WOW64; rv:55.0) Gecko/20100101 Firefox/55.0
Accept: application/json
Accept-Language: en-US,en;q=0.5
Accept-Encoding: gzip, deflate, br
origin: null
Connection: close

`

//If you update this test don't forgot to update the content link
var requestDataTags = `POST /test?echo=zzXSSzz HTTP/1.1
Host: test.com
User-Agent: Mozilla/5.0 (Windows NT 6.1; WOW64; rv:55.0) Gecko/20100101 Firefox/55.0
Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8
Accept-Language: en-US,en;q=0.5
Accept-Encoding: gzip, deflate, br
Content-Length: 29
Content-Type: text/plain
Connection: close
Pragma: no-cache
Cache-Control: no-cache

test1=zzXSSzz&test2=zzPLAINzz`

// Setup the proxy and a test httpserver
// Do it once so that you don't have port
// collisions and db lock issues
var ln, _ = setupProxy()
var ts = setupHTTPTestServer()
var tstls = setupHTTPSTestServer()

func TestAddTracersBodyWithNoTags(t *testing.T) {
	numTracers, err := testAddTracersBodyHelper(requestDataNoTags)
	if err != nil {
		t.Fatalf("tried to read parse but got the following error: %+v", err)
	} else if numTracers != 0 {
		t.Fatalf("Failed to find tracers %d", numTracers)
	}
}

func TestAddTracersBodyWithTags(t *testing.T) {
	numTracers, err := testAddTracersBodyHelper(requestDataTags)
	if err != nil {
		t.Fatalf("tried to read parse but got the following error: %+v", err)
	} else if numTracers != 3 {
		t.Fatalf("Failed to find tracers, %d", numTracers)
	}
}

func BenchmarkTracersBodyWithTags(b *testing.B) {
	for i := 0; i < b.N; i++ {
		numTracers, err := testAddTracersBodyHelper(requestDataTags)
		if err != nil {
			b.Fatalf("tried to read parse but got the following error: %+v", err)
		} else if numTracers != 3 {
			b.Fatalf("Failed to find tracers, %d", numTracers)
		}
	}
}

func testAddTracersBodyHelper(requestDataString string) (int, error) {
	request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(requestDataString)))
	if err != nil {
		return 0, err
	}

	addedTracers, err := replaceTracers(request)
	if err != nil {
		return 0, err
	}

	requestData, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return 0, err
	}
	for _, addedTracer := range addedTracers {
		i := bytes.Index(append([]byte(request.URL.RawQuery), requestData...), []byte(addedTracer.TracerPayload))
		if i == -1 {
			return 0, fmt.Errorf("Could not find Tracer")
		}
	}

	return len(addedTracers), nil
}

func TestAddTracersQueryNoTags(t *testing.T) {
	numTracers, err := testAddTracerQueryHelper(requestDataNoTags)
	if err != nil {
		t.Fatalf("Failed to insert Tracers with error: %+v", err)
	} else if numTracers != 0 { //1 is the number of exspected tracers
		t.Fatal("Failed to find all Tracers")
	}
}

func TestAddTracersQueryTags(t *testing.T) {
	numTracers, err := testAddTracerQueryHelper(requestDataTags)
	if err != nil {
		t.Fatalf("Failed to insert Tracers with error: %+v", err)
	} else if numTracers != 1 { //1 is the number of expected tracers
		t.Fatalf("Failed to find all Tracers %d", numTracers)
	}
}

func testAddTracerQueryHelper(requestData string) (int, error) {
	request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(requestData)))
	if err != nil {
		return 0, err
	}

	newQuery, addedTracers := replaceTracerStrings([]byte(request.URL.RawQuery))

	for _, addedTracer := range addedTracers {
		i := strings.Index(string(newQuery), addedTracer.TracerPayload)
		if i == -1 {
			return 0, fmt.Errorf("no tracer found")
		}
	}
	return len(addedTracers), nil
}

var responseStringTracer = `HTTP/1.1 200 OK
Date: Tue, 03 Oct 2017 20:45:47 GMT
Content-Length: 80
Content-Type: text/plain; charset=utf-8
Connection: close

{"ID":1,"Data":"an event!","Location":"AASDFG","EventType":"a type of event"}`

var responseStringNoTracer = `HTTP/1.1 200 OK
Date: Tue, 03 Oct 2017 20:45:47 GMT
Content-Length: 80
Content-Type: text/plain; charset=utf-8
Connection: close

{"ID":1,"Data":"an event!","Location":"aa","EventType":"a type of event"}`

func TestFindTracers(t *testing.T) {
	//findTracers(responseString string, tracers map[int]types.Tracer) []types.Tracer {
	tracers := make([]types.Request, 1)
	tracer := types.Tracer{TracerPayload: "AASDFG"}
	tracers[0].Tracers = make([]types.Tracer, 1)
	tracers[0].Tracers[0] = tracer

	numHits, err := testFindTracersHelper(responseStringTracer, tracers)

	if err != nil {
		t.Fatal("Magic just happened") //error should always be null
	} else if numHits != 1 {
		t.Fatal("Failed to find tracer")
	}
}

func TestFindNoTracers(t *testing.T) {
	//findTracers(responseString string, tracers map[int]types.Tracer) []types.Tracer {
	tracers := make([]types.Request, 1)
	tracer := types.Tracer{TracerPayload: "AASDFG"}
	tracers[0].Tracers = make([]types.Tracer, 1)
	tracers[0].Tracers[0] = tracer

	numHits, err := testFindTracersHelper(responseStringNoTracer, tracers)

	if err != nil {
		t.Fatal("Magic just happened") //error should always be null
	} else if numHits != 0 {
		t.Fatalf("Found too many tracers? %d", numHits)
	}
}

func testFindTracersHelper(responseData string, tracers []types.Request) (int, error) {
	foundTracers := findTracersInResponseBody(responseData, "www.test.com", tracers)

	return len(foundTracers), nil
}

func TestFullProxy(t *testing.T) {
	// Test just sending data, no trace strings
	body, err := makeRequest(ts.URL, "a")
	if err != nil || bytes.Compare(body, []byte("<div>a</div>\n")) != 0 {
		log.Error.Println(err)
		t.FailNow()
	}
}
func TestFullProxyTLS(t *testing.T) {
	// Test just sending data, no trace strings
	body, err := makeRequest(tstls.URL, "a")
	if err != nil || bytes.Compare(body, []byte("<div>a</div>\n")) != 0 {
		log.Error.Println(err)
		t.FailNow()
	}
}

func BenchmarkFullProxy(b *testing.B) {
	// Benchmark proxying data with both types of trace strings
	for i := 0; i < b.N; i++ {
		if _, err := makeRequest(ts.URL, "test1=zzXSSzz&test2=zzPLAINzz"); err != nil {
			log.Error.Println(err)
			b.FailNow()
		}
	}
}

func BenchmarkFullProxyTLS(b *testing.B) {
	// Benchmark proxying data with both types of trace strings
	for i := 0; i < b.N; i++ {
		if _, err := makeRequest(tstls.URL, "test1=zzXSSzz&test2=zzPLAINzz"); err != nil {
			log.Error.Println(err)
			b.FailNow()
		}
	}
}

func proxier(r *http.Request) (*url.URL, error) {
	// Need a function to return what the proxy url for an http.Transport

	return &url.URL{
		Scheme: "http",
		Host:   configure.Current.ProxyServer.Addr(),
	}, nil
}

func setupProxy() (net.Listener, error) {
	// Minimal steps needed to setup the proxy, configure the
	// logging, setup the DB, create a proxy object and let it accept
	log.Configure()
	configure.Setup()
	if err := store.Open(configure.Current.DatabasePath, log.Verbose); err != nil {
		log.Error.Fatal(err.Error())
		return nil, err
	}
	certsJSON, err := ioutil.ReadFile(configure.Current.CertCachePath)
	if err != nil {
		certsJSON = []byte("[]")
		// Can recover from this. Simply make a cache file and
		// instantiate an empty cache.
		ioutil.WriteFile(configure.Current.CertCachePath, certsJSON, os.ModePerm)
	}

	var certs []CertCacheEntry
	if err := json.Unmarshal(certsJSON, &certs); err != nil {
		log.Error.Print(err)
		return nil, err
	}

	cache := make(map[string]tls.Certificate)
	for _, cert := range certs {
		keyPEM := cert.Certs.KeyPEM
		certPEM := cert.Certs.CertPEM

		cachedCert, err := tls.X509KeyPair(
			pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: certPEM}),
			pem.EncodeToMemory(&pem.Block{
				Type:  "EC PRIVATE KEY",
				Bytes: keyPEM}))

		if err != nil {
			log.Error.Println(err)
			continue
		}

		cache[cert.Host] = cachedCert
	}
	SetCertCache(cache)
	configure.Certificates()
	ln, h, t := configure.ProxyServer()
	p := New(ln, h, t)
	go p.Accept()
	return ln, nil
}

func setupHTTPTestServer() *httptest.Server {
	// Simple echo server that responds with the incoming body
	// wrapped in divs to make sure the tracer is working on both ends
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		fmt.Fprintln(w, "<div>"+string(body)+"</div>")
	}))
	return ts
}

func setupHTTPSTestServer() *httptest.Server {
	// Simple echo server that responds with the incoming body
	// wrapped in divs to make sure the tracer is working on both ends
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		fmt.Fprintln(w, "<div>"+string(body)+"</div>")
	}))
	return ts
}

func makeRequest(url string, b string) ([]byte, error) {
	// Given a url & a body, will make a post request to that
	// url through the proxy.
	request, err := http.NewRequest("post", url, bufio.NewReader(strings.NewReader(b)))
	if err != nil {
		return nil, err
	}
	defer request.Body.Close()
	transport := &http.Transport{
		Proxy: proxier,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	client := &http.Client{
		Transport: transport,
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}
