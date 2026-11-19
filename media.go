package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Some links open a web page instead of a file. When yt-dlp is installed,
// swoop asks it which files the page offers, lets the user pick a quality,
// then downloads them with its own engine (ffmpeg joins separate picture
// and sound files).

type pageInfo struct {
	Title     string       `json:"title"`
	Duration  float64      `json:"duration"`
	Formats   []pageFormat `json:"formats"`
	Requested []pageFormat `json:"requested_formats"`
	pageFormat
}

type pageFormat struct {
	ID       string            `json:"format_id"`
	Ext      string            `json:"ext"`
	VCodec   string            `json:"vcodec"`
	ACodec   string            `json:"acodec"`
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Size     int64             `json:"filesize"`
	Approx   int64             `json:"filesize_approx"`
	TBR      float64           `json:"tbr"`
	Protocol string            `json:"protocol"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"http_headers"`
	Note     string            `json:"format_note"`
	DL       struct {
		ChunkSize int64 `json:"http_chunk_size"`
	} `json:"downloader_options"`
}

func (f pageFormat) hasVideo() bool { return f.VCodec != "" && f.VCodec != "none" }
func (f pageFormat) hasAudio() bool { return f.ACodec != "" && f.ACodec != "none" }
func (f pageFormat) size() int64 {
	if f.Size > 0 {
		return f.Size
	}
	return f.Approx
}

var noteP = regexp.MustCompile(`^(\d{3,4})p`)

// p is the quality name people know (1080p). It comes from the site's own
// label when there is one; otherwise it's worked out from the long side of
// the picture, so ultra-wide (1920x886) and vertical (1080x1920) videos
// still come out as 1080p.
func (f pageFormat) p() int {
	if m := noteP.FindStringSubmatch(f.Note); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	long, short := f.Width, f.Height
	if short > long {
		long, short = short, long
	}
	if long == 0 {
		return short
	}
	for _, l := range []struct{ long, p int }{{7680, 4320}, {5120, 2880}, {3840, 2160}, {2560, 1440},
		{1920, 1080}, {1280, 720}, {854, 480}, {640, 360}, {426, 240}, {256, 144}} {
		if long >= l.long*95/100 {
			return l.p
		}
	}
	return short
}

func (f pageFormat) direct() bool { return f.Protocol == "https" || f.Protocol == "http" }

// rank prefers files swoop can split (plain https), then mp4 (plays
// everywhere, including QuickTime), then higher quality.
func rank(f pageFormat, wantExt string) float64 {
	r := f.TBR
	if f.direct() {
		r += 1e7
	}
	if f.Ext == wantExt {
		r += 1e6
	}
	return r
}

type quality struct {
	choice string // "p:1080", "audio" or "best"
	label  string
	video  *pageFormat
	audio  *pageFormat
}

// selector is the same choice in yt-dlp's words, for when yt-dlp downloads.
func (q quality) selector() string {
	switch {
	case q.video != nil && q.audio != nil:
		return q.video.ID + "+" + q.audio.ID
	case q.video != nil:
		return q.video.ID
	case q.audio != nil:
		return q.audio.ID
	}
	return "bv*+ba/b"
}

// qualities builds the menu: one line per quality the page offers, plus
// sound only. Each line already knows exactly which files it needs.
func qualities(info pageInfo) []quality {
	bestAudio := func(wantExt string) *pageFormat {
		var best *pageFormat
		for i := range info.Formats {
			f := &info.Formats[i]
			if f.hasAudio() && !f.hasVideo() && (best == nil || rank(*f, wantExt) > rank(*best, wantExt)) {
				best = f
			}
		}
		return best
	}
	byP := map[int]*pageFormat{}
	for i := range info.Formats {
		f := &info.Formats[i]
		if !f.hasVideo() || f.p() <= 0 {
			continue
		}
		if cur := byP[f.p()]; cur == nil || rank(*f, "mp4") > rank(*cur, "mp4") {
			byP[f.p()] = f
		}
	}
	var ps []int
	for p := range byP {
		ps = append(ps, p)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ps)))
	if len(ps) > 6 {
		ps = ps[:6]
	}
	var out []quality
	for _, p := range ps {
		q := quality{choice: "p:" + strconv.Itoa(p), video: byP[p]}
		size := q.video.size()
		if !q.video.hasAudio() {
			want := "m4a"
			if q.video.Ext == "webm" {
				want = "webm"
			}
			if q.audio = bestAudio(want); q.audio == nil {
				continue // picture without any sound to go with it
			}
			if size > 0 {
				size += q.audio.size()
			}
		}
		tag := ""
		switch {
		case p >= 4320:
			tag = "8K"
		case p >= 2160:
			tag = "4K"
		case p >= 1440:
			tag = "2K"
		case p >= 720:
			tag = "HD"
		}
		q.label = fmt.Sprintf("%-6s %-3s", strconv.Itoa(p)+"p", tag)
		if size > 0 {
			q.label += "  ~" + fmtBytes(size)
		}
		out = append(out, q)
	}
	if a := bestAudio("m4a"); a != nil {
		q := quality{choice: "audio", audio: a, label: "sound only"}
		if a.size() > 0 {
			q.label = fmt.Sprintf("%-10s  ~%s", "sound only", fmtBytes(a.size()))
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		out = append(out, quality{choice: "best", label: "best available"})
	}
	return out
}

// streams lists the files to download for a quality.
func (q quality) streams(info pageInfo) []pageFormat {
	var out []pageFormat
	if q.video != nil {
		out = append(out, *q.video)
	}
	if q.audio != nil {
		out = append(out, *q.audio)
	}
	if len(out) == 0 { // "best": whatever yt-dlp picked by default
		out = info.Requested
		if len(out) == 0 && info.URL != "" {
			out = []pageFormat{info.pageFormat}
		}
	}
	return out
}

func ytdlpJSON(ctx context.Context, bin string, args ...string) (pageInfo, error) {
	var info pageInfo
	cmd := exec.CommandContext(ctx, bin, append([]string{"-J", "--no-playlist", "--no-warnings"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(out, &info)
	return info, err
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

func runMedia(pageURL, choice string, opts Options, tty bool, st *Store) error {
	bin, err := exec.LookPath("yt-dlp")
	if err != nil {
		fail("This link opens a web page, not a file, so there's nothing to download directly.")
		fmt.Println("    " + c(dimClr, "If you install yt-dlp, swoop can use it to find the file on pages like this."))
		return err
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		fail("can't use the download folder: " + err.Error())
		return err
	}
	if tty {
		fmt.Print(c(dimClr, "  looking at the page…"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	info, err := ytdlpJSON(ctx, bin, pageURL)
	cancel()
	if tty {
		fmt.Print("\r\x1b[2K")
	}
	if err != nil || (len(info.Formats) == 0 && info.URL == "") {
		fail("No downloadable file found on this page.")
		return errors.New("nothing found")
	}
	title := sanitize(info.Title)
	if title == "" {
		title = "download"
	}

	head := fmt.Sprintf("  %s %s", c(accent, "↳"), c(boldClr, truncate(info.Title, termWidth()-20)))
	if info.Duration > 0 {
		head += c(dimClr, " · "+fmtDur(time.Duration(info.Duration)*time.Second))
	}
	fmt.Println(head)

	qs := qualities(info)
	var q quality
	if choice != "" { // continuing from the list: same quality as before
		choice = strings.Replace(choice, "r:", "p:", 1) // entries from v0.2.0 test builds
		q = qs[0]
		for _, x := range qs {
			if x.choice == choice {
				q = x
			}
		}
	} else {
		i := 0
		if len(qs) > 1 {
			labels := make([]string, len(qs))
			for k, x := range qs {
				labels[k] = x.label
			}
			if i = pick("choose quality", labels); i < 0 {
				fmt.Println("  " + c(dimClr, "cancelled"))
				return nil
			}
		}
		q = qs[i]
	}
	choice = q.choice
	streams := q.streams(info)
	if len(streams) == 0 {
		fail("No downloadable file found on this page.")
		return errors.New("nothing found")
	}

	tmp := filepath.Join(opts.Dir, ".swoop-tmp", shortHash(pageURL+"|"+choice))
	e := st.track(&Entry{Kind: "media", URL: pageURL, Format: choice, Name: info.Title, Dir: opts.Dir, Extra: []string{tmp}})

	direct := true
	for _, s := range streams {
		if s.Protocol != "https" && s.Protocol != "http" {
			direct = false
		}
	}
	if len(streams) > 1 {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			fail("This quality comes as separate picture and sound files, and joining them needs ffmpeg (free):")
			fmt.Println("      " + c(dimClr, "Mac:     ") + "brew install ffmpeg")
			fmt.Println("      " + c(dimClr, "Windows: ") + "winget install ffmpeg")
			fmt.Println("      " + c(dimClr, "Linux:   ") + "sudo apt install ffmpeg")
			st.finish(e, "failed", 0)
			return err
		}
	}

	var out outcome
	var final string
	if direct {
		out, final, err = mediaDirect(pageURL, choice, title, streams, tmp, opts, tty)
		var se *statusErr
		if out == failed && errors.As(err, &se) && (se.code == 403 || se.code == 429) {
			// the site doesn't like split downloading: let yt-dlp fetch it its own way
			debugf("direct download refused: %v", err)
			fmt.Println("  " + c(dimClr, "this site refused split downloading, switching to a slower method…"))
			out, final, err = mediaHandoff(bin, pageURL, q.selector(), tmp, opts, tty)
		}
	} else {
		out, final, err = mediaHandoff(bin, pageURL, q.selector(), tmp, opts, tty)
	}
	switch out {
	case finished:
		removeTemp(tmp)
		e.Path = final
		var size int64
		if fi, err := os.Stat(final); err == nil {
			size = fi.Size()
		}
		e.Size = size
		st.finish(e, "done", size)
		fmt.Printf("  %s %s  %s\n", c(okClr, "✓"), c(boldClr, truncate(filepath.Base(final), termWidth()-30)), fmtBytes(size))
		fmt.Println("    " + c(dimClr, "→ "+tildify(final)))
	case paused:
		st.finish(e, "paused", 0)
		fmt.Printf("  %s %s  paused%s\n", c(warnClr, "■"), c(boldClr, truncate(info.Title, termWidth()-50)), c(dimClr, " · type list to continue it later"))
	case cancelled:
		removeTemp(tmp)
		st.remove(e)
		fmt.Printf("  %s %s  cancelled, unfinished files deleted\n", c(dimClr, "✗"), c(boldClr, truncate(info.Title, termWidth()-40)))
	case failed:
		st.finish(e, "failed", 0)
		fail(friendlyErr(err))
	}
	return outcomeErr(out, err)
}

// mediaDirect downloads each stream with swoop's own engine, then joins them.
func mediaDirect(pageURL, choice, title string, streams []pageFormat, tmp string, opts Options, tty bool) (outcome, string, error) {
	conns := opts.Conns
	if conns > 4 {
		conns = 4 // media sites are touchy about many connections
	}
	var files []string
	for i, s := range streams {
		label := "file"
		if len(streams) > 1 {
			label = "sound"
			if s.hasVideo() {
				label = "picture"
			}
		}
		h := http.Header{}
		for k, v := range s.Headers {
			h.Set(k, v)
		}
		chunk := s.DL.ChunkSize
		if chunk <= 0 {
			chunk = 10 << 20 // small requests look like normal playback
		}
		o := Options{Conns: conns, Dir: tmp, Headers: h, Name: fmt.Sprintf("part%d.%s", i, s.Ext),
			ResumeKey: pageURL + "|" + choice + "|" + s.ID, ChunkSize: chunk}
		if exists(filepath.Join(tmp, o.Name)) { // finished in an earlier run
			files = append(files, filepath.Join(tmp, o.Name))
			continue
		}
		d, err := Probe(context.Background(), s.URL, o)
		if err != nil {
			return failed, "", err
		}
		if err := d.Prepare(); err != nil {
			return failed, "", err
		}
		out, err := downloadHTTP(d, tty, label+" "+strconv.Itoa(i+1)+"/"+strconv.Itoa(len(streams)))
		if out != finished {
			return out, "", err
		}
		files = append(files, d.Path)
	}

	ext := streams[0].Ext
	if len(streams) > 1 {
		ext = "mkv"
		v, a := streams[0].Ext, streams[1].Ext
		if v == "mp4" && (a == "m4a" || a == "mp4") {
			ext = "mp4"
		} else if v == "webm" && a == "webm" {
			ext = "webm"
		}
	}
	final := freeName(opts.Dir, title+"."+ext)
	if len(files) == 1 {
		return finished, final, os.Rename(files[0], final)
	}
	if tty {
		fmt.Print(c(dimClr, "  joining picture and sound…"))
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, f := range files {
		args = append(args, "-i", f)
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", final)
	outb, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if tty {
		fmt.Print("\r\x1b[2K")
	}
	if err != nil {
		os.Remove(final)
		return failed, "", fmt.Errorf("joining failed: %s", strings.TrimSpace(string(outb)))
	}
	return finished, final, nil
}

// mediaHandoff lets yt-dlp download streams that come in many small
// pieces, and shows its progress in swoop's live view.
func mediaHandoff(bin, pageURL, f, tmp string, opts Options, tty bool) (outcome, string, error) {
	cmd := exec.Command(bin, "--no-playlist", "--no-warnings", "-f", f,
		"-N", "8", "--newline", "--progress", "--merge-output-format", "mp4/mkv",
		"--progress-template", "download:SWOOP %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
		"-P", "home:"+opts.Dir, "-P", "temp:"+tmp, "-o", "%(title)s.%(ext)s",
		"--print", "after_move:filepath", pageURL)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed, "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return failed, "", err
	}
	var mu sync.Mutex
	var snap Snap
	var base int64 = -1
	var final string
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if fs := strings.Fields(line); len(fs) == 4 && fs[0] == "SWOOP" {
				done, _ := strconv.ParseInt(strings.Split(fs[1], ".")[0], 10, 64)
				total, _ := strconv.ParseInt(strings.Split(fs[2], ".")[0], 10, 64)
				if total == 0 {
					total, _ = strconv.ParseInt(strings.Split(fs[3], ".")[0], 10, 64)
				}
				mu.Lock()
				if base < 0 || done < snap.Done {
					base = done // a new stream started
				}
				snap = Snap{Size: total, Done: done, Written: done - base, Active: 8}
				mu.Unlock()
			} else if line != "" && !strings.HasPrefix(line, "[") {
				mu.Lock()
				final = line
				mu.Unlock()
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		<-scanned // read all output (including the final file path) before reaping
		done <- cmd.Wait()
	}()
	started := time.Now()
	j := &job{
		title:   func() string { return "downloading from the page" },
		started: started,
		snap:    func() Snap { mu.Lock(); defer mu.Unlock(); return snap },
		done:    done,
		stop: func() {
			if runtime.GOOS == "windows" {
				cmd.Process.Kill()
			} else {
				cmd.Process.Signal(os.Interrupt) // lets it keep what it has for next time
			}
		},
		info: func(Snap) string { return "in pieces · " + fmtDur(time.Since(started)) },
	}
	out, werr := watch(j, tty)
	if out == failed {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		if msg != "" {
			werr = errors.New(msg)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return out, final, werr
}

// removeTemp deletes a download's temp folder, and the shared .swoop-tmp
// folder too once it's empty.
func removeTemp(tmp string) {
	os.RemoveAll(tmp)
	os.Remove(filepath.Dir(tmp))
}

func freeName(dir, name string) string {
	for i := 0; i < 1000; i++ {
		p := filepath.Join(dir, withIndex(name, i))
		if !exists(p) {
			return p
		}
	}
	return filepath.Join(dir, name)
}
