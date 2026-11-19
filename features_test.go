package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseKeys(t *testing.T) {
	ks := parseKeys([]byte("p\x1b[A\x1b[B\r\x03x\x7f\x1bé"))
	want := []string{"rune:p", "up", "down", "enter", "ctrl-c", "rune:x", "backspace", "esc", "rune:é"}
	if len(ks) != len(want) {
		t.Fatalf("got %d keys %+v, want %d", len(ks), ks, len(want))
	}
	for i, k := range ks {
		got := k.name
		if k.name == "rune" {
			got += ":" + string(k.r)
		}
		if got != want[i] {
			t.Errorf("key %d = %s, want %s", i, got, want[i])
		}
	}
}

func TestStoreTrackFinishRemove(t *testing.T) {
	t.Setenv("SWOOP_HOME", t.TempDir())
	st := loadStore()
	e := st.track(&Entry{Kind: "file", URL: "https://x/a.bin", Name: "a.bin", Dir: "/tmp"})
	st.finish(e, "paused", 10)
	again := st.track(&Entry{Kind: "file", URL: "https://x/a.bin", Name: "a.bin", Dir: "/tmp"})
	if again != e || len(st.Entries) != 1 {
		t.Fatal("same link should reuse its entry")
	}
	st2 := loadStore() // read back from disk
	if len(st2.Entries) != 1 || st2.Entries[0].Status != "downloading" {
		t.Fatalf("store not saved: %+v", st2.Entries)
	}
	if st2.unfinished() != 1 {
		t.Fatal("expected 1 unfinished")
	}
	st.remove(e)
	if len(loadStore().Entries) != 0 {
		t.Fatal("entry not removed")
	}
}

// Delete-with-file must never touch anything outside the download folder.
func TestDeleteFilesStaysInsideFolder(t *testing.T) {
	root := t.TempDir()
	dl := filepath.Join(root, "Downloads")
	os.MkdirAll(dl, 0o755)
	outside := filepath.Join(root, "precious.txt")
	os.WriteFile(outside, []byte("keep me"), 0o644)
	inside := filepath.Join(dl, "movie.bin")
	os.WriteFile(inside, []byte("x"), 0o644)
	os.WriteFile(inside+".part", []byte("x"), 0o644)

	e := &Entry{Dir: dl, Path: inside, Extra: []string{outside, dl, filepath.Join(dl, "..", "precious.txt")}}
	e.deleteFiles()
	if exists(inside) || exists(inside+".part") {
		t.Fatal("download files should be gone")
	}
	if !exists(outside) || !exists(dl) {
		t.Fatal("deleted something outside the download")
	}
}

func TestTorrentInput(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "my file.torrent")
	os.WriteFile(tf, []byte("d4:infod4:name1:aee"), 0o644)
	cases := map[string]bool{
		"magnet:?xt=urn:btih:abc&dn=Ubuntu":     true,
		"https://example.com/linux.iso.torrent": true,
		strings.ReplaceAll(tf, " ", `\ `):       true, // what a terminal pastes for a dragged file
		`"` + tf + `"`:                          true,
		"https://example.com/file.zip":          false,
		"/does/not/exist.torrent":               false,
	}
	for in, want := range cases {
		if _, got := torrentInput(in); got != want {
			t.Errorf("torrentInput(%q) = %v, want %v", in, got, want)
		}
	}
	if magnetName("magnet:?xt=urn:btih:abc&dn=Ubuntu+24.04") != "Ubuntu 24.04" {
		t.Error("magnet name not read")
	}
}

// fakeAria2 imitates aria2's control interface: a magnet first fetches
// metadata, then hands over to the real download, which completes.
type fakeAria2 struct {
	mu    sync.Mutex
	polls int
}

func (f *fakeAria2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if len(req.Params) == 0 || req.Params[0] != "token:s3cret" {
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 1, "message": "Unauthorized"}})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any
	switch req.Method {
	case "aria2.getVersion":
		result = map[string]any{"version": "1.37.0"}
	case "aria2.addUri":
		result = "meta1"
	case "aria2.tellStatus":
		gid := req.Params[1].(string)
		if gid == "meta1" {
			result = map[string]any{"status": "complete", "totalLength": "0", "completedLength": "0", "followedBy": []string{"real2"}}
			break
		}
		f.polls++
		done := f.polls * 400
		st := "active"
		if done >= 1000 {
			done, st = 1000, "complete"
		}
		result = map[string]any{"status": st, "totalLength": "1000", "completedLength": itoa(int64(done)),
			"connections": "5", "numSeeders": "3", "infoHash": "abc", "dir": "/dl",
			"bittorrent": map[string]any{"info": map[string]any{"name": "ubuntu.iso"}}}
	default:
		result = "OK"
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "swoop", "result": result})
}

func TestTorrentFollowsMetadataToCompletion(t *testing.T) {
	srv := httptest.NewServer(&fakeAria2{})
	defer srv.Close()
	a := newAria2Client(srv.URL, "s3cret")
	raw, err := a.call("aria2.addUri", []string{"magnet:?xt=urn:btih:abc"})
	if err != nil {
		t.Fatal(err)
	}
	var gid string
	json.Unmarshal(raw, &gid)
	tr := &torrentRun{a: a, gid: gid}
	done := make(chan error, 1)
	go tr.poll(done)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("torrent failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("torrent never completed")
	}
	if tr.name() != "ubuntu.iso" {
		t.Fatalf("name = %q", tr.name())
	}
	if sn := tr.snapshot(); sn.Done != 1000 || sn.Size != 1000 || sn.Active != 5 {
		t.Fatalf("snapshot = %+v", sn)
	}
	if _, err := newAria2Client(srv.URL, "wrong").call("aria2.getVersion"); err == nil {
		t.Fatal("wrong secret must be rejected")
	}
}

func TestQualitiesMenu(t *testing.T) {
	info := pageInfo{Formats: []pageFormat{
		{ID: "a-webm", Ext: "webm", VCodec: "none", ACodec: "opus", TBR: 160, Size: 4 << 20, Protocol: "https"},
		{ID: "a-m4a", Ext: "m4a", VCodec: "none", ACodec: "mp4a", TBR: 128, Size: 3 << 20, Protocol: "https"},
		// ultra-wide video: the site calls 1920x886 "1080p"
		{ID: "v1080-webm", Ext: "webm", VCodec: "vp9", ACodec: "none", Width: 1920, Height: 886, Note: "1080p", TBR: 2500, Size: 60 << 20, Protocol: "https"},
		{ID: "v1080-mp4", Ext: "mp4", VCodec: "avc1", ACodec: "none", Width: 1920, Height: 886, Note: "1080p60", TBR: 2000, Size: 50 << 20, Protocol: "https"},
		{ID: "v2160", Ext: "webm", VCodec: "vp9", ACodec: "none", Width: 3840, Height: 1772, TBR: 9000, Size: 300 << 20, Protocol: "https"},
		// vertical video without a label: long side 1920 means 1080p
		{ID: "vert", Ext: "mp4", VCodec: "avc1", ACodec: "none", Width: 1080, Height: 1920, TBR: 1500, Size: 40 << 20, Protocol: "m3u8_native"},
		{ID: "m360", Ext: "mp4", VCodec: "avc1", ACodec: "mp4a", Width: 640, Height: 360, Size: 9 << 20, Protocol: "https"},
	}}
	qs := qualities(info)
	var got []string
	for _, q := range qs {
		got = append(got, q.choice+"="+q.selector())
	}
	want := "p:2160=v2160+a-webm,p:1080=v1080-mp4+a-m4a,p:360=m360,audio=a-m4a"
	if strings.Join(got, ",") != want {
		t.Fatalf("menu =\n %s\nwant\n %s", strings.Join(got, ","), want)
	}
	if !strings.Contains(qs[0].label, "2160p") || !strings.Contains(qs[0].label, "4K") {
		t.Fatalf("ultra-wide 4K label = %q", qs[0].label)
	}
	if !strings.Contains(qs[1].label, "53.00 MB") {
		t.Fatalf("1080p label = %q (should include the sound size)", qs[1].label)
	}
	if len(qs[1].streams(info)) != 2 || len(qs[2].streams(info)) != 1 {
		t.Fatal("wrong number of files for a quality")
	}
	if qs := qualities(pageInfo{}); len(qs) != 1 || qs[0].choice != "best" {
		t.Fatal("empty page should offer 'best'")
	}
}

// Some servers refuse big requests: with ChunkSize set, no request may ask
// for more than that, and the file must still come out identical.
func TestChunkedRequests(t *testing.T) {
	data := testData(t, 5<<20+333)
	var mu sync.Mutex
	maxReq := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err == nil {
			mu.Lock()
			if b-a+1 > maxReq {
				maxReq = b - a + 1
			}
			mu.Unlock()
		}
		http.ServeContent(w, r, "c.bin", time.Time{}, strings.NewReader(string(data)))
	}))
	defer srv.Close()
	dir := t.TempDir()
	download(t, srv.URL+"/c.bin", Options{Conns: 3, Dir: dir, ChunkSize: 512 << 10})
	got, _ := os.ReadFile(filepath.Join(dir, "c.bin"))
	if string(got) != string(data) {
		t.Fatal("file differs")
	}
	if maxReq > 512<<10 {
		t.Fatalf("a request asked for %d bytes, limit is %d", maxReq, 512<<10)
	}
}

func TestFriendlyErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&statusErr{404, "404 Not Found"}, "isn't there"},
		{&statusErr{403, "403 Forbidden"}, "refused access"},
		{&net.DNSError{Err: "no such host", Name: "x.invalid"}, "can't find that website"},
		{io.EOF, "closed the connection"},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, "couldn't connect"},
	}
	for _, tc := range cases {
		if got := friendlyErr(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("friendlyErr(%v) = %q, want it to mention %q", tc.err, got, tc.want)
		}
	}
}

// Media streams get new URLs every visit, so resume must follow the
// ResumeKey, not the URL.
func TestResumeKeySurvivesURLChange(t *testing.T) {
	data := testData(t, 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "v", time.Time{}, strings.NewReader(string(data)))
	}))
	defer srv.Close()
	dir := t.TempDir()
	half := int64(len(data) / 2)
	part := filepath.Join(dir, "part0.mp4.part")
	buf := make([]byte, len(data))
	copy(buf, data[:half])
	os.WriteFile(part, buf, 0o644)
	st := `{"url":"page|r:720|137","size":` + itoa(int64(len(data))) + `,"etag":"","segs":[[` + itoa(half) + `,` + itoa(int64(len(data))) + `]]}`
	os.WriteFile(part+".json", []byte(st), 0o644)

	d := download(t, srv.URL+"/new-signed-url?sig=xyz", Options{Conns: 4, Dir: dir, Name: "part0.mp4", ResumeKey: "page|r:720|137"})
	if d.Resumed != half {
		t.Fatalf("resumed at %d, want %d", d.Resumed, half)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "part0.mp4"))
	if string(got) != string(data) {
		t.Fatal("file differs after resume")
	}
}
