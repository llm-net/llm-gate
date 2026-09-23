package admin

// Agent远控页的「文件」页签（agenthost/files.go）：以纳管用的 SSH 用户身份只读浏览主机。
//
//	GET /admin/v1/agent-hosts/{id}/agent/files?path=                 列一层目录（path 空 = 家目录）（LAN）
//	GET /admin/v1/agent-hosts/{id}/agent/files/preview?path=         文件开头一段：文本 / 二进制标记     （LAN）
//	GET /admin/v1/agent-hosts/{id}/agent/files/raw?path=[&download=1] 整份原字节（图片预览、下载）     （LAN）
//
// 三个都恒 LANOnly：读的是另一台机器上的任意文件（配置、密钥文件都在其中），与 devd 透传同档。
// 不写审计、不进操作日志——只读，不动主机；路径与内容不进日志（§15.1）。

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
)

// hostFileImageTypes 是 raw 端点按扩展名给出真实类型的图片（页面拿来做 <img> 预览）；
// 其余一律 application/octet-stream。SVG 也在列：响应带 sandbox CSP，直接打开也跑不了脚本。
var hostFileImageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".ico": "image/x-icon", ".avif": "image/avif",
	".svg": "image/svg+xml",
}

func (s *Server) handleAgentFilesList(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentFilesHost(w, r)
	if !ok {
		return
	}
	l, err := s.hosts.ListDir(r.Context(), id, r.URL.Query().Get("path"))
	if err != nil {
		s.writeHostFilesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleAgentFilesPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentFilesHost(w, r)
	if !ok {
		return
	}
	f, err := s.hosts.PreviewFile(r.Context(), id, r.URL.Query().Get("path"))
	if err != nil {
		s.writeHostFilesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleAgentFilesRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentFilesHost(w, r)
	if !ok {
		return
	}
	f, err := s.hosts.ReadFile(r.Context(), id, r.URL.Query().Get("path"))
	if err != nil {
		s.writeHostFilesError(w, r, err)
		return
	}
	name := path.Base(f.Path)
	ctype, image := hostFileImageTypes[strings.ToLower(path.Ext(name))]
	if !image {
		ctype = "application/octet-stream"
	}
	disposition := "attachment"
	if image && r.URL.Query().Get("download") != "1" {
		disposition = "inline"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(f.Content)))
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": name}); v != "" {
		h.Set("Content-Disposition", v)
	} else {
		h.Set("Content-Disposition", disposition)
	}
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if !f.ModTime.IsZero() {
		h.Set("Last-Modified", f.ModTime.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.Content)
	}
}

// agentFilesHost 取路径里的主机 id；主机是否存在由 agenthost 在连接时判。
func (s *Server) agentFilesHost(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !s.requireAgentHosts(w) {
		return 0, false
	}
	return agentHostID(w, r)
}

// writeHostFilesError 映射文件面的错误码；连接类错误照主机管理的映射。
func (s *Server) writeHostFilesError(w http.ResponseWriter, r *http.Request, err error) {
	var he *agenthost.Error
	if errors.As(err, &he) {
		status := 0
		switch he.Code {
		case agenthost.CodeInvalidPath, agenthost.CodeNotDirectory, agenthost.CodeIsDirectory, agenthost.CodeNotRegularFile:
			status = http.StatusBadRequest
		case agenthost.CodePathNotFound:
			status = http.StatusNotFound
		case agenthost.CodePermissionDenied:
			status = http.StatusForbidden
		case agenthost.CodeFileTooLarge:
			status = http.StatusRequestEntityTooLarge
		}
		if status != 0 {
			writeError(w, status, he.Code, he.Msg)
			return
		}
	}
	s.writeAgentHostError(w, r, err)
}
