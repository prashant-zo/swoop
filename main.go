// swoop: fast downloads in your terminal.
package main

import (
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

var version = "0.2.0"

var (
	errPaused    = errors.New("paused")
	errCancelled = errors.New("cancelled")
	errBadLink   = errors.New("not a link")
)

func outcomeErr(out outcome, err error) error {
	switch out {
	case paused:
		return errPaused
	case cancelled:
		return errCancelled
	case failed:
		return err
	}
	return nil
}

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
	fmt.Fprint(os.Stderr, `swoop: fast downloads in your terminal

  swoop                   open swoop, then paste a link and press Enter
  swoop [options] LINK…   download right away (links, magnet links, .torrent files)
  swoop list              see your downloads

options:
  -c N        connections per file (default 16, max 32)
  -o FOLDER   where to save (default: your Downloads folder)
  -H "K: V"   extra request header, can repeat (for links that need a login)
  -v          show the version
`)
}

func main() {
	fs := flag.NewFlagSet("swoop", flag.ExitOnError)
	conns := fs.Int("c", 16, "connections per file")
	dir := fs.String("o", defaultDir(), "save folder")
	var hdr headerList
	fs.Var(&hdr, "H", "extra header")
	showVer := fs.Bool("v", false, "print version")
	fs.Usage = usage

	// flags may come before or after links: swoop LINK -c 32
	args := os.Args[1:]
	var links []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		links = append(links, fs.Arg(0))
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
	tty := isTTY() && enableVT() // very old consoles without escape support get plain output
	useColor = useColor && tty
	ttyOut = tty

	startInput()
	if tty && isStdinTTY() && setRaw() {
		rawKeys = true
	}
	defer restoreTerminal()
	handleSignals()
	st := loadStore()

	if len(links) == 1 && links[0] == "list" {
		if e := listView(st); e != nil {
			resume(e, opts, tty, st)
		}
		return
	}
	if len(links) == 0 {
		if !tty {
			restoreTerminal()
			usage()
			os.Exit(2)
		}
		interactive(opts, st)
		return
	}
	failedAny := false
	for _, l := range links {
		err := runInput(l, opts, tty, st)
		if errors.Is(err, errPaused) || errors.Is(err, errCancelled) {
			restoreTerminal()
			os.Exit(130)
		}
		if err != nil {
			failedAny = true
		}
	}
	if failedAny {
		restoreTerminal()
		os.Exit(1)
	}
}

func interactive(opts Options, st *Store) {
	printBanner(opts, st)
	shownClip, lastGot := "", ""
	for {
		clip := clipboardURL()
		if clip == lastGot {
			clip = ""
		}
		if clip != "" && clip != shownClip {
			fmt.Println(c(dimClr, "  you copied a link: "+truncate(clip, 60)+"  (press Enter to download it)"))
			shownClip = clip
		}
		line, ok := promptLine(c(boldClr+accent, "❯ "))
		if !ok {
			return
		}
		line = strings.TrimSpace(line)
		word, arg, _ := strings.Cut(line, " ")
		switch strings.ToLower(word) {
		case "q", "quit", "exit":
			return
		case "help", "h", "?":
			printHelp()
			continue
		case "open":
			if err := openDir(opts.Dir); err != nil {
				fail("couldn't open " + tildify(opts.Dir))
			}
			continue
		case "list", "l", "ls":
			if e := listView(st); e != nil {
				resume(e, opts, true, st)
				fmt.Println()
			}
			continue
		case "folder", "dir":
			setFolder(&opts, strings.TrimSpace(arg))
			continue
		case "":
			if clip == "" {
				continue
			}
			line = clip
		}
		for _, item := range splitInputs(line) {
			runInput(item, opts, true, st)
			lastGot = normalizeURL(item)
			if lastGot == "" {
				lastGot = item
			}
		}
		fmt.Println()
	}
}

func setFolder(opts *Options, arg string) {
	if arg == "" {
		fmt.Println("  new downloads go to " + c(boldClr, tildify(opts.Dir)) + c(dimClr, "  ·  change it with: folder ~/Movies"))
		return
	}
	p, err := filepath.Abs(expandHome(strings.Trim(strings.ReplaceAll(arg, `\ `, " "), `"'`)))
	if err == nil {
		err = os.MkdirAll(p, 0o755)
	}
	if err != nil {
		fail("can't use that folder: " + err.Error())
		return
	}
	opts.Dir = p
	fmt.Println("  " + c(okClr, "✓") + " new downloads go to " + c(boldClr, tildify(p)))
}

// splitInputs allows several links on one line, but keeps a dragged-in
// file path with spaces in one piece.
func splitInputs(line string) []string {
	if localPath(line) != "" {
		return []string{line}
	}
	return strings.Fields(line)
}

// runInput figures out what the user pasted and starts the right download.
func runInput(raw string, opts Options, tty bool, st *Store) error {
	if t, ok := torrentInput(raw); ok {
		return runTorrent(t, opts, tty, st)
	}
	u := normalizeURL(raw)
	if u == "" {
		fail("that doesn't look like a link: " + truncate(raw, 60))
		return errBadLink
	}
	if tty {
		fmt.Print(c(dimClr, "  connecting…"))
	}
	d, err := Probe(context.Background(), u, opts)
	if tty {
		fmt.Print("\r\x1b[2K")
	}
	if err != nil {
		fail("couldn't start: " + friendlyErr(err))
		return err
	}
	if strings.Contains(strings.ToLower(d.CType), "bittorrent") {
		return runTorrent(u, opts, tty, st)
	}
	if d.IsPage() {
		return runMedia(u, "", opts, tty, st)
	}
	return runFile(d, opts, tty, st)
}

// resume continues a download picked from the list.
func resume(e *Entry, opts Options, tty bool, st *Store) error {
	if e.Dir != "" {
		opts.Dir = e.Dir
	}
	switch e.Kind {
	case "torrent":
		return runTorrent(e.URL, opts, tty, st)
	case "media":
		return runMedia(e.URL, e.Format, opts, tty, st)
	}
	return runInput(e.URL, opts, tty, st)
}

// ---- signals and the terminal ----

var (
	curMu   sync.Mutex
	curStop func()
)

func setCurrent(f func()) {
	curMu.Lock()
	curStop = f
	curMu.Unlock()
}

var ttyOut bool

func restoreTerminal() {
	setCooked()
	if ttyOut {
		fmt.Print(showCursor)
	}
}

// In key-by-key mode Ctrl+C arrives as a key. Otherwise it arrives as a
// signal: pause the current download, or quit at the prompt.
func handleSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range ch {
			curMu.Lock()
			stop := curStop
			curMu.Unlock()
			if stop != nil && s == os.Interrupt {
				stop()
				continue
			}
			restoreTerminal()
			fmt.Println()
			os.Exit(130)
		}
	}()
}

func normalizeURL(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"'<>`)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") && strings.Contains(s, ".") && !strings.ContainsAny(s, " \\") {
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
	if s == "" || strings.ContainsAny(s, " \n\r\t") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "magnet:?") {
		return s
	}
	if !strings.Contains(s, "://") {
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

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
