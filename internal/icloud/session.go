package icloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crypto/rand"
)

// SessionFile is everything persisted between runs. It is written with mode
// 0600. The password is only stored when the user explicitly asks for it.
type SessionFile struct {
	AppleID   string            `json:"apple_id"`
	ClientID  string            `json:"client_id"`
	Data      map[string]string `json:"session_data"`
	Cookies   []StoredCookie    `json:"cookies"`
	Password  string            `json:"password,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func DefaultSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "icloudsync", "session.json"), nil
}

func newClientID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("auth-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// LoadSession reads the session file; a missing file yields an empty session.
func LoadSession(path string) (*SessionFile, error) {
	s := &SessionFile{}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, s); err != nil {
			return nil, fmt.Errorf("session file %s is corrupt: %w", path, err)
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(path, 0o600)
		}
	}
	if s.Data == nil {
		s.Data = map[string]string{}
	}
	if s.ClientID == "" {
		s.ClientID = newClientID()
	}
	return s, nil
}

// Save writes the session atomically with restrictive permissions.
func (s *SessionFile) Save(path string) error {
	s.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SameAccount reports whether the stored session belongs to appleID.
func (s *SessionFile) SameAccount(appleID string) bool {
	return s.AppleID == "" || strings.EqualFold(s.AppleID, appleID)
}

// Reset drops all authentication state (keeps nothing but the client id).
func (s *SessionFile) Reset(appleID string) {
	*s = SessionFile{AppleID: appleID, ClientID: newClientID(), Data: map[string]string{}}
}
