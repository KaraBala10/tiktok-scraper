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

const geoBlockedMsg = "tiktok blocked this IP (HTTP 451). a saved msToken cannot bypass a country ban; use a VPN"

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
		sess = &pinnedSession{}
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

	minted := false
	var lastMint time.Time
	applyIPRegion := func() {
		if r := detectIPRegion(); r != "" {
			sess.Region = r
		}
	}
	doMint := func() error {
		s, err := mintSession()
		if err != nil {
			return err
		}
		*sess = *s
		if v := strings.TrimSpace(*c.deviceID); v != "" {
			sess.DeviceID = v
		}
		applyIPRegion()
		saveSession(sessPath, sess)
		minted = true
		lastMint = time.Now()
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

	body, err := fetchWebComedy(cat, *c.count, prof, sess, "", false)
	if isGeoBlocked(err) {
		fmt.Fprintln(os.Stderr, geoBlockedMsg)
		os.Exit(1)
	}
	if (webTokenDead(body, err) || webEmpty(body)) && !minted {
		fmt.Fprintln(os.Stderr, "web token expired; minting...")
		if merr := doMint(); merr != nil {
			if isGeoBlocked(merr) && sess.Cookie != "" {
				fmt.Fprintln(os.Stderr, "mint blocked (HTTP 451); keeping saved session")
			} else {
				fmt.Fprintln(os.Stderr, merr)
				os.Exit(1)
			}
		} else {
			body, err = fetchWebComedy(cat, *c.count, prof, sess, "", false)
		}
	}
	if err != nil || webTokenDead(body, err) || webEmpty(body) {
		if isGeoBlocked(err) {
			fmt.Fprintln(os.Stderr, geoBlockedMsg)
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "empty web comedy response")
		}
		os.Exit(1)
	}
	sess.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	saveSession(sessPath, sess)
	if !minted {
		fmt.Fprintf(os.Stderr, "web comedy session region=%s -> %s\n", orDash(sess.Region), sessPath)
	}

	next := func(cursor string, fresh bool) ([]byte, error) {
		if fresh && (lastMint.IsZero() || time.Since(lastMint) >= 30*time.Second) {
			fmt.Fprintln(os.Stderr, "explore stalled; minting a new session")
			if merr := doMint(); merr != nil {
				if isGeoBlocked(merr) && sess.Cookie != "" {
					fmt.Fprintln(os.Stderr, "mint blocked (HTTP 451); retrying with saved session")
				} else {
					fmt.Fprintln(os.Stderr, merr)
				}
			} else {
				cursor = ""
				fresh = false
			}
		}
		b, err := fetchWebComedy(cat, *c.count, prof, sess, cursor, fresh)
		if isGeoBlocked(err) {
			return nil, fmt.Errorf("%s", geoBlockedMsg)
		}
		if err == nil && webEmpty(b) && (fresh || cursor == "") && !webTokenDead(b, err) {
			b, err = fetchWebComedy(cat, *c.count, prof, sess, "", false)
		}
		if !webLoginDead(b) {
			return b, err
		}
		fmt.Fprintln(os.Stderr, "web token expired; minting...")
		if merr := doMint(); merr != nil {
			if isGeoBlocked(merr) && sess.Cookie != "" {
				return b, fmt.Errorf("mint blocked (HTTP 451); keeping saved session")
			}
			return nil, merr
		}
		return fetchWebComedy(cat, *c.count, prof, sess, "", false)
	}

	n := stream(ctx, body, next, func(b []byte) ([]video, string, error) {
		return parseWeb(b, cat)
	}, *c.dumpJSON, *c.limit, sessPath, sess)
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "stopped total=%d\n", n)
	}
}
