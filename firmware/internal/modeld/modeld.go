// Package modeld 是「智能体 → 主机/SoC」里**模型服务节点**上运行的守护进程 llmgate-modeld 的
// 核心。一台模型服务节点做五件事：
//
//   - 管理多台**算力服务器**（backends.go）：登记每台的地址、承载的模型与并发槽位，定期探活；
//   - **任务队列与调度**（tasks.go）：任务按优先级与先后排队，按「支持该模型、有空槽、负载最低」
//     挑一台算力服务器派发（HTTP），陪等到终态，派发失败换一台重试；
//   - **对外 API**（api.go）：在一个 TCP 端口上以 Bearer 令牌对外提供任务提交 / 查询 / 取消、
//     模型清单与模型文件下载；令牌只存摘要（tokens.go）；
//   - **模型文件缓存**（cache.go）：节点上的一块目录，从 URL 拉取（可校验 SHA-256）、列出、删除，
//     经对外 API 给算力服务器取用；
//   - **引擎会话**（engines.go）：直接在本机拉起 Codex App Server（stdio JSON-RPC）或 Claude Code
//     （stream-json）——大模型 API 仍从设备（LLM Gate）走：会话创建时带来设备地址与 API 密钥，
//     密钥只落在 0600 的实例目录里。
//
// 信任模型与 devd 相同：管理面只在本机 Unix socket 上监听，设备凭访问证书经 SSH 转发通道连到
// 它，守护进程自己不做鉴权；对外 API 是另一个监听器（TCP），每条请求都要令牌。
//
// 本包只依赖标准库，守护进程二进制要嵌进固件、越小越好。
//
// 日志纪律（§15.1）：只记 id、路径与结果；任务入参 / 输出、提示词、模型回复、密钥与令牌不进日志。
package modeld

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 错误码。设备把它们原样透给界面。
const (
	CodeBadRequest     = "bad_request"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeIOError        = "io_error"
	CodeBackendFailed  = "backend_failed"
	CodeNoBackend      = "no_backend"
	CodeUnauthorized   = "unauthorized"
	CodeEngineMissing  = "engine_missing"
	CodeEngineFailed   = "engine_failed"
	CodeSessionBusy    = "session_busy"
	CodeInvalidPath    = "invalid_path"
	CodeListenFailed   = "listen_failed"
	CodeTaskNotRunning = "task_not_running"
)

// Error 是本包对外的错误形态。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func bad(msg string) error      { return &Error{Code: CodeBadRequest, Msg: msg} }
func notFound(msg string) error { return &Error{Code: CodeNotFound, Msg: msg} }
func conflict(msg string) error { return &Error{Code: CodeConflict, Msg: msg} }

// Options 是 New 的入参。
type Options struct {
	// StateDir 是持久化状态与模型缓存的目录（必填；systemd StateDirectory）。
	StateDir string
	// Home / User / Shell 交给引擎子进程的环境；空即继承。
	Home  string
	User  string
	Shell string
	// Version 是守护进程版本（GET /v1/info 回给设备）。
	Version string
	Logger  *slog.Logger
	// Listen 是对外 API 的缺省监听地址（状态里没有设置时用它；空 = 不开对外 API）。
	Listen string
	// HTTPClient 是派发任务、探活与拉取模型文件用的客户端（nil = 缺省）。
	HTTPClient *http.Client
	// Now 只给测试。
	Now func() time.Time
}

// Server 是守护进程本体。
type Server struct {
	stateDir string
	home     string
	user     string
	shell    string
	version  string
	log      *slog.Logger
	http     *http.Client
	now      func() time.Time

	st *state

	cache   *cacheStore
	engines *engineManager

	// api 是对外 API 的监听器（可能为 nil：没配地址或起不来）。
	apiMu   sync.Mutex
	apiLn   net.Listener
	apiSrv  *http.Server
	apiErr  string
	apiAddr string

	kickCh chan struct{}
	bg     context.Context
	bgStop context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// New 装配 Server 并从 StateDir 读回状态。
func New(o Options) (*Server, error) {
	if o.StateDir == "" {
		return nil, errors.New("须指定状态目录")
	}
	s := &Server{stateDir: o.StateDir, home: o.Home, user: o.User, shell: o.Shell, version: o.Version, log: o.Logger, http: o.HTTPClient, now: o.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.home == "" {
		s.home, _ = os.UserHomeDir()
	}
	if s.http == nil {
		s.http = &http.Client{Timeout: 60 * time.Second}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	st, err := loadState(o.StateDir, o.Listen)
	if err != nil {
		return nil, err
	}
	s.st = st
	s.cache = newCacheStore(s, filepath.Join(o.StateDir, "models"))
	s.engines = newEngineManager(s)
	s.kickCh = make(chan struct{}, 1)
	s.bg, s.bgStop = context.WithCancel(context.Background())
	return s, nil
}

// StateDir 是状态目录（测试用）。
func (s *Server) StateDir() string { return s.stateDir }

// Start 起后台工作：调度器、探活与对外 API。可重复调用（只生效一次）。
func (s *Server) Start() {
	s.once.Do(func() {
		s.recoverTasks()
		s.wg.Add(2)
		go func() { defer s.wg.Done(); s.schedulerLoop(s.bg) }()
		go func() { defer s.wg.Done(); s.healthLoop(s.bg) }()
		if addr := s.st.apiListen(); addr != "" {
			if err := s.bindAPI(addr); err != nil {
				s.log.Warn("对外 API 监听失败", "listen", addr, "error", err.Error())
			}
		}
	})
}

// Stop 停掉后台工作、引擎会话与对外 API。
func (s *Server) Stop() {
	s.bgStop()
	s.engines.closeAll()
	s.unbindAPI()
	s.wg.Wait()
}

// ---- HTTP（管理面 /v1/*）----

// Handler 是管理面路由。能连上 socket 的只有运行用户自己（设备经 SSH 以它登录），这里不鉴权。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.handleInfo)
	mux.HandleFunc("GET /v1/summary", s.handleSummary)
	mux.HandleFunc("GET /v1/api", s.handleAPIGet)
	mux.HandleFunc("PUT /v1/api", s.handleAPIPut)
	mux.HandleFunc("GET /v1/tokens", s.handleTokensList)
	mux.HandleFunc("POST /v1/tokens", s.handleTokenCreate)
	mux.HandleFunc("DELETE /v1/tokens/{id}", s.handleTokenDelete)
	mux.HandleFunc("GET /v1/backends", s.handleBackendsList)
	mux.HandleFunc("POST /v1/backends", s.handleBackendCreate)
	mux.HandleFunc("PUT /v1/backends/{id}", s.handleBackendUpdate)
	mux.HandleFunc("DELETE /v1/backends/{id}", s.handleBackendDelete)
	mux.HandleFunc("POST /v1/backends/{id}/check", s.handleBackendCheck)
	mux.HandleFunc("GET /v1/tasks", s.handleTasksList)
	mux.HandleFunc("POST /v1/tasks", s.handleTaskCreate)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleTaskGet)
	mux.HandleFunc("DELETE /v1/tasks/{id}", s.handleTaskCancel)
	mux.HandleFunc("POST /v1/tasks/{id}/retry", s.handleTaskRetry)
	mux.HandleFunc("DELETE /v1/tasks", s.handleTasksClear)
	mux.HandleFunc("GET /v1/cache", s.handleCacheList)
	mux.HandleFunc("POST /v1/cache/pull", s.handleCachePull)
	mux.HandleFunc("GET /v1/cache/pulls", s.handleCachePulls)
	mux.HandleFunc("DELETE /v1/cache/pulls/{id}", s.handleCachePullCancel)
	mux.HandleFunc("DELETE /v1/cache/{name}", s.handleCacheDelete)
	mux.HandleFunc("GET /v1/cache/files/{name}", s.handleCacheFile)
	mux.HandleFunc("GET /v1/engines", s.handleEnginesInfo)
	mux.HandleFunc("GET /v1/engines/sessions", s.handleSessionsList)
	mux.HandleFunc("POST /v1/engines/sessions", s.handleSessionCreate)
	mux.HandleFunc("GET /v1/engines/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("DELETE /v1/engines/sessions/{id}", s.handleSessionClose)
	mux.HandleFunc("POST /v1/engines/sessions/{id}/turns", s.handleSessionTurn)
	mux.HandleFunc("POST /v1/engines/sessions/{id}/interrupt", s.handleSessionInterrupt)
	mux.HandleFunc("GET /v1/engines/sessions/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "路径不存在")
	})
	return s.withLog(mux, "admin")
}

func (s *Server) withLog(next http.Handler, plane string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.log.Info("access", "plane", plane, "method", r.Method, "path", r.URL.Path, "status", rec.code(), "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Flush 让流式响应透过记录器。
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Info 是 GET /v1/info 的响应。字段形状与 devd.Info 兼容（设备用同一段代码写回主机行）。
type Info struct {
	Version   string `json:"version"`
	Daemon    string `json:"daemon"`
	Hostname  string `json:"hostname"`
	System    string `json:"system"`
	Arch      string `json:"arch"`
	User      string `json:"user"`
	Home      string `json:"home"`
	Shell     string `json:"shell"`
	Tmux      bool   `json:"tmux"`
	Git       bool   `json:"git"`
	StateDir  string `json:"state_dir"`
	StartedAt string `json:"started_at"`
	// API 是对外 API 的现状。
	API apiStatus `json:"api"`
}

var startedAt = time.Now().UTC()

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.info())
}

func (s *Server) info() Info {
	host, _ := os.Hostname()
	return Info{
		Version: s.version, Daemon: "modeld", Hostname: host, System: systemDescription(), Arch: runtime.GOARCH,
		User: s.user, Home: s.home, Shell: s.shell, StateDir: s.stateDir,
		StartedAt: startedAt.Format(time.RFC3339), API: s.apiStatus(),
	}
}

// Summary 是 GET /v1/summary 的响应：一屏读数。
type Summary struct {
	Info     Info          `json:"info"`
	Backends []BackendView `json:"backends"`
	Queue    QueueStats    `json:"queue"`
	Cache    CacheStats    `json:"cache"`
	Engines  EnginesInfo   `json:"engines"`
	Tokens   int           `json:"tokens"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Summary{
		Info: s.info(), Backends: s.backendViews(), Queue: s.queueStats(), Cache: s.cache.stats(),
		Engines: s.engines.info(), Tokens: s.st.tokenCount(),
	})
}

// ---- 监听 ----

// ListenAndServe 在 Unix socket 上服务管理面，并按状态起对外 API；ctx 结束时收掉全部。
func (s *Server) ListenAndServe(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	s.Start()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		s.Stop()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		os.Remove(socketPath)
		return nil
	case err := <-errCh:
		s.Stop()
		os.Remove(socketPath)
		return err
	}
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeErr(w http.ResponseWriter, err error) {
	var de *Error
	if !errors.As(err, &de) {
		writeError(w, http.StatusInternalServerError, CodeIOError, err.Error())
		return
	}
	status := http.StatusBadRequest
	switch de.Code {
	case CodeNotFound:
		status = http.StatusNotFound
	case CodeConflict, CodeSessionBusy, CodeTaskNotRunning:
		status = http.StatusConflict
	case CodeUnauthorized:
		status = http.StatusUnauthorized
	case CodeEngineMissing:
		status = http.StatusNotImplemented
	case CodeIOError, CodeBackendFailed, CodeEngineFailed, CodeListenFailed:
		status = http.StatusBadGateway
	case CodeNoBackend:
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, de.Code, de.Msg)
}

// decodeJSON 解请求体（上限 maxBody），空体按空对象。
func decodeJSON(r *http.Request, v any, maxBody int64) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return bad("读取请求体失败")
	}
	if int64(len(raw)) > maxBody {
		return bad("请求体过大")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return bad("请求体不是合法 JSON：" + err.Error())
	}
	return nil
}

func newID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func systemDescription() string {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`) + " " + runtime.GOARCH
			}
		}
	}
	return runtime.GOOS + " " + runtime.GOARCH
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}
