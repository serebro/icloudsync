package icloud

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// StoredCookie is a serializable cookie. Session cookies (no expiry) are kept
// as well, just like pyicloud does (ignore_discard=True): Apple relies on them.
type StoredCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Quoted   bool      `json:"quoted,omitempty"`
	Domain   string    `json:"domain"`
	HostOnly bool      `json:"host_only,omitempty"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires,omitzero"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

// Jar is a minimal persistent http.CookieJar.
type Jar struct {
	mu      sync.Mutex
	cookies []StoredCookie
	now     func() time.Time
}

func NewJar(initial []StoredCookie) *Jar {
	j := &Jar{now: time.Now}
	j.cookies = append(j.cookies, initial...)
	return j
}

func (j *Jar) Snapshot() []StoredCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	out := make([]StoredCookie, 0, len(j.cookies))
	for _, c := range j.cookies {
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (j *Jar) Clear() {
	j.mu.Lock()
	j.cookies = nil
	j.mu.Unlock()
}

func domainMatch(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func defaultPath(u *url.URL) string {
	p := u.Path
	if p == "" || p[0] != '/' {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i == 0 {
		return "/"
	}
	return p[:i]
}

func pathMatch(reqPath, cookiePath string) bool {
	if reqPath == "" {
		reqPath = "/"
	}
	if reqPath == cookiePath {
		return true
	}
	if strings.HasPrefix(reqPath, cookiePath) {
		return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
	}
	return false
}

func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	host := strings.ToLower(u.Hostname())
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	for _, c := range cookies {
		sc := StoredCookie{
			Name: c.Name, Value: c.Value, Quoted: c.Quoted,
			Secure: c.Secure, HTTPOnly: c.HttpOnly, Path: c.Path,
		}
		if c.Domain == "" {
			sc.Domain, sc.HostOnly = host, true
		} else {
			d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
			if !domainMatch(host, d) {
				continue
			}
			sc.Domain = d
		}
		if sc.Path == "" || sc.Path[0] != '/' {
			sc.Path = defaultPath(u)
		}
		expired := false
		switch {
		case c.MaxAge < 0:
			expired = true
		case c.MaxAge > 0:
			sc.Expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		case !c.Expires.IsZero():
			sc.Expires = c.Expires
			expired = !c.Expires.After(now)
		}
		// replace existing cookie with the same identity
		kept := j.cookies[:0]
		for _, old := range j.cookies {
			if old.Name == sc.Name && old.Domain == sc.Domain && old.Path == sc.Path {
				continue
			}
			kept = append(kept, old)
		}
		j.cookies = kept
		if !expired {
			j.cookies = append(j.cookies, sc)
		}
	}
}

func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	host := strings.ToLower(u.Hostname())
	secure := u.Scheme == "https"
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	var matched []StoredCookie
	for _, c := range j.cookies {
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			continue
		}
		if c.HostOnly {
			if host != c.Domain {
				continue
			}
		} else if !domainMatch(host, c.Domain) {
			continue
		}
		if c.Secure && !secure {
			continue
		}
		if !pathMatch(u.Path, c.Path) {
			continue
		}
		matched = append(matched, c)
	}
	sort.SliceStable(matched, func(a, b int) bool { return len(matched[a].Path) > len(matched[b].Path) })
	out := make([]*http.Cookie, len(matched))
	for i, c := range matched {
		out[i] = &http.Cookie{Name: c.Name, Value: c.Value, Quoted: c.Quoted}
	}
	return out
}
