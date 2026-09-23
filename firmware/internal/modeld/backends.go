package modeld

// 算力服务器（backend）登记与探活。一台算力服务器是一个 HTTP 服务，约定的接口（契约见
// docs-dev/firmware-model-service.md）：
//
//	GET    {base_url}/healthz                 2xx 即在线；可选 JSON {"models":[…],"slots":N} 作为自述
//	POST   {base_url}/v1/tasks                {"id","model","input"} → {"id"?,"status","output"?,"error"?}
//	GET    {base_url}/v1/tasks/{id}           同上形状，status ∈ queued|running|succeeded|failed|cancelled
//	DELETE {base_url}/v1/tasks/{id}           取消
//
// 登记的 models 是这台机器承载的模型名（空 = 任何模型都接），slots 是同时能跑的任务数。
// 运行期读数（在线 / 忙闲 / 计数）只在内存里，重启归零。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Backend 是一台算力服务器的登记项（持久化）。
type Backend struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	Slots   int      `json:"slots"`
	Enabled bool     `json:"enabled"`
	Note    string   `json:"note,omitempty"`

	// 运行期读数（不持久化）。
	rt backendRuntime
}

type backendRuntime struct {
	// status: "" 未探过 / ready / error。
	status    string
	lastError string
	checkedAt time.Time
	latencyMs int64
	reported  backendReport
	active    int
	completed int64
	failed    int64
}

// backendReport 是 healthz 里可选的自述。
type backendReport struct {
	Models []string `json:"models"`
	Slots  int      `json:"slots"`
}

// BackendView 是给界面的读数。
type BackendView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	Models    []string `json:"models"`
	Slots     int      `json:"slots"`
	Enabled   bool     `json:"enabled"`
	Note      string   `json:"note,omitempty"`
	Status    string   `json:"status"`
	LastError string   `json:"last_error,omitempty"`
	CheckedAt string   `json:"checked_at,omitempty"`
	LatencyMs int64    `json:"latency_ms,omitempty"`
	Active    int      `json:"active"`
	Completed int64    `json:"completed"`
	Failed    int64    `json:"failed"`
	// Reported 是 healthz 自述（有就带）。
	ReportedModels []string `json:"reported_models,omitempty"`
	ReportedSlots  int      `json:"reported_slots,omitempty"`
}

func (b *Backend) view() BackendView {
	v := BackendView{
		ID: b.ID, Name: b.Name, BaseURL: b.BaseURL, Models: append([]string{}, b.Models...), Slots: b.Slots, Enabled: b.Enabled, Note: b.Note,
		Status: b.rt.status, LastError: b.rt.lastError, LatencyMs: b.rt.latencyMs, Active: b.rt.active, Completed: b.rt.completed, Failed: b.rt.failed,
		ReportedModels: b.rt.reported.Models, ReportedSlots: b.rt.reported.Slots,
	}
	if v.Status == "" {
		v.Status = "unknown"
	}
	if !b.rt.checkedAt.IsZero() {
		v.CheckedAt = b.rt.checkedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// supports 报告这台机器接不接这个模型。
func (b *Backend) supports(model string) bool {
	if len(b.Models) == 0 {
		return true
	}
	for _, m := range b.Models {
		if m == model {
			return true
		}
	}
	return false
}

// backendInput 是登记 / 修改的入参。
type backendInput struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	Slots   int      `json:"slots"`
	Enabled *bool    `json:"enabled"`
	Note    string   `json:"note"`
}

func (in *backendInput) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return bad("算力服务器地址须是 http:// 或 https:// 开头的站点地址")
	}
	if in.Name == "" {
		in.Name = u.Host
	}
	if len([]rune(in.Name)) > 64 {
		return bad("名称不能超过 64 个字符")
	}
	if in.Slots <= 0 {
		in.Slots = 1
	}
	if in.Slots > 256 {
		return bad("并发槽位不能超过 256")
	}
	models := make([]string, 0, len(in.Models))
	seen := map[string]bool{}
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if len(m) > 128 || strings.ContainsAny(m, "\n\r\t") {
			return bad("模型名形态异常：" + firstLine(m))
		}
		seen[m] = true
		models = append(models, m)
	}
	in.Models = models
	if len([]rune(in.Note)) > 500 {
		return bad("备注不能超过 500 个字符")
	}
	return nil
}

func (s *Server) backendViews() []BackendView {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	out := make([]BackendView, 0, len(s.st.backends))
	for _, b := range s.st.backends {
		out = append(out, b.view())
	}
	return out
}

func (s *Server) handleBackendsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"backends": s.backendViews()})
}

func (s *Server) handleBackendCreate(w http.ResponseWriter, r *http.Request) {
	var in backendInput
	if err := decodeJSON(r, &in, 64<<10); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.normalize(); err != nil {
		writeErr(w, err)
		return
	}
	b := &Backend{ID: newID("bk_"), Name: in.Name, BaseURL: in.BaseURL, Models: in.Models, Slots: in.Slots, Enabled: true, Note: in.Note}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	s.st.mu.Lock()
	for _, o := range s.st.backends {
		if o.BaseURL == b.BaseURL {
			s.st.mu.Unlock()
			writeErr(w, conflict("这个地址已经登记过了："+o.Name))
			return
		}
	}
	s.st.backends = append(s.st.backends, b)
	err := s.st.saveLocked()
	s.st.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("登记算力服务器", "backend", b.ID, "name", b.Name)
	// 登记完立刻探一次，界面马上有读数。
	s.checkBackend(r.Context(), b)
	s.kick()
	writeJSON(w, http.StatusCreated, map[string]any{"backend": s.viewOf(b.ID)})
}

func (s *Server) viewOf(id string) BackendView {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	for _, b := range s.st.backends {
		if b.ID == id {
			return b.view()
		}
	}
	return BackendView{}
}

func (s *Server) findBackendLocked(id string) *Backend {
	for _, b := range s.st.backends {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (s *Server) handleBackendUpdate(w http.ResponseWriter, r *http.Request) {
	var in backendInput
	if err := decodeJSON(r, &in, 64<<10); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.normalize(); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	s.st.mu.Lock()
	b := s.findBackendLocked(id)
	if b == nil {
		s.st.mu.Unlock()
		writeErr(w, notFound("算力服务器不存在"))
		return
	}
	for _, o := range s.st.backends {
		if o.ID != id && o.BaseURL == in.BaseURL {
			s.st.mu.Unlock()
			writeErr(w, conflict("这个地址已经登记过了："+o.Name))
			return
		}
	}
	b.Name, b.BaseURL, b.Models, b.Slots, b.Note = in.Name, in.BaseURL, in.Models, in.Slots, in.Note
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	err := s.st.saveLocked()
	s.st.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	s.kick()
	writeJSON(w, http.StatusOK, map[string]any{"backend": s.viewOf(id)})
}

func (s *Server) handleBackendDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.st.mu.Lock()
	idx := -1
	for i, b := range s.st.backends {
		if b.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		s.st.mu.Unlock()
		writeErr(w, notFound("算力服务器不存在"))
		return
	}
	if s.st.backends[idx].rt.active > 0 {
		s.st.mu.Unlock()
		writeErr(w, conflict("这台算力服务器上还有任务在执行，先取消任务或等它结束"))
		return
	}
	s.st.backends = append(s.st.backends[:idx], s.st.backends[idx+1:]...)
	err := s.st.saveLocked()
	s.st.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("移除算力服务器", "backend", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleBackendCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.st.mu.Lock()
	b := s.findBackendLocked(id)
	s.st.mu.Unlock()
	if b == nil {
		writeErr(w, notFound("算力服务器不存在"))
		return
	}
	s.checkBackend(r.Context(), b)
	s.kick()
	writeJSON(w, http.StatusOK, map[string]any{"backend": s.viewOf(id)})
}

const (
	healthInterval = 30 * time.Second
	healthTimeout  = 10 * time.Second
)

// checkBackend 探一次活并把读数记进运行期字段。
func (s *Server) checkBackend(ctx context.Context, b *Backend) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/healthz", nil)
	if err != nil {
		s.noteBackend(b, err, 0, backendReport{})
		return
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.noteBackend(b, err, 0, backendReport{})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		s.noteBackend(b, errors.New("healthz 应答 "+resp.Status), time.Since(start).Milliseconds(), backendReport{})
		return
	}
	var rep backendReport
	_ = json.Unmarshal(raw, &rep)
	s.noteBackend(b, nil, time.Since(start).Milliseconds(), rep)
}

func (s *Server) noteBackend(b *Backend, err error, latency int64, rep backendReport) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	b.rt.checkedAt = s.now()
	b.rt.latencyMs = latency
	if err != nil {
		if b.rt.status != "error" {
			s.log.Warn("算力服务器不可达", "backend", b.ID, "name", b.Name, "error", err.Error())
		}
		b.rt.status, b.rt.lastError = "error", err.Error()
		return
	}
	b.rt.status, b.rt.lastError, b.rt.reported = "ready", "", rep
}

// healthLoop 定期探活全部算力服务器。
func (s *Server) healthLoop(ctx context.Context) {
	s.checkAll(ctx)
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkAll(ctx)
			s.kick()
		}
	}
}

func (s *Server) checkAll(ctx context.Context) {
	s.st.mu.Lock()
	list := append([]*Backend{}, s.st.backends...)
	s.st.mu.Unlock()
	for _, b := range list {
		if ctx.Err() != nil {
			return
		}
		s.checkBackend(ctx, b)
	}
}

// pickBackendLocked 为一个模型挑一台算力服务器：启用、在线（或还没探过）、接这个模型、有空槽，
// 负载比（active/slots）最低者优先，再按已完成数少者（把新机器也用起来）。调用方持锁。
func (s *Server) pickBackendLocked(model string) *Backend {
	var cands []*Backend
	for _, b := range s.st.backends {
		if !b.Enabled || b.rt.status == "error" || !b.supports(model) || b.rt.active >= b.Slots {
			continue
		}
		cands = append(cands, b)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		li := float64(cands[i].rt.active) / float64(cands[i].Slots)
		lj := float64(cands[j].rt.active) / float64(cands[j].Slots)
		if li != lj {
			return li < lj
		}
		return cands[i].rt.completed < cands[j].rt.completed
	})
	return cands[0]
}

// servedModels 是全部启用的算力服务器承载的模型并集（空 models 的机器不贡献名字）。
func (s *Server) servedModels() []string {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, b := range s.st.backends {
		if !b.Enabled {
			continue
		}
		for _, m := range b.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}
