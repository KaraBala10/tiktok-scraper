package profile

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

var verbose bool

func vlog(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func bindFlags(fs *flag.FlagSet) (limit *int) {
	limit = fs.Int("limit", 0, "max videos to print (0 = all)")
	fs.BoolVar(&verbose, "v", false, "print per-request timing to stderr")
	return limit
}

func PrintDefaults() {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bindFlags(fs)
	fs.PrintDefaults()
}

// Run scrapes a profile's video URLs. args are CLI arguments after the program name.
func Run(args []string) {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	limit := bindFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [--help] [-limit N] [-v] <username|@username|url>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "       %s -explore [--help] [-refresh] [-category comedy] [-region KW] [-limit N]\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	username := normalizeUsername(fs.Arg(0))
	if username == "" {
		fmt.Fprintf(os.Stderr, "error: empty username\n")
		os.Exit(1)
	}

	n, t, err := fetchVideoURLs(username, *limit)
	sessionCache.flushNow()
	dnsStore.flushNow()
	if err != nil && n == 0 {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "error: no videos found for @%s\n", username)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if verbose {
		fmt.Fprintln(os.Stderr, t)
	}
}

func normalizeUsername(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.Contains(strings.ToLower(s), "tiktok.com") {
		toParse := s
		if !strings.Contains(s, "://") {
			toParse = "https://" + s
		}
		if u, err := url.Parse(toParse); err == nil {
			s = strings.Trim(u.Path, "/")
		}
	}
	s = strings.TrimPrefix(s, "@")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
