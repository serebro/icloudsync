package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/serebro/icloudsync/internal/creds"
	"github.com/serebro/icloudsync/internal/icloud"
)

// the tests must never read or write the real keychain
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

// these cases never reach the network
func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.json")
	missing := filepath.Join(dir, "not-mounted")
	t.Setenv("ICLOUDSYNC_DEST", "")
	t.Setenv("ICLOUD_APPLE_ID", "")
	t.Setenv("ICLOUD_PASSWORD", "")
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, exitUsage},
		{"help", []string{"--help"}, exitOK},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"unknown flag", []string{"sync", "--dset", dir}, exitUsage},
		{"sync without dest", []string{"sync", "--session", sess}, exitUsage},
		{"only flags exclusive", []string{"sync", "--session", sess, "--dest", dir, "--only-drive", "--only-photos"}, exitUsage},
		// flag parsing stops at a stray argument; it must not silently drop --dry-run
		{"stray argument", []string{"sync", "--session", sess, "--dest", dir, "extra", "--dry-run"}, exitUsage},
		{"stray argument status", []string{"status", "--session", sess, "x"}, exitUsage},
		{"drive not mounted", []string{"sync", "--session", sess, "--dest", missing}, exitError},
		// no Apple ID and no terminal: a human is needed
		{"sync without apple id", []string{"sync", "--session", sess, "--dest", dir}, exitInteraction},
		{"status without session", []string{"status", "--session", sess}, exitOK},
		{"logout without session", []string{"logout", "--session", sess}, exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a missing destination was created")
	}
}

func TestLogoutKeepsPasswordOnRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s, err := icloud.LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	s.AppleID, s.Password = "me@example.com", "pw"
	s.Data["session_token"] = "tok"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"logout", "--session", path, "--keep-password"}); got != exitOK {
		t.Fatalf("exit %d", got)
	}
	s2, err := icloud.LoadSession(path)
	if err != nil || s2.Password != "pw" || s2.Data["session_token"] != "" {
		t.Fatalf("%+v %v", s2, err)
	}
	if got := run([]string{"logout", "--session", path}); got != exitOK {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout left the session file")
	}
}

func newTestApp(t *testing.T, filePassword string) *app {
	t.Helper()
	t.Setenv("ICLOUD_PASSWORD", "")
	sess := &icloud.SessionFile{AppleID: "me@example.com", Password: filePassword, Data: map[string]string{}}
	return &app{sess: sess, tty: &tty{}} // stdin is not a terminal under go test
}

func TestPasswordSourceOrder(t *testing.T) {
	keyring.MockInit()
	// keychain beats the session file
	if err := credStore.Set("me@example.com", "from-keychain"); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, "from-file")
	if pw, err := a.password(); err != nil || pw != "from-keychain" || a.pwSource != pwKeyring {
		t.Fatalf("got %q %v source %v", pw, err, a.pwSource)
	}
	// the environment beats the keychain
	t.Setenv("ICLOUD_PASSWORD", "from-env")
	if pw, _ := a.password(); pw != "from-env" || a.pwSource != pwEnv {
		t.Fatalf("got %q source %v", pw, a.pwSource)
	}
	// no keychain entry: the session file
	t.Setenv("ICLOUD_PASSWORD", "")
	_ = credStore.Delete("me@example.com")
	a = newTestApp(t, "from-file")
	if pw, err := a.password(); err != nil || pw != "from-file" || a.pwSource != pwStored {
		t.Fatalf("got %q %v source %v", pw, err, a.pwSource)
	}
}

func TestUnavailableKeychainFallsThrough(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	defer keyring.MockInit()
	a := newTestApp(t, "from-file")
	if pw, err := a.password(); err != nil || pw != "from-file" {
		t.Fatalf("got %q %v", pw, err)
	}
	// nothing anywhere and no terminal: a human is needed, not a crash
	a = newTestApp(t, "")
	if _, err := a.password(); !errors.Is(err, icloud.ErrInteractionRequired) {
		t.Fatalf("got %v", err)
	}
}

func TestLogoutClearsKeychain(t *testing.T) {
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "session.json")
	s, _ := icloud.LoadSession(path)
	s.AppleID = "me@example.com"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	_ = credStore.Set("me@example.com", "pw")
	if got := run([]string{"logout", "--session", path, "--keep-password"}); got != exitOK {
		t.Fatalf("exit %d", got)
	}
	if _, err := credStore.Get("me@example.com"); err != nil {
		t.Fatalf("--keep-password removed the keychain entry: %v", err)
	}
	_ = s.Save(path)
	if got := run([]string{"logout", "--session", path}); got != exitOK {
		t.Fatalf("exit %d", got)
	}
	if _, err := credStore.Get("me@example.com"); !errors.Is(err, creds.ErrNotFound) {
		t.Fatalf("logout left the keychain entry: %v", err)
	}
}

func TestLoginFlagsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if got := run([]string{"login", "--session", path, "--keyring", "--save-password"}); got != exitUsage {
		t.Fatalf("exit %d", got)
	}
}

func TestNotifyOnInteraction(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"$ICLOUDSYNC_EVENT\" >> "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(dir, "session.json")
	t.Setenv("ICLOUDSYNC_DEST", "")
	t.Setenv("ICLOUD_APPLE_ID", "")
	t.Setenv("ICLOUD_PASSWORD", "")
	// no Apple ID and no terminal: exit 3, which announces "interaction"
	if got := run([]string{"sync", "--session", sess, "--dest", dir, "--notify-script", hook}); got != exitInteraction {
		t.Fatalf("exit %d", got)
	}
	if b, _ := os.ReadFile(out); string(b) != "interaction\n" {
		t.Fatalf("script output %q", b)
	}
	// "partial" is not in the default event list; a failing script must not change the exit code
	_ = os.Remove(out)
	bad := filepath.Join(dir, "bad.sh")
	_ = os.WriteFile(bad, []byte("#!/bin/sh\nexit 9\n"), 0o755)
	if got := run([]string{"sync", "--session", sess, "--dest", dir, "--notify-script", bad}); got != exitInteraction {
		t.Fatalf("a failing notifier changed the exit code to %d", got)
	}
	// a drive that is not mounted is exit 1: silent unless "error" is requested
	missing := filepath.Join(dir, "gone")
	if got := run([]string{"sync", "--session", sess, "--dest", missing, "--notify-script", hook}); got != exitError {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("notified for an event that was not requested")
	}
	if got := run([]string{"sync", "--session", sess, "--dest", missing, "--notify-script", hook, "--notify-on", "error"}); got != exitError {
		t.Fatalf("exit %d", got)
	}
	if b, _ := os.ReadFile(out); string(b) != "error\n" {
		t.Fatalf("script output %q", b)
	}
}

func TestNotifyFlagValidation(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.json")
	for _, args := range [][]string{
		{"sync", "--session", sess, "--dest", dir, "--notify-email", "a@b.c"},                      // no --smtp-host
		{"sync", "--session", sess, "--dest", dir, "--notify-script", "x", "--notify-on", "bogus"}, // unknown event
	} {
		if got := run(args); got != exitUsage {
			t.Errorf("run(%q) = %d, want usage error", args, got)
		}
	}
}

func TestPhotoAndTrashFlagValidation(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.json")
	base := []string{"sync", "--session", sess, "--dest", dir}
	for name, extra := range map[string][]string{
		"unknown size":         {"--size", "huge"},
		"negative recent":      {"--recent", "-1"},
		"albums and album":     {"--albums", "--album", "Family"},
		"force-size, original": {"--force-size"},
		"trash-days no trash":  {"--trash-days", "7"},
		"negative trash-days":  {"--trash", "--trash-days", "-1"},
	} {
		if got := run(append(append([]string{}, base...), extra...)); got != exitUsage {
			t.Errorf("%s: exit %d, want usage error", name, got)
		}
	}
	// valid combinations get past flag parsing (and stop at "no Apple ID, no terminal")
	t.Setenv("ICLOUD_APPLE_ID", "")
	for _, extra := range [][]string{
		{"--size", "original,adjusted"},
		{"--size", "adjusted", "--force-size"},
		{"--album", "A", "--album", "B", "--xmp-sidecar", "--recent", "5", "--until-found", "3"},
		{"--trash", "--trash-days", "30"},
	} {
		if got := run(append(append([]string{}, base...), extra...)); got != exitInteraction {
			t.Errorf("%v: exit %d, want %d", extra, got, exitInteraction)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		if got := runCmd([]string{arg}, &notifyState{}); got != exitOK {
			t.Errorf("%s: exit %d, want %d", arg, got, exitOK)
		}
	}
}
