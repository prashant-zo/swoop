package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testData(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func download(t *testing.T, url string, opts Options) *Download {
	t.Helper()
	ctx := context.Background()
	d, err := Probe(ctx, url, opts)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := d.Prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := d.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	return d
}

// Real HTTP server with Range support: the file must come back byte for byte.
func TestDownloadParallelEndToEnd(t *testing.T) {
	data := testData(t, 9<<20+12345) // odd size to catch off-by-one at the end
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "test.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := download(t, srv.URL+"/test.bin", Options{Conns: 8, Dir: dir})
	if !d.Ranges || d.Workers() < 2 {
		t.Fatalf("expected a parallel download, ranges=%v", d.Ranges)
	}
	got, err := os.ReadFile(filepath.Join(dir, "test.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded file differs from the source")
	}
	if exists(filepath.Join(dir, "test.bin.part")) || exists(filepath.Join(dir, "test.bin.part.json")) {
		t.Fatal("leftover .part files")
	}
	// same link again must not overwrite the first file
	download(t, srv.URL+"/test.bin", Options{Conns: 8, Dir: dir})
	if !exists(filepath.Join(dir, "test (1).bin")) {
		t.Fatal("expected test (1).bin for the second download")
	}
}

// Server that ignores Range: swoop must fall back to one stream.
func TestDownloadSingleStreamFallback(t *testing.T) {
	data := testData(t, 3<<20+7)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := download(t, srv.URL+"/plain.bin", Options{Conns: 8, Dir: dir})
	if d.Ranges {
		t.Fatal("server has no range support, expected single stream")
	}
	got, err := os.ReadFile(filepath.Join(dir, "plain.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded file differs from the source")
	}
}

// Resume: a saved state file means only the missing ranges are fetched.
func TestResumeFetchesOnlyMissingBytes(t *testing.T) {
	data := testData(t, 4<<20)
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		cw := &countingWriter{ResponseWriter: w, n: &served}
		http.ServeContent(cw, r, "r.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	dir := t.TempDir()
	part := filepath.Join(dir, "r.bin.part")
	half := int64(len(data) / 2)
	// pretend an earlier run finished the first half, then was stopped
	buf := make([]byte, len(data))
	copy(buf, data[:half])
	if err := os.WriteFile(part, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	st := `{"url":"` + srv.URL + `/r.bin","size":` + itoa(int64(len(data))) + `,"etag":"\"v1\"","segs":[[` + itoa(half) + `,` + itoa(int64(len(data))) + `]]}`
	if err := os.WriteFile(part+".json", []byte(st), 0o644); err != nil {
		t.Fatal(err)
	}
	d := download(t, srv.URL+"/r.bin", Options{Conns: 4, Dir: dir})
	if d.Resumed != half {
		t.Fatalf("resumed at %d, want %d", d.Resumed, half)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "r.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs from the source")
	}
	if served.Load() > half+64<<10 { // probe byte + the missing half, small slack
		t.Fatalf("server sent %d bytes, expected about %d", served.Load(), half)
	}
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.ResponseWriter.Write(p)
}

func itoa(n int64) string { return fmtInt(n) }

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestPanelLinesHaveEqualWidth(t *testing.T) {
	j := &job{title: func() string { return "ubuntu-24.04-desktop-amd64.iso" }, started: time.Now(), segmap: true}
	sn := Snap{Size: 100 << 20, Done: 40 << 20, Written: 40 << 20, Active: 3, Segs: []segView{
		{start: 0, pos: 30 << 20, end: 50 << 20, color: 0, active: true},
		{start: 50 << 20, pos: 60 << 20, end: 100 << 20, color: 1, active: true},
	}}
	r := &renderer{speed: 5 << 20, samples: 3}
	lines := r.panel(j, sn)
	w := vlen(lines[0])
	for i, l := range lines {
		if vlen(l) != w {
			t.Fatalf("line %d is %d wide, want %d: %q", i, vlen(l), w, stripANSI(l))
		}
	}
}

func TestParseContentRange(t *testing.T) {
	if got := parseTotal("bytes 0-0/12345"); got != 12345 {
		t.Fatalf("total = %d", got)
	}
	if got := parseTotal("bytes 0-0/*"); got != -1 {
		t.Fatalf("unknown total = %d", got)
	}
	if got := parseStart("bytes 100-199/12345"); got != 100 {
		t.Fatalf("start = %d", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"a\\b\\c.zip":      "c.zip",
		"..":               "",
		"ok file.iso":      "ok file.iso",
		"bad\x00name.txt":  "badname.txt",
		`what?"<x>|*.zip`:  "whatx.zip",
		"trailing. . ":     "trailing",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithIndex(t *testing.T) {
	if got := withIndex("file.bin", 2); got != "file (2).bin" {
		t.Fatalf("got %q", got)
	}
}
