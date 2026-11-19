package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
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
	// nearest color in the xterm 256-color 6x6x6 cube
	q := func(v int) int { return (v*5 + 127) / 255 }
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

func printBanner(opts Options) {
	fmt.Println()
	if termWidth() >= 50 {
		for _, row := range bannerRows {
			runes := []rune(row)
			var b strings.Builder
			b.WriteString("  ")
			for i, r := range runes {
				t := float64(i) / float64(len(runes)-1) // violet -> cyan gradient
				col := rgb(int(190-100*t), int(120+90*t), 255)
				b.WriteString(c(col, string(r)))
			}
			fmt.Println(b.String())
		}
	} else {
		fmt.Println("  " + c(boldClr+accent, "swoop"))
	}
	fmt.Println()
	fmt.Println("  " + c(boldClr, "IDM-style multi-connection downloads") + c(dimClr, "  ·  v"+version))
	fmt.Println("  " + c(dimClr, fmt.Sprintf("saving to %s  ·  %d connections  ·  forced HTTP/1.1 streams", tildify(opts.Dir), opts.Conns)))
	fmt.Println("  " + c(dimClr, "paste a link + Enter  ·  open  ·  help  ·  q to quit  ·  Ctrl+C pauses a download"))
	fmt.Println()
}

func printHelp() {
	fmt.Println(c(dimClr, `
  paste one or more links (space separated) and press Enter
  Enter on an empty line   grab the link sitting in your clipboard
  open                     open the download folder
  q / quit                 exit
  Ctrl+C                   pause the current download (paste the same link to resume)

  from the shell: swoop -c 32 -o ~/Movies URL   ·   swoop -H "Cookie: a=b" URL
`))
}

// ---- live panel ----

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
		r.speed = 0.3*inst + 0.7*r.speed // EWMA so the number does not jump around
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

func (r *renderer) draw(d *Download, sn Snap) {
	lines := r.panel(d, sn)
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

func (r *renderer) panel(d *Download, sn Snap) []string {
	W := termWidth()
	if W > 84 {
		W = 84
	}
	if W < 40 {
		W = 40
	}
	iw := W - 4
	title := " " + truncate(d.Name, iw-6) + " "
	row := func(s string) string { return c(dimClr, "│ ") + padTo(s, iw) + c(dimClr, " │") }
	out := []string{
		c(dimClr, "╭─") + c(boldClr, title) + c(dimClr, strings.Repeat("─", W-3-vlen(title))+"╮"),
	}

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
		out = append(out, row(fmt.Sprintf("%s downloaded (size unknown)   %s", fmtBytes(sn.Done), c(boldClr+accent, "↓ "+speed))))
	}
	info := fmt.Sprintf("%d conns · %d splits · %d retries · %s elapsed",
		sn.Active, sn.Steals, sn.Retries, fmtDur(time.Since(d.Started)))
	if d.Resumed > 0 {
		info += " · resumed at " + fmtBytes(d.Resumed)
	}
	if !d.Ranges {
		info = "single stream (server has no range support) · " + fmtDur(time.Since(d.Started)) + " elapsed"
	}
	out = append(out, row(c(dimClr, info)))
	if sn.Size > 0 {
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
				switch {
				case mid < s.pos:
					col := okClr
					if s.color >= 0 {
						col = palette[s.color%len(palette)]
					}
					glyph = c(col, "█")
				default:
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

// ---- one download, start to finish ----

func runOne(parent context.Context, rawURL string, opts Options, tty bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	setCurrent(cancel)
	defer setCurrent(nil)

	if tty {
		fmt.Print(c(dimClr, "  connecting…"))
	}
	d, err := Probe(ctx, rawURL, opts)
	if tty {
		fmt.Print("\r\x1b[2K")
	}
	if err != nil {
		fmt.Println(c(errClr, "  ✗ ") + "could not start: " + err.Error())
		return err
	}
	if err := d.Prepare(); err != nil {
		fmt.Println(c(errClr, "  ✗ ") + err.Error())
		return err
	}
	mode := fmt.Sprintf("%d connections", d.Workers())
	if !d.Ranges {
		mode = c(warnClr, "single stream: server does not allow split downloads")
	}
	size := "unknown size"
	if d.Size > 0 {
		size = fmtBytes(d.Size)
	}
	line := fmt.Sprintf("  %s %s · %s · %s", c(accent, "↳"), c(boldClr, d.Name), size, mode)
	if d.Resumed > 0 {
		line += c(okClr, fmt.Sprintf(" · resuming, %s already done", fmtBytes(d.Resumed)))
	}
	fmt.Println(line)

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

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
	for {
		select {
		case err := <-done:
			if tty {
				r.erase()
			}
			report(d, err)
			return err
		case <-tick.C:
			sn := d.Snapshot()
			r.update(sn)
			if tty {
				r.draw(d, sn)
			} else if sn.Size > 0 {
				fmt.Printf("  %5.1f%%  %s / %s  %s/s\n", float64(sn.Done)*100/float64(sn.Size),
					fmtBytes(sn.Done), fmtBytes(sn.Size), fmtBytes(int64(r.speed)))
			}
		}
	}
}

func report(d *Download, err error) {
	el := d.Finished.Sub(d.Started)
	switch {
	case err == nil:
		avg := float64(d.written.Load()) / el.Seconds()
		fmt.Printf("  %s %s  %s in %s · avg %s/s · %d splits\n", c(okClr, "✓"), c(boldClr, d.Name),
			fmtBytes(d.Size), fmtDur(el), fmtBytes(int64(avg)), d.steals.Load())
		fmt.Println("    " + c(dimClr, "→ "+tildify(d.Path)))
	case errors.Is(err, context.Canceled):
		sn := d.Snapshot()
		msg := "stopped"
		if d.Ranges && sn.Size > 0 {
			msg = fmt.Sprintf("paused at %.1f%%, paste the same link again to resume", float64(sn.Done)*100/float64(sn.Size))
		}
		fmt.Printf("  %s %s  %s\n", c(warnClr, "■"), c(boldClr, d.Name), msg)
	default:
		fmt.Printf("  %s %s  %s\n", c(errClr, "✗"), c(boldClr, d.Name), err.Error())
		if d.Ranges && exists(d.Part) {
			fmt.Println("    " + c(dimClr, "partial file kept, paste the same link again to resume"))
		}
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
