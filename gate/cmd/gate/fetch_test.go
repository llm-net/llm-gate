package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// artifactServer 是一台会按 Range 作答、并且能在指定字节处掐断连接的假上游，
// 用来复现盒子中继被分钟级正文上限掐断的现场。
type artifactServer struct {
	body []byte
	// cutFirst 大于 0 时，第一次应答只吐这么多字节就掐断连接。
	cutFirst int
	// ignoreRange 让它对带 Range 的请求照样回 200 全量（模拟不认断点的上游）。
	ignoreRange bool
	// rangeStatus 非 0 时覆盖带 Range 请求的应答码，用来造 416。
	rangeStatus int

	mu       sync.Mutex
	requests []string // 每次请求的 Range 头；空串表示没带
}

func (s *artifactServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *artifactServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		s.mu.Lock()
		s.requests = append(s.requests, rng)
		nth := len(s.requests)
		s.mu.Unlock()

		if rng != "" && !s.ignoreRange {
			if s.rangeStatus != 0 {
				w.WriteHeader(s.rangeStatus)
				return
			}
			var start int
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil || start >= len(s.body) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.body)-1, len(s.body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(s.body)-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(s.body[start:])
			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
		w.WriteHeader(http.StatusOK)
		if s.cutFirst > 0 && nth == 1 {
			_, _ = w.Write(s.body[:s.cutFirst])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // net/http 静默关连接，客户端看到短正文
		}
		_, _ = w.Write(s.body)
	}
}

func artifactBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// newFetchApp 造一个只用于取回的 app：out 收进缓冲，重试不等待。
func newFetchApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	prevBackoff := artifactFetchBackoff
	artifactFetchBackoff = 0
	t.Cleanup(func() { artifactFetchBackoff = prevBackoff })
	out := &bytes.Buffer{}
	return &app{root: t.TempDir(), out: out, err: &bytes.Buffer{}}, out
}

func grokFetch(client *http.Client, url, dir string, body []byte, withMeta bool) artifactFetch {
	f := artifactFetch{
		client: client, url: url, label: "Grok",
		dir: dir, prefix: ".grok-download",
		max: 1 << 20, mode: 0o700,
	}
	if withMeta {
		f.size = int64(len(body))
		f.digest = hexDigest(body)
	}
	return f
}

// 链路被掐断后必须从断点接着传：第二次请求带 Range，且只补差额而不是重下全量。
func TestFetchArtifactResumesAfterLinkCut(t *testing.T) {
	body := artifactBody(73728)
	srv := &artifactServer{body: body, cutFirst: 20000}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, out := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	got, err := a.fetchArtifact(grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"", "bytes=20000-"}; !slices.Equal(srv.seen(), want) {
		t.Fatalf("requests = %q, want %q", srv.seen(), want)
	}
	installed, err := os.ReadFile(got)
	if err != nil || !bytes.Equal(installed, body) {
		t.Fatalf("resumed artifact mismatch: err=%v len=%d want %d", err, len(installed), len(body))
	}
	if text := out.String(); !strings.Contains(text, "续传重试") {
		t.Fatalf("user was not told the link broke: %q", text)
	}
}

// 上一次进程留下的半截文件必须被接着用，而不是重新从 0 下。
func TestFetchArtifactResumesAcrossInvocations(t *testing.T) {
	body := artifactBody(50000)
	srv := &artifactServer{body: body}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:12345], 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := a.fetchArtifact(f)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"bytes=12345-"}; !slices.Equal(srv.seen(), want) {
		t.Fatalf("requests = %q, want %q", srv.seen(), want)
	}
	installed, err := os.ReadFile(got)
	if err != nil || !bytes.Equal(installed, body) {
		t.Fatalf("resumed artifact mismatch: err=%v len=%d", err, len(installed))
	}
}

// 上游不认 Range（照回 200 全量）时必须丢掉断点从头写，不能把全量接到半截后面。
func TestFetchArtifactRestartsWhenUpstreamIgnoresRange(t *testing.T) {
	body := artifactBody(40000)
	srv := &artifactServer{body: body, ignoreRange: true}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:9999], 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := a.fetchArtifact(f)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	installed, err := os.ReadFile(got)
	if err != nil || !bytes.Equal(installed, body) {
		t.Fatalf("restarted artifact mismatch: err=%v len=%d want %d", err, len(installed), len(body))
	}
}

// 盒子透传面对 416 只回状态码、不带 Content-Range，客户端只能清空重来。
func TestFetchArtifactRestartsOnRangeNotSatisfiable(t *testing.T) {
	body := artifactBody(30000)
	srv := &artifactServer{body: body, rangeStatus: http.StatusRequestedRangeNotSatisfiable}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:7777], 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := a.fetchArtifact(f)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if seen := srv.seen(); len(seen) != 2 || seen[0] != "bytes=7777-" || seen[1] != "" {
		t.Fatalf("requests = %q, want [bytes=7777- \"\"]", seen)
	}
	installed, err := os.ReadFile(got)
	if err != nil || !bytes.Equal(installed, body) {
		t.Fatalf("artifact after 416 restart mismatch: err=%v len=%d", err, len(installed))
	}
}

// 断点与上游报的 Content-Range 对不上就必须拒绝：接上去是错位的字节。
func TestFetchArtifactRefusesMisalignedContentRange(t *testing.T) {
	body := artifactBody(20000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 客户端要的是 5000-，这里谎报从 0 开始。
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:5000], 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := a.fetchArtifact(f); err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("misaligned resume must fail closed: %v", err)
	}
}

// 摘要不符是终端失败：报错之外还必须把半截文件删掉，不能毒到下一次。
func TestFetchArtifactDropsPartOnDigestMismatch(t *testing.T) {
	body := artifactBody(16384)
	srv := &artifactServer{body: body}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	f.digest = hexDigest([]byte("something else"))

	if _, err := a.fetchArtifact(f); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("digest mismatch must fail closed: %v", err)
	}
	if _, err := os.Stat(f.partPath()); !os.IsNotExist(err) {
		t.Fatalf("bad artifact left behind as a resume point: %v", err)
	}
}

// 链路一直不推进时要停下来，并且**保留**断点给下一次——这是与终端失败的分界。
func TestFetchArtifactKeepsPartWhenLinkKeepsFailing(t *testing.T) {
	body := artifactBody(40000)
	const seeded = 12000
	var hits int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", seeded, len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-seeded))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // 一个字节都不给，连着掐
	}))
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:seeded], 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := a.fetchArtifact(f); err == nil {
		t.Fatal("dead link must fail")
	}
	// 连着两轮零推进就收手，不能把 5 次配额烧完。
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("stalled link retried %d times, want 2", got)
	}
	st, err := os.Stat(f.partPath())
	if err != nil || st.Size() != seeded {
		t.Fatalf("link failure must keep the resume point: err=%v size=%v", err, st)
	}
}

// 超出单件上限是终端失败：读正文之前就闭合，且不留半截文件。
func TestFetchArtifactSizeCapFailsClosedWithoutPart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(int64(1<<20)+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, nil, false)

	if _, err := a.fetchArtifact(f); err == nil || !strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("oversized artifact must fail closed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("terminal failure left files behind: %v", entries)
	}
}

// 取回成功后，同一工具其它版本留下的续传件要清掉，只留本次这一个。
func TestFetchArtifactClearsOtherVersionParts(t *testing.T) {
	body := artifactBody(8192)
	srv := &artifactServer{body: body}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, _ := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".grok-download-deadbeefcafe.part")
	if err := os.WriteFile(stale, []byte("old version"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := a.fetchArtifact(grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale resume file from another version survived: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("current artifact went missing: %v", err)
	}
}

// 进度：非终端输出按行追加，收尾必须有一行说清收了多少、用了多久。
func TestFetchArtifactReportsProgress(t *testing.T) {
	prev := artifactProgressPlain
	artifactProgressPlain = 0 // 每次写都出一行
	t.Cleanup(func() { artifactProgressPlain = prev })

	body := artifactBody(200000)
	srv := &artifactServer{body: body}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, out := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	f.max = 1 << 21
	if _, err := a.fetchArtifact(f); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	text := out.String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected periodic progress lines, got %q", text)
	}
	for _, want := range []string{"Grok", "195 KiB", "%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("progress output missing %q: %q", want, text)
		}
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "取回完成") || !strings.Contains(last, "用时") {
		t.Fatalf("missing completion summary: %q", last)
	}
	// 非终端不能用 \r 原地刷新，否则日志会被压成一行。
	if strings.Contains(text, "\r") {
		t.Fatalf("plain output must not use carriage returns: %q", text)
	}
}

// 续传时的进度必须从断点起算，并在收尾点明这次只补了多少。
func TestProgressCountsResumedPrefixSeparately(t *testing.T) {
	body := artifactBody(60000)
	srv := &artifactServer{body: body}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	a, out := newFetchApp(t)
	dir := filepath.Join(a.root, "tools", "grok", "bin")
	f := grokFetch(ts.Client(), ts.URL+"/grok-1.0.5-linux-x86_64", dir, body, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.partPath(), body[:40000], 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.fetchArtifact(f); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if text := out.String(); !strings.Contains(text, "本次续传") {
		t.Fatalf("summary should separate resumed bytes from the prefix: %q", text)
	}
}

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		in    string
		start int64
		total int64
		ok    bool
	}{
		{"bytes 166000000-166854367/166854368", 166000000, 166854368, true},
		{"bytes 0-0/1", 0, 1, true},
		{"bytes 10-99/*", 10, 0, true},
		{"", 0, 0, false},
		{"items 0-1/2", 0, 0, false},
		{"bytes 0-1", 0, 0, false},
		{"bytes x-1/2", 0, 0, false},
		{"bytes 0-1/x", 0, 0, false},
		{"bytes -1-1/2", 0, 0, false},
	} {
		start, total, ok := parseContentRange(tc.in)
		if start != tc.start || total != tc.total || ok != tc.ok {
			t.Errorf("parseContentRange(%q) = %d,%d,%v want %d,%d,%v",
				tc.in, start, total, ok, tc.start, tc.total, tc.ok)
		}
	}
}

func TestHumanBytesAndDuration(t *testing.T) {
	for in, want := range map[int64]string{
		0:         "0 B",
		999:       "999 B",
		1024:      "1 KiB",
		166854368: "159.1 MiB",
		1 << 30:   "1.00 GiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		0:                       "0s",
		41 * time.Second:        "41s",
		414 * time.Second:       "6m54s",
		-3 * time.Second:        "0s",
		3605 * time.Second:      "60m05s",
		1500 * time.Millisecond: "1s",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q want %q", in, got, want)
		}
	}
}
