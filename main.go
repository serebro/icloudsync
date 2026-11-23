// Command icloudsync mirrors iCloud Drive and iCloud Photos to a local/external disk.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/serebro/icloudsync/internal/creds"
	"github.com/serebro/icloudsync/internal/icloud"
	"github.com/serebro/icloudsync/internal/notify"
	"github.com/serebro/icloudsync/internal/syncer"
)

const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitInteraction = 3
	exitPartial     = 4
	exitInterrupted = 130
)

const usage = `icloudsync — mirror iCloud Drive and iCloud Photos to an external disk.

Usage:
  icloudsync login  [--apple-id EMAIL] [--keyring | --save-password]
  icloudsync sync   --dest DIR [--only-drive | --only-photos] [--dry-run]
                    [--no-delete] [--force-delete] [--trash [--trash-days N]] [--workers N]
                    [--size LIST [--force-size]] [--xmp-sidecar] [--albums | --album NAME]...
                    [--recent N] [--until-found N]
                    [--notify-script PROG] [--notify-email ADDR --smtp-host HOST ...]
  icloudsync albums [--apple-id EMAIL]
  icloudsync status
  icloudsync logout [--keep-password]

Common flags:
  --apple-id EMAIL     Apple ID (or $ICLOUD_APPLE_ID; remembered in the session file)
  --session FILE       session file (or $ICLOUDSYNC_SESSION; default in the user config dir)
  --debug-log FILE     append a redacted HTTP trace to FILE

Password, in this order: $ICLOUD_PASSWORD, the system keychain (login --keyring),
the session file (login --save-password, plain text) or a terminal prompt.
The password is never accepted as a command-line flag.

Photos: --size takes a comma-separated list of original, adjusted (the edited version),
alternative (JPEG of a RAW), medium, thumb; default original. --xmp-sidecar writes .xmp files
with caption, keywords, location and favorite flag. --albums / --album NAME mirror your own
albums into Photos/Albums/ (list them with: icloudsync albums). --recent N and
--until-found N list only part of the library; nothing is deleted in such a run.

Notifications (sync only): --notify-script PROG runs PROG, and --notify-email ADDR with
--smtp-host/--smtp-port/--smtp-username/--smtp-no-tls/--notify-email-from sends a message,
when the run ends in a state listed in --notify-on (interaction, partial, error; default
interaction). The SMTP password is read from $ICLOUDSYNC_SMTP_PASSWORD.

Run "icloudsync version" to print the version.

Exit codes: 0 ok, 1 error, 2 usage, 3 needs a human (password/2FA), 4 finished with failures.
`

// version is set at release time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

type common struct {
	appleID  string
	session  string
	debugLog string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.appleID, "apple-id", os.Getenv("ICLOUD_APPLE_ID"), "Apple ID e-mail")
	fs.StringVar(&c.session, "session", os.Getenv("ICLOUDSYNC_SESSION"), "session file path")
	fs.StringVar(&c.debugLog, "debug-log", "", "write a redacted HTTP trace to this file")
}

// notifyState carries what the sync command learned for the final notification.
type notifyState struct {
	cfg     notify.Config
	err     error
	partial bool
}

// run executes a command and, for sync, announces a state that needs a human.
// A notification problem is printed but never changes the exit code.
func run(args []string) int {
	var ns notifyState
	code := runCmd(args, &ns)
	if !ns.cfg.Enabled() {
		return code
	}
	event := ""
	switch code {
	case exitInteraction:
		event = notify.EventInteraction
	case exitPartial:
		event = notify.EventPartial
	case exitError:
		event = notify.EventError
	}
	if event == "" || !ns.cfg.Wants(event) {
		return code
	}
	subject, body := notifyText(event, ns)
	// not tied to the Ctrl+C context: the run is over, the message should still go out
	nctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ns.cfg.Notify(nctx, event, subject, body); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	return code
}

func notifyText(event string, ns notifyState) (subject, body string) {
	host, _ := os.Hostname()
	switch event {
	case notify.EventInteraction:
		subject = "icloudsync: sign-in needed"
		body = "The iCloud session expired and an unattended run cannot sign in again.\nRun `icloudsync login` in a terminal on " + host + "."
	case notify.EventPartial:
		subject = "icloudsync: finished with problems"
		body = "Some files failed or deletions were blocked. See the log of the run on " + host + "."
	default:
		subject = "icloudsync: sync failed"
		body = "The sync did not complete on " + host + "."
	}
	if ns.err != nil {
		body += "\n\n" + ns.err.Error()
	}
	return subject, body
}

func runCmd(args []string, ns *notifyState) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("icloudsync", version)
		return exitOK
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		// first Ctrl+C: graceful stop; default handling again so a second one kills at once
		signal.Stop(sigCh)
		fmt.Fprintln(os.Stderr, "\nstopping... (press Ctrl+C again to quit immediately)")
		cancel()
	}()
	stdinCtx = ctx

	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var cf common
	cf.register(fs)

	var err error
	switch cmd {
	case "login":
		savePw := fs.Bool("save-password", false, "store the password (plaintext, mode 0600) in the session file")
		useKeyring := fs.Bool("keyring", false, "store the password in the system keychain")
		if !parseFlags(fs, rest) {
			return exitUsage
		}
		if *savePw && *useKeyring {
			fmt.Fprintln(os.Stderr, "login: --save-password and --keyring are mutually exclusive")
			return exitUsage
		}
		err = cmdLogin(ctx, cf, *savePw, *useKeyring)
	case "sync":
		dest := fs.String("dest", os.Getenv("ICLOUDSYNC_DEST"), "destination directory on the external disk")
		onlyDrive := fs.Bool("only-drive", false, "sync only iCloud Drive")
		onlyPhotos := fs.Bool("only-photos", false, "sync only iCloud Photos")
		dry := fs.Bool("dry-run", false, "only print what would change")
		noDel := fs.Bool("no-delete", false, "never delete files on the destination")
		force := fs.Bool("force-delete", false, "allow mass deletions that the safety check would block")
		workers := fs.Int("workers", 4, "parallel downloads")
		po, pf := registerPhotoFlags(fs)
		trash := fs.Bool("trash", false, "move files removed from iCloud to .icloudsync/trash/ instead of deleting them")
		trashDays := fs.Int("trash-days", 0, "with --trash: purge trash batches older than N days (0 = keep forever)")
		nf := registerNotifyFlags(fs)
		if !parseFlags(fs, rest) {
			return exitUsage
		}
		photoOpts, perr := pf.options(po)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "sync:", perr)
			return exitUsage
		}
		ncfg, nerr := nf.config()
		if nerr != nil {
			fmt.Fprintln(os.Stderr, "sync:", nerr)
			return exitUsage
		}
		if *dest == "" {
			fmt.Fprintln(os.Stderr, "sync: --dest is required")
			return exitUsage
		}
		if *trashDays < 0 || (*trashDays > 0 && !*trash) {
			fmt.Fprintln(os.Stderr, "sync: --trash-days needs --trash and a positive number of days")
			return exitUsage
		}
		if *onlyDrive && *onlyPhotos {
			fmt.Fprintln(os.Stderr, "sync: --only-drive and --only-photos are mutually exclusive (omit both to sync everything)")
			return exitUsage
		}
		drive, photos := !*onlyPhotos, !*onlyDrive
		ns.cfg = ncfg
		var partial bool
		partial, err = cmdSync(ctx, cf, syncArgs{dest: *dest, drive: drive, photos: photos, dry: *dry, noDelete: *noDel, forceDelete: *force, trash: *trash, trashDays: *trashDays, workers: *workers, photoOpts: photoOpts}, &ns.cfg)
		ns.err, ns.partial = err, partial
		if err == nil && partial {
			return exitPartial
		}
	case "albums":
		if !parseFlags(fs, rest) {
			return exitUsage
		}
		err = cmdAlbums(ctx, cf)
	case "status":
		if !parseFlags(fs, rest) {
			return exitUsage
		}
		err = cmdStatus(ctx, cf)
	case "logout":
		keep := fs.Bool("keep-password", false, "keep a saved password")
		if !parseFlags(fs, rest) {
			return exitUsage
		}
		err = cmdLogout(cf, *keep)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if errors.Is(err, errInterrupted) || (err != nil && ctx.Err() != nil) {
		if errors.Is(err, errInterrupted) {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "interrupted")
		}
		return exitInterrupted
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, icloud.ErrInteractionRequired) {
			return exitInteraction
		}
		return exitError
	}
	return exitOK
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type photoFlags struct {
	size                  *string
	force, xmp, allAlbums *bool
	recent, untilFound    *int
}

func registerPhotoFlags(fs *flag.FlagSet) (*stringList, photoFlags) {
	albums := &stringList{}
	fs.Var(albums, "album", "mirror this album of your library into Photos/Albums/ (repeatable; \"Folder/Album\" or just the name)")
	return albums, photoFlags{
		size:       fs.String("size", "original", "comma-separated renditions: original, adjusted, alternative, medium, thumb"),
		force:      fs.Bool("force-size", false, "skip a photo that lacks a requested rendition instead of using its original"),
		xmp:        fs.Bool("xmp-sidecar", false, "write .xmp sidecars with caption, keywords, location and favorite flag"),
		allAlbums:  fs.Bool("albums", false, "mirror all albums of your library into Photos/Albums/"),
		recent:     fs.Int("recent", 0, "list only the first N photos of each library, and of each album (nothing is deleted)"),
		untilFound: fs.Int("until-found", 0, "stop after N consecutive photos that are already up to date (nothing is deleted)"),
	}
}

func (p photoFlags) options(albums *stringList) (syncer.PhotoOptions, error) {
	sizes, err := icloud.ParsePhotoSizes(*p.size)
	if err != nil {
		return syncer.PhotoOptions{}, fmt.Errorf("--size: %w", err)
	}
	if *p.recent < 0 || *p.untilFound < 0 {
		return syncer.PhotoOptions{}, errors.New("--recent and --until-found must not be negative")
	}
	if *p.allAlbums && len(*albums) > 0 {
		return syncer.PhotoOptions{}, errors.New("--albums and --album are mutually exclusive")
	}
	if *p.force && len(sizes) == 1 && sizes[0] == icloud.SizeOriginal {
		return syncer.PhotoOptions{}, errors.New("--force-size only makes sense with a --size other than original")
	}
	return syncer.PhotoOptions{
		Sizes: sizes, ForceSize: *p.force, XMP: *p.xmp,
		Albums: []string(*albums), AllAlbums: *p.allAlbums,
		Recent: *p.recent, UntilFound: *p.untilFound,
	}, nil
}

type notifyFlags struct {
	script, email, from, host, user, on *string
	port                                *int
	noTLS                               *bool
}

func registerNotifyFlags(fs *flag.FlagSet) notifyFlags {
	return notifyFlags{
		script: fs.String("notify-script", "", "program to run when the run needs attention (see --notify-on)"),
		email:  fs.String("notify-email", "", "e-mail address to notify (needs --smtp-host)"),
		from:   fs.String("notify-email-from", "", "sender address (default: --smtp-username)"),
		host:   fs.String("smtp-host", "", "SMTP server for --notify-email"),
		port:   fs.Int("smtp-port", 587, "SMTP port (587 STARTTLS, 465 implicit TLS)"),
		user:   fs.String("smtp-username", "", "SMTP user; the password is read from $ICLOUDSYNC_SMTP_PASSWORD"),
		noTLS:  fs.Bool("smtp-no-tls", false, "send without TLS (only for a trusted local relay)"),
		on:     fs.String("notify-on", notify.EventInteraction, "comma-separated: interaction, partial, error"),
	}
}

func (n notifyFlags) config() (notify.Config, error) {
	events, err := notify.ParseEvents(*n.on)
	if err != nil {
		return notify.Config{}, err
	}
	c := notify.Config{
		Script: *n.script, To: *n.email, From: *n.from, Host: *n.host, Port: *n.port,
		Username: *n.user, Password: os.Getenv("ICLOUDSYNC_SMTP_PASSWORD"), NoTLS: *n.noTLS, Events: events,
	}
	return c, c.Validate()
}

// parseFlags parses a subcommand's flags and rejects stray positional
// arguments: flag parsing stops at the first one, so "--dest X extra --dry-run"
// would otherwise silently drop --dry-run.
func parseFlags(fs *flag.FlagSet, args []string) bool {
	if fs.Parse(args) != nil {
		return false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q (see --help)\n", fs.Name(), fs.Arg(0))
		return false
	}
	return true
}

// ---- terminal prompter ----

type tty struct {
	in *bufio.Reader
}

// stdinCtx is cancelled by Ctrl+C; prompts give up when it is.
var stdinCtx = context.Background()

var errInterrupted = errors.New("interrupted")

func newTTY() *tty { return &tty{in: bufio.NewReader(os.Stdin)} }

func (t *tty) Interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// readCancellable runs a blocking terminal read and returns early on Ctrl+C.
func readCancellable(read func() (string, error)) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := read()
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-stdinCtx.Done():
		fmt.Fprintln(os.Stderr)
		return "", errInterrupted
	}
}

func (t *tty) ReadLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	return readCancellable(func() (string, error) {
		s, err := t.in.ReadString('\n')
		if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
			return "", err
		}
		return strings.TrimSpace(s), nil
	})
}

func (t *tty) Println(a ...any) { fmt.Fprintln(os.Stderr, a...) }

func (t *tty) ReadPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	state, _ := term.GetState(fd)
	fmt.Fprint(os.Stderr, prompt)
	s, err := readCancellable(func() (string, error) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	})
	if errors.Is(err, errInterrupted) && state != nil {
		_ = term.Restore(fd, state) // ReadPassword was cut short: turn echo back on
	} else {
		fmt.Fprintln(os.Stderr)
	}
	return s, err
}

// ---- session / client setup ----

type passwordSource int

const (
	pwNone passwordSource = iota
	pwEnv
	pwKeyring
	pwStored
	pwPrompt
)

// credStore is where --keyring passwords live; tests replace it.
var credStore creds.Store = creds.System{}

type app struct {
	cf       common
	path     string
	sess     *icloud.SessionFile
	client   *icloud.Client
	tty      *tty
	trace    io.Closer
	pw       string
	pwSource passwordSource
}

func openApp(cf common, needAppleID bool) (*app, error) {
	path := cf.session
	if path == "" {
		p, err := icloud.DefaultSessionPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	sess, err := icloud.LoadSession(path)
	if err != nil {
		return nil, err
	}
	a := &app{cf: cf, path: path, sess: sess, tty: newTTY()}

	appleID := strings.TrimSpace(cf.appleID)
	if appleID != "" && !sess.SameAccount(appleID) {
		fmt.Fprintf(os.Stderr, "Apple ID changed (%s -> %s): starting a new session.\n", sess.AppleID, appleID)
		sess.Reset(appleID)
	}
	if appleID == "" {
		appleID = sess.AppleID
	}
	if appleID == "" && needAppleID {
		if !a.tty.Interactive() {
			return nil, fmt.Errorf("%w: Apple ID unknown; pass --apple-id or set ICLOUD_APPLE_ID", icloud.ErrInteractionRequired)
		}
		appleID, err = a.tty.ReadLine("Apple ID (e-mail): ")
		if err != nil {
			return nil, err
		}
	}
	sess.AppleID = appleID

	var trace io.Writer
	if cf.debugLog != "" {
		f, err := icloud.OpenTrace(cf.debugLog)
		if err != nil {
			return nil, err
		}
		a.trace = f
		trace = f
	}
	a.client = icloud.NewClient(sess, path, a.tty, trace)
	a.client.Warnf = logf
	return a, nil
}

func (a *app) close() {
	if a.trace != nil {
		a.trace.Close()
	}
}

func (a *app) password() (string, error) {
	if pw := os.Getenv("ICLOUD_PASSWORD"); pw != "" {
		a.pw, a.pwSource = pw, pwEnv
		return pw, nil
	}
	switch pw, err := credStore.Get(a.sess.AppleID); {
	case err == nil && pw != "":
		a.pw, a.pwSource = pw, pwKeyring
		return pw, nil
	case err != nil && !errors.Is(err, creds.ErrNotFound):
		// no keychain or Secret Service here (cron, ssh, a locked keychain): try the next source
		fmt.Fprintf(os.Stderr, "warning: the system keychain is unavailable (%v)\n", err)
	}
	if a.sess.Password != "" {
		a.pw, a.pwSource = a.sess.Password, pwStored
		return a.sess.Password, nil
	}
	if !a.tty.Interactive() {
		return "", fmt.Errorf("%w: the iCloud session expired and no password is available; run `icloudsync login`", icloud.ErrInteractionRequired)
	}
	pw, err := a.tty.ReadPassword(fmt.Sprintf("Apple ID password for %s: ", a.sess.AppleID))
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", errors.New("empty password")
	}
	a.pw, a.pwSource = pw, pwPrompt
	return pw, nil
}

func (a *app) authenticate(ctx context.Context) error {
	err := a.client.Authenticate(ctx, a.password)
	if errors.Is(err, icloud.ErrBadCredentials) && a.pwSource == pwKeyring {
		// same rule as for the session file: a rejected stored password is never retried
		delErr := credStore.Delete(a.sess.AppleID)
		if delErr != nil {
			return fmt.Errorf("%w\nthe keychain password was rejected but could not be removed (%v); run `icloudsync login --keyring`", err, delErr)
		}
		return fmt.Errorf("%w\nthe keychain password was rejected and has been removed; run `icloudsync login --keyring`", err)
	}
	if errors.Is(err, icloud.ErrBadCredentials) && a.pwSource == pwStored {
		// never retry a rejected stored password: scheduled runs would lock the Apple ID
		a.sess.Password = ""
		_ = a.client.Persist()
		return fmt.Errorf("%w\nthe saved password was rejected and has been removed from %s; run `icloudsync login`", err, a.path)
	}
	return err
}

// ---- commands ----

func cmdLogin(ctx context.Context, cf common, savePw, useKeyring bool) error {
	a, err := openApp(cf, true)
	if err != nil {
		return err
	}
	defer a.close()
	if err := a.authenticate(ctx); err != nil {
		return err
	}
	if savePw || useKeyring {
		if a.pwSource == pwNone {
			// the session was still valid, so no password was asked for
			if _, err := a.password(); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "The session is still valid, so the password was saved without being checked; it will be checked on the next full sign-in.")
		}
		if useKeyring {
			if err := credStore.Set(a.sess.AppleID, a.pw); err != nil {
				return fmt.Errorf("could not store the password in the system keychain: %w", err)
			}
			if a.sess.Password != "" { // do not leave an older plain-text copy behind
				a.sess.Password = ""
				if err := a.client.Persist(); err != nil {
					return err
				}
			}
			fmt.Fprintln(os.Stderr, "Password saved in the system keychain.")
		} else {
			a.sess.Password = a.pw
			if err := a.client.Persist(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Password saved IN PLAIN TEXT to %s (mode 0600).\n", a.path)
		}
	}
	acc := a.client.Account()
	fmt.Printf("Signed in: %s (%s)\nSession: %s\n", a.sess.AppleID, acc.DsInfo.FullName, a.path)
	return nil
}

type syncArgs struct {
	dest                         string
	drive, photos, dry, noDelete bool
	forceDelete                  bool
	trash                        bool
	trashDays                    int
	workers                      int
	photoOpts                    syncer.PhotoOptions
}

// logf prints a timestamped progress line.
func logf(format string, args ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func cmdSync(ctx context.Context, cf common, sa syncArgs, ncfg *notify.Config) (partial bool, err error) {
	// check the destination before touching the network: an unmounted disk must not cost a sign-in
	if fi, err := os.Stat(sa.dest); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("%w: %s (is the drive mounted?)", syncer.ErrDestMissing, sa.dest)
	}
	a, err := openApp(cf, true)
	if err != nil {
		return false, err
	}
	defer a.close()
	ncfg.AppleID = a.sess.AppleID
	if err := a.authenticate(ctx); err != nil {
		return false, err
	}
	defer a.client.Persist()

	start := time.Now()
	var services []syncer.Service
	if sa.drive {
		services = append(services, syncer.DriveService(a.client))
	}
	if sa.photos {
		services = append(services, syncer.PhotosServicesWith(a.client, logf, sa.photoOpts)...)
	}
	if sa.dry {
		logf("DRY RUN: nothing will be written")
	}
	stats, runErr := syncer.Run(ctx, syncer.Options{
		Dest: sa.dest, AppleID: a.sess.AppleID, DryRun: sa.dry, NoDelete: sa.noDelete,
		ForceDelete: sa.forceDelete, Trash: sa.trash, TrashDays: sa.trashDays, Workers: sa.workers, Logf: logf,
	}, services)

	fmt.Println()
	for _, name := range []string{"drive", "photos", "photos-shared", "photos-albums", "photos-myalbums"} {
		st, ok := stats[name]
		if !ok {
			continue
		}
		fmt.Printf("%-14s listed %d, downloaded %d (%s), moved %d, adopted %d, unchanged %d, deleted %d, failed %d\n",
			name+":", st.Listed, st.Downloaded, syncer.HumanBytes(st.Bytes), st.Moved, st.Adopted, st.Skipped, st.Deleted, st.Failed)
		if st.Partial {
			fmt.Printf("               partial listing (--recent/--until-found): nothing was deleted\n")
		}
		if st.ListError != nil {
			fmt.Printf("               listing error: %v\n", st.ListError)
			partial = true
		}
		if st.DeletionsBlocked != "" {
			fmt.Printf("               deletions blocked: %s\n", st.DeletionsBlocked)
			partial = true
		}
		if st.Failed > 0 {
			partial = true
		}
	}
	fmt.Printf("done in %s\n", time.Since(start).Round(time.Second))
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return true, fmt.Errorf("%w (progress saved, the next run continues)", errInterrupted)
		}
		return true, runErr
	}
	return partial, nil
}

func cmdAlbums(ctx context.Context, cf common) error {
	a, err := openApp(cf, true)
	if err != nil {
		return err
	}
	defer a.close()
	if err := a.authenticate(ctx); err != nil {
		return err
	}
	defer a.client.Persist()
	libs, err := a.client.PhotoLibraries(ctx)
	if err != nil {
		return err
	}
	for _, lib := range libs {
		if lib.Kind != "primary" {
			continue
		}
		albums, err := a.client.PhotoAlbums(ctx, lib)
		if err != nil {
			return err
		}
		if len(albums) == 0 {
			fmt.Println("no albums")
		}
		for _, al := range albums {
			n, cerr := a.client.AlbumCount(ctx, al)
			if cerr != nil {
				fmt.Printf("%s\n", al.Path())
				continue
			}
			fmt.Printf("%s (%d)\n", al.Path(), n)
		}
	}
	return nil
}

func cmdStatus(ctx context.Context, cf common) error {
	a, err := openApp(cf, false)
	if err != nil {
		return err
	}
	defer a.close()
	s := a.sess
	fmt.Printf("Session file:  %s\n", a.path)
	if s.AppleID == "" {
		fmt.Println("Apple ID:      (none, run `icloudsync login`)")
		return nil
	}
	fmt.Printf("Apple ID:      %s\n", s.AppleID)
	if !s.UpdatedAt.IsZero() {
		fmt.Printf("Last updated:  %s\n", s.UpdatedAt.Local().Format(time.RFC1123))
	}
	fmt.Printf("Trusted (2FA): %v\n", s.Data["trust_token"] != "")
	fmt.Printf("Saved password: %s\n", savedPasswordWhere(s))
	acc, err := a.client.Status(ctx)
	switch {
	case err == nil:
		fmt.Printf("Session:       valid (%s)\n", acc.DsInfo.FullName)
	case icloud.IsSessionInvalid(err):
		fmt.Println("Session:       expired; next sync will sign in again (2FA only if Apple asks)")
	default:
		return err
	}
	return nil
}

// savedPasswordWhere says where a password for the session's Apple ID is kept.
func savedPasswordWhere(s *icloud.SessionFile) string {
	var where []string
	if pw, err := credStore.Get(s.AppleID); err == nil && pw != "" {
		where = append(where, "system keychain")
	}
	if s.Password != "" {
		where = append(where, "session file (plain text)")
	}
	if len(where) == 0 {
		return "no"
	}
	return strings.Join(where, " and ")
}

func cmdLogout(cf common, keepPassword bool) error {
	a, err := openApp(cf, false)
	if err != nil {
		return err
	}
	defer a.close()
	if !keepPassword && a.sess.AppleID != "" {
		if err := credStore.Delete(a.sess.AppleID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove the password from the system keychain: %v\n", err)
		}
	}
	if keepPassword && a.sess.Password != "" {
		pw := a.sess.Password
		a.sess.Reset(a.sess.AppleID)
		a.sess.Password = pw
		if err := a.sess.Save(a.path); err != nil {
			return err
		}
		fmt.Println("Session cleared (password kept).")
		return nil
	}
	if err := os.Remove(a.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("Session file removed:", a.path)
	return nil
}
