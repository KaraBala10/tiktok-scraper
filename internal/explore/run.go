package explore

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func Requested(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "-explore", "--explore":
			if strings.Contains(a, "=") && (strings.HasSuffix(a, "=false") || strings.HasSuffix(a, "=0")) {
				return false
			}
			return true
		}
	}
	return false
}

type cli struct {
	category, region, deviceID, cookieFile *string
	count, limit                           *int
	refresh, dumpJSON                      *bool
}

func bindFlags(fs *flag.FlagSet) cli {
	var c cli
	c.category = fs.String("category", "comedy", "explore category (comedy=104). follows current IP")
	c.count = fs.Int("count", 12, "page size")
	c.limit = fs.Int("limit", 0, "max videos to print (0 = until Ctrl+C)")
	c.region = fs.String("region", env("REGION", "KW"), "region query pack (feed geo follows IP)")
	c.refresh = fs.Bool("refresh", false, "force a new web msToken even if a session exists")
	c.deviceID = fs.String("device", env("DEVICE_ID", ""), "web device_id")
	c.cookieFile = fs.String("cookies", defaultCookieFile(), "Cookie header file for the web comedy chip")
	c.dumpJSON = fs.Bool("json", false, "print raw JSON pages (still loops until Ctrl+C)")
	return c
}

func PrintDefaults() {
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bindFlags(fs)
	fs.PrintDefaults()
}

func stripModeFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "-explore", "--explore":
			continue
		}
		out = append(out, a)
	}
	return out
}

func Run(args []string) {
	fs := flag.NewFlagSet("explore", flag.ExitOnError)
	c := bindFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s -explore [--help] [-refresh] [-category comedy] [-region KW] [-limit N]\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(stripModeFlags(args)); err != nil {
		os.Exit(2)
	}

	prof, err := loadRegion(strings.ToUpper(strings.TrimSpace(*c.region)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cat, err := resolveCategory(*c.category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessPath := defaultSessionPath()
	sess := loadSession(sessPath)
	if sess == nil {
		if old := loadSession("session.json"); old != nil {
			sess = old
			saveSession(sessPath, sess)
		} else {
			sess = &pinnedSession{Region: prof.Code}
		}
	}
	if v := strings.TrimSpace(*c.deviceID); v != "" {
		sess.DeviceID = v
	}
	if sess.DeviceID == "" {
		sess.DeviceID = loadDeviceID(*c.cookieFile)
	}
	if ck, err := loadCookieHeader(*c.cookieFile); err == nil && sess.Cookie == "" {
		sess.Cookie = ck
	}

	savePin := true
	minted := false
	doMint := func() error {
		s, err := mintSession(ctx)
		if err != nil {
			return err
		}
		*sess = *s
		if v := strings.TrimSpace(*c.deviceID); v != "" {
			sess.DeviceID = v
		}
		saveSession(sessPath, sess)
		minted = true
		fmt.Fprintf(os.Stderr, "minted web session region=%s -> %s\n", orDash(sess.Region), sessPath)
		return nil
	}

	ensureSession := func() error {
		if *c.refresh || sess.Cookie == "" {
			if sess.Cookie == "" && !*c.refresh {
				fmt.Fprintln(os.Stderr, "no session/token; minting...")
			}
			return doMint()
		}
		return nil
	}
	if err := ensureSession(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	body, err := fetchWebComedy(cat, *c.count, prof, sess)
	if webTokenDead(body, err) && !minted {
		fmt.Fprintln(os.Stderr, "web token expired; minting...")
		if merr := doMint(); merr != nil {
			fmt.Fprintln(os.Stderr, merr)
			os.Exit(1)
		}
		body, err = fetchWebComedy(cat, *c.count, prof, sess)
	}
	if err != nil || webTokenDead(body, err) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "empty web comedy response")
		}
		os.Exit(1)
	}
	if sess.Region == "" {
		sess.Region = prof.Code
	}
	sess.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	saveSession(sessPath, sess)
	if !minted {
		fmt.Fprintf(os.Stderr, "web comedy session region=%s -> %s\n", orDash(sess.Region), sessPath)
	}

	next := func() ([]byte, error) {
		b, err := fetchWebComedy(cat, *c.count, prof, sess)
		if !webTokenDead(b, err) {
			return b, err
		}
		if minted {
			return b, fmt.Errorf("web comedy unavailable")
		}
		fmt.Fprintln(os.Stderr, "web token expired; minting...")
		if merr := doMint(); merr != nil {
			return nil, merr
		}
		return fetchWebComedy(cat, *c.count, prof, sess)
	}

	n := stream(ctx, body, next, func(b []byte) ([]video, string, error) {
		return parseWeb(b, cat, prof)
	}, *c.dumpJSON, *c.limit, sessPath, sess, &savePin)
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "stopped total=%d\n", n)
	}
}
