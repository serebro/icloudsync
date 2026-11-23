package notify

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseEvents(t *testing.T) {
	ev, err := ParseEvents("interaction, partial")
	if err != nil || !ev[EventInteraction] || !ev[EventPartial] || ev[EventError] {
		t.Fatalf("%v %v", ev, err)
	}
	if _, err := ParseEvents("interaction,bogus"); err == nil {
		t.Fatal("accepted an unknown event")
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{To: "a@b.c"}).Validate(); err == nil {
		t.Fatal("e-mail without a host was accepted")
	}
	if err := (Config{To: "not an address", Host: "h"}).Validate(); err == nil {
		t.Fatal("bad recipient was accepted")
	}
	if err := (Config{To: "a@b.c", Host: "h"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (Config{}).Enabled() {
		t.Fatal("empty config is enabled")
	}
}

func TestScript(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\nprintf '%s|%s|%s' \"$ICLOUDSYNC_EVENT\" \"$ICLOUDSYNC_APPLE_ID\" \"$ICLOUDSYNC_MESSAGE\" > " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Config{Script: script, AppleID: "me@example.com"}
	if err := c.Notify(context.Background(), EventInteraction, "subject", "body"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "interaction|me@example.com|subject\nbody" {
		t.Fatalf("script saw %q", got)
	}
	// a failing script is reported, not fatal
	bad := filepath.Join(dir, "bad.sh")
	_ = os.WriteFile(bad, []byte("#!/bin/sh\necho nope >&2\nexit 3\n"), 0o755)
	if err := (Config{Script: bad}).Notify(context.Background(), EventError, "s", "b"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

func TestHeaderInjectionIsBlocked(t *testing.T) {
	c := Config{To: "a@b.c", Host: "h", From: "x@y.z"}
	_, msg := c.message("hello\r\nBcc: evil@example.com", "body")
	if strings.Contains(string(msg), "\r\nBcc:") {
		t.Fatalf("subject injected a header:\n%s", msg)
	}
}

// a minimal SMTP server that records one message
func fakeSMTP(t *testing.T) (port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		var sb strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					got <- sb.String()
					w("250 queued")
					continue
				}
				sb.WriteString(line + "\n")
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				w("250 fake")
			case strings.HasPrefix(line, "MAIL"):
				sb.WriteString(line + "\n")
				w("250 ok")
			case strings.HasPrefix(line, "RCPT"):
				sb.WriteString(line + "\n")
				w("250 ok")
			case line == "DATA":
				inData = true
				w("354 go")
			case line == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return port, got
}

func TestSendMailPlain(t *testing.T) {
	port, got := fakeSMTP(t)
	c := Config{To: "me@example.com", From: "icloudsync@example.com", Host: "127.0.0.1", Port: port, NoTLS: true}
	if err := c.Notify(context.Background(), EventInteraction, "iCloud needs you", "run login"); err != nil {
		t.Fatal(err)
	}
	m := <-got
	for _, want := range []string{"MAIL FROM:<icloudsync@example.com>", "RCPT TO:<me@example.com>", "Subject: iCloud needs you", "run login"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
}

func TestSendMailRefusesPlainWithoutOptIn(t *testing.T) {
	port, _ := fakeSMTP(t)
	c := Config{To: "me@example.com", Host: "127.0.0.1", Port: port}
	if err := c.Notify(context.Background(), EventError, "s", "b"); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a server without STARTTLS must be refused unless --smtp-no-tls: %v", err)
	}
}
