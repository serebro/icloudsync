package icloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Endpoints are variables only so tests can point them at a fake server.
var (
	authRootEndpoint = "https://idmsa.apple.com"
	authEndpoint     = "https://idmsa.apple.com/appleauth/auth"
	homeEndpoint     = "https://www.icloud.com"
	setupEndpoint    = "https://setup.icloud.com/setup/ws/1"
)

const (
	widgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

// Response headers Apple uses to carry auth state between requests.
var sessionHeaders = map[string]string{
	"X-Apple-ID-Account-Country": "account_country",
	"X-Apple-ID-Session-Id":      "session_id",
	"X-Apple-Session-Token":      "session_token",
	"X-Apple-TwoSV-Trust-Token":  "trust_token",
	"scnt":                       "scnt",
}

// Prompter is how the client talks to a human (2FA codes etc).
type Prompter interface {
	Interactive() bool
	ReadLine(prompt string) (string, error)
	Println(a ...any)
}

type Client struct {
	HTTP     *http.Client
	jar      *Jar
	sess     *SessionFile
	sessPath string
	prompter Prompter

	traceMu sync.Mutex
	trace   io.Writer

	dataMu sync.Mutex // guards sess.Data (responses arrive concurrently)

	account *AccountData
	params  url.Values

	// Warnf, when set, receives user-facing notices such as retried requests.
	Warnf func(format string, a ...any)
}

// warnf reports a notice to the user (if Warnf is set) and to the trace.
func (c *Client) warnf(format string, a ...any) {
	if c.Warnf != nil {
		c.Warnf(format, a...)
	}
	c.tracef(format, a...)
}

type AccountData struct {
	DsInfo struct {
		Dsid                      string `json:"dsid"`
		HsaVersion                int    `json:"hsaVersion"`
		HasICloudQualifyingDevice bool   `json:"hasICloudQualifyingDevice"`
		FullName                  string `json:"fullName"`
	} `json:"dsInfo"`
	HsaChallengeRequired bool `json:"hsaChallengeRequired"`
	HsaTrustedBrowser    bool `json:"hsaTrustedBrowser"`
	Webservices          map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	} `json:"webservices"`
	DomainToUse string `json:"domainToUse"`
}

func defaultTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
}

// newTransport is swapped in tests.
var newTransport = defaultTransport

func NewClient(sess *SessionFile, sessPath string, prompter Prompter, trace io.Writer) *Client {
	jar := NewJar(sess.Cookies)
	transport := newTransport()
	return &Client{
		HTTP:     &http.Client{Jar: jar, Transport: transport, CheckRedirect: stripAppleHeadersOnRedirect},
		jar:      jar,
		sess:     sess,
		sessPath: sessPath,
		prompter: prompter,
		trace:    trace,
	}
}

// stripAppleHeadersOnRedirect keeps Apple session headers from following a
// redirect to another host (Go forwards custom headers; only Authorization
// and Cookie are dropped on its own).
func stripAppleHeadersOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		for name := range req.Header {
			if l := strings.ToLower(name); l == "scnt" || strings.HasPrefix(l, "x-apple-") {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

// Persist saves cookies and session headers to the session file.
func (c *Client) Persist() error {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	c.sess.Cookies = c.jar.Snapshot()
	return c.sess.Save(c.sessPath)
}

func (c *Client) setData(key, value string) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	if value == "" {
		delete(c.sess.Data, key)
	} else {
		c.sess.Data[key] = value
	}
}

func (c *Client) data(key string) string {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	return c.sess.Data[key]
}

func (c *Client) Session() *SessionFile { return c.sess }

// Do sends a request with the default iCloud headers, records Apple session
// headers and (optionally) writes a redacted trace line.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if req.Header.Get("Origin") == "" {
		req.Header.Set("Origin", homeEndpoint)
		req.Header.Set("Referer", homeEndpoint+"/")
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = redactURL(req.URL) // keep dsid and signed content URLs out of logs
		}
		c.tracef("%s %s -> error: %v", req.Method, redactURL(req.URL), err)
		return nil, err
	}
	c.dataMu.Lock()
	for h, key := range sessionHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.sess.Data[key] = v
		}
	}
	c.dataMu.Unlock()
	var errBody []byte
	if resp.StatusCode >= 400 {
		errBody, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
	}
	if c.trace != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s -> %d (%s)\n", req.Method, redactURL(req.URL), resp.StatusCode, time.Since(start).Round(time.Millisecond))
		for name, vals := range resp.Header {
			fmt.Fprintf(&b, "    < %s: %s\n", name, redactHeader(name, strings.Join(vals, "; ")))
		}
		if len(errBody) > 0 && (req.URL.Host == "idmsa.apple.com" || req.URL.Host == "setup.icloud.com") {
			// sign-in and account answers can carry tokens and personal data
			fmt.Fprintf(&b, "    body: <%d bytes omitted>\n", len(errBody))
		} else if len(errBody) > 0 {
			s := string(errBody)
			if len(s) > 1500 {
				s = s[:1500] + "..."
			}
			fmt.Fprintf(&b, "    body: %s\n", s)
		}
		c.tracef("%s", strings.TrimRight(b.String(), "\n"))
	}
	return resp, nil
}

func (c *Client) tracef(format string, a ...any) {
	if c.trace == nil {
		return
	}
	c.traceMu.Lock()
	defer c.traceMu.Unlock()
	fmt.Fprintf(c.trace, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, a...)...)
}

func redactHeader(name, value string) string {
	switch strings.ToLower(name) {
	case "set-cookie":
		// keep cookie names and attributes, drop values
		parts := strings.Split(value, "; ")
		for i, p := range parts {
			if k, _, ok := strings.Cut(p, "="); ok && !isCookieAttr(k) {
				parts[i] = k + "=<redacted>"
			}
		}
		return strings.Join(parts, "; ")
	case "scnt", "location", "x-apple-id-session-id", "x-apple-auth-attributes":
		return "<redacted>"
	}
	l := strings.ToLower(name)
	for _, w := range []string{"token", "session", "auth", "challenge", "grant", "oauth", "secret", "key"} {
		if strings.Contains(l, w) {
			return "<redacted>"
		}
	}
	return value
}

func isCookieAttr(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "domain", "path", "expires", "max-age", "samesite":
		return true
	}
	return false
}

// redactPath hides account identifiers that some services put in the URL
// path: the dsid (a long number) and the shared-streams account/album prefix.
func redactPath(p string) string {
	if i := strings.Index(p, "/sharedstreams/"); i > 0 {
		p = "/x" + p[i:]
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if len(part) >= 6 && strings.Trim(part, "0123456789") == "" {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

func redactURL(u *url.URL) string {
	cp := *u
	q := cp.Query()
	for k := range q {
		switch strings.ToLower(k) {
		case "dsid", "document_id", "clientid":
			q.Set(k, "x")
		}
	}
	cp.RawQuery = q.Encode()
	cp.Path, cp.RawPath = redactPath(cp.Path), ""
	if strings.Contains(cp.Host, "icloud-content.com") {
		cp.Path = "/<content>"
		cp.RawQuery = ""
	}
	return cp.String()
}

// APIError is a non-success HTTP answer from Apple.
type APIError struct {
	Status  int
	Code    string
	Message string
	URL     string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s: HTTP %d, %s (%s)", e.URL, e.Status, msg, e.Code)
	}
	return fmt.Sprintf("%s: HTTP %d, %s", e.URL, e.Status, msg)
}

func newAPIError(resp *http.Response) *APIError {
	u := resp.Request.URL.Host + redactPath(resp.Request.URL.Path)
	if strings.Contains(resp.Request.URL.Host, "icloud-content.com") {
		u = resp.Request.URL.Host + "/<content>"
	}
	e := &APIError{Status: resp.StatusCode, URL: u}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var parsed struct {
		ServiceErrors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"serviceErrors"`
		ServiceErrorsSnake []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"service_errors"`
		ErrorCode       any    `json:"errorCode"`
		ErrorMessage    string `json:"errorMessage"`
		ServerErrorCode string `json:"serverErrorCode"`
		Reason          string `json:"reason"`
		Error           any    `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		switch {
		case len(parsed.ServiceErrors) > 0:
			e.Code, e.Message = parsed.ServiceErrors[0].Code, parsed.ServiceErrors[0].Message
		case len(parsed.ServiceErrorsSnake) > 0:
			e.Code, e.Message = parsed.ServiceErrorsSnake[0].Code, parsed.ServiceErrorsSnake[0].Message
		case parsed.ServerErrorCode != "":
			e.Code, e.Message = parsed.ServerErrorCode, parsed.Reason
		case parsed.ErrorMessage != "":
			e.Message = parsed.ErrorMessage
			if parsed.ErrorCode != nil {
				e.Code = fmt.Sprint(parsed.ErrorCode)
			}
		case parsed.Reason != "":
			e.Message = parsed.Reason
		}
	}
	if e.Code == "" {
		e.Code = resp.Header.Get("X-Apple-I-Ercd")
	}
	return e
}

// doJSON sends body (nil, []byte/string raw, or a value to marshal) and decodes the reply into out.
// Any status outside okStatuses becomes an *APIError.
func (c *Client) doJSON(ctx context.Context, method, rawURL string, headers http.Header, body any, out any, okStatuses ...int) (*http.Response, error) {
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	if rdr != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if len(okStatuses) == 0 {
		okStatuses = []int{http.StatusOK}
	}
	ok := false
	for _, s := range okStatuses {
		if resp.StatusCode == s {
			ok = true
		}
	}
	if !ok {
		return resp, newAPIError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp, err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return resp, nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("%s: unexpected response: %w", req.URL.Host+req.URL.Path, err)
		}
	}
	return resp, nil
}

// Params are the query parameters iCloud web services expect.
func (c *Client) Params() url.Values {
	out := url.Values{}
	for k, v := range c.params {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (c *Client) ServiceURL(name string) (string, error) {
	if c.account == nil {
		return "", errors.New("not authenticated")
	}
	ws, ok := c.account.Webservices[name]
	if !ok || ws.URL == "" {
		return "", fmt.Errorf("iCloud service %q is not available for this account", name)
	}
	return strings.TrimRight(ws.URL, "/"), nil
}

func (c *Client) Account() *AccountData { return c.account }

// OpenTrace opens a debug trace file (append mode, 0600).
func OpenTrace(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// IsTransient reports whether retrying err later may succeed.
func IsTransient(err error) bool {
	if errors.Is(err, ErrStalled) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		// CloudKit asks to retry: "Failed to provide read-consistent view" (409),
		// throttling and temporary unavailability
		case "TRY_AGAIN_LATER", "RETRY_LATER", "THROTTLED", "SERVICE_UNAVAILABLE", "ZONE_BUSY":
			return true
		}
		return apiErr.Status >= 500 || apiErr.Status == http.StatusTooManyRequests
	}
	// *url.Error itself implements net.Error, so look at what it wraps: only
	// genuine network trouble is worth waiting for, not e.g. a bad certificate
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return true
	case errors.As(err, &opErr) && opErr.Op != "remote error": // TLS alerts are not network trouble
		return true
	case errors.As(err, &netErr) && netErr.Timeout():
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "http2: server sent GOAWAY") ||
		strings.Contains(msg, "http2: client connection lost") || strings.Contains(msg, "unexpected EOF")
}

// IsAuthError reports whether err means the iCloud session died (sign in again).
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 421 || apiErr.Status == 450)
}
