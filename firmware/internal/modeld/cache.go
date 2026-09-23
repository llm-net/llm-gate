package modeld

// 模型文件缓存：<state_dir>/models 下的一层文件（名字只收 [A-Za-z0-9._-]，不建子目录）。从 URL 拉取
// 是后台任务（pull）：先下到 .part 临时文件、可选核对 SHA-256、再 rename 到位；进度在内存里。
// 算力服务器经对外 API 的 GET /api/v1/files/{name} 取文件（支持 Range）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var cacheNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

const (
	pullKeep    = 50
	pullTimeout = 12 * time.Hour
)

// CacheFile 是缓存里的一个文件。
type CacheFile struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	SHA256     string `json:"sha256,omitempty"`
}

// CacheStats 是缓存一屏读数。
type CacheStats struct {
	Dir   string `json:"dir"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// FreeBytes 是所在文件系统的剩余空间（取不到为 0）。
	FreeBytes int64 `json:"free_bytes"`
	Pulling   int   `json:"pulling"`
}

// Pull 是一次拉取记录。
type Pull struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256,omitempty"`
	Status     string `json:"status"` // running | succeeded | failed | cancelled
	Received   int64  `json:"received"`
	Total      int64  `json:"total"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`

	cancel context.CancelFunc
}

type cacheStore struct {
	s   *Server
	dir string

	mu     sync.Mutex
	pulls  []*Pull
	sums   map[string]string // name → sha256（拉取时算出来的，重启即忘）
	active map[string]bool   // 正在拉取的文件名
}

func newCacheStore(s *Server, dir string) *cacheStore {
	os.MkdirAll(dir, 0o700)
	return &cacheStore{s: s, dir: dir, sums: map[string]string{}, active: map[string]bool{}}
}

func validCacheName(name string) bool {
	return cacheNameRE.MatchString(name) && !strings.HasSuffix(name, ".part")
}

func (c *cacheStore) list() ([]CacheFile, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CacheFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !validCacheName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, CacheFile{Name: e.Name(), Size: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339), SHA256: c.sums[e.Name()]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *cacheStore) stats() CacheStats {
	files, _ := c.list()
	st := CacheStats{Dir: c.dir, Files: len(files), FreeBytes: freeBytes(c.dir)}
	for _, f := range files {
		st.Bytes += f.Size
	}
	c.mu.Lock()
	for _, p := range c.pulls {
		if p.Status == "running" {
			st.Pulling++
		}
	}
	c.mu.Unlock()
	return st
}

func (c *cacheStore) pullViews() []Pull {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Pull, 0, len(c.pulls))
	for i := len(c.pulls) - 1; i >= 0; i-- {
		p := *c.pulls[i]
		p.cancel = nil
		out = append(out, p)
	}
	return out
}

// pullInput 是拉取的入参。
type pullInput struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func (c *cacheStore) startPull(in pullInput) (*Pull, error) {
	in.URL = strings.TrimSpace(in.URL)
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, bad("拉取地址须是 http:// 或 https:// 开头")
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = filepath.Base(u.Path)
	}
	if !validCacheName(in.Name) {
		return nil, bad("文件名只能含字母、数字、点、下划线与连字符，且不能以点开头")
	}
	in.SHA256 = strings.ToLower(strings.TrimSpace(in.SHA256))
	if in.SHA256 != "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(in.SHA256) {
		return nil, bad("sha256 须是 64 位十六进制")
	}
	c.mu.Lock()
	if c.active[in.Name] {
		c.mu.Unlock()
		return nil, conflict("这个文件正在拉取中")
	}
	if _, err := os.Stat(filepath.Join(c.dir, in.Name)); err == nil {
		c.mu.Unlock()
		return nil, conflict("缓存里已有同名文件，先删除再拉取")
	}
	ctx, cancel := context.WithTimeout(c.s.bg, pullTimeout)
	p := &Pull{ID: newID("pull_"), Name: in.Name, URL: in.URL, SHA256: in.SHA256, Status: "running", StartedAt: c.s.now().UTC().Format(time.RFC3339), cancel: cancel}
	c.active[in.Name] = true
	c.pulls = append(c.pulls, p)
	c.trimLocked()
	c.mu.Unlock()
	c.s.log.Info("开始拉取模型文件", "pull", p.ID, "name", p.Name)
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		c.run(ctx, p)
	}()
	return p, nil
}

func (c *cacheStore) trimLocked() {
	finished := 0
	for _, p := range c.pulls {
		if p.Status != "running" {
			finished++
		}
	}
	for finished > pullKeep {
		for i, p := range c.pulls {
			if p.Status != "running" {
				c.pulls = append(c.pulls[:i], c.pulls[i+1:]...)
				finished--
				break
			}
		}
	}
}

func (c *cacheStore) run(ctx context.Context, p *Pull) {
	err := c.download(ctx, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, p.Name)
	p.FinishedAt = c.s.now().UTC().Format(time.RFC3339)
	p.cancel = nil
	switch {
	case err == nil:
		p.Status = "succeeded"
	case errors.Is(err, context.Canceled):
		p.Status, p.Error = "cancelled", ""
	default:
		p.Status, p.Error = "failed", err.Error()
	}
	c.s.log.Info("拉取模型文件结束", "pull", p.ID, "name", p.Name, "status", p.Status)
}

func (c *cacheStore) download(ctx context.Context, p *Pull) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: c.s.http.Transport, Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errors.New("下载应答 " + resp.Status)
	}
	part := filepath.Join(c.dir, p.Name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(part)
	h := sha256.New()
	c.mu.Lock()
	p.Total = resp.ContentLength
	c.mu.Unlock()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			h.Write(buf[:n])
			c.mu.Lock()
			p.Received += int64(n)
			c.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if p.SHA256 != "" && sum != p.SHA256 {
		return errors.New("SHA-256 不符：下载得到 " + sum[:16] + "…")
	}
	if err := os.Rename(part, filepath.Join(c.dir, p.Name)); err != nil {
		return err
	}
	c.mu.Lock()
	c.sums[p.Name] = sum
	c.mu.Unlock()
	return nil
}

func (c *cacheStore) cancelPull(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.pulls {
		if p.ID != id {
			continue
		}
		if p.Status == "running" {
			if p.cancel != nil {
				p.cancel()
			}
			return nil
		}
		c.pulls = append(c.pulls[:i], c.pulls[i+1:]...)
		return nil
	}
	return notFound("拉取记录不存在")
}

func (c *cacheStore) remove(name string) error {
	if !validCacheName(name) {
		return &Error{Code: CodeInvalidPath, Msg: "文件名形态异常"}
	}
	c.mu.Lock()
	if c.active[name] {
		c.mu.Unlock()
		return conflict("这个文件正在拉取中，先取消拉取")
	}
	delete(c.sums, name)
	c.mu.Unlock()
	if err := os.Remove(filepath.Join(c.dir, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return notFound("文件不存在")
		}
		return err
	}
	return nil
}

// serve 按原字节给出一个缓存文件（支持 Range）。
func (c *cacheStore) serve(w http.ResponseWriter, r *http.Request, name string) {
	if !validCacheName(name) {
		writeError(w, http.StatusBadRequest, CodeInvalidPath, "文件名形态异常")
		return
	}
	f, err := os.Open(filepath.Join(c.dir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "文件不存在")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, CodeNotFound, "文件不存在")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// ---- 管理面端点 ----

func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	files, err := s.cache.list()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "stats": s.cache.stats(), "pulls": s.cache.pullViews()})
}

func (s *Server) handleCachePull(w http.ResponseWriter, r *http.Request) {
	var in pullInput
	if err := decodeJSON(r, &in, 64<<10); err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.cache.startPull(in)
	if err != nil {
		writeErr(w, err)
		return
	}
	v := *p
	v.cancel = nil
	writeJSON(w, http.StatusAccepted, map[string]any{"pull": v})
}

func (s *Server) handleCachePulls(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pulls": s.cache.pullViews()})
}

func (s *Server) handleCachePullCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.cache.cancelPull(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.cache.remove(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCacheFile(w http.ResponseWriter, r *http.Request) {
	s.cache.serve(w, r, r.PathValue("name"))
}
