// swoop: IDM-style multi-connection downloader for the terminal.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var version = "0.1.0"

type headerList struct{ h http.Header }

func (l *headerList) String() string { return "" }
func (l *headerList) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return errors.New(`header must look like "Name: value"`)
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	l.h.Add(strings.TrimSpace(k), strings.TrimSpace(val))
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `swoop: IDM-style multi-connection downloads in your terminal

  swoop                  open the interactive UI (paste a link, press Enter)
  swoop [flags] URL...   download right away

flags:
  -c N        connections per download (default 16, max 32)
  -o DIR      save folder (default ~/Downloads)
  -H "K: V"   extra request header, repeatable (cookies, auth tokens)
  -v          print version
`)
}

func main() {
	fs := flag.NewFlagSet("swoop", flag.ExitOnError)
	conns := fs.Int("c", 16, "connections per download")
	dir := fs.String("o", defaultDir(), "save folder")
	var hdr headerList
	fs.Var(&hdr, "H", "extra header")
	showVer := fs.Bool("v", false, "print version")
	fs.Usage = usage

	// allow flags before or after URLs: swoop URL -c 32
	args := os.Args[1:]
	var urls []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		urls = append(urls, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *showVer {
		fmt.Println("swoop", version)
		return
	}
	if *conns < 1 {
		*conns = 1
	}
	if *conns > 32 {
		*conns = 32
	}
	abs, err := filepath.Abs(expandHome(*dir))
	if err != nil {
		abs = *dir
	}
	opts := Options{Conns: *conns, Dir: abs, Headers: hdr.h, NoSteal: os.Getenv("SWOOP_NOSTEAL") == "1"}
	tty := isTTY() && enableVT() // old consoles without escape support get plain output
	useColor = useColor && tty
	handleSignals()

	if len(urls) == 0 {
		if !tty {
			usage()
			os.Exit(2)
		}
		interactive(opts)
		return
	}
	failed := false
	for _, u := range urls {
		u = normalizeURL(u)
		if u == "" {
			fmt.Println(c(errClr, "  ✗ ") + "not a link")
			failed = true
			continue
		}
		err := runOne(context.Background(), u, opts, tty)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		if err != nil {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func interactive(opts Options) {
	printBanner(opts)
	in := bufio.NewReader(os.Stdin)
	shownClip, lastGot := "", ""
	for {
		clip := clipboardURL()
		if clip == lastGot {
			clip = ""
		}
		if clip != "" && clip != shownClip {
			fmt.Println(c(dimClr, "  clipboard: "+truncate(clip, 64)+"  (Enter to grab it)"))
			shownClip = clip
		}
		fmt.Print(c(boldClr+accent, "❯ "))
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		switch strings.ToLower(line) {
		case "q", "quit", "exit":
			return
		case "help", "h", "?":
			printHelp()
			continue
		case "open":
			if err := openDir(opts.Dir); err != nil {
				fmt.Println(c(errClr, "  ✗ ") + "could not open " + tildify(opts.Dir))
			}
			continue
		case "":
			if clip == "" {
				continue
			}
			line = clip
		}
		for _, raw := range strings.Fields(line) {
			u := normalizeURL(raw)
			if u == "" {
				fmt.Println(c(errClr, "  ✗ ") + "not a link: " + raw)
				continue
			}
			runOne(context.Background(), u, opts, true)
			lastGot = u
		}
		fmt.Println()
	}
}

// Ctrl+C during a download pauses it (state is saved for resume).
// Ctrl+C at the prompt quits.
var (
	curMu     sync.Mutex
	curCancel context.CancelFunc
)

func setCurrent(cf context.CancelFunc) {
	curMu.Lock()
	curCancel = cf
	curMu.Unlock()
}

func handleSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range ch {
			curMu.Lock()
			cf := curCancel
			curMu.Unlock()
			if cf != nil && s == os.Interrupt {
				cf()
				continue
			}
			fmt.Print(showCursor + "\n")
			os.Exit(130)
		}
	}()
}

func normalizeURL(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"'<>`)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") && strings.Contains(s, ".") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// clipboardURL returns the clipboard content only if it is a single link.
func clipboardURL() string {
	s := strings.TrimSpace(readClipboard())
	if s == "" || strings.ContainsAny(s, " \n\r\t") || !strings.Contains(s, "://") {
		return ""
	}
	return normalizeURL(s)
}

func defaultDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Downloads")
	}
	return "."
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
