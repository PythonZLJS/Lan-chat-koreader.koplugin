package main

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The browser UI calls its own backend directly. Sending those requests through
// the public listener can deadlock when browser connections fill its limit.
// Responses still stream, rather than buffering uploads/downloads in RAM.
type internalTransport struct{ api *API }
type internalWriter struct {
	header  http.Header
	once    sync.Once
	ready   chan *http.Response
	reader  *io.PipeReader
	writer  *io.PipeWriter
	request *http.Request
}

func (w *internalWriter) Header() http.Header { return w.header }
func (w *internalWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.ready <- &http.Response{StatusCode: code, Status: strconv.Itoa(code) + " " + http.StatusText(code), Header: w.header.Clone(), Body: w.reader, Request: w.request, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	})
}
func (w *internalWriter) Write(b []byte) (int, error) { w.WriteHeader(200); return w.writer.Write(b) }
func (t *internalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	req := request.Clone(request.Context())
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
	req.RemoteAddr = "127.0.0.1:0"
	reader, writer := io.Pipe()
	w := &internalWriter{header: make(http.Header), ready: make(chan *http.Response, 1), reader: reader, writer: writer, request: req}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer writer.Close()
		if req.Body != nil {
			defer req.Body.Close()
		}
		t.api.ServeHTTP(w, req)
		w.WriteHeader(200)
	}()
	go func() {
		select {
		case <-done:
		case <-req.Context().Done():
			if req.Body != nil {
				_ = req.Body.Close()
			}
			_ = reader.CloseWithError(req.Context().Err())
			_ = writer.CloseWithError(req.Context().Err())
		}
	}()
	select {
	case response := <-w.ready:
		return response, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}
func newLocalRemote(origin, pin string, api *API) *Remote {
	jar, _ := cookiejar.New(nil)
	return &Remote{Origin: origin, Pin: pin, Client: &http.Client{Transport: &internalTransport{api: api}, Jar: jar, Timeout: 120 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ownerRemote is available only to the capability-protected local controller.
// It grants the Kindle owner admin rights without exposing a LAN login bypass.
func (a *App) ownerRemote() (*Remote, error) {
	r := a.ownRemote()
	origin, e := url.Parse(r.Origin)
	if e != nil {
		return nil, e
	}
	var uid int64
	if e = a.Store.DB.QueryRow("SELECT id FROM users WHERE username=?", adminUsername).Scan(&uid); e != nil {
		return nil, e
	}
	_, version := a.Store.adminState()
	session := a.API.Sessions.grantOwner(uid, version)
	r.Client.Jar.SetCookies(origin, []*http.Cookie{{Name: "nc_api", Value: session.ID, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}})
	r.CSRF = session.CSRF
	return r, nil
}
