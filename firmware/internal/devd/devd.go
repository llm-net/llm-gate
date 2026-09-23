// Package devd 是「智能体 → 主机/SoC」守护进程 devd llmgate-devd 的核心：一个只在
// 本机 Unix socket 上监听的 HTTP 服务，给设备界面的工作空间提供文件目录、Git 与 tmux
// 终端三样能力。它跑在被纳管的主机上、以纳管时指定的那个用户身份运行，能碰到什么由
// 主机的文件权限决定。
//
// 信任模型：守护进程自己不做鉴权，也不开任何网络端口。socket 文件 0600、只有运行
// 用户连得上；设备凭访问证书（SSH 公钥）以同一个用户登录主机，再经 SSH 的
// direct-streamlocal 通道连到这个 socket——鉴权与对端身份（主机公钥钉死）都由那条
// SSH 连接承担，主机上没有第二套证书。
//
// 本包只依赖标准库（含 wire 子包），守护进程二进制要嵌进固件、越小越好。
//
// 日志纪律（§15.1）：只记路径与结果，不记文件内容、终端字节与提交说明。
package devd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 错误码。设备把它们原样透给界面。
const (
	CodeInvalidPath    = "invalid_path"
	CodeNotFound       = "not_found"
	CodeForbidden      = "forbidden"
	CodeIOError        = "io_error"
	CodeNotRepo        = "not_a_repository"
	CodeGitMissing     = "git_missing"
	CodeGitFailed      = "git_failed"
	CodeTmuxMissing    = "tmux_missing"
	CodeTmuxFailed     = "tmux_failed"
	CodeInvalidSession = "invalid_session"
	CodeSessionExists  = "session_exists"
	CodeBadRequest     = "bad_request"
)

// Error 是本包对外的错误形态。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Options 是 New 的入参。
type Options struct {
	// Home 是文件目录与终端的起点（必填）。
	Home string
	// User / Shell 交给终端子进程的环境；空即继承。
	User  string
	Shell string
	// Version 是守护进程版本（GET /v1/info 回给设备）。
	Version string
	Logger  *slog.Logger
	// StateDir 是守护进程自有文件（attach 时 source 的 tmux.conf）落脚的目录；空即
	// ListenAndServe 用 socket 所在目录，直接用 Handler 时退到进程私有的临时目录。
	StateDir string
}

// Server 是守护进程本体。
type Server struct {
	home     string
	user     string
	shell    string
	version  string
	log      *slog.Logger
	stateDir string
	confMu   sync.Mutex
}

// New 装配 Server。
func New(o Options) *Server {
	s := &Server{home: o.Home, user: o.User, shell: o.Shell, version: o.Version, log: o.Logger, stateDir: o.StateDir}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.home == "" {
		s.home, _ = os.UserHomeDir()
	}
	return s
}

// ---- HTTP ----

// Handler 是 /v1/* 路由。能连上 socket 的只有运行用户自己（设备经 SSH 以它登录），
// 这里不再鉴权。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.handleInfo)
	mux.HandleFunc("GET /v1/fs/list", s.handleFSList)
	mux.HandleFunc("GET /v1/fs/read", s.handleFSRead)
	mux.HandleFunc("PUT /v1/fs/write", s.handleFSWrite)
	mux.HandleFunc("GET /v1/fs/raw", s.handleFSRaw)
	mux.HandleFunc("PUT /v1/fs/raw", s.handleFSRawPut)
	mux.HandleFunc("POST /v1/fs/mkdir", s.handleFSMkdir)
	mux.HandleFunc("POST /v1/fs/rename", s.handleFSRename)
	mux.HandleFunc("POST /v1/fs/delete", s.handleFSDelete)
	mux.HandleFunc("GET /v1/git/status", s.handleGitStatus)
	mux.HandleFunc("GET /v1/git/log", s.handleGitLog)
	mux.HandleFunc("GET /v1/git/diff", s.handleGitDiff)
	mux.HandleFunc("GET /v1/git/branches", s.handleGitBranches)
	mux.HandleFunc("POST /v1/git/stage", s.handleGitStage)
	mux.HandleFunc("POST /v1/git/discard", s.handleGitDiscard)
	mux.HandleFunc("POST /v1/git/commit", s.handleGitCommit)
	mux.HandleFunc("POST /v1/git/checkout", s.handleGitCheckout)
	mux.HandleFunc("POST /v1/git/pull", s.handleGitRemote("pull"))
	mux.HandleFunc("POST /v1/git/push", s.handleGitRemote("push"))
	mux.HandleFunc("POST /v1/git/fetch", s.handleGitRemote("fetch"))
	mux.HandleFunc("GET /v1/tmux/sessions", s.handleTmuxList)
	mux.HandleFunc("POST /v1/tmux/sessions", s.handleTmuxNew)
	mux.HandleFunc("DELETE /v1/tmux/sessions/{name}", s.handleTmuxKill)
	mux.HandleFunc("GET /v1/tmux/attach", s.handleTmuxAttach)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "路径不存在")
	})
	return s.withLog(mux)
}

func (s *Server) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.log.Info("access", "method", r.Method, "path", r.URL.Path, "status", rec.code(), "duration_ms", time.Since(start).Milliseconds())
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

// Unwrap 让 http.ResponseController 找得到底层 writer（attach 要 Hijack）。
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Info 是 GET /v1/info 的响应。
type Info struct {
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	System    string `json:"system"`
	Arch      string `json:"arch"`
	User      string `json:"user"`
	Home      string `json:"home"`
	Shell     string `json:"shell"`
	Tmux      bool   `json:"tmux"`
	Git       bool   `json:"git"`
	StartedAt string `json:"started_at"`
}

var startedAt = time.Now().UTC()

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	_, gitErr := lookPath("git")
	writeJSON(w, http.StatusOK, Info{
		Version: s.version, Hostname: host, System: systemDescription(), Arch: runtime.GOARCH,
		User: s.user, Home: s.home, Shell: s.shell, Tmux: tmuxPath() != "", Git: gitErr == nil,
		StartedAt: startedAt.Format(time.RFC3339),
	})
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	out, err := s.listDir(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFSRead(w http.ResponseWriter, r *http.Request) {
	out, err := s.readFile(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFSWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.writeFile(req.Path, req.Content)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFSRaw 按原字节读一个文件（http.ServeContent：支持 Range，Content-Type 按扩展名）；
// handleFSRawPut 把请求体按原字节写成文件（先落同目录临时文件再 rename）。两者给设备上的
// 创作工作空间搬运图像 / 视频用：JSON 的 fs/read / fs/write 只装得下文本。
func (s *Server) handleFSRaw(w http.ResponseWriter, r *http.Request) {
	f, info, err := s.openRaw(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (s *Server) handleFSRawPut(w http.ResponseWriter, r *http.Request) {
	out, err := s.writeRaw(r.URL.Query().Get("path"), r.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.mkdir(req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": p})
}

func (s *Server) handleFSRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.rename(req.From, req.To)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": p})
}

func (s *Server) handleFSDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.remove(req.Path, req.Recursive); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	out, err := s.gitStatus(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	out, err := s.gitLog(r.Context(), r.URL.Query().Get("path"), n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": out})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := s.gitDiff(r.Context(), q.Get("path"), q.Get("file"), q.Get("staged") == "1" || q.Get("staged") == "true")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"diff": out})
}

func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	out, err := s.gitBranches(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
}

func (s *Server) handleGitStage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string   `json:"path"`
		Files   []string `json:"files"`
		Unstage bool     `json:"unstage"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.gitStage(r.Context(), req.Path, req.Files, req.Unstage); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGitDiscard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string   `json:"path"`
		Files []string `json:"files"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.gitDiscard(r.Context(), req.Path, req.Files); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.gitCommit(r.Context(), req.Path, req.Message)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGitCheckout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Branch string `json:"branch"`
		Create bool   `json:"create"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.gitCheckout(r.Context(), req.Path, req.Branch, req.Create)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGitRemote(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		out, err := s.gitRemote(r.Context(), req.Path, action)
		if err != nil {
			// 失败也把输出带回去：推送被拒的原因就在里面。
			var de *Error
			if errors.As(err, &de) && out != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"code": de.Code, "message": de.Msg}, "output": out.Output})
				return
			}
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) handleTmuxList(w http.ResponseWriter, r *http.Request) {
	out, err := s.tmuxList(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTmuxNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
		// Tool 非空时会话建好后直接启动 `gate <tool>`（DevTools 之一）。
		Tool string `json:"tool"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.tmuxNew(r.Context(), req.Name, req.Dir, req.Tool); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTmuxKill(w http.ResponseWriter, r *http.Request) {
	if err := s.tmuxKill(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// UpgradeProtocol 是 attach 的 Upgrade 头值：101 之后这条连接就是 wire 帧流。
const UpgradeProtocol = "llmgate-pty"

// handleTmuxAttach 把连接升级成 wire 帧流并挂到一个 tmux 客户端上。
func (s *Server) handleTmuxAttach(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), UpgradeProtocol) {
		writeError(w, http.StatusUpgradeRequired, CodeBadRequest, "需要 Upgrade: "+UpgradeProtocol)
		return
	}
	q := r.URL.Query()
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	if cols < 0 || cols > 1000 || rows < 0 || rows > 1000 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "终端尺寸不合法")
		return
	}
	name := q.Get("name")
	if name == "" {
		name = "main"
	}
	ts, err := s.attach(name, q.Get("dir"), uint16(cols), uint16(rows))
	if err != nil {
		writeErr(w, err)
		return
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		ts.close()
		writeError(w, http.StatusInternalServerError, CodeTmuxFailed, "连接不支持升级")
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+UpgradeProtocol+"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		ts.close()
		return
	}
	s.log.Info("终端已接入", "session", name)
	ts.serve(conn)
	s.log.Info("终端已断开", "session", name)
}

// ---- 响应 ----

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
	case CodeNotFound, CodeNotRepo:
		status = http.StatusNotFound
	case CodeForbidden:
		status = http.StatusForbidden
	case CodeSessionExists, CodeGateMissing, CodeGateUnconfigured:
		status = http.StatusConflict
	case CodeGitMissing, CodeTmuxMissing:
		status = http.StatusNotImplemented
	case CodeIOError, CodeGitFailed, CodeTmuxFailed:
		status = http.StatusBadGateway
	}
	writeError(w, status, de.Code, de.Msg)
}

const maxBody = 16 << 20

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "请求体不是合法 JSON")
		return false
	}
	return true
}

// ---- 杂项 ----

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

// ListenAndServe 在 Unix socket 上起服务，ctx 结束即优雅停机。上次没清掉的残留
// socket 文件先删；socket 收成 0600，只有运行用户连得上。
func (s *Server) ListenAndServe(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	if s.stateDir == "" {
		s.stateDir = filepath.Dir(socketPath)
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
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.log.Info("llmgate-devd 已启动", "socket", socketPath, "home", s.home, "user", s.user)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
