package icloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signedIn runs a full first login against the fake and returns the session path.
func signedIn(t *testing.T, f *fakeApple, trace io.Writer) (string, *Client) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	sess, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	sess.AppleID = "me@example.com"
	c := NewClient(sess, path, &fakePrompter{interactive: true, lines: []string{"123456"}}, trace)
	if err := c.Authenticate(context.Background(), func() (string, error) { return "secret", nil }); err != nil {
		t.Fatal(err)
	}
	return path, c
}

func reopen(t *testing.T, path string) *Client {
	t.Helper()
	sess, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(sess, path, nil, nil)
}

func (f *fakeApple) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestThrottledValidateDoesNotSignIn(t *testing.T) {
	for _, status := range []int{429, 503} {
		f := &fakeApple{validTokens: map[string]bool{}}
		withFakeApple(t, f)
		path, _ := signedIn(t, f, nil)
		before := f.count("POST /appleauth/auth/signin/init")
		f.validateStatus = status
		err := reopen(t, path).Authenticate(context.Background(), func() (string, error) {
			t.Fatal("password requested although the session was never rejected")
			return "", nil
		})
		if err == nil || errors.Is(err, ErrBadCredentials) {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if f.count("POST /appleauth/auth/signin/init") != before {
			t.Fatalf("status %d: a sign-in was attempted", status)
		}
		// validate was retried (RetryDelays has 3 entries in tests)
		if n := f.count("POST /setup/ws/1/validate"); n < 4 {
			t.Fatalf("status %d: validate called %d times", status, n)
		}
	}
}

func TestRejectedSessionSignsInAgainWithTrustToken(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	path, _ := signedIn(t, f, nil)
	f.validateStatus = 421
	pw := 0
	if err := reopen(t, path).Authenticate(context.Background(), func() (string, error) { pw++; return "secret", nil }); err != nil {
		t.Fatal(err)
	}
	if pw != 1 || f.count("POST /appleauth/auth/signin/complete") != 2 {
		t.Fatalf("pw=%d calls=%v", pw, f.calls)
	}
}

func TestThrottledSignInIsNotABadPassword(t *testing.T) {
	for status, wantBad := range map[int]bool{429: false, 503: false, 401: true, 403: true} {
		f := &fakeApple{validTokens: map[string]bool{}, completeErr: status}
		withFakeApple(t, f)
		c := newTestClient(t, filepath.Join(t.TempDir(), "s.json"), &fakePrompter{interactive: true})
		err := c.Authenticate(context.Background(), func() (string, error) { return "secret", nil })
		if err == nil || errors.Is(err, ErrBadCredentials) != wantBad {
			t.Errorf("status %d: err = %v, want bad credentials %v", status, err, wantBad)
		}
	}
}

func TestTraceIsRedacted(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	var buf bytes.Buffer
	_, c := signedIn(t, f, &buf)
	trace := buf.String()
	if !strings.Contains(trace, "signin/complete") {
		t.Fatalf("trace looks empty:\n%s", trace)
	}
	for _, secret := range []string{"tok-untrusted", "tok-trusted", "trust-1", "scnt-1", "sid-1", "t=abc", "web-ok", "c-token", "dsid=42", c.sess.ClientID} {
		if strings.Contains(trace, secret) {
			t.Errorf("trace leaks %q", secret)
		}
	}
}

func TestRedactPathAndURL(t *testing.T) {
	u, _ := url.Parse("https://p42-sharedstreams.icloud.com/1311947019/sharedstreams/webgetassets?dsid=1311947019&clientId=abc&x=1")
	got := redactURL(u)
	if strings.Contains(got, "1311947019") || strings.Contains(got, "abc") || !strings.Contains(got, "/sharedstreams/webgetassets") {
		t.Fatalf("redactURL = %s", got)
	}
	if p := redactPath("/v2T2ECAEQARoQnoiqJOrw/sharedstreams/webgetassetcount"); p != "/x/sharedstreams/webgetassetcount" {
		t.Fatalf("album path = %s", p)
	}
	u, _ = url.Parse("https://cvws.icloud-content.com/B/secretpath?o=signed")
	if got := redactURL(u); strings.Contains(got, "secretpath") || strings.Contains(got, "signed") {
		t.Fatalf("content URL = %s", got)
	}
	if v := redactHeader("X-Apple-Repair-Session-Token", "s3cr3t"); v != "<redacted>" {
		t.Fatalf("unknown token header kept: %s", v)
	}
	if v := redactHeader("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=abc; Path=/; Domain=.icloud.com"); strings.Contains(v, "abc") || !strings.Contains(v, "Path=/") {
		t.Fatalf("cookie header = %s", v)
	}
	if v := redactHeader("Content-Type", "application/json"); v != "application/json" {
		t.Fatalf("harmless header redacted: %s", v)
	}
}

func TestIsTransientClassification(t *testing.T) {
	dns := &url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}}
	reset := &url.Error{Op: "Get", URL: "u", Err: syscall.ECONNRESET}
	scheme := &url.Error{Op: "Get", URL: "u", Err: errors.New("unsupported protocol scheme \"ftp\"")}
	for err, want := range map[error]bool{
		dns:                             true,
		reset:                           true,
		io.ErrUnexpectedEOF:             true,
		ErrStalled:                      true,
		fmt.Errorf("x: %w", ErrStalled): true,
		&APIError{Status: 503}:          true,
		&APIError{Status: 429}:          true,
		&APIError{Status: 409, Code: "TRY_AGAIN_LATER"}: true,
		scheme:                  false,
		&APIError{Status: 404}:  false,
		&APIError{Status: 401}:  false,
		context.Canceled:        false,
		errors.New("bad input"): false,
	} {
		if got := IsTransient(err); got != want {
			t.Errorf("IsTransient(%v) = %v, want %v", err, got, want)
		}
	}
	if !IsAuthError(&APIError{Status: 421}) || IsAuthError(&APIError{Status: 500}) || IsAuthError(errors.New("x")) {
		t.Fatal("IsAuthError")
	}
}

func TestRedirectToOtherHostDropsAppleHeaders(t *testing.T) {
	var got http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer first.Close()
	c := NewTestClient(t.TempDir(), "42", nil, nil)
	req, _ := http.NewRequest(http.MethodGet, first.URL, nil)
	req.Header.Set("scnt", "s")
	req.Header.Set("X-Apple-ID-Session-Id", "sid")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got == nil || got.Get("scnt") != "" || got.Get("X-Apple-ID-Session-Id") != "" {
		t.Fatalf("headers after redirect: %v", got)
	}
}

func TestStalledDownloadIsAborted(t *testing.T) {
	old := DownloadIdleTimeout
	DownloadIdleTimeout = 100 * time.Millisecond
	defer func() { DownloadIdleTimeout = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select { // then nothing more
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := NewTestClient(t.TempDir(), "42", nil, nil)
	resp, err := c.Fetch(context.Background(), srv.URL+"/f", false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStalled) || !IsTransient(err) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled download was not aborted")
	}
}

func TestDriveFoldersOrderAndMissingItems(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	c := NewTestClient(t.TempDir(), "42", map[string]string{"drivews": srv.URL}, nil)

	body = `[{"drivewsid":"B","items":[{"name":"b"}]},{"drivewsid":"A","items":[],"numberOfItems":0}]`
	got, err := c.DriveFolders(context.Background(), []string{"A", "B"})
	if err != nil || got[0].DrivewsID != "A" || got[1].DrivewsID != "B" {
		t.Fatalf("%+v %v", got, err)
	}
	body = `[{"drivewsid":"A","numberOfItems":0}]` // explicitly empty
	if _, err := c.DriveFolders(context.Background(), []string{"A"}); err != nil {
		t.Fatal(err)
	}
	body = `[{"drivewsid":"A","status":"UNKNOWN"}]` // no child list: not an empty folder
	if _, err := c.DriveFolders(context.Background(), []string{"A"}); err == nil {
		t.Fatal("entry without items accepted as an empty folder")
	}
	body = `[]`
	if _, err := c.DriveFolders(context.Background(), []string{"A"}); err == nil {
		t.Fatal("missing folder accepted")
	}
}

func TestPhotosListStepsOverUnreadablePage(t *testing.T) {
	var ranks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(b, []byte(`"INT64","value":0}`)):
			ranks = append(ranks, "0")
			fmt.Fprint(w, `{"records":[{"recordName":"x1","serverErrorCode":"NOT_FOUND"},{"recordName":"x2","serverErrorCode":"NOT_FOUND"}]}`)
		case bytes.Contains(b, []byte(`"INT64","value":2}`)):
			ranks = append(ranks, "2")
			fmt.Fprintf(w, `{"records":[%s,%s]}`, asset("a1", "m1", 1700000000000), master("m1", "IMG.JPG", "public.jpeg", false, "https://x"))
		default:
			ranks = append(ranks, "end")
			fmt.Fprint(w, `{"records":[]}`)
		}
	}))
	defer srv.Close()
	c := NewTestClient(t.TempDir(), "42", map[string]string{"ckdatabasews": srv.URL}, nil)
	var got []PhotoAsset
	err := c.PhotosList(context.Background(), PhotoLibrary{Kind: "primary", Zone: map[string]any{"zoneName": "PrimarySync"}}, func(p []PhotoAsset) error {
		got = append(got, p...)
		return nil
	})
	if len(got) != 1 || fmt.Sprint(ranks) != "[0 2 end]" {
		t.Fatalf("got %d assets, ranks %v", len(got), ranks)
	}
	if err == nil {
		t.Fatal("skipped records must make the listing untrusted")
	}
}

func TestPhotosCountAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"batch":[{"records":[{"fields":{"itemCount":{"value":12646}}}]}]}`)
	}))
	defer srv.Close()
	c := NewTestClient(t.TempDir(), "42", map[string]string{"ckdatabasews": srv.URL}, nil)
	n, err := c.PhotosCount(context.Background(), PhotoLibrary{Zone: map[string]any{"zoneName": "PrimarySync"}})
	if err != nil || n != 12646 {
		t.Fatalf("count %d %v", n, err)
	}
	// no session at all: Status says so without touching the network
	if _, err := c.Status(context.Background()); !IsSessionInvalid(err) {
		t.Fatalf("status err = %v", err)
	}
}

func TestStatusWithLiveSession(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	path, _ := signedIn(t, f, nil)
	acc, err := reopen(t, path).Status(context.Background())
	if err != nil || acc.DsInfo.Dsid != "42" {
		t.Fatalf("%+v %v", acc, err)
	}
}

func TestSessionFileHandling(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "session.json")
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	s.AppleID, s.Password = "Me@Example.com", "pw"
	s.Data["session_token"] = "t"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", fi.Mode(), di.Mode())
	}
	s2, err := LoadSession(path)
	if err != nil || s2.Data["session_token"] != "t" || s2.ClientID != s.ClientID {
		t.Fatalf("%+v %v", s2, err)
	}
	if !s2.SameAccount("me@example.com") || s2.SameAccount("other@example.com") {
		t.Fatal("SameAccount")
	}
	old := s2.ClientID
	s2.Reset("other@example.com")
	if s2.Password != "" || len(s2.Data) != 0 || s2.ClientID == old || s2.AppleID != "other@example.com" {
		t.Fatalf("reset %+v", s2)
	}
}

func TestJarPersistsAndClears(t *testing.T) {
	j := NewJar(nil)
	u, _ := url.Parse("https://www.icloud.com/a/b")
	j.SetCookies(u, []*http.Cookie{
		{Name: "keep", Value: "1", Domain: ".icloud.com", Path: "/"},
		{Name: "gone", Value: "2", MaxAge: -1},
	})
	other, _ := url.Parse("https://setup.icloud.com/x")
	if cs := j.Cookies(other); len(cs) != 1 || cs[0].Name != "keep" {
		t.Fatalf("domain cookie not sent to a subdomain: %v", cs)
	}
	foreign, _ := url.Parse("https://example.com/")
	if len(j.Cookies(foreign)) != 0 {
		t.Fatal("cookie leaked to another domain")
	}
	j2 := NewJar(j.Snapshot())
	if len(j2.Cookies(other)) != 1 {
		t.Fatal("snapshot lost the cookie")
	}
	j2.Clear()
	if len(j2.Cookies(other)) != 0 {
		t.Fatal("Clear kept cookies")
	}
}

func TestSecondRoundEdgeCases(t *testing.T) {
	tlsAlert := &url.Error{Op: "Get", URL: "u", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}}
	if IsTransient(tlsAlert) {
		t.Fatal("TLS alert treated as transient")
	}
	if isPasswordErrorCode("") || !isPasswordErrorCode("-20101") {
		t.Fatal("isPasswordErrorCode")
	}

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	c := NewTestClient(t.TempDir(), "42", map[string]string{"drivews": srv.URL}, nil)
	body = `[{"drivewsid":"A","items":[]},{"drivewsid":"Z","items":[]}]` // B rewritten as Z
	if _, err := c.DriveFolders(context.Background(), []string{"A", "B"}); err == nil {
		t.Fatal("partially matching response accepted")
	}
	body = `[{"drivewsid":"A","items":[{"name":"x"}],"numberOfItems":3}]`
	if _, err := c.DriveFolders(context.Background(), []string{"A"}); err == nil {
		t.Fatal("truncated item list accepted")
	}
	body = `[{"items":[{"name":"x"}]}]` // ids not echoed at all: positional
	if got, err := c.DriveFolders(context.Background(), []string{"A"}); err != nil || len(got[0].Items) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
