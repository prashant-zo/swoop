package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	minSeg       = 1 << 20   // never split a piece smaller than 1 MiB
	bufSize      = 256 << 10 // per-connection read buffer (must stay < minSeg)
	maxRetries   = 5
	stallTimeout = 15 * time.Second
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var errNoRange = errors.New("server stopped honoring range requests")

// seg is one byte range [pos, end) being downloaded by one connection.
// Only the owning worker advances pos. Only a stealer holding Download.mu
// shrinks end. Because a steal always leaves >= minSeg for the owner and a
// single write is <= bufSize, an in-flight write can never cross the split.
type seg struct {
	start int64 // where this piece began (display only, immutable)
	color int   // worker that started it (display only, immutable)
	pos   atomic.Int64
	end   atomic.Int64
	owner int // worker id, -1 = nobody; guarded by Download.mu
}

type Options struct {
	Conns     int
	Dir       string
	Headers   http.Header
	Name      string // save under this name instead of the server's
	ResumeKey string // identity for resume when the URL changes between runs
	ChunkSize int64  // max bytes per request, for servers that refuse big ones
	NoSteal   bool   // benchmarking only: static split like basic downloaders
}

type Download struct {
	URL      string
	FinalURL string
	Name     string
	Path     string
	Part     string
	Size     int64 // -1 when the server does not say
	ETag     string
	CType    string // Content-Type of the first response
	LastMod  string
	Ranges   bool
	Resumed  int64
	Started  time.Time
	Finished time.Time

	opts   Options
	client *http.Client
	file   *os.File
	cancel context.CancelFunc

	mu   sync.Mutex
	segs []*seg

	written atomic.Int64
	active  atomic.Int32
	retries atomic.Int32
	steals  atomic.Int32

	errMu   sync.Mutex
	lastErr error
	fatal   error
}

func newClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Force HTTP/1.1. With HTTP/2, all "connections" would be multiplexed
		// onto ONE TCP socket, which defeats the whole point of splitting.
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ReadBufferSize:        bufSize,
	}
	// No Client.Timeout: it would kill long downloads. Stalls are handled
	// per connection by a watchdog instead.
	return &http.Client{Transport: tr}
}

func (d *Download) newReq(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	for k, vs := range d.opts.Headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// Probe asks for the first byte only. GET with Range works on servers and
// CDNs that reject HEAD (S3 presigned URLs, many file hosts).
func Probe(ctx context.Context, rawURL string, opts Options) (*Download, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		d, err := probeOnce(ctx, rawURL, opts)
		if err == nil {
			return d, nil
		}
		lastErr = err
		var se *statusErr
		if errors.As(err, &se) || ctx.Err() != nil {
			break // the server answered clearly, asking again won't change it
		}
	}
	return nil, lastErr
}

func probeOnce(ctx context.Context, rawURL string, opts Options) (*Download, error) {
	d := &Download{URL: rawURL, opts: opts, client: newClient(), Size: -1}
	req, err := d.newReq(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &statusErr{resp.StatusCode, resp.Status}
	}
	d.FinalURL = resp.Request.URL.String()
	d.CType = resp.Header.Get("Content-Type")
	d.ETag = resp.Header.Get("ETag")
	d.LastMod = resp.Header.Get("Last-Modified")
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if total := parseTotal(resp.Header.Get("Content-Range")); total > 0 {
			d.Size, d.Ranges = total, true
		}
	case http.StatusOK:
		d.Size = resp.ContentLength
	}
	d.Name = pickName(resp, d.FinalURL)
	if opts.Name != "" {
		d.Name = sanitize(opts.Name)
	}
	return d, nil
}

// IsPage reports whether the link opened a web page instead of a file.
func (d *Download) IsPage() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.CType)), "text/html")
}

func (d *Download) resumeKey() string {
	if d.opts.ResumeKey != "" {
		return d.opts.ResumeKey
	}
	return d.URL
}

// Prepare picks the output file. If a matching .part + state file exists,
// it resumes from there instead of starting over.
func (d *Download) Prepare() error {
	if err := os.MkdirAll(d.opts.Dir, 0o755); err != nil {
		return err
	}
	for i := 0; i < 1000; i++ {
		name := withIndex(d.Name, i)
		final := filepath.Join(d.opts.Dir, name)
		part := final + ".part"
		if d.Ranges {
			if st, ok := loadState(part + ".json"); ok && st.URL == d.resumeKey() && st.Size == d.Size && st.ETag == d.ETag && exists(part) {
				d.Name, d.Path, d.Part = name, final, part
				var pending int64
				for _, r := range st.Segs {
					if r[0] < r[1] {
						s := &seg{start: r[0], color: -1, owner: -1}
						s.pos.Store(r[0])
						s.end.Store(r[1])
						d.segs = append(d.segs, s)
						pending += r[1] - r[0]
					}
				}
				d.Resumed = d.Size - pending
				f, err := os.OpenFile(part, os.O_RDWR, 0o644)
				d.file = f
				return err
			}
		}
		if !exists(final) && !exists(part) {
			d.Name, d.Path, d.Part = name, final, part
			break
		}
	}
	if d.Path == "" {
		return errors.New("could not find a free file name")
	}
	f, err := os.OpenFile(d.Part, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	d.file = f
	if d.Size > 0 {
		if err := f.Truncate(d.Size); err != nil { // sparse pre-allocation
			return err
		}
	}
	if d.Ranges {
		n := d.opts.Conns
		if max := int(d.Size / minSeg); n > max {
			n = max
		}
		if n < 1 {
			n = 1
		}
		step := d.Size / int64(n)
		for i := 0; i < n; i++ {
			s := &seg{start: int64(i) * step, color: -1, owner: -1}
			s.pos.Store(s.start)
			end := s.start + step
			if i == n-1 {
				end = d.Size
			}
			s.end.Store(end)
			d.segs = append(d.segs, s)
		}
	}
	return nil
}

// Run downloads everything, then renames .part to the final name.
func (d *Download) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	defer cancel()
	if d.Started.IsZero() {
		d.Started = time.Now()
	}

	var err error
	if d.Ranges {
		err = d.runParallel(ctx)
	} else {
		err = d.runSingle(ctx)
	}
	d.Finished = time.Now()
	if err != nil {
		d.file.Close()
		return err
	}
	if err := d.file.Close(); err != nil {
		return err
	}
	os.Remove(d.Part + ".json")
	return os.Rename(d.Part, d.Path)
}

// Workers returns how many connections this download will open.
func (d *Download) Workers() int {
	if !d.Ranges {
		return 1
	}
	workers := d.opts.Conns
	if max := int(d.pending() / minSeg); workers > max {
		workers = max
	}
	if workers < len(d.segs) {
		workers = len(d.segs)
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// presplit cuts the remaining work into enough pieces before any request
// goes out. Stealing after a request is in flight wastes the bytes the
// server already sent, so on resume we split up front instead.
func (d *Download) presplit(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.segs) < n {
		var victim *seg
		var best int64
		for _, s := range d.segs {
			if rem := s.end.Load() - s.pos.Load(); rem > best {
				best, victim = rem, s
			}
		}
		if victim == nil || best < 2*minSeg {
			return
		}
		pos, end := victim.pos.Load(), victim.end.Load()
		mid := pos + (end-pos)/2
		ns := &seg{start: mid, color: -1, owner: -1}
		ns.pos.Store(mid)
		ns.end.Store(end)
		victim.end.Store(mid)
		d.segs = append(d.segs, ns)
	}
}

func (d *Download) runParallel(ctx context.Context) error {
	workers := d.Workers()
	d.presplit(workers)
	stop := make(chan struct{})
	saved := make(chan struct{})
	go func() { // checkpoint once a second so a crash loses at most ~1s
		defer close(saved)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.saveState()
			case <-stop:
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			d.worker(ctx, id)
		}(i)
	}
	wg.Wait()
	close(stop)
	<-saved
	if d.pending() > 0 {
		d.saveState()
		if d.fatal != nil {
			return d.fatal
		}
		if ctx.Err() != nil {
			return context.Canceled
		}
		d.errMu.Lock()
		defer d.errMu.Unlock()
		return fmt.Errorf("incomplete after retries: %w", d.lastErr)
	}
	return nil
}

func (d *Download) worker(ctx context.Context, id int) {
	buf := make([]byte, bufSize)
	var s *seg
	attempts := 0
	for ctx.Err() == nil {
		if s == nil || s.pos.Load() >= s.end.Load() {
			if s = d.claim(id); s == nil {
				return
			}
			attempts = 0
		}
		progressed, err := d.fetch(ctx, s, buf)
		if err == nil {
			attempts = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if progressed {
			attempts = 0
		}
		attempts++
		d.retries.Add(1)
		d.setErr(err)
		var se *statusErr
		isStatus := errors.As(err, &se)
		tooMany := isStatus && (se.code == 429 || se.code == 503)
		// a refusal like 403 or 404 rarely fixes itself, so give up sooner
		refused := isStatus && se.code >= 400 && se.code < 500 && se.code != 408 && se.code != 429 && attempts >= 2
		if errors.Is(err, errNoRange) || attempts > maxRetries || refused || (tooMany && d.active.Load() > 0) {
			// Server limits connections (or this one keeps failing): hand the
			// piece back so a healthy connection can take it, and retire.
			d.release(s)
			return
		}
		backoff := time.Duration(250<<attempts) * time.Millisecond // 0.5s .. 8s
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}

// claim gives a worker its next piece: an unowned one first, otherwise it
// steals the second half of the biggest remaining piece (dynamic
// segmentation, so the end of the download does not crawl on one socket).
func (d *Download) claim(id int) *seg {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.segs {
		if s.owner == -1 && s.end.Load() > s.pos.Load() {
			s.owner = id
			return s
		}
	}
	if d.opts.NoSteal {
		return nil
	}
	var victim *seg
	var best int64
	for _, s := range d.segs {
		if rem := s.end.Load() - s.pos.Load(); rem > best {
			best, victim = rem, s
		}
	}
	if victim == nil || best < 2*minSeg {
		return nil
	}
	pos, end := victim.pos.Load(), victim.end.Load()
	mid := pos + (end-pos)/2
	ns := &seg{start: mid, color: id, owner: id}
	ns.pos.Store(mid)
	ns.end.Store(end)
	victim.end.Store(mid)
	d.segs = append(d.segs, ns)
	d.steals.Add(1)
	return ns
}

func (d *Download) release(s *seg) {
	d.mu.Lock()
	s.owner = -1
	d.mu.Unlock()
}

func (d *Download) fetch(ctx context.Context, s *seg, buf []byte) (bool, error) {
	pos, end := s.pos.Load(), s.end.Load()
	if pos >= end {
		return false, nil
	}
	// Some servers refuse big requests, so a piece may be fetched as several
	// smaller requests one after another on the same connection.
	stop := end
	if c := d.opts.ChunkSize; c > 0 && stop-pos > c {
		stop = pos + c
	}
	limit := func() int64 {
		if e := s.end.Load(); e < stop {
			return e
		}
		return stop
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := d.newReq(cctx, d.FinalURL)
	if err != nil {
		return false, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, stop-1))
	d.active.Add(1)
	defer d.active.Add(-1)
	resp, err := d.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK {
			return false, errNoRange
		}
		return false, &statusErr{resp.StatusCode, resp.Status}
	}
	if start := parseStart(resp.Header.Get("Content-Range")); start != pos {
		return false, errNoRange
	}
	d.mu.Lock()
	if s.color < 0 {
		s.color = s.owner
	}
	d.mu.Unlock()

	watchdog := time.AfterFunc(stallTimeout, cancel) // no bytes for 15s = reconnect
	defer watchdog.Stop()
	progressed := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			p, e := s.pos.Load(), limit()
			if int64(n) > e-p {
				n = int(e - p)
			}
			if n > 0 {
				if _, werr := d.file.WriteAt(buf[:n], p); werr != nil {
					d.fail(werr)
					return progressed, werr
				}
				s.pos.Store(p + int64(n)) // only after the bytes are written
				d.written.Add(int64(n))
				progressed = true
			}
			if s.pos.Load() >= limit() {
				if s.pos.Load() < s.end.Load() {
					// this chunk is done but the piece goes on: read to the end of
					// the response so the connection can be reused for the next one
					io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
				}
				return progressed, nil // done, chunk done, or the rest was stolen
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if s.pos.Load() >= limit() {
					return progressed, nil
				}
				return progressed, io.ErrUnexpectedEOF
			}
			return progressed, rerr
		}
	}
}

// runSingle is the fallback for servers without range support: one stream,
// and a retry has to start over from byte 0.
func (d *Download) runSingle(ctx context.Context) error {
	end := d.Size
	if end <= 0 {
		end = 1<<62 - 1
	}
	s := &seg{start: 0, color: 0, owner: 0}
	s.end.Store(end)
	d.mu.Lock()
	d.segs = []*seg{s}
	d.mu.Unlock()

	buf := make([]byte, bufSize)
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			d.retries.Add(1)
			select {
			case <-time.After(time.Duration(250<<attempt) * time.Millisecond):
			case <-ctx.Done():
				return context.Canceled
			}
		}
		s.pos.Store(0)
		d.written.Store(0)
		err := func() error {
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			req, err := d.newReq(cctx, d.FinalURL)
			if err != nil {
				return err
			}
			d.active.Add(1)
			defer d.active.Add(-1)
			resp, err := d.client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return &statusErr{resp.StatusCode, resp.Status}
			}
			watchdog := time.AfterFunc(stallTimeout, cancel)
			defer watchdog.Stop()
			var off int64
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					watchdog.Reset(stallTimeout)
					if _, werr := d.file.WriteAt(buf[:n], off); werr != nil {
						return werr
					}
					off += int64(n)
					s.pos.Store(off)
					d.written.Add(int64(n))
				}
				if rerr == io.EOF {
					if d.Size > 0 && off != d.Size {
						return io.ErrUnexpectedEOF
					}
					if d.Size <= 0 {
						d.Size = off
						s.end.Store(off)
					}
					return d.file.Truncate(off)
				}
				if rerr != nil {
					return rerr
				}
			}
		}()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return context.Canceled
		}
		lastErr = err
	}
	return fmt.Errorf("failed after retries: %v", lastErr)
}

func (d *Download) pending() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var p int64
	for _, s := range d.segs {
		if r := s.end.Load() - s.pos.Load(); r > 0 {
			p += r
		}
	}
	return p
}

func (d *Download) setErr(err error) {
	d.errMu.Lock()
	d.lastErr = err
	d.errMu.Unlock()
}

func (d *Download) fail(err error) {
	d.errMu.Lock()
	if d.fatal == nil {
		d.fatal = err
	}
	d.errMu.Unlock()
	if d.cancel != nil {
		d.cancel()
	}
}

// ---- resume state ----

type state struct {
	URL     string     `json:"url"`
	Size    int64      `json:"size"`
	ETag    string     `json:"etag"`
	LastMod string     `json:"last_modified"`
	Segs    [][2]int64 `json:"segs"`
}

func (d *Download) saveState() {
	st := state{URL: d.resumeKey(), Size: d.Size, ETag: d.ETag, LastMod: d.LastMod}
	d.mu.Lock()
	for _, s := range d.segs {
		if p, e := s.pos.Load(), s.end.Load(); p < e {
			st.Segs = append(st.Segs, [2]int64{p, e})
		}
	}
	d.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := d.Part + ".json.tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, d.Part+".json")
	}
}

func loadState(p string) (state, bool) {
	var st state
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

// ---- snapshot for the UI ----

type segView struct {
	start, pos, end int64
	color           int
	active          bool
}

type Snap struct {
	Size    int64
	Done    int64
	Written int64
	Active  int
	Retries int
	Steals  int
	Segs    []segView
}

func (d *Download) Snapshot() Snap {
	sn := Snap{
		Size:    d.Size,
		Written: d.written.Load(),
		Active:  int(d.active.Load()),
		Retries: int(d.retries.Load()),
		Steals:  int(d.steals.Load()),
	}
	d.mu.Lock()
	var pending int64
	for _, s := range d.segs {
		v := segView{start: s.start, pos: s.pos.Load(), end: s.end.Load(), color: s.color}
		v.active = s.owner >= 0 && v.pos < v.end
		if r := v.end - v.pos; r > 0 {
			pending += r
		}
		sn.Segs = append(sn.Segs, v)
	}
	d.mu.Unlock()
	if d.Ranges && d.Size > 0 {
		sn.Done = d.Size - pending
	} else {
		sn.Done = sn.Written
	}
	return sn
}

// ---- helpers ----

type statusErr struct {
	code int
	text string
}

func (e *statusErr) Error() string { return "server said " + e.text }

// "bytes 0-0/12345" -> 12345 (or -1)
func parseTotal(cr string) int64 {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// "bytes 100-199/12345" -> 100 (or -1)
func parseStart(cr string) int64 {
	cr = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cr), "bytes"))
	i := strings.IndexByte(cr, '-')
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[:i]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func pickName(resp *http.Response, finalURL string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := sanitize(params["filename"]); fn != "" {
				return fn
			}
		}
	}
	if u, err := url.Parse(finalURL); err == nil {
		base := path.Base(u.Path)
		if un, err := url.PathUnescape(base); err == nil {
			base = un
		}
		if fn := sanitize(base); fn != "" {
			return fn
		}
	}
	name := "download"
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
			name += exts[0]
		}
	}
	return name
}

// sanitize keeps a bare file name: no directories, no control characters,
// so a hostile server cannot write outside the download folder.
func sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || strings.ContainsRune(`/:<>"|?*`, r) { // also illegal on Windows
			return -1
		}
		return r
	}, name)
	name = strings.TrimRight(strings.TrimSpace(name), ". ") // Windows rejects trailing dots/spaces
	if name == "" || name == "." || name == ".." || name == "/" {
		return ""
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		name = name[:200-len(ext)] + ext
	}
	return name
}

func withIndex(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
