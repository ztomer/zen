/*
 * This file contains some code originally licensed under the GPL-3.0 license.
 * Original Author: Andrey Meshkov <am@adguard.com>
 * Original Source: https://github.com/AdguardTeam/gomitmproxy
 *
 * Modifications made by: Ansar Smagulov <me@anfragment.net>
 *
 * This modified code is licensed under the GPL-3.0 license. The full text of the GPL-3.0 license
 * is included in the COPYING file in the root of this project.
 *
 * Note: This project as a whole is licensed under the MIT License, but this particular file,
 * due to its use of GPL-3.0 licensed code, is an exception and remains licensed under the GPL-3.0.
 */
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anfragment/zen/certmanager"
	"github.com/anfragment/zen/config"
	"github.com/anfragment/zen/filter"
)

type Proxy struct {
	port           int
	filter         *filter.Filter
	server         *http.Server
	ignoredHosts   []string
	ignoredHostsMu sync.RWMutex
	ctx            context.Context
}

func NewProxy(ctx context.Context) *Proxy {
	p := Proxy{
		filter:       filter.NewFilter(),
		ignoredHosts: []string{},
		ctx:          ctx,
	}

	return &p
}

// Start starts the proxy on the given address.
func (p *Proxy) Start() error {
	p.filter.Init()
	if err := certmanager.GetCertManager().Init(); err != nil {
		log.Printf("failed to initialize certmanager: %v", err)
		return fmt.Errorf("initialize certmanager: %v", err)
	}
	p.initExclusionList()

	p.server = &http.Server{
		Handler: p,
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", "127.0.0.1", config.Config.GetPort()))
	if err != nil {
		return fmt.Errorf("listen: %v", err)
	}
	p.port = listener.Addr().(*net.TCPAddr).Port
	log.Printf("proxy listening on port %d", p.port)

	go func() {
		if err := p.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
		}
	}()

	if err := p.setSystemProxy(); err != nil {
		return fmt.Errorf("set system proxy: %v", err)
	}

	return nil
}

func (p *Proxy) initExclusionList() {
	var wg sync.WaitGroup
	wg.Add(len(exclusionListURLs))
	for _, url := range exclusionListURLs {
		go func(url string) {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				log.Printf("failed to get exclusion list: %v", err)
				return
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				host := strings.TrimSpace(scanner.Text())
				if len(host) == 0 || strings.HasPrefix(host, "#") {
					continue
				}

				p.ignoredHostsMu.Lock()
				p.ignoredHosts = append(p.ignoredHosts, host)
				p.ignoredHostsMu.Unlock()
			}
			if err := scanner.Err(); err != nil {
				log.Printf("error scanning exclusion list: %v", err)
			}
		}(url)
	}
	wg.Wait()
}

// Stop stops the proxy.
// If clearCaches is true, the certificate cache will be cleared.
func (p *Proxy) Stop(purgeCache bool) error {
	if p.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := p.server.Shutdown(ctx); err != nil {
		// As per documentation:
		// Shutdown does not attempt to close nor wait for hijacked connections such as WebSockets. The caller of Shutdown should separately notify such long-lived connections of shutdown and wait for them to close, if desired. See RegisterOnShutdown for a way to register shutdown notification functions.
		// TODO: implement websocket shutdown
		log.Printf("shutdown failed: %v", err)
	}

	if err := p.unsetSystemProxy(); err != nil {
		return fmt.Errorf("unset system proxy: %v", err)
	}

	if purgeCache {
		certmanager.GetCertManager().PurgeCache()
		p.filter = nil
	}

	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.proxyConnect(w, r)
	} else {
		p.proxyHTTP(w, r)
	}
}

// proxyHTTP proxies the HTTP request to the remote server.
func (p *Proxy) proxyHTTP(w http.ResponseWriter, r *http.Request) {
	if res := p.filter.HandleRequest(p.ctx, r); res != nil {
		res.Write(w)
		return
	}

	if isWS(r) {
		// should we remove hop-by-hop headers here?
		p.proxyWebsocket(w, r)
		return
	}

	client := &http.Client{
		// let the client handle any redirects
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	r.RequestURI = ""

	removeConnectionHeaders(r.Header)
	removeHopHeaders(r.Header)

	resp, err := client.Do(r)
	if err != nil {
		log.Printf("client.Do: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	removeConnectionHeaders(resp.Header)
	removeHopHeaders(resp.Header)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyConnect proxies the initial CONNECT and subsequent data between the
// client and the remote server.
func (p *Proxy) proxyConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Fatal("http server does not support hijacking")
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		log.Printf("hijacking connection(%s): %v", r.Host, err)
		return
	}
	defer clientConn.Close()

	removeHopHeaders(r.Header)
	removeConnectionHeaders(r.Header)

	if res := p.filter.HandleRequest(p.ctx, r); res != nil {
		res.Write(clientConn)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		log.Printf("splitting host and port(%s): %v", r.Host, err)
		return
	}

	if !p.shouldMITM(host) || net.ParseIP(host) != nil {
		// TODO: implement upstream certificate sniffing
		// https://docs.mitmproxy.org/stable/concepts-howmitmproxyworks/#complication-1-whats-the-remote-hostname
		p.tunnel(clientConn, r)
		return
	}

	tlsCert, err := certmanager.GetCertManager().GetCertificate(host)
	if err != nil {
		log.Printf("getting certificate(%s): %v", r.Host, err)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		log.Printf("writing 200 OK to client(%s): %v", r.Host, err)
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
	}

	tlsConn := tls.Server(clientConn, tlsConfig)
	defer tlsConn.Close()
	connReader := bufio.NewReader(tlsConn)

	for {
		req, err := http.ReadRequest(connReader)
		if err != nil {
			if err != io.EOF {
				if strings.Contains(err.Error(), "tls: ") {
					log.Printf("adding %s to ignored hosts", host)
					p.ignoredHostsMu.Lock()
					p.ignoredHosts = append(p.ignoredHosts, host)
					p.ignoredHostsMu.Unlock()
				}

				log.Printf("reading request(%s): %v", r.Host, err)
			}
			break
		}

		req.URL.Host = r.Host

		if isWS(req) {
			p.proxyWebsocketTLS(w, req, tlsConfig, tlsConn)
			break
		}

		req.URL.Scheme = "https"

		if res := p.filter.HandleRequest(p.ctx, req); res != nil {
			res.Write(tlsConn)
			break
		}

		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			if strings.Contains(err.Error(), "tls: ") {
				log.Printf("adding %s to ignored hosts", host)
				p.ignoredHostsMu.Lock()
				p.ignoredHosts = append(p.ignoredHosts, host)
				p.ignoredHostsMu.Unlock()
			}

			log.Printf("roundtrip(%s): %v", r.Host, err)
			tlsConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			break
		}

		if err := resp.Write(tlsConn); err != nil {
			log.Printf("writing response(%s): %v", r.Host, err)
			resp.Body.Close()
			break
		}

		if (resp.ContentLength == 0 || resp.ContentLength == -1) &&
			!resp.Close &&
			resp.ProtoAtLeast(1, 1) &&
			!resp.Uncompressed &&
			(len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked") {
			resp.Body.Close()
			break
		}

		resp.Body.Close()
	}
}

// shouldMITM returns true if the host should be MITM'd.
func (p *Proxy) shouldMITM(host string) bool {
	p.ignoredHostsMu.RLock()
	defer p.ignoredHostsMu.RUnlock()

	for _, ignoredHost := range p.ignoredHosts {
		if strings.HasSuffix(host, ignoredHost) {
			return false
		}
	}

	return true
}

// tunnel tunnels the connection between the client and the remote server
// without inspecting the traffic.
func (p *Proxy) tunnel(w net.Conn, r *http.Request) {
	remoteConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		log.Printf("dialing remote(%s): %v", r.Host, err)
		w.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer remoteConn.Close()

	if _, err := w.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		log.Printf("writing 200 OK to client(%s): %v", r.Host, err)
		return
	}

	linkBidirectionalTunnel(w, remoteConn)
}

func linkBidirectionalTunnel(src, dst io.ReadWriter) {
	doneC := make(chan struct{}, 2)
	go tunnelConn(src, dst, doneC)
	go tunnelConn(dst, src, doneC)
	<-doneC
	<-doneC
}

// tunnelConn tunnels the data between src and dst.
func tunnelConn(dst io.Writer, src io.Reader, done chan<- struct{}) {
	if _, err := io.Copy(dst, src); err != nil && !isCloseable(err) {
		log.Printf("copying: %v", err)
	}
	done <- struct{}{}
}

// isCloseable returns true if the error is one that indicates the connection
// can be closed.
func isCloseable(err error) (ok bool) {
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	switch err {
	case io.EOF, io.ErrClosedPipe, io.ErrUnexpectedEOF:
		return true
	default:
		return false
	}
}

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
// Note: this may be out of date, see RFC 7230 Section 6.1
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // spelling per https://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopHeaders(header http.Header) {
	for _, h := range hopHeaders {
		header.Del(h)
	}
}

// removeConnectionHeaders removes hop-by-hop headers listed in the "Connection"
// header of h. See RFC 7230, section 6.1
func removeConnectionHeaders(h http.Header) {
	for _, f := range h["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
}
