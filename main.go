package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"tiktok_scraper/internal/explore"
	"tiktok_scraper/internal/profile"
)

func wantsHelp(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

func printHelp() {
	name := os.Args[0]
	fmt.Fprintf(os.Stderr, "usage: %s [--help] [-limit N] [-v] <username|@username|url>\n", name)
	fmt.Fprintf(os.Stderr, "       %s -explore [--help] [-refresh] [-category comedy] [-region KW] [-limit N]\n\n", name)
	global := flag.NewFlagSet("tiktok_scraper", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	global.Bool("help", false, "show this help")
	global.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nProfile options:\n")
	profile.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nExplore options:\n")
	explore.PrintDefaults()
}

func main() {
	if wantsHelp(os.Args[1:]) {
		printHelp()
		return
	}
	if explore.Requested(os.Args[1:]) {
		explore.Run(os.Args[1:])
		return
	}
	if len(os.Args) == 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [--help] [-limit N] [-v] <username|@username|url>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "       %s -explore [--help] [-refresh] [-category comedy] [-region KW] [-limit N]\n", os.Args[0])
		os.Exit(1)
	}
	profile.Run(os.Args[1:])
}
