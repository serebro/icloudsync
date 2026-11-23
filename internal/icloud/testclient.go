package icloud

import (
	"io"
	"path/filepath"
)

// NewTestClient returns a client that is already "signed in" with the given
// dsid and web-service base URLs (e.g. {"ckdatabasews": srv.URL}). It is meant
// for tests of packages that consume the client against a fake server.
func NewTestClient(dir, dsid string, services map[string]string, trace io.Writer) *Client {
	path := filepath.Join(dir, "session.json")
	sess := &SessionFile{AppleID: "test@example.com", ClientID: "auth-test", Data: map[string]string{}}
	c := NewClient(sess, path, nil, trace)
	acc := &AccountData{}
	acc.DsInfo.Dsid = dsid
	acc.Webservices = map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	}{}
	for k, v := range services {
		acc.Webservices[k] = struct {
			URL    string `json:"url"`
			Status string `json:"status"`
		}{URL: v, Status: "active"}
	}
	c.setAccount(acc)
	return c
}
