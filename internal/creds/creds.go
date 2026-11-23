// Package creds keeps the Apple ID password in the operating system's
// credential store (macOS Keychain, Linux Secret Service, Windows Credential
// Manager) instead of a plain-text file.
package creds

import (
	"errors"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the name entries are filed under in the credential store.
const Service = "icloudsync"

// ErrNotFound: nothing is stored for that Apple ID.
var ErrNotFound = errors.New("no password in the credential store")

// Store is a password store keyed by Apple ID.
type Store interface {
	Get(appleID string) (string, error)
	Set(appleID, password string) error
	Delete(appleID string) error
}

// System is the operating system's credential store.
type System struct{}

func key(appleID string) string { return strings.ToLower(strings.TrimSpace(appleID)) }

// Get returns ErrNotFound when there is no entry. Any other error means the
// store itself is unavailable (no D-Bus session in cron, a locked keychain).
func (System) Get(appleID string) (string, error) {
	pw, err := keyring.Get(Service, key(appleID))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return pw, err
}

func (System) Set(appleID, password string) error {
	return keyring.Set(Service, key(appleID), password)
}

// Delete succeeds when there is nothing to delete.
func (System) Delete(appleID string) error {
	err := keyring.Delete(Service, key(appleID))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
