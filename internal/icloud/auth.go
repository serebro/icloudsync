package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrInteractionRequired: a human is needed (password or 2FA code) but there is no terminal.
	ErrInteractionRequired = errors.New("interaction required")
	// ErrBadCredentials: Apple rejected the Apple ID / password.
	ErrBadCredentials = errors.New("apple id or password rejected")

	errSessionInvalid = errors.New("session is no longer valid")
)

// PasswordFunc supplies the password when a full sign-in is needed.
type PasswordFunc func() (string, error)

func (c *Client) authHeaders(originOverride bool) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/javascript")
	h.Set("Content-Type", "application/json")
	h.Set("X-Apple-OAuth-Client-Id", widgetKey)
	h.Set("X-Apple-OAuth-Client-Type", "firstPartyAuth")
	h.Set("X-Apple-OAuth-Redirect-URI", homeEndpoint)
	h.Set("X-Apple-OAuth-Require-Grant-Code", "true")
	h.Set("X-Apple-OAuth-Response-Mode", "web_message")
	h.Set("X-Apple-OAuth-Response-Type", "code")
	h.Set("X-Apple-OAuth-State", c.sess.ClientID)
	h.Set("X-Apple-Widget-Key", widgetKey)
	if v := c.data("scnt"); v != "" {
		h.Set("scnt", v)
	}
	if v := c.data("session_id"); v != "" {
		h.Set("X-Apple-ID-Session-Id", v)
	}
	if originOverride {
		h.Set("Origin", authRootEndpoint)
		h.Set("Referer", authRootEndpoint+"/")
	}
	return h
}

// Authenticate reuses the stored session while Apple accepts it and falls
// back to a full SRP sign-in (and 2FA when Apple asks for it).
// At most one password attempt is made per call.
func (c *Client) Authenticate(ctx context.Context, password PasswordFunc) error {
	if c.sess.AppleID == "" {
		return errors.New("apple id is not set")
	}
	if c.data("session_token") != "" {
		acc, err := c.validate(ctx)
		switch {
		case err == nil && !needs2FA(acc):
			c.setAccount(acc)
			return c.Persist()
		case err == nil, errors.Is(err, errSessionInvalid):
			// fall through to a fresh sign-in
		default:
			return err
		}
	}

	interactive := c.prompter != nil && c.prompter.Interactive()
	if !interactive && c.data("needs_login") != "" {
		// a previous unattended run already learned that Apple wants a 2FA code;
		// signing in again from cron would only add failed attempts to the Apple ID
		return fmt.Errorf("%w: Apple asks for a two-factor code; run `icloudsync login` in a terminal", ErrInteractionRequired)
	}
	pw, err := password()
	if err != nil {
		return err
	}
	if err := c.signin(ctx, pw); err != nil {
		_ = c.Persist()
		return err
	}
	acc, err := c.accountLogin(ctx)
	if err != nil {
		_ = c.Persist()
		return err
	}
	if needs2FA(acc) {
		if !interactive {
			c.setData("needs_login", "1")
			_ = c.Persist()
			return fmt.Errorf("%w: Apple asks for a two-factor code; run `icloudsync login` in a terminal", ErrInteractionRequired)
		}
		acc, err = c.twoFactor(ctx)
		if err != nil {
			_ = c.Persist()
			return err
		}
	}
	c.setData("needs_login", "")
	c.setAccount(acc)
	return c.Persist()
}

func needs2FA(a *AccountData) bool {
	return a.DsInfo.HsaVersion >= 1 && (a.HsaChallengeRequired || !a.HsaTrustedBrowser)
}

func (c *Client) setAccount(a *AccountData) {
	c.account = a
	c.params = url.Values{
		"clientBuildNumber":     {"2522Project44"},
		"clientMasteringNumber": {"2522B2"},
		"clientId":              {c.sess.ClientID},
		"dsid":                  {a.DsInfo.Dsid},
	}
}

func (c *Client) validate(ctx context.Context) (*AccountData, error) {
	var acc AccountData
	// network hiccups (DNS timeouts etc.) are retried; validate never counts as a sign-in attempt
	_, err := Retry(ctx, func(err error, d time.Duration) {
		c.warnf("checking the session failed (%v), retrying in %s", err, d)
	}, func() (struct{}, error) {
		acc = AccountData{}
		_, err := c.doJSON(ctx, http.MethodPost, setupEndpoint+"/validate", nil, "null", &acc)
		return struct{}{}, err
	})
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		// 421/450/500 (and other 4xx) mean the session is gone; throttling and
		// other server trouble must not burn a sign-in attempt
		if apiErr.Status == http.StatusTooManyRequests || apiErr.Status > 500 {
			return nil, err
		}
		c.tracef("validate: session rejected (%d)", apiErr.Status)
		return nil, errSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if acc.DsInfo.Dsid == "" {
		return nil, errSessionInvalid
	}
	return &acc, nil
}

func (c *Client) signin(ctx context.Context, password string) error {
	srp, err := newSRPClient(c.sess.AppleID)
	if err != nil {
		return err
	}
	initReq := map[string]any{
		"a":           base64.StdEncoding.EncodeToString(srp.PublicA()),
		"accountName": c.sess.AppleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	}
	var initResp struct {
		Iteration int    `json:"iteration"`
		Salt      string `json:"salt"`
		Protocol  string `json:"protocol"`
		B         string `json:"b"`
		C         string `json:"c"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, authEndpoint+"/signin/init", c.authHeaders(true), initReq, &initResp); err != nil {
		// init never sees the password, so its errors say nothing about it
		return fmt.Errorf("sign-in init failed: %w", err)
	}
	salt, err1 := base64.StdEncoding.DecodeString(initResp.Salt)
	b, err2 := base64.StdEncoding.DecodeString(initResp.B)
	if err := errors.Join(err1, err2); err != nil || len(b) == 0 {
		return fmt.Errorf("sign-in init: malformed response: %v", err)
	}
	key, err := deriveSRPPassword(password, initResp.Protocol, salt, initResp.Iteration)
	if err != nil {
		return err
	}
	m1, m2, err := srp.ProcessChallenge(salt, b, key)
	if err != nil {
		return err
	}
	trust := []string{}
	if t := c.data("trust_token"); t != "" {
		trust = append(trust, t)
	}
	completeReq := map[string]any{
		"accountName": c.sess.AppleID,
		"c":           initResp.C,
		"m1":          base64.StdEncoding.EncodeToString(m1),
		"m2":          base64.StdEncoding.EncodeToString(m2),
		"rememberMe":  true,
		"trustTokens": trust,
	}
	resp, err := c.doJSON(ctx, http.MethodPost, authEndpoint+"/signin/complete?isRememberMeEnabled=true",
		c.authHeaders(true), completeReq, nil, http.StatusOK, http.StatusConflict, http.StatusPreconditionFailed)
	if err != nil {
		var apiErr *APIError
		// only an explicit rejection means a wrong password; throttling (429)
		// or a server error must not get a stored password deleted
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized ||
			apiErr.Status == http.StatusForbidden && isPasswordErrorCode(apiErr.Code)) {
			return fmt.Errorf("%w: %v", ErrBadCredentials, err)
		}
		return fmt.Errorf("sign-in failed: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusConflict:
		c.tracef("signin: two-factor authentication required")
	case http.StatusPreconditionFailed:
		// non-2FA accounts must acknowledge a "repair" step
		if _, err := c.doJSON(ctx, http.MethodPost, authEndpoint+"/repair/complete", c.authHeaders(false), "{}", nil,
			http.StatusOK, http.StatusNoContent); err != nil {
			return fmt.Errorf("sign-in repair step failed: %w", err)
		}
	}
	return nil
}

// isPasswordErrorCode: Apple's codes for a wrong password (-20101) and an
// account locked by failed attempts (-20209, -20283). Other 403s (throttling,
// challenges) must not get a stored password deleted.
func isPasswordErrorCode(code string) bool {
	return code == "-20101" || code == "-20209" || code == "-20283"
}

func (c *Client) accountLogin(ctx context.Context) (*AccountData, error) {
	req := map[string]any{
		"accountCountryCode": c.data("account_country"),
		"dsWebAuthToken":     c.data("session_token"),
		"extended_login":     true,
		"trustToken":         c.data("trust_token"),
	}
	var acc AccountData
	if _, err := c.doJSON(ctx, http.MethodPost, setupEndpoint+"/accountLogin", nil, req, &acc); err != nil {
		return nil, fmt.Errorf("iCloud account login failed: %w", err)
	}
	if acc.DomainToUse != "" {
		return nil, fmt.Errorf("apple requires the %s domain for this account (China mainland accounts are not supported)", acc.DomainToUse)
	}
	if acc.DsInfo.Dsid == "" {
		return nil, errors.New("iCloud account login returned no account data")
	}
	return &acc, nil
}

type trustedPhone struct {
	ID     int    `json:"id"`
	Number string `json:"obfuscatedNumber"`
}

var bootArgsRe = regexp.MustCompile(`(?s)<script[^>]*class="boot_args"[^>]*>(.*?)</script>`)

func parseTrustedPhones(html []byte) []trustedPhone {
	m := bootArgsRe.FindSubmatch(html)
	if m == nil {
		return nil
	}
	type pnv struct {
		TrustedPhoneNumbers []trustedPhone `json:"trustedPhoneNumbers"`
	}
	var boot struct {
		Direct struct {
			TwoSV struct {
				PhoneNumberVerification pnv `json:"phoneNumberVerification"`
				BridgeInitiateData      struct {
					PhoneNumberVerification pnv `json:"phoneNumberVerification"`
				} `json:"bridgeInitiateData"`
			} `json:"twoSV"`
		} `json:"direct"`
	}
	if json.Unmarshal(m[1], &boot) != nil {
		return nil
	}
	phones := boot.Direct.TwoSV.PhoneNumberVerification.TrustedPhoneNumbers
	if len(phones) == 0 {
		phones = boot.Direct.TwoSV.BridgeInitiateData.PhoneNumberVerification.TrustedPhoneNumbers
	}
	return phones
}

func (c *Client) trustedPhones(ctx context.Context) []trustedPhone {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authEndpoint, nil)
	if err != nil {
		return nil
	}
	h := c.authHeaders(false)
	h.Del("Content-Type")
	h.Set("Accept", "text/html, */*")
	req.Header = h
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil
	}
	return parseTrustedPhones(body)
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type codeResult struct {
	SecurityCode struct {
		Valid *bool `json:"valid"`
	} `json:"securityCode"`
}

func (c *Client) twoFactor(ctx context.Context) (*AccountData, error) {
	phones := c.trustedPhones(ctx)

	// Ask Apple to push a code to trusted devices (needed by the 2026+ flow; failure is not fatal).
	h := c.authHeaders(false)
	h.Set("Accept", "application/json")
	h.Del("Content-Type")
	if _, err := c.doJSON(ctx, http.MethodPut, authEndpoint+"/verify/trusteddevice/securitycode", h, nil, nil,
		http.StatusOK, http.StatusAccepted, http.StatusNoContent); err != nil {
		c.tracef("2fa: push trigger failed: %v", err)
	}

	p := c.prompter
	p.Println("Two-factor authentication is required for this Apple ID.")
	p.Println("A code has been sent to your trusted Apple devices.")
	letters := "abcdefghijklmnopqrstuvwxyz"
	if len(phones) > len(letters) {
		phones = phones[:len(letters)]
	}
	for i, ph := range phones {
		p.Println(fmt.Sprintf("  %c: send an SMS to %s", letters[i], strings.ReplaceAll(ph.Number, "•", "*")))
	}

	const maxAttempts = 3
	var smsPhone *trustedPhone
	for attempt := 1; attempt <= maxAttempts; {
		prompt := "Enter the 6-digit code"
		if smsPhone == nil && len(phones) > 0 {
			prompt += " or a letter for SMS"
		}
		in, err := p.ReadLine(prompt + ": ")
		if err != nil {
			return nil, err
		}
		in = strings.ToLower(strings.TrimSpace(in))
		if smsPhone == nil && len(in) == 1 && strings.Contains(letters[:len(phones)], in) {
			ph := phones[strings.Index(letters, in)]
			body := map[string]any{"phoneNumber": map[string]any{"id": ph.ID}, "mode": "sms"}
			sh := c.authHeaders(false)
			sh.Set("Content-Type", "application/json; charset=utf-8")
			if _, err := c.doJSON(ctx, http.MethodPut, authEndpoint+"/verify/phone", sh, body, nil,
				http.StatusOK, http.StatusAccepted, http.StatusNoContent); err != nil {
				return nil, fmt.Errorf("could not send SMS: %w", err)
			}
			smsPhone = &ph
			p.Println("SMS sent.")
			continue
		}
		if !isDigits(in, 6) {
			p.Println("The code must be 6 digits.")
			continue
		}
		var (
			verr   error
			result codeResult
			resp   *http.Response
		)
		if smsPhone != nil {
			body := map[string]any{"phoneNumber": map[string]any{"id": smsPhone.ID}, "securityCode": map[string]any{"code": in}, "mode": "sms"}
			sh := c.authHeaders(false)
			sh.Set("Content-Type", "application/json; charset=utf-8")
			sh.Set("Accept", "application/json; charset=utf-8")
			resp, verr = c.doJSON(ctx, http.MethodPost, authEndpoint+"/verify/phone/securitycode", sh, body, &result,
				http.StatusOK, http.StatusNoContent, http.StatusConflict)
		} else {
			dh := c.authHeaders(false)
			dh.Set("Accept", "application/json")
			body := map[string]any{"securityCode": map[string]any{"code": in}}
			resp, verr = c.doJSON(ctx, http.MethodPost, authEndpoint+"/verify/trusteddevice/securitycode", dh, body, &result,
				http.StatusOK, http.StatusNoContent, http.StatusConflict)
		}
		// Apple's current flow answers an accepted code with 409 + securityCode.valid=true
		// (icloudpd treats 409 as success too); only an explicit valid=false means rejection.
		if verr == nil && resp.StatusCode == http.StatusConflict && result.SecurityCode.Valid != nil && !*result.SecurityCode.Valid {
			verr = &APIError{Status: resp.StatusCode, Code: "-21669", Message: "Incorrect verification code.", URL: resp.Request.URL.Host + resp.Request.URL.Path}
		}
		if verr == nil {
			c.tracef("2fa: code accepted (HTTP %d)", resp.StatusCode)
			break
		}
		var apiErr *APIError
		if errors.As(verr, &apiErr) && apiErr.Code == "-21669" && attempt < maxAttempts {
			p.Println("Code rejected: " + apiErr.Message + " Try again.")
			attempt++
			continue
		}
		return nil, fmt.Errorf("two-factor code rejected: %w", verr)
	}

	// Mark this session as trusted so future runs skip 2FA until Apple expires it.
	if _, err := c.doJSON(ctx, http.MethodGet, authEndpoint+"/2sv/trust", c.authHeaders(false), nil, nil,
		http.StatusOK, http.StatusNoContent); err != nil {
		return nil, fmt.Errorf("could not trust session: %w", err)
	}
	acc, err := c.accountLogin(ctx)
	if err != nil {
		return nil, err
	}
	if needs2FA(acc) {
		return nil, errors.New("apple still requires two-factor authentication after the code was accepted")
	}
	return acc, nil
}

// Status checks whether the stored session is still accepted, without signing in.
func (c *Client) Status(ctx context.Context) (*AccountData, error) {
	if c.data("session_token") == "" {
		return nil, errSessionInvalid
	}
	acc, err := c.validate(ctx)
	if err != nil {
		return nil, err
	}
	if needs2FA(acc) {
		return acc, errSessionInvalid
	}
	_ = c.Persist()
	return acc, nil
}

// IsSessionInvalid reports whether err means "sign in again".
func IsSessionInvalid(err error) bool { return errors.Is(err, errSessionInvalid) }
