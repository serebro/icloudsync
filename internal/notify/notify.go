// Package notify tells a human that an unattended run needs attention: it runs
// a user-supplied script and/or sends an e-mail.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Events a notification can be sent for.
const (
	EventInteraction = "interaction" // the sign-in expired: a password or 2FA code is needed
	EventPartial     = "partial"     // finished, but some files failed or deletions were blocked
	EventError       = "error"       // the run failed
)

// ParseEvents turns "interaction,partial" into a set; unknown names are an error.
func ParseEvents(list string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		switch e {
		case EventInteraction, EventPartial, EventError:
			out[e] = true
		default:
			return nil, fmt.Errorf("unknown notification event %q (use %s, %s, %s)", e, EventInteraction, EventPartial, EventError)
		}
	}
	return out, nil
}

// Config says where notifications go.
type Config struct {
	Script string // program run with ICLOUDSYNC_EVENT / _MESSAGE / _APPLE_ID in its environment

	To       string // e-mail recipient
	From     string // sender; defaults to the SMTP username, then to To
	Host     string
	Port     int
	Username string
	Password string // from $ICLOUDSYNC_SMTP_PASSWORD, never from a flag
	NoTLS    bool

	Events  map[string]bool
	AppleID string
}

func (c Config) Enabled() bool { return c.Script != "" || c.To != "" }

// Wants reports whether event should be announced.
func (c Config) Wants(event string) bool { return c.Events[event] }

// Validate checks the settings before a long run starts.
func (c Config) Validate() error {
	if c.To != "" {
		if c.Host == "" {
			return errors.New("--notify-email needs --smtp-host")
		}
		if _, err := mail.ParseAddress(c.To); err != nil {
			return fmt.Errorf("--notify-email: %w", err)
		}
		if c.From != "" {
			if _, err := mail.ParseAddress(c.From); err != nil {
				return fmt.Errorf("--notify-email-from: %w", err)
			}
		}
	}
	return nil
}

// Notify delivers the message through every configured channel and returns
// the errors of the channels that failed. A failure here never changes how the
// program exits.
func (c Config) Notify(ctx context.Context, event, subject, body string) error {
	var errs []error
	if c.Script != "" {
		if err := c.runScript(ctx, event, subject+"\n"+body); err != nil {
			errs = append(errs, fmt.Errorf("notification script: %w", err))
		}
	}
	if c.To != "" {
		if err := c.sendMail(ctx, subject, body); err != nil {
			errs = append(errs, fmt.Errorf("notification e-mail: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (c Config) runScript(ctx context.Context, event, message string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Script)
	cmd.Env = append(os.Environ(),
		"ICLOUDSYNC_EVENT="+event,
		"ICLOUDSYNC_MESSAGE="+message,
		"ICLOUDSYNC_APPLE_ID="+c.AppleID,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clean removes line breaks so a value cannot inject extra mail headers.
func clean(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func (c Config) message(subject, body string) (from string, msg []byte) {
	from = c.From
	if from == "" {
		from = c.Username
	}
	if _, err := mail.ParseAddress(from); err != nil {
		from = c.To
	}
	hdr := []string{
		"From: " + clean(from),
		"To: " + clean(c.To),
		"Subject: " + mime.QEncoding.Encode("utf-8", clean(subject)),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
	}
	return from, []byte(strings.Join(hdr, "\r\n") + "\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")
}

func (c Config) sendMail(ctx context.Context, subject, body string) error {
	port := c.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if port == 465 && !c.NoTLS { // implicit TLS
		conn = tls.Client(conn, &tls.Config{ServerName: c.Host})
	}
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if !c.NoTLS && port != 465 {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS (use --smtp-no-tls for a plain connection)")
		}
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
			return err
		}
	}
	if c.Username != "" && c.Password != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	from, msg := c.message(subject, body)
	fromAddr, _ := mail.ParseAddress(from)
	toAddr, err := mail.ParseAddress(c.To)
	if err != nil {
		return err
	}
	if err := cl.Mail(fromAddr.Address); err != nil {
		return err
	}
	if err := cl.Rcpt(toAddr.Address); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
