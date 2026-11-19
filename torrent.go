package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Torrents are handled by aria2, a mature open-source download engine.
// swoop starts it in the background, talks to it over its local control
// port, and shows the same live view and keys as for normal downloads.

var errNoAria2 = errors.New("aria2 is not installed")

type aria2 struct {
	endpoint string
	secret   string
	cmd      *exec.Cmd
	client   *http.Client
}

func startAria2(dir string) (*aria2, error) {
	bin, err := exec.LookPath("aria2c")
	if err != nil {
		return nil, errNoAria2
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	sb := make([]byte, 16)
	rand.Read(sb)
	secret := hex.EncodeToString(sb)

	cmd := exec.Command(bin,
		"--enable-rpc", "--rpc-listen-all=false", "--rpc-listen-port="+port, "--rpc-secret="+secret,
		"--dir="+dir, "--continue=true", "--seed-time=0", "--bt-save-metadata=true",
		"--follow-torrent=mem", "--file-allocation=none", "--quiet=true", "--summary-interval=0",
		"--max-connection-per-server=16", "--split=16", "--min-split-size=1M")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := newAria2Client("http://127.0.0.1:"+port+"/jsonrpc", secret)
	a.cmd = cmd
	for i := 0; i < 50; i++ {
		if _, err := a.call("aria2.getVersion"); err == nil {
			return a, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.close()
	return nil, errors.New("aria2 didn't start")
}

func newAria2Client(endpoint, secret string) *aria2 {
	return &aria2{
		endpoint: endpoint,
		secret:   secret,
		client:   &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

func (a *aria2) call(method string, params ...any) (json.RawMessage, error) {
	p := append([]any{"token:" + a.secret}, params...)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "swoop", "method": method, "params": p})
	resp, err := a.client.Post(a.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, errors.New(out.Error.Message)
	}
	return out.Result, nil
}

// close stops aria2. It writes its .aria2 progress file first, which is
// what lets a paused torrent continue later.
func (a *aria2) close() {
	if a.cmd == nil {
		return
	}
	a.call("aria2.forceShutdown")
	exited := make(chan struct{})
	go func() { a.cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		a.cmd.Process.Kill()
		<-exited
	}
}

type a2status struct {
	Status          string   `json:"status"`
	TotalLength     string   `json:"totalLength"`
	CompletedLength string   `json:"completedLength"`
	Connections     string   `json:"connections"`
	NumSeeders      string   `json:"numSeeders"`
	FollowedBy      []string `json:"followedBy"`
	ErrorMessage    string   `json:"errorMessage"`
	InfoHash        string   `json:"infoHash"`
	Dir             string   `json:"dir"`
	Bittorrent      struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
}

func (a *aria2) status(gid string) (a2status, error) {
	var s a2status
	raw, err := a.call("aria2.tellStatus", gid, []string{"status", "totalLength", "completedLength",
		"connections", "numSeeders", "followedBy", "errorMessage", "infoHash", "dir", "bittorrent"})
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s)
	return s, err
}

func num(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// torrentRun follows one torrent inside aria2. A magnet link first fetches
// the torrent's info ("metadata"), then aria2 starts the real download
// under a new id, which we follow.
type torrentRun struct {
	a        *aria2
	gid      string
	mu       sync.Mutex
	st       a2status
	start    int64 // bytes already on disk when this run started
	haveBase bool
	resuming bool // the list says this torrent was paused before
	stopped  atomic.Bool
}

func (t *torrentRun) poll(done chan<- error) {
	fails := 0
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		if t.stopped.Load() {
			done <- context.Canceled
			return
		}
		s, err := t.a.status(t.gid)
		if err != nil {
			if fails++; fails > 10 {
				done <- err
				return
			}
			continue
		}
		fails = 0
		if len(s.FollowedBy) > 0 {
			t.gid = s.FollowedBy[0] // torrent info is in, the real download starts now
			continue
		}
		t.mu.Lock()
		t.st = s
		if !t.haveBase && s.Bittorrent.Info.Name != "" && num(s.TotalLength) > 0 {
			t.haveBase = true
			if t.resuming {
				t.start = num(s.CompletedLength)
			}
		}
		t.mu.Unlock()
		switch s.Status {
		case "complete":
			done <- nil
			return
		case "error":
			msg := s.ErrorMessage
			if msg == "" {
				msg = "the torrent failed"
			}
			done <- errors.New(msg)
			return
		case "removed":
			done <- context.Canceled
			return
		}
	}
}

func (t *torrentRun) snapshot() Snap {
	t.mu.Lock()
	defer t.mu.Unlock()
	done := num(t.st.CompletedLength)
	w := done - t.start
	if w < 0 {
		w = 0
	}
	return Snap{Size: num(t.st.TotalLength), Done: done, Written: w, Active: int(num(t.st.Connections))}
}

func (t *torrentRun) name() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Bittorrent.Info.Name
}

// ---- input detection ----

// torrentInput reports whether the user gave a torrent, and returns it
// cleaned up (a magnet link, a .torrent link, or a .torrent file path).
func torrentInput(s string) (string, bool) {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "magnet:?") {
		return s, true
	}
	if p := localPath(s); p != "" && strings.HasSuffix(strings.ToLower(p), ".torrent") {
		return p, true
	}
	if u, err := url.Parse(s); err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		strings.HasSuffix(strings.ToLower(u.Path), ".torrent") {
		return s, true
	}
	return "", false
}

// localPath turns what a terminal pastes for a dragged file ("~/a\ b.torrent",
// quoted paths, file:// links) into a real path, if that file exists.
func localPath(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if strings.HasPrefix(s, "file://") {
		if u, err := url.Parse(s); err == nil {
			s = u.Path
		}
	}
	s = strings.ReplaceAll(s, `\ `, " ")
	s = expandHome(s)
	if fi, err := os.Stat(s); err == nil && !fi.IsDir() {
		abs, _ := filepath.Abs(s)
		return abs
	}
	return ""
}

func magnetName(m string) string {
	if u, err := url.Parse(m); err == nil {
		if dn := u.Query().Get("dn"); dn != "" {
			return dn
		}
	}
	return "torrent"
}

// ---- run ----

func runTorrent(input string, opts Options, tty bool, st *Store) error {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		fail("can't use the download folder: " + err.Error())
		return err
	}
	if tty {
		fmt.Print(c(dimClr, "  starting the torrent engine…"))
	}
	a, err := startAria2(opts.Dir)
	if tty {
		fmt.Print("\r\x1b[2K")
	}
	if errors.Is(err, errNoAria2) {
		fail("Torrents need a free helper app called aria2. Install it once, then paste the link again:")
		fmt.Println("      " + c(dimClr, "Mac:     ") + "brew install aria2")
		fmt.Println("      " + c(dimClr, "Windows: ") + "winget install aria2.aria2")
		fmt.Println("      " + c(dimClr, "Linux:   ") + "sudo apt install aria2")
		return err
	}
	if err != nil {
		fail("couldn't start the torrent engine: " + err.Error())
		return err
	}
	defer a.close()
	return followTorrent(a, input, opts, tty, st)
}

func followTorrent(a *aria2, input string, opts Options, tty bool, st *Store) error {
	var raw json.RawMessage
	var err error
	if strings.HasPrefix(strings.ToLower(input), "magnet:") || strings.Contains(input, "://") {
		raw, err = a.call("aria2.addUri", []string{input})
	} else {
		b, rerr := os.ReadFile(input)
		if rerr != nil {
			fail("couldn't read the torrent file: " + rerr.Error())
			return rerr
		}
		raw, err = a.call("aria2.addTorrent", base64.StdEncoding.EncodeToString(b))
	}
	var gid string
	if err == nil {
		err = json.Unmarshal(raw, &gid)
	}
	if err != nil {
		fail("the torrent engine didn't accept that: " + err.Error())
		return err
	}

	guess := magnetName(input)
	if !strings.HasPrefix(strings.ToLower(input), "magnet:") {
		guess = strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	}
	startLine(guess, "torrent", "finding peers", "")
	prev := st.find("torrent", input, "")
	resuming := prev != nil && prev.Done > 0
	e := st.track(&Entry{Kind: "torrent", URL: input, Name: guess, Dir: opts.Dir})

	t := &torrentRun{a: a, gid: gid, resuming: resuming}
	done := make(chan error, 1)
	go t.poll(done)
	started := time.Now()
	j := &job{
		title: func() string {
			if n := t.name(); n != "" {
				return n
			}
			return guess + " (getting torrent info…)"
		},
		started: started,
		snap:    t.snapshot,
		done:    done,
		stop:    func() { t.stopped.Store(true) },
		info: func(sn Snap) string {
			t.mu.Lock()
			seeds := num(t.st.NumSeeders)
			base := t.start
			t.mu.Unlock()
			s := fmt.Sprintf("%d seeds · %d connections · %s", seeds, sn.Active, fmtDur(time.Since(started)))
			if base > 0 {
				s += " · continued from " + fmtBytes(base)
			}
			return s
		},
	}
	out, werr := watch(j, tty)

	t.mu.Lock()
	s := t.st
	t.mu.Unlock()
	if name := s.Bittorrent.Info.Name; name != "" {
		dir := s.Dir
		if dir == "" {
			dir = opts.Dir
		}
		e.Name, e.Path = name, filepath.Join(dir, name)
		if s.InfoHash != "" {
			e.Extra = []string{filepath.Join(dir, s.InfoHash+".torrent")}
		}
	}
	e.Size = num(s.TotalLength)
	sn := t.snapshot()
	switch out {
	case finished:
		st.finish(e, "done", e.Size)
		fmt.Printf("  %s %s  %s in %s\n", c(okClr, "✓"), c(boldClr, truncate(e.Name, termWidth()-40)), fmtBytes(e.Size), fmtDur(time.Since(started)))
		fmt.Println("    " + c(dimClr, "→ "+tildify(e.Path)))
	case paused:
		st.finish(e, "paused", sn.Done)
		reportPaused(e.Name, sn.Done, sn.Size, true)
	case cancelled:
		a.close() // let go of the files before deleting them
		e.deleteFiles()
		st.remove(e)
		fmt.Printf("  %s %s  cancelled, unfinished files deleted\n", c(dimClr, "✗"), c(boldClr, truncate(e.Name, termWidth()-40)))
	case failed:
		st.finish(e, "failed", sn.Done)
		fmt.Printf("  %s %s  %s\n", c(errClr, "✗"), c(boldClr, truncate(e.Name, termWidth()-40)), werr.Error())
	}
	return outcomeErr(out, werr)
}
