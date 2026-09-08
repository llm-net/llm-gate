package updated

// UDS HTTP 服务端：GET /status、POST /install、POST /rollback。传输层就是
// 权限模型——socket 文件 0660 root:llmgate，能连上的只有 root 与 gatewayd；
// 请求体是本机进程间的 JSON，无会话、无 CSRF（那些是浏览器面的事）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"time"
)

// Server 把引擎挂上一个 UDS 监听。
type Server struct {
	engine *Engine
	logger *slog.Logger
	hs     *http.Server
}

// NewServer 构造 UDS 服务端。
func NewServer(engine *Engine, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{engine: engine, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /install", s.handleInstall)
	mux.HandleFunc("POST /rollback", s.handleRollback)
	// 第三方组件槽位与 Cloudflare Tunnel connector 控制（components.go）。
	mux.HandleFunc("GET /components/{name}", s.handleComponentStatus)
	mux.HandleFunc("POST /components/{name}/install", s.handleComponentInstall)
	mux.HandleFunc("POST /components/{name}/rollback", s.handleComponentRollback)
	mux.HandleFunc("POST /components/{name}/remove", s.handleComponentRemove)
	mux.HandleFunc("GET /tunnel", s.handleTunnelStatus)
	mux.HandleFunc("POST /tunnel/start", s.handleTunnelStart)
	mux.HandleFunc("POST /tunnel/stop", s.handleTunnelStop)
	mux.HandleFunc("GET /proxy-core", s.handleProxyStatus)
	mux.HandleFunc("POST /proxy-core/start", s.handleProxyStart)
	mux.HandleFunc("POST /proxy-core/stop", s.handleProxyStop)
	s.hs = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

func (s *Server) handleComponentStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.ComponentStatus(r.PathValue("name"))
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleComponentInstall(w http.ResponseWriter, r *http.Request) {
	var req ComponentInstallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体不是合法 JSON"))
		return
	}
	st, err := s.engine.ComponentInstall(r.Context(), r.PathValue("name"), req)
	if err != nil {
		// 安装已落位但新版本未就绪、已切回旧版：把状态与原因一起带回（409 表示
		// 「做了但没换成」）。
		if st != nil {
			writeJSON(w, http.StatusConflict, errBody(err.Error()))
			return
		}
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleComponentRollback(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.ComponentRollback(r.Context(), r.PathValue("name"))
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleComponentRemove(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.ComponentRemove(r.Context(), r.PathValue("name"))
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.TunnelStatus(r.Context()))
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	// token 只在这一次解码里出现；不落日志、不进状态。
	var req tunnelStartWire
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体不是合法 JSON"))
		return
	}
	st, err := s.engine.TunnelStart(r.Context(), req.Token)
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.TunnelStop(r.Context())
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.ProxyStatus(r.Context()))
}

// handleProxyStart 收内核配置正文（含节点凭据）：只在这一次解码里出现，不落日志、不进状态。
func (s *Server) handleProxyStart(w http.ResponseWriter, r *http.Request) {
	var req proxyStartWire
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, ProxyConfigCap+(1<<20))).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体不是合法 JSON"))
		return
	}
	st, err := s.engine.ProxyStart(r.Context(), req.Config)
	if err != nil {
		if st != nil {
			writeJSON(w, http.StatusConflict, errBody(err.Error()))
			return
		}
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleProxyStop(w http.ResponseWriter, r *http.Request) {
	st, err := s.engine.ProxyStop(r.Context())
	if err != nil {
		writeComponentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// writeComponentError 把组件/Tunnel 的哨兵错误映射到状态码：未知组件 404，
// 组件未装/无上一版本 409，其余 400。
func writeComponentError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnknownComponent):
		status = http.StatusNotFound
	case errors.Is(err, ErrComponentMissing), errors.Is(err, ErrNoPreviousSlot):
		status = http.StatusConflict
	}
	writeJSON(w, status, errBody(err.Error()))
}

// Listen 在 socket 路径上开始监听并调好权限（0660，属组尽力 chown 成
// groupName；组不存在——dev 机——则跳过，靠目录权限兜底）。
func (s *Server) Listen(socket, groupName string) (net.Listener, error) {
	// 上一次进程残留的 socket 文件会让 bind 报 address already in use。
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("清理残留 socket: %w", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("设置 socket 权限: %w", err)
	}
	if groupName != "" && os.Geteuid() == 0 {
		if g, err := user.LookupGroup(groupName); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				if err := os.Chown(socket, 0, gid); err != nil {
					s.logger.Warn("设置 socket 属组失败", "group", groupName, "err", err.Error())
				}
			}
		} else {
			s.logger.Warn("系统里没有该属组，跳过 socket chown", "group", groupName)
		}
	}
	return ln, nil
}

// Serve 阻塞服务直到 ctx 取消，然后优雅关闭。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.hs.Serve(ln) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.hs.Shutdown(shCtx)
		<-errCh
		return nil
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Snapshot())
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体不是合法 JSON"))
		return
	}
	if err := s.engine.Install(req); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.engine.Snapshot())
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Rollback(); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.engine.Snapshot())
}

func writeEngineError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, ErrBusy) {
		status = http.StatusConflict
	}
	writeJSON(w, status, errBody(err.Error()))
}

type wireError struct {
	Error string `json:"error" i18n:"text"`
}

func errBody(msg string) wireError { return wireError{Error: msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
