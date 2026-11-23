package creds

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSystemStore(t *testing.T) {
	keyring.MockInit() // never touch the real keychain
	var s Store = System{}
	if _, err := s.Get("Me@Example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	if err := s.Set("Me@Example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	// Apple IDs are case-insensitive
	if pw, err := s.Get("me@example.com"); err != nil || pw != "pw" {
		t.Fatalf("got %q %v", pw, err)
	}
	if err := s.Delete("me@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("me@example.com"); err != nil {
		t.Fatalf("deleting twice must succeed: %v", err)
	}
	if _, err := s.Get("me@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestUnavailableStore(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	if _, err := (System{}).Get("a@b.c"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("an unavailable store must not look empty: %v", err)
	}
}
