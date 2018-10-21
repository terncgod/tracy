package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/nccgroup/tracy/api/common"
	"github.com/nccgroup/tracy/api/store"
	"github.com/nccgroup/tracy/api/types"
	"github.com/nccgroup/tracy/configure"
	"github.com/nccgroup/tracy/log"
)

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 1024*4)
	},
}

// Proxy is the object that stores the configured proxy TCP listener
// for the tracy proxy.
type Proxy struct {
	Listener        net.Listener
	HTTPTransport   http.Transport
	WebSocketDialer websocket.Dialer
}

// Accept is the wrapper function for accepting TCP connections for
// the proxy.
func (p *Proxy) Accept() {
	for {
		conn, err := p.Listener.Accept()
		if err != nil {
			log.Error.Println(err)
			continue
		}

		go p.handleConnection(conn)
	}

}

// New instantiates a Proxy object with the passed in net.Listener,
// http.Transport, and websocket.Dialer.
func New(listener net.Listener, transport http.Transport, dialer websocket.Dialer) *Proxy {
	return &Proxy{
		Listener:        listener,
		HTTPTransport:   transport,
		WebSocketDialer: dialer,
	}
}

// identifyRequestsForGenereateTracer is a helper that looks for generated
// tracer payloads that might not have had a request associated with them
// yet. When the proxy finds one, it associates that HTTP request with the
// payload so that there is a record of where the input was sent to the
// server for the fist time.
func identifyRequestsforGeneratedTracer(dump, method string) {
	// TODO: probably should change this to a websocket notifier so that we
	// don't have to do this database lookup.
	tracersBytes, err := common.GetTracers()
	if err != nil {
		log.Error.Print(err)
		return
	}

	var requests []types.Request
	err = json.Unmarshal(tracersBytes, &requests)
	if err != nil {
		log.Error.Print(err)
		return
	}

	for _, req := range requests {
		for _, tracer := range req.Tracers {
			if !strings.Contains(dump, tracer.TracerPayload) ||
				req.RawRequest != "GENERATED" {
				continue
			}
			req.RawRequest = dump
			req.RequestMethod = method

			err = store.DB.Save(&req).Error
			if err != nil {
				log.Error.Print(err)
				return
			}
			common.UpdateSubscribers(req)
		}
	}
}

// identifyTracersInResponse looks for all the registered tracers in an HTTP
// response and makes an event for each of them.
func identifyTracersInResponse(b []byte, reqURL *url.URL, gziped bool) {
	if configure.HostInWhitelist(reqURL.Host) {
		return
	}

	// Check that the server actually sent compressed data
	if gziped {
		reader, err := gzip.NewReader(bytes.NewReader(b))
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Error.Print(err)
			return
		}

		if reader == nil {
			return
		}

		defer reader.Close()
		b, err = ioutil.ReadAll(reader)

	}

	requestsJSON, err := common.GetTracers()
	if err != nil {
		log.Error.Print(err)
		return
	}

	var requests []types.Request
	err = json.Unmarshal(requestsJSON, &requests)
	if err != nil {
		log.Error.Print(err)
		return
	}

	if len(requests) == 0 {
		return
	}

	tracers := findTracersInResponseBody(string(b), reqURL.String(), requests)

	if len(tracers) == 0 {
		return
	}

	// We have to do this first so that we can get the ID of the raw event
	// and insert it with the event structure.
	rawEvent, err := common.AddEventData(string(b))
	if err != nil {
		log.Error.Print(err)
		return
	}

	for _, tracer := range tracers {
		for _, event := range tracer.TracerEvents {
			//TODO: should probably make a bulk add events function
			event.RawEventID = rawEvent.ID
			event.RawEvent = rawEvent

			if err = store.DB.First(&tracer, "id = ?", event.TracerID).Error; err != nil {
				// Don't print errors about the key being unique in the DB.
				if !strings.Contains(err.Error(), "UNIQUE") {
					log.Error.Println(err)
					return
				}
			}
			_, err = common.AddEvent(tracer, event)
			if err != nil {
				if !strings.Contains(err.Error(), "UNIQUE") {
					log.Error.Print(err)
					return
				}
			}
		}
	}
}

// handleConnect handles the upgrade requests for 'CONNECT' HTTP requests.
func handleConnect(client net.Conn, request *http.Request) (net.Conn, *http.Request, bool, error) {
	// Try to upgrade the 'CONNECT' request to a TLS connection with
	// the configured certificate.
	c, isHTTPS, err := upgradeConnectionTLS(client, request.URL.Host)
	if err != nil {
		log.Warning.Println(err)
		return nil, nil, isHTTPS, err
	}

	// After the connection has been upgraded, reread the request
	// structure over TLS.
	req, err := http.ReadRequest(bufio.NewReader(c))

	if err != nil {
		return nil, nil, isHTTPS, err
	}

	return c, req, isHTTPS, nil
}

// handleConnection handles any TCP connections to the proxy. Client refers to
// the client making the connection to the proxy. Server refers to the actual
// server they are attempting to connect to after going through the proxy.
func (p *Proxy) handleConnection(client net.Conn) {
	// Read a request structure from the TCP connection.
	request, err := http.ReadRequest(bufio.NewReader(client))

	if err == io.EOF {
		return
	}

	if err != nil {
		log.Error.Print(err)
		return
	}

	// If the request method is 'CONNECT', it's either a TLS connection or a
	// websocket.

	// URL is not fully built, so parse it here and rebuild it
	port := "80"
	protocol := "http"
	isHTTPS := false
	if request.Method == http.MethodConnect {
		client, request, isHTTPS, err = handleConnect(client, request)
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Error.Print(err)
			return
		}

		if isHTTPS {
			port = "443"
			protocol = "https"
		}
	}

	// ReadRequest doesn't properly build the request object from a raw socket
	// by itself, so we need to ammend some of the fields so we can use them
	// later.

	// RequestURI is not supposed to be set if we are sending it back again
	//request.RequestURI = ""

	ports := strings.Split(request.Host, ":")
	var host string
	if len(ports) == 2 {
		host = request.Host
	} else {
		host = request.Host + ":" + port
	}
	var path string
	if request.URL.RawQuery != "" {
		path = fmt.Sprintf("%s?%s", request.URL.Path, request.URL.RawQuery)
	} else {
		path = fmt.Sprintf("%s", request.URL.Path)
	}

	u, err := url.Parse(protocol + "://" + host + path)
	if err != nil {
		return
	}
	request.URL = u

	if request.Header.Get("Upgrade") == "websocket" {
		if request.URL.Scheme == "https" {
			request.URL.Scheme = "wss"
		} else {
			request.URL.Scheme = "ws"
		}
	}

	// Dump the request structure as a slice of bytes.
	if log.Verbose {
		dump, _ := httputil.DumpRequest(request, true)
		log.Trace.Println(string(dump))
	}

	// Moving the client close here for the same reason as request, above.
	if client != nil {
		defer client.Close()
	}

	// Requests with the custom HTTP header "X-TRACY" are opting out of the
	// swapping of tracy payloads. Also, we keep a whitelist of hosts that
	// shouldn't swap out tracy payloads so that recursion issues don't
	// happen. For example, tracy payloads will occur all over the UI and
	// we don't want those to be swapped.
	xTracyHeader := request.Header.Get("X-TRACY")
	if !configure.HostInWhitelist(request.URL.Host) && xTracyHeader == "" {
		// Look for tracers that might have been generated out-of-band
		// using the API. Do this by checking if there exists a tracer,
		// but we have no record of which request it came from.
		dump, err := httputil.DumpRequest(request, true)
		if err != nil {
			log.Error.Print(err)
			return
		}

		go identifyRequestsforGeneratedTracer(string(dump), request.Method)

		// Search through the request for the tracer keyword.
		tracers, err := replaceTracers(request)

		if err != nil {
			log.Error.Println(err)
			return
		}

		// Check if the host is the tracer API server. We don't want to
		// trigger anything if we accidentally proxied a tracer server
		// API call because it will cause a recursion.
		if !configure.HostInWhitelist(request.URL.Host) && len(tracers) > 0 {
			go func() {
				// Have to do this again because we changed the
				// contents of the request and the request object
				// is a pointer.
				dump, err := httputil.DumpRequest(request, true)
				if err != nil {
					log.Error.Print(err)
					return
				}

				req := types.Request{
					RawRequest:    string(dump),
					RequestURL:    request.Host + request.RequestURI,
					RequestMethod: request.Method,
					Tracers:       tracers,
				}

				_, err = common.AddTracer(req)
				if err != nil {
					log.Error.Print(err)
				}
			}()
		}
	}

	var b []byte
	if strings.HasPrefix(xTracyHeader, "GET-CACHE") {
		e := strings.Split(xTracyHeader, ";")
		if len(e) != 2 {
			log.Error.Print(`incorrect usage of GET-CACHE header. expected "GET-CACHE;<BASE64(EXPLOIT:TRACERSTRING)>"`)
			return
		}
		// So that we don't have to keep state in the proxy, encode the
		// tracer string to replace and the exploit in the header.
		et, err := base64.StdEncoding.DecodeString(e[1])
		if err != nil {
			log.Error.Print(err)
			return
		}
		ent := strings.Split(string(et), "--")
		if len(ent) != 2 {
			log.Error.Print(`incorrect usage of GET-CACHE header. expected "GET-CACHE;<BASE64(EXPLOIT:TRACERSTRING)>"`)
			return
		}
		b, err = getCachedResponse(request.URL.String(), request.Method)
		if err != nil {
			// If the cache didn't have an entry for this request and host
			// we should fall back to making the actual HTTP request.
			// TODO: is this the expected behaviour? It might mess things ups.
			return
		}

		// If we are using the cache, swap out the tracer string with the
		// exploit.
		b = bytes.Replace(b, []byte(ent[1]), []byte(ent[0]), 1)
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), request)

		if resp != nil {
			defer resp.Body.Close()
		}

		if err != nil {
			log.Warning.Println(err)
			return
		}

		resp.Write(client)
	} else {
		err = p.connectToTarget(request, client)
		if err == io.EOF {
			return
		}

		if err != nil {
			log.Error.Print(err)
			return
		}
	}
}

// connectToTarget connects to a backend server, sends the
// proxied request, and reads the response as a slice of bytes.
func (p *Proxy) connectToTarget(req *http.Request, client net.Conn) error {
	// If we are prepping the cache, remove any cache headers.
	prepingCache := strings.EqualFold(req.Header.Get("X-TRACY"), "set-cache")
	if prepingCache {
		req.Header.Del("If-None-Match")
		req.Header.Del("Etag")
		req.Header.Del("Cache-Control")
	}

	if req.Header.Get("Upgrade") == "websocket" {
		concat := []byte(req.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
		sha1 := sha1.Sum(concat)
		challengeResponse := base64.StdEncoding.EncodeToString(sha1[:])

		req.Header.Del("Sec-Websocket-Version")
		req.Header.Del("Sec-Websocket-Extensions")
		req.Header.Del("Sec-Websocket-Key")
		req.Header.Del("Connection")
		req.Header.Del("Upgrade")

		server, resp, err := p.WebSocketDialer.Dial(req.URL.String(), req.Header)
		if err != nil {
			log.Warning.Print(err)
			return err
		}

		defer server.Close()

		//replace the challenge response
		resp.Header.Set("Sec-WebSocket-Accept", challengeResponse)
		// tell the client you are switching protocols.
		resp.Write(client)

		// then bridge the sockets together and send bytes to eachother
		s := server.UnderlyingConn()
		go bridge(client, s)
		bridge(s, client)
		return nil
	}

	resp, err := p.HTTPTransport.RoundTrip(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		log.Error.Print(err)
		return err
	}

	var save bytes.Buffer
	b, err := ioutil.ReadAll(io.TeeReader(resp.Body, &save))
	if err != nil {
		log.Error.Print(err)
		return err
	}

	resp.Body = ioutil.NopCloser(&save)

	// If the request has the X-TRACY: SET-CACHE value, make sure
	// we cache this response.
	if prepingCache {
		// If we are prepping the cache, create a copy of the
		// response so we can work with it on our own without
		// slowing up the proxy.
		respb, err := httputil.DumpResponse(resp, true)
		if err != nil {
			log.Warning.Print(err)
			return err
		}

		go prepCache(respb, req.URL, req.Method)
	} else {
		// Search for any known tracers in the response. Since the list of tracers
		// might get large, perform this operation in a goroutine.
		// The proxy can finish this connection before this finishes.
		// We don't need to do this in the cases where we are prepping the cache.
		gzip := strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip")
		go identifyTracersInResponse(b, req.URL, gzip)
	}

	// Write the response back to the client.
	resp.Write(client)
	return nil
}

// prepCache processes a copy of the HTTP response so that it is uncompressed
// and unchunked before storing it in the cache. This makes it easier for us
// to swap out the payloads.
func prepCache(respbc []byte, reqURL *url.URL, method string) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(respbc)), nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == io.EOF {
		return
	}
	if err != nil {
		log.Error.Print(err)
		return
	}

	// NOTE: ioutil.ReadAll handles all the chunking.
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error.Print(err)
		return
	}
	//...but we still need to modify our cached HTTP response properly.
	resp.TransferEncoding = []string{}

	// Check that the server actually sent compressed data.
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(bytes.NewReader(b))
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Error.Print(err)
			return
		}

		if gr == nil {
			return
		}

		defer gr.Close()
		b, err = ioutil.ReadAll(gr)

		if err != nil {
			log.Error.Print(err)
			return
		}
	}
	//...and update the body and content length after normalizing
	// chunking and compression.
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Del("Content-Encoding")
	respb, err := httputil.DumpResponse(resp, true)

	if err != nil {
		log.Error.Print(err)
		return
	}

	setCacheResponse(reqURL, method, respb)
}

// cacheResponse adds a cache entry in the proxy cache for the HTTP response
// corresponding to the method and URL. This happens only if the request
// has been marked with the HTTP request header SET-CACHE.
func setCacheResponse(reqURL *url.URL, method string, resp []byte) {
	r := &requestCacheSet{
		url:    reqURL.String(),
		method: method,
		resp:   resp,
	}

	requestCacheSetChan <- r
}

// getCachedResponse gets a cache entry from the proxy cache based on a
// request method and URL. This happens only if the request has been marked
// with the HTTP request header FROM-CACHE.
func getCachedResponse(url, method string) ([]byte, error) {
	r := &requestCacheGet{
		url:    url,
		method: method,
		ok:     make(chan bool),
		resp:   make(chan []byte),
	}
	requestCacheGetChan <- r
	if ok := <-r.ok; !ok {
		// Collecting the resp so that it is clear for the next cache request.
		<-r.resp
		err := fmt.Errorf("The request didn't have a cache entry")
		log.Warning.Print(err)
		return []byte{}, err
	}

	return <-r.resp, nil

}

// bridge reads bytes from one connection and writes them to another connection.
// TODO: add code to look for tracy payloads in websockets so that
// we can identify when sources of input include data coming from the server in
// websocket.
func bridge(src, dst net.Conn) {
	buf := bufferPool.Get().([]byte)

	// CopyBuffer copies between the two parties until an EOF is found.
	if _, err := io.CopyBuffer(src, dst, buf); err != nil {
		log.Error.Println(err)
	}

	bufferPool.Put(buf)
}
