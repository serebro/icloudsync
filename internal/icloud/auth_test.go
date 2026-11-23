package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakePrompter struct {
	interactive bool
	lines       []string
	asked       int
}

func (f *fakePrompter) Interactive() bool { return f.interactive }
func (f *fakePrompter) ReadLine(string) (string, error) {
	if f.asked >= len(f.lines) {
		return "", io.EOF
	}
	f.asked++
	return f.lines[f.asked-1], nil
}
func (f *fakePrompter) Println(...any) {}

// fakeApple emulates the subset of idmsa/setup endpoints the client uses.
type fakeApple struct {
	mu             sync.Mutex
	calls          []string
	trusted        bool
	validTokens    map[string]bool
	completeErr    int // status to return from signin/complete
	validateStatus int // status to force from validate (0: normal)
	pushed         bool
	sms            bool
}

func (f *fakeApple) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	note := func(r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
	}
	requireSessionHeaders := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("scnt") != "scnt-1" || r.Header.Get("X-Apple-ID-Session-Id") != "sid-1" {
			t.Errorf("%s %s: missing scnt/session id headers", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /appleauth/auth/signin/init", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["accountName"] != "me@example.com" || body["a"] == "" {
			t.Errorf("bad init body %v", body)
		}
		if r.Header.Get("Origin") != authRootEndpoint {
			t.Errorf("init origin = %q", r.Header.Get("Origin"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"iteration":1000,"salt":%q,"protocol":"s2k","b":%q,"c":"c-token"}`,
			base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
			base64.StdEncoding.EncodeToString([]byte{0x12, 0x34, 0x56}))
	})
	mux.HandleFunc("POST /appleauth/auth/signin/complete", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if r.URL.Query().Get("isRememberMeEnabled") != "true" {
			t.Error("isRememberMeEnabled missing")
		}
		var body struct {
			C, M1, M2   string
			TrustTokens []string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.C != "c-token" || body.M1 == "" || body.M2 == "" {
			t.Errorf("bad complete body %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if f.completeErr != 0 {
			w.WriteHeader(f.completeErr)
			fmt.Fprint(w, `{"serviceErrors":[{"code":"-20101","message":"Your Apple ID or password was incorrect."}]}`)
			return
		}
		w.Header().Set("scnt", "scnt-1")
		w.Header().Set("X-Apple-ID-Session-Id", "sid-1")
		w.Header().Set("X-Apple-ID-Account-Country", "USA")
		w.Header().Set("X-Apple-Session-Token", "tok-untrusted")
		http.SetCookie(w, &http.Cookie{Name: "aasp", Value: "v=1:t=abc", Path: "/", Quoted: true})
		if len(body.TrustTokens) == 1 && body.TrustTokens[0] == "trust-1" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(409)
		fmt.Fprint(w, `{"authType":"hsa2"}`)
	})
	mux.HandleFunc("GET /appleauth/auth", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if !requireSessionHeaders(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><script type="application/json" class="boot_args">{"direct":{"twoSV":{"phoneNumberVerification":{"trustedPhoneNumbers":[{"id":3,"obfuscatedNumber":"+• (•••) •••-••12"}]}}}}</script></html>`)
	})
	mux.HandleFunc("PUT /appleauth/auth/verify/trusteddevice/securitycode", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		f.pushed = true
		w.WriteHeader(202)
	})
	mux.HandleFunc("POST /appleauth/auth/verify/trusteddevice/securitycode", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if !requireSessionHeaders(w, r) {
			return
		}
		var body struct{ SecurityCode struct{ Code string } }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.SecurityCode.Code != "123456" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Header().Set("X-Apple-I-Ercd", "-21669")
			fmt.Fprint(w, `{"service_errors":[{"code":"-21669","title":"Incorrect Verification Code","message":"Incorrect verification code."}],"hasError":true}`)
			return
		}
		// what idmsa really answers for an accepted code (seen in a live trace)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		fmt.Fprint(w, `{"success":0,"trustedDeviceCount":2,"securityCode":{"code":"123456","valid":true}}`)
	})
	mux.HandleFunc("PUT /appleauth/auth/verify/phone", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		f.sms = true
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /appleauth/auth/verify/phone/securitycode", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		var body struct {
			PhoneNumber  struct{ ID int }
			SecurityCode struct{ Code string }
			Mode         string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.PhoneNumber.ID != 3 || body.SecurityCode.Code != "654321" || body.Mode != "sms" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /appleauth/auth/2sv/trust", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if !requireSessionHeaders(w, r) {
			return
		}
		f.mu.Lock()
		f.trusted = true
		f.mu.Unlock()
		w.Header().Set("X-Apple-Session-Token", "tok-trusted")
		w.Header().Set("X-Apple-TwoSV-Trust-Token", "trust-1")
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /setup/ws/1/accountLogin", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		tok, _ := body["dsWebAuthToken"].(string)
		trusted := tok == "tok-trusted" || (tok == "tok-untrusted" && body["trustToken"] == "trust-1")
		if trusted {
			f.mu.Lock()
			f.validTokens["web-ok"] = true
			f.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "v=2:t=web-ok", Path: "/", Quoted: true})
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"dsInfo":{"dsid":"42","hsaVersion":2,"hasICloudQualifyingDevice":true,"fullName":"Test"},"hsaChallengeRequired":%v,"hsaTrustedBrowser":%v,"webservices":{"drivews":{"url":"https://drive.example"}}}`, !trusted, trusted)
	})
	mux.HandleFunc("POST /setup/ws/1/validate", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if f.validateStatus != 0 {
			w.WriteHeader(f.validateStatus)
			return
		}
		ck, err := r.Cookie("X-APPLE-WEBAUTH-TOKEN")
		f.mu.Lock()
		ok := err == nil && f.validTokens[strings.TrimPrefix(ck.Value, "v=2:t=")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(421)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"dsInfo":{"dsid":"42","hsaVersion":2,"fullName":"Test"},"hsaChallengeRequired":false,"hsaTrustedBrowser":true,"webservices":{}}`)
	})
	return mux
}

func withFakeApple(t *testing.T, f *fakeApple) {
	srv := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(srv.Close)
	saved := []string{authRootEndpoint, authEndpoint, homeEndpoint, setupEndpoint}
	authRootEndpoint = srv.URL
	authEndpoint = srv.URL + "/appleauth/auth"
	homeEndpoint = srv.URL
	setupEndpoint = srv.URL + "/setup/ws/1"
	t.Cleanup(func() {
		authRootEndpoint, authEndpoint, homeEndpoint, setupEndpoint = saved[0], saved[1], saved[2], saved[3]
	})
	insecure := srv.Client().Transport
	newTransport = func() http.RoundTripper { return insecure }
	t.Cleanup(func() { newTransport = defaultTransport })
}

func newTestClient(t *testing.T, path string, p Prompter) *Client {
	sess, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	sess.AppleID = "me@example.com"
	return NewClient(sess, path, p, nil)
}

func TestLoginWith2FAThenReuseSession(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	path := filepath.Join(t.TempDir(), "s", "session.json")

	// first run: wrong code once, then the right one
	p := &fakePrompter{interactive: true, lines: []string{"000000", "123456"}}
	c := newTestClient(t, path, p)
	pwCalls := 0
	if err := c.Authenticate(context.Background(), func() (string, error) { pwCalls++; return "secret", nil }); err != nil {
		t.Fatalf("authenticate: %v\ncalls: %v", err, f.calls)
	}
	if pwCalls != 1 || !f.pushed || !f.trusted {
		t.Fatalf("pw=%d pushed=%v trusted=%v", pwCalls, f.pushed, f.trusted)
	}
	if c.Account().DsInfo.Dsid != "42" || c.Params().Get("dsid") != "42" {
		t.Fatal("account not set")
	}

	// second run from the saved file: no password, no 2FA
	c2 := newTestClient(t, path, &fakePrompter{})
	if c2.Session().Data["trust_token"] != "trust-1" {
		t.Fatalf("trust token not persisted: %v", c2.Session().Data)
	}
	f.calls = nil
	if err := c2.Authenticate(context.Background(), func() (string, error) { t.Fatal("password asked"); return "", nil }); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "POST /setup/ws/1/validate" {
		t.Fatalf("expected only validate, got %v", f.calls)
	}

	// session expired on Apple's side: password sign-in with trust token, no 2FA
	f.validTokens = map[string]bool{}
	c3 := newTestClient(t, path, &fakePrompter{})
	if err := c3.Authenticate(context.Background(), func() (string, error) { return "secret", nil }); err != nil {
		t.Fatalf("re-login with trust token: %v (calls %v)", err, f.calls)
	}
}

func TestSMSCode(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	p := &fakePrompter{interactive: true, lines: []string{"a", "654321"}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "session.json"), p)
	if err := c.Authenticate(context.Background(), func() (string, error) { return "secret", nil }); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !f.sms {
		t.Fatal("sms not requested")
	}
}

func TestNonInteractive2FAFailsFast(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}}
	withFakeApple(t, f)
	c := newTestClient(t, filepath.Join(t.TempDir(), "session.json"), &fakePrompter{interactive: false})
	err := c.Authenticate(context.Background(), func() (string, error) { return "secret", nil })
	if !errors.Is(err, ErrInteractionRequired) {
		t.Fatalf("want ErrInteractionRequired, got %v", err)
	}
	// the next unattended run must not sign in again
	path := c.sessPath
	c2 := newTestClient(t, path, &fakePrompter{interactive: false})
	f.calls = nil
	err = c2.Authenticate(context.Background(), func() (string, error) { t.Fatal("password asked"); return "", nil })
	if !errors.Is(err, ErrInteractionRequired) || strings.Contains(strings.Join(f.calls, " "), "signin") {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	// an interactive login clears the flag
	c3 := newTestClient(t, path, &fakePrompter{interactive: true, lines: []string{"123456"}})
	if err := c3.Authenticate(context.Background(), func() (string, error) { return "secret", nil }); err != nil {
		t.Fatal(err)
	}
	if c3.Session().Data["needs_login"] != "" {
		t.Fatal("needs_login not cleared")
	}
}

func TestBadPassword(t *testing.T) {
	f := &fakeApple{validTokens: map[string]bool{}, completeErr: 401}
	withFakeApple(t, f)
	c := newTestClient(t, filepath.Join(t.TempDir(), "session.json"), &fakePrompter{interactive: true})
	err := c.Authenticate(context.Background(), func() (string, error) { return "wrong", nil })
	if !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("want ErrBadCredentials, got %v", err)
	}
	if !strings.Contains(err.Error(), "incorrect") {
		t.Fatalf("apple message lost: %v", err)
	}
}
