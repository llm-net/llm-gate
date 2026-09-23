package modeld

// 对外 API（TCP 监听器）：给外部客户端与算力服务器用。每条请求都要 `Authorization: Bearer <令牌>`，
// 令牌由管理面签发（只存摘要）。路径：
//
//	GET    /api/v1/healthz             不要令牌：{"ok":true}
//	GET    /api/v1/models              承载的模型清单
//	POST   /api/v1/tasks               提交任务 {"model","input","priority"?,"timeout_sec"?} → 202 {"task"}
//	GET    /api/v1/tasks[?status=]     本令牌提交的任务
//	GET    /api/v1/tasks/{id}          查询（只看本令牌提交的）
//	DELETE /api/v1/tasks/{id}          取消
//	GET    /api/v1/files/{name}        模型缓存文件（支持 Range）
//	GET    /api/v1/files               模型缓存清单
//
// 监听地址可经管理面 PUT /v1/api {"listen"} 改动（立刻重新绑定并持久化；空串 = 关闭）。

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiStatus 是对外 API 的现状。
type apiStatus struct {
	Listen  string `json:"listen"`
	Running bool   `json:"running"`
	Addr    string `json:"addr,omitempty"`
	Error   string `json:"error,omitempty"`
	Tokens  int    `json:"tokens"`
}

func (s *Server) apiStatus() apiStatus {
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	return apiStatus{Listen: s.st.apiListen(), Running: s.apiLn != nil, Addr: s.apiAddr, Error: s.apiErr, Tokens: s.st.tokenCount()}
}

// bindAPI 在 addr 上起对外 API（先收掉已有的）。
func (s *Server) bindAPI(addr string) error {
	s.unbindAPI()
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	if addr == "" {
		s.apiErr = ""
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.apiErr = err.Error()
		return &Error{Code: CodeListenFailed, Msg: "监听 " + addr + " 失败：" + err.Error()}
	}
	srv := &http.Server{Handler: s.apiHandler(), ReadHeaderTimeout: 30 * time.Second}
	s.apiLn, s.apiSrv, s.apiErr, s.apiAddr = ln, srv, "", ln.Addr().String()
	go srv.Serve(ln)
	s.log.Info("对外 API 已监听", "addr", s.apiAddr)
	return nil
}

func (s *Server) unbindAPI() {
	s.apiMu.Lock()
	srv, ln := s.apiSrv, s.apiLn
	s.apiSrv, s.apiLn, s.apiAddr = nil, nil, ""
	s.apiMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		ln.Close()
	}
}

// APIAddr 是对外 API 实际监听的地址（测试用；没起为空）。
func (s *Server) APIAddr() string {
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	return s.apiAddr
}

func (s *Server) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "daemon": "modeld", "version": s.version})
	})
	mux.HandleFunc("GET /api/v1/models", s.withToken(func(w http.ResponseWriter, r *http.Request, _ *Token) {
		writeJSON(w, http.StatusOK, map[string]any{"models": s.servedModels()})
	}))
	mux.HandleFunc("POST /api/v1/tasks", s.withToken(func(w http.ResponseWriter, r *http.Request, tok *Token) {
		var in taskInput
		if err := decodeJSON(r, &in, maxTaskInput); err != nil {
			writeErr(w, err)
			return
		}
		t, err := s.submitTask(in, "api", tok.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		v, _ := s.taskView(t.ID, tok.ID)
		writeJSON(w, http.StatusAccepted, map[string]any{"task": v})
	}))
	mux.HandleFunc("GET /api/v1/tasks", s.withToken(func(w http.ResponseWriter, r *http.Request, tok *Token) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": s.taskViews(r.URL.Query().Get("status"), tok.ID, limit, false)})
	}))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.withToken(func(w http.ResponseWriter, r *http.Request, tok *Token) {
		v, err := s.taskView(r.PathValue("id"), tok.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": v})
	}))
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.withToken(func(w http.ResponseWriter, r *http.Request, tok *Token) {
		v, err := s.cancelTask(r.Context(), r.PathValue("id"), tok.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": v})
	}))
	mux.HandleFunc("GET /api/v1/files", s.withToken(func(w http.ResponseWriter, r *http.Request, _ *Token) {
		files, err := s.cache.list()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
	}))
	mux.HandleFunc("GET /api/v1/files/{name}", s.withToken(func(w http.ResponseWriter, r *http.Request, _ *Token) {
		s.cache.serve(w, r, r.PathValue("name"))
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "路径不存在")
	})
	return s.withLog(mux, "api")
}

// withToken 校验 Bearer 令牌。没签发过任何令牌时一律 503：对外 API 不能裸奔。
func (s *Server) withToken(next func(http.ResponseWriter, *http.Request, *Token)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.st.tokenCount() == 0 {
			writeError(w, http.StatusServiceUnavailable, CodeUnauthorized, "对外 API 还没有签发任何令牌")
			return
		}
		auth := r.Header.Get("Authorization")
		plain, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "缺少 Bearer 令牌")
			return
		}
		tok, ok := s.st.verifyToken(strings.TrimSpace(plain), s.now())
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "令牌无效")
			return
		}
		next(w, r, tok)
	}
}

// ---- 管理面：API 设置与令牌 ----

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"api": s.apiStatus(), "tokens": s.st.tokenViews()})
}

func (s *Server) handleAPIPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Listen string `json:"listen"`
	}
	if err := decodeJSON(r, &in, 4<<10); err != nil {
		writeErr(w, err)
		return
	}
	in.Listen = strings.TrimSpace(in.Listen)
	if in.Listen != "" {
		host, port, err := net.SplitHostPort(in.Listen)
		if err != nil {
			writeErr(w, bad("监听地址须是 主机:端口 形状（如 0.0.0.0:8790）"))
			return
		}
		if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
			writeErr(w, bad("端口须在 0–65535（0 = 随机端口）"))
			return
		}
		if host != "" && net.ParseIP(host) == nil && host != "localhost" {
			writeErr(w, bad("监听地址的主机部分须是 IP 地址"))
			return
		}
	}
	if err := s.st.setAPIListen(in.Listen); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.bindAPI(in.Listen); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api": s.apiStatus(), "tokens": s.st.tokenViews()})
}

func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tokens": s.st.tokenViews()})
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &in, 4<<10); err != nil {
		writeErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "token"
	}
	if len([]rune(in.Name)) > 64 {
		writeErr(w, bad("名称不能超过 64 个字符"))
		return
	}
	if s.st.tokenCount() >= 64 {
		writeErr(w, conflict("令牌不能超过 64 把"))
		return
	}
	view, plain, err := s.st.createToken(in.Name, s.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("签发对外 API 令牌", "token", view.ID)
	// 明文只在这一份响应里出现一次。
	writeJSON(w, http.StatusCreated, map[string]any{"token": view, "plaintext": plain})
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.st.deleteToken(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
