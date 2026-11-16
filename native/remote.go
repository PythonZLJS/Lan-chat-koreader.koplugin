package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Remote struct {
	Origin, Pin, CSRF string
	Client            *http.Client
	Ctx               context.Context
}
type remoteStatusError struct {
	Status  int
	Message string
}

func (e *remoteStatusError) Error() string { return e.Message }

type SessionInfo struct {
	CSRF          string `json:"csrf"`
	Authenticated bool   `json:"authenticated"`
	Admin         bool   `json:"admin"`
	Display       string `json:"display"`
	Version       int    `json:"version"`
	UserID        int64  `json:"user_id"`
}
type ChatData struct {
	Messages    []Message  `json:"messages"`
	Files       []FileInfo `json:"files"`
	Users       []User     `json:"users"`
	More        bool       `json:"more"`
	UploadLimit int64      `json:"upload_limit"`
	Room        string     `json:"room"`
	Rooms       []string   `json:"rooms"`
	RoomChanged bool       `json:"room_changed"`
}
type AdminData struct {
	Pending     []User           `json:"pending"`
	Users       []User           `json:"users"`
	Files       []FileInfo       `json:"files"`
	Messages    []Message        `json:"messages"`
	Counts      map[string]int64 `json:"counts"`
	UploadLimit int64            `json:"upload_limit"`
}

func privateOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	bare := !strings.Contains(raw, "://")
	if bare {
		raw = "https://" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "\\") {
		return "", errors.New("Enter the host's Wi-Fi address, for example 192.168.1.42:8000.")
	}
	ip, e := netip.ParseAddr(u.Hostname())
	if e != nil || !ip.Is4() || !(ip.IsPrivate() || ip.IsLoopback()) || ip.IsUnspecified() {
		return "", errors.New("Use a numeric IPv4 address on your private Wi-Fi network.")
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/kindle" && u.Path != "/login" {
		return "", errors.New("Enter the host address without an extra page path.")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if bare {
			port = "8000"
		}
	}
	if _, e = net.LookupPort("tcp", port); e != nil {
		return "", errors.New("Enter a valid port number.")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", errors.New("Choose a port between 1 and 65535.")
	}
	return "https://" + net.JoinHostPort(ip.String(), port), nil
}
func probeCertificate(origin string) (string, error) {
	u, e := url.Parse(origin)
	if e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 8 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}} // probe only: sends no HTTP request or credentials
	conn, e := dialer.DialContext(ctx, "tcp", u.Host)
	if e != nil {
		return "", errors.New("Could not reach that host. Check Wi-Fi, the address, and that hosting is on.")
	}
	defer conn.Close()
	tlsConn := conn.(*tls.Conn)
	cert := tlsConn.ConnectionState().PeerCertificates[0]
	if time.Now().After(cert.NotAfter) || time.Now().Before(cert.NotBefore) {
		return "", errors.New("The host certificate is expired or not yet valid. Check device dates and the host certificate.")
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}
func newRemote(origin, pin string) *Remote {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableCompression: true, TLSHandshakeTimeout: 8 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("Host certificate missing.")
			}
			cert := state.PeerCertificates[0]
			sum := sha256.Sum256(cert.Raw)
			if !equal(hex.EncodeToString(sum[:]), pin) {
				return errors.New("The host certificate changed. Verify the host again before signing in.")
			}
			if time.Now().After(cert.NotAfter) || time.Now().Before(cert.NotBefore) {
				return errors.New("Host certificate is not currently valid.")
			}
			return nil
		}}}
	return &Remote{Origin: origin, Pin: pin, Client: &http.Client{Transport: transport, Jar: jar, Timeout: 120 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (r *Remote) request(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n\\") {
		return nil, errors.New("Invalid server action.")
	}
	req, e := http.NewRequest(method, r.Origin+path, body)
	if e != nil {
		return nil, e
	}
	if r.Ctx != nil {
		req = req.WithContext(r.Ctx)
	}
	if method == "POST" {
		req.Header.Set("X-CSRF-Token", r.CSRF)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "NeighbourChat/4 Kindle")
	response, e := r.Client.Do(req)
	if e != nil {
		return nil, errors.New("Could not reach the chat server. Check Wi-Fi or reconnect to verify its certificate.")
	}
	return response, nil
}
func (r *Remote) call(method, path string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	response, e := r.request(method, path, body, "application/json")
	if e != nil {
		return e
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if e != nil {
		return errors.New("The reply was interrupted. Refresh and retry.")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &v)
		if v.Error != "" {
			return &remoteStatusError{Status: response.StatusCode, Message: truncate(v.Error, 240)}
		}
		return &remoteStatusError{Status: response.StatusCode, Message: fmt.Sprintf("The server could not complete this action (%d).", response.StatusCode)}
	}
	var csrf struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(b, &csrf) != nil {
		return errors.New("The host returned an invalid response. Use a Neighbour Chat server.")
	}
	if csrf.CSRF != "" {
		if len(csrf.CSRF) > 128 {
			return errors.New("The host returned an invalid session token.")
		}
		r.CSRF = csrf.CSRF
	}
	if result != nil {
		if e = json.Unmarshal(b, result); e != nil {
			return errors.New("The host returned incompatible chat data.")
		}
	}
	return nil
}
func (r *Remote) info() (SessionInfo, error) {
	var v SessionInfo
	e := r.call("GET", "/api/session", nil, &v)
	return v, e
}
func (r *Remote) upload(filename, recipient string, source io.Reader, cancelSource func(), roomHint ...string) error {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		_ = form.WriteField("recipient_id", recipient)
		if len(roomHint) > 0 {
			_ = form.WriteField("room_hint", roomHint[0])
		}
		part, e := form.CreateFormFile("file", filename)
		if e == nil {
			_, e = io.Copy(part, source)
		}
		if e == nil {
			e = form.Close()
		}
		_ = writer.CloseWithError(e)
		done <- e
	}()
	response, e := r.request("POST", "/api/upload", reader, form.FormDataContentType())
	if e != nil {
		reader.CloseWithError(e)
		if cancelSource != nil {
			cancelSource()
		}
		<-done
		return e
	}
	defer response.Body.Close()
	reader.Close()
	if response.StatusCode != 200 && cancelSource != nil {
		cancelSource()
	}
	uploadError := <-done
	b, e := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if e != nil {
		return e
	}
	if response.StatusCode != 200 {
		var v struct{ Error string }
		_ = json.Unmarshal(b, &v)
		if v.Error != "" {
			return errors.New(truncate(v.Error, 240))
		}
		return errors.New("Upload failed. Check the current file limit.")
	}
	return uploadError
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func checkCode(pin string) string {
	pin = strings.ToUpper(pin)
	if len(pin) < 16 {
		return pin
	}
	return pin[:4] + "-" + pin[4:8] + "-" + pin[8:12] + "-" + pin[12:16]
}
