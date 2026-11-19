package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	useColor  = os.Getenv("NO_COLOR") == ""
	trueColor = detectTrueColor()
)

// detectTrueColor decides between 24-bit and 256-color output. Terminals
// without 24-bit support (macOS Terminal before Tahoe, for one) misread
// 24-bit codes as random styles, so only use them when the terminal says so.
func detectTrueColor() bool {
	switch strings.ToLower(os.Getenv("SWOOP_COLOR")) {
	case "truecolor", "24bit":
		return true
	case "256":
		return false
	}
	if os.Getenv("TERM_PROGRAM") == "Apple_Terminal" {
		return false
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}
	if os.Getenv("WT_SESSION") != "" || runtime.GOOS == "windows" {
		return true // Windows Terminal and the Win10+ console both do 24-bit
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "ghostty", "vscode", "WarpTerminal":
		return true
	}
	return false
}

func rgb(r, g, b int) string {
	if trueColor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	}
	q := func(v int) int { return (v*5 + 127) / 255 } // nearest step in the 6x6x6 color cube
	return fmt.Sprintf("\x1b[38;5;%dm", 16+36*q(r)+6*q(g)+q(b))
}

var (
	accent  = rgb(130, 170, 255)
	okClr   = rgb(80, 220, 140)
	errClr  = rgb(255, 95, 95)
	warnClr = rgb(229, 192, 123)
	dimClr  = "\x1b[2m"
	boldClr = "\x1b[1m"
	palette = []string{
		rgb(97, 175, 239), rgb(198, 120, 221), rgb(229, 192, 123), rgb(86, 182, 194),
		rgb(224, 108, 117), rgb(152, 195, 121), rgb(209, 154, 102), rgb(160, 140, 255),
	}
)

func c(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return code + s + "\x1b[0m"
}

const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
)

var bannerRows = []string{
	"███████╗██╗    ██╗ ██████╗  ██████╗ ██████╗ ",
	"██╔════╝██║    ██║██╔═══██╗██╔═══██╗██╔══██╗",
	"███████╗██║ █╗ ██║██║   ██║██║   ██║██████╔╝",
	"╚════██║██║███╗██║██║   ██║██║   ██║██╔═══╝ ",
	"███████║╚███╔███╔╝╚██████╔╝╚██████╔╝██║     ",
	"╚══════╝ ╚══╝╚══╝  ╚═════╝  ╚═════╝ ╚═╝     ",
}

func printBanner(opts Options, st *Store) {
	fmt.Println()
	if termWidth() >= 50 {
		for _, row := range bannerRows {
			runes := []rune(row)
			var b strings.Builder
			b.WriteString("  ")
			for i, r := range runes {
				t := float64(i) / float64(len(runes)-1) // violet -> cyan
				b.WriteString(c(rgb(int(190-100*t), int(120+90*t), 255), string(r)))
			}
			fmt.Println(b.String())
		}
	} else {
		fmt.Println("  " + c(boldClr+accent, "swoop"))
	}
	fmt.Println()
	fmt.Println("  " + c(boldClr, "fast downloads, right in your terminal") + c(dimClr, "   v"+version))
	fmt.Println("  " + c(dimClr, fmt.Sprintf("saves to %s  ·  up to %d connections per file", tildify(opts.Dir), opts.Conns)))
	fmt.Println("  " + c(dimClr, "paste a link and press Enter  ·  type ") + c(boldClr, "help") + c(dimClr, " anytime"))
	if n := st.unfinished(); n > 0 {
		word := "download"
		if n > 1 {
			word += "s"
		}
		fmt.Println("  " + c(warnClr, fmt.Sprintf("■ %d unfinished %s", n, word)) + c(dimClr, "  ·  type ") + c(boldClr, "list") + c(dimClr, " to continue"))
	}
	fmt.Println()
}

func printHelp() {
	h := func(s string) string { return c(boldClr, s) }
	d := func(s string) string { return c(dimClr, s) }
	row := func(keys, desc string) {
		pad := 14 - vlen(keys)
		if pad < 1 {
			pad = 1
		}
		fmt.Println("    " + c(boldClr+accent, keys) + strings.Repeat(" ", pad) + desc)
	}
	fmt.Println()
	fmt.Println("  " + h("Download something"))
	fmt.Println("    Paste a link and press " + c(boldClr+accent, "Enter") + ". That's it.")
	fmt.Println("    " + d("Works with normal download links, magnet links and .torrent files."))
	fmt.Println("    " + d("You can also drag a .torrent file into this window."))
	fmt.Println("    " + d("Just copied a link? Press Enter on an empty line to use it."))
	fmt.Println()
	fmt.Println("  " + h("While it downloads"))
	row("p", "pause it, you can continue later")
	row("x", "cancel it and delete the unfinished file "+d("(press x twice)"))
	row("Ctrl+C", "pause it")
	fmt.Println()
	fmt.Println("  " + h("Your downloads"))
	row("list", "see everything you downloaded, continue paused ones,")
	fmt.Println("                  open them, or clean up old ones")
	fmt.Println("                  " + d("in the list:  ↑ ↓ choose · Enter continue or open · o show in folder"))
	fmt.Println("                  " + d("              d remove from list · x delete the file too · Esc back"))
	fmt.Println()
	fmt.Println("  " + h("Settings for this session"))
	row("folder PATH", "save new downloads somewhere else, like "+d("folder ~/Movies"))
	row("open", "open the download folder")
	fmt.Println()
	fmt.Println("  " + h("Leave"))
	row("q", "quit "+d("(paused downloads stay in your list)"))
	fmt.Println()
}

// ---- live view ----

// job is anything swoop can show progress for: a file, a torrent, a stream.
type job struct {
	title   func() string
	started time.Time
	resumed int64
	segmap  bool              // draw the per-connection map
	info    func(Snap) string // the dim stats line
	snap    func() Snap
	done    <-chan error
	stop    func() // stop now; whatever was downloaded stays on disk
}

type outcome int

const (
	finished outcome = iota
	paused
	cancelled
	failed
)

// watch draws a job until it ends, and handles p / x / Ctrl+C.
func watch(j *job, tty bool) (outcome, error) {
	setCurrent(j.stop)
	defer setCurrent(nil)
	r := &renderer{}
	every := 100 * time.Millisecond
	if !tty {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	if tty {
		fmt.Print(hideCursor)
		defer fmt.Print(showCursor)
	}
	want := outcome(-1)
	confirmX := false
	for {
		select {
		case err := <-j.done:
			if tty {
				r.erase()
			}
			drainKeys()
			switch {
			case want == paused || want == cancelled:
				return want, nil
			case err == nil:
				return finished, nil
			case errors.Is(err, context.Canceled):
				return paused, nil
			default:
				return failed, err
			}
		case <-tick.C:
			if rawKeys && want < 0 {
				for _, k := range takeKeys() {
					isRune := func(rs ...rune) bool {
						for _, x := range rs {
							if k.name == "rune" && k.r == x {
								return true
							}
						}
						return false
					}
					switch {
					case k.name == "ctrl-c" || isRune('p', 'P', ' '):
						want = paused
					case isRune('x', 'X'):
						if confirmX {
							want = cancelled
						}
						confirmX = true
					default:
						confirmX = false
					}
					if want >= 0 {
						j.stop()
						break
					}
				}
			}
			sn := j.snap()
			r.update(sn)
			if tty {
				r.draw(j, sn, footer(want, confirmX))
			} else if sn.Size > 0 {
				fmt.Printf("  %5.1f%%  %s / %s  %s/s\n", float64(sn.Done)*100/float64(sn.Size),
					fmtBytes(sn.Done), fmtBytes(sn.Size), fmtBytes(int64(r.speed)))
			}
		}
	}
}

func footer(want outcome, confirmX bool) string {
	switch {
	case !rawKeys:
		return c(dimClr, "  Ctrl+C pause")
	case want == paused:
		return c(warnClr, "  pausing…")
	case want == cancelled:
		return c(warnClr, "  cancelling…")
	case confirmX:
		return c(warnClr, "  press x again to cancel and delete what's downloaded so far") + c(dimClr, " · any other key keeps going")
	}
	k := func(s string) string { return c(boldClr, s) }
	return "  " + k("p") + c(dimClr, " pause   ") + k("x") + c(dimClr, " cancel   ") + k("Ctrl+C") + c(dimClr, " pause")
}

type renderer struct {
	lines   int
	lastT   time.Time
	lastB   int64
	speed   float64
	samples int
}

func (r *renderer) update(sn Snap) {
	now := time.Now()
	if r.lastT.IsZero() || sn.Written < r.lastB {
		r.lastT, r.lastB = now, sn.Written
		return
	}
	dt := now.Sub(r.lastT).Seconds()
	if dt < 0.5 {
		return
	}
	inst := float64(sn.Written-r.lastB) / dt
	if r.samples == 0 {
		r.speed = inst
	} else {
		r.speed = 0.3*inst + 0.7*r.speed // smoothed so the number doesn't jump around
	}
	r.samples++
	r.lastT, r.lastB = now, sn.Written
}

func (r *renderer) eta(sn Snap) string {
	if r.samples < 2 || r.speed < 1024 || sn.Size <= 0 {
		return "--"
	}
	return fmtDur(time.Duration(float64(sn.Size-sn.Done) / r.speed * float64(time.Second)))
}

func (r *renderer) erase() {
	if r.lines > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dF\x1b[J", r.lines)
		r.lines = 0
	}
}

func (r *renderer) draw(j *job, sn Snap, foot string) {
	lines := r.panel(j, sn)
	lines = append(lines, foot)
	var b strings.Builder
	if r.lines > 0 {
		fmt.Fprintf(&b, "\x1b[%dF\x1b[J", r.lines)
	}
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	r.lines = len(lines)
	os.Stdout.WriteString(b.String())
}

func panelWidth() int {
	w := termWidth()
	if w > 84 {
		w = 84
	}
	if w < 40 {
		w = 40
	}
	return w
}

func (r *renderer) panel(j *job, sn Snap) []string {
	W := panelWidth()
	iw := W - 4
	title := " " + truncate(j.title(), iw-6) + " "
	row := func(s string) string { return c(dimClr, "│ ") + padTo(s, iw) + c(dimClr, " │") }
	out := []string{c(dimClr, "╭─") + c(boldClr, title) + c(dimClr, strings.Repeat("─", W-3-vlen(title))+"╮")}

	speed := fmtBytes(int64(r.speed)) + "/s"
	if r.samples == 0 {
		speed = "--"
	}
	if sn.Size > 0 {
		frac := float64(sn.Done) / float64(sn.Size)
		out = append(out, row(bar(frac, iw-8)+fmt.Sprintf(" %6.1f%%", frac*100)))
		out = append(out, row(fmt.Sprintf("%s / %s   %s   ETA %s",
			fmtBytes(sn.Done), fmtBytes(sn.Size), c(boldClr+accent, "↓ "+speed), r.eta(sn))))
	} else {
		out = append(out, row(fmt.Sprintf("%s so far   %s", fmtBytes(sn.Done), c(boldClr+accent, "↓ "+speed))))
	}
	var info string
	if j.info != nil {
		info = j.info(sn)
	} else {
		info = fmt.Sprintf("%d connections · %d splits · %d retries · %s", sn.Active, sn.Steals, sn.Retries, fmtDur(time.Since(j.started)))
		if j.resumed > 0 {
			info += " · continued from " + fmtBytes(j.resumed)
		}
	}
	out = append(out, row(c(dimClr, truncate(info, iw))))
	if j.segmap && sn.Size > 0 {
		out = append(out, row(""))
		cells := segMap(sn, iw*2)
		out = append(out, row(strings.Join(cells[:iw], "")), row(strings.Join(cells[iw:], "")))
	}
	out = append(out, c(dimClr, "╰"+strings.Repeat("─", W-2)+"╯"))
	return out
}

// segMap draws the file as cells: each connection's finished bytes in its
// own color, the read head of an active connection as ▓, pending as ░.
func segMap(sn Snap, cells int) []string {
	out := make([]string, cells)
	cs := float64(sn.Size) / float64(cells)
	for i := range out {
		mid := int64((float64(i) + 0.5) * cs)
		glyph := c(okClr, "█") // not covered by any piece = finished in an earlier run
		for _, s := range sn.Segs {
			if mid >= s.start && mid < s.end {
				if mid < s.pos {
					col := okClr
					if s.color >= 0 {
						col = palette[s.color%len(palette)]
					}
					glyph = c(col, "█")
				} else {
					glyph = c(dimClr, "░")
				}
				break
			}
		}
		out[i] = glyph
	}
	for _, s := range sn.Segs {
		if s.active && s.pos < s.end && s.color >= 0 {
			if i := int(float64(s.pos) / cs); i >= 0 && i < cells {
				out[i] = c(boldClr+palette[s.color%len(palette)], "▓")
			}
		}
	}
	return out
}

func bar(frac float64, w int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	eighths := int(frac * float64(w*8))
	full := eighths / 8
	s := strings.Repeat("█", full)
	if rem := eighths % 8; rem > 0 && full < w {
		s += string([]rune("▏▎▍▌▋▊▉")[rem-1])
		full++
	}
	return c(accent, s) + c(dimClr, strings.Repeat("░", w-full))
}

// ---- plain-file downloads ----

func startLine(name, size, mode, extra string) {
	max := termWidth() - 34
	if max < 20 {
		max = 20
	}
	line := fmt.Sprintf("  %s %s · %s · %s", c(accent, "↳"), c(boldClr, truncate(name, max)), size, mode)
	if extra != "" {
		line += extra
	}
	fmt.Println(line)
}

// downloadHTTP shows the live view for a prepared download.
func downloadHTTP(d *Download, tty bool, label string) (outcome, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Started = time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	title := d.Name
	if label != "" {
		title = label + " · " + d.Name
	}
	j := &job{
		title:   func() string { return title },
		started: d.Started,
		resumed: d.Resumed,
		segmap:  d.Ranges,
		snap:    d.Snapshot,
		done:    done,
		stop:    cancel,
	}
	if !d.Ranges {
		j.info = func(Snap) string {
			return "one connection (this server doesn't allow split downloads) · " + fmtDur(time.Since(d.Started))
		}
	}
	return watch(j, tty)
}

func runFile(d *Download, opts Options, tty bool, st *Store) error {
	if err := d.Prepare(); err != nil {
		fail("couldn't create the file: " + err.Error())
		return err
	}
	mode := fmt.Sprintf("%d connections", d.Workers())
	if !d.Ranges {
		mode = c(warnClr, "1 connection")
	}
	size := "size unknown"
	if d.Size > 0 {
		size = fmtBytes(d.Size)
	}
	extra := ""
	if d.Resumed > 0 {
		extra = c(okClr, fmt.Sprintf(" · continuing, %s already done", fmtBytes(d.Resumed)))
	}
	startLine(d.Name, size, mode, extra)
	e := st.track(&Entry{Kind: "file", URL: d.URL, Name: d.Name, Path: d.Path, Dir: opts.Dir, Size: d.Size})

	out, err := downloadHTTP(d, tty, "")
	sn := d.Snapshot()
	switch out {
	case finished:
		st.finish(e, "done", d.Size)
		el := d.Finished.Sub(d.Started)
		avg := float64(d.written.Load()) / el.Seconds()
		fmt.Printf("  %s %s  %s in %s · avg %s/s\n", c(okClr, "✓"), c(boldClr, truncate(d.Name, termWidth()-40)),
			fmtBytes(d.Size), fmtDur(el), fmtBytes(int64(avg)))
		fmt.Println("    " + c(dimClr, "→ "+tildify(d.Path)))
	case paused:
		st.finish(e, "paused", sn.Done)
		reportPaused(d.Name, sn.Done, sn.Size, d.Ranges)
	case cancelled:
		os.Remove(d.Part)
		os.Remove(d.Part + ".json")
		st.remove(e)
		fmt.Printf("  %s %s  cancelled, unfinished file deleted\n", c(dimClr, "✗"), c(boldClr, truncate(d.Name, termWidth()-40)))
	case failed:
		st.finish(e, "failed", sn.Done)
		fmt.Printf("  %s %s  %s\n", c(errClr, "✗"), c(boldClr, truncate(d.Name, termWidth()-40)), friendlyErr(err))
		if d.Ranges && exists(d.Part) {
			fmt.Println("    " + c(dimClr, "what's downloaded is kept, type list to try again later"))
		}
	}
	return outcomeErr(out, err)
}

func reportPaused(name string, done, size int64, canResume bool) {
	msg := "stopped"
	if canResume && size > 0 {
		msg = fmt.Sprintf("paused at %.0f%%", float64(done)*100/float64(size))
	}
	fmt.Printf("  %s %s  %s%s\n", c(warnClr, "■"), c(boldClr, truncate(name, termWidth()-50)), msg,
		c(dimClr, " · type list to continue it later"))
}

func fail(msg string) { fmt.Println("  " + c(errClr, "✗ ") + msg) }

// debugf prints details only when SWOOP_DEBUG=1, for reporting problems.
func debugf(format string, a ...any) {
	if os.Getenv("SWOOP_DEBUG") != "" {
		fmt.Println("  " + c(dimClr, "[debug] "+fmt.Sprintf(format, a...)))
	}
}

// friendlyErr turns network errors into sentences a non-programmer can act on.
func friendlyErr(err error) string {
	if err == nil {
		return ""
	}
	debugf("%v", err)
	var se *statusErr
	var dnsErr *net.DNSError
	var nerr net.Error
	var opErr *net.OpError
	switch {
	case errors.As(err, &se):
		switch {
		case se.code == 401 || se.code == 403:
			return fmt.Sprintf("the server refused access (%d). The link may have expired or needs a login.", se.code)
		case se.code == 404 || se.code == 410:
			return fmt.Sprintf("the file isn't there (%d). Check the link.", se.code)
		case se.code == 429:
			return "the server says too many requests. Wait a little and try again."
		case se.code >= 500:
			return fmt.Sprintf("the server is having problems (%d). Try again later.", se.code)
		}
		return "the server said " + se.text
	case errors.As(err, &dnsErr):
		return "can't find that website. Check the link and your internet connection."
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "the server closed the connection without sending anything. It may be busy, try again later."
	case errors.As(err, &nerr) && nerr.Timeout():
		return "the server didn't answer. It may be down, or blocked on your network."
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "couldn't connect to the server. It may be down, or blocked on your network."
	case strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "tls:"):
		return "the secure connection failed (certificate problem)."
	}
	return err.Error()
}

// ---- menus ----

// pick shows a numbered menu and returns the chosen index, or -1 for Esc.
func pick(question string, items []string) int {
	if len(items) == 0 {
		return -1
	}
	if !rawKeys {
		fmt.Println("    " + question)
		for i, it := range items {
			fmt.Printf("      %d  %s\n", i+1, it)
		}
		line, ok := promptLine("    number ❯ ")
		if !ok {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(items) {
			return -1
		}
		return n - 1
	}
	sel := 0
	lines := 0
	draw := func() {
		var b strings.Builder
		if lines > 0 {
			fmt.Fprintf(&b, "\x1b[%dF\x1b[J", lines)
		}
		b.WriteString("    " + question + c(dimClr, "  (↑ ↓ then Enter, or press a number · Esc cancels)") + "\n")
		for i, it := range items {
			if i == sel {
				fmt.Fprintf(&b, "    %s %s  %s\n", c(boldClr+accent, "❯"), c(boldClr, strconv.Itoa(i+1)), c(boldClr, it))
			} else {
				fmt.Fprintf(&b, "      %s  %s\n", c(dimClr, strconv.Itoa(i+1)), it)
			}
		}
		lines = len(items) + 1
		os.Stdout.WriteString(b.String())
	}
	fmt.Print(hideCursor)
	defer fmt.Print(showCursor)
	draw()
	for {
		k, ok := nextKey()
		if !ok {
			return -1
		}
		switch {
		case k.name == "up":
			sel = (sel + len(items) - 1) % len(items)
		case k.name == "down" || k.name == "tab":
			sel = (sel + 1) % len(items)
		case k.name == "enter":
			return sel
		case k.name == "esc" || k.name == "ctrl-c" || (k.name == "rune" && k.r == 'q'):
			return -1
		case k.name == "rune" && k.r >= '1' && k.r <= '9':
			if n := int(k.r - '1'); n < len(items) {
				sel = n
				draw()
				return sel
			}
		}
		draw()
	}
}

// ---- formatting ----

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func fmtDur(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && !(s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z') {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func vlen(s string) int { return utf8.RuneCountInString(stripANSI(s)) }

func padTo(s string, w int) string {
	if n := vlen(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func truncate(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 4 {
		return s
	}
	return string(r[:w-1]) + "…"
}

func tildify(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
