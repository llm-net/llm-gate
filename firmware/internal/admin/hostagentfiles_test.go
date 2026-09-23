package admin_test

// Agent远控「文件」页签的端点验收：假 SSH 主机把设备下发的文件脚本交本机 /bin/sh 跑
// （目录是临时目录），验列目录、预览、整份读取的响应形状与错误码映射，以及路由档位。

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// withFilesHost 纳管一台受控主机：它把文件脚本交本机 /bin/sh 执行，HOME 指向 home。
func withFilesHost(t *testing.T, e *env, home string) int64 {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("本机没有 /bin/sh")
	}
	fake := withAgentHosts(t, e, agenthosttest.Options{Username: "admin", SudoNoPasswd: true,
		Command: func(command, stdin string) (string, int, bool) {
			if !strings.HasPrefix(command, "/bin/sh -c '# llmgate-host-files") {
				return "", 0, false
			}
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			cmd.Stdin = strings.NewReader(stdin)
			var out bytes.Buffer
			cmd.Stdout = &out
			err := cmd.Run()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return out.String(), exitErr.ExitCode(), true
			}
			if err != nil {
				t.Errorf("跑脚本: %v", err)
			}
			return out.String(), 0, true
		}})
	root := e.rootSession()
	wantStatus(t, e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`), http.StatusOK)
	addr, port := fake.Addr()
	resp := e.do("POST", "/admin/v1/agent-hosts", root,
		`{"name":"板子","kind":"managed","address":"`+addr+`","port":`+strconv.Itoa(port)+`,"username":"admin","password":"`+hostPassword+`"}`)
	wantStatus(t, resp, http.StatusOK)
	var one struct {
		Host agentHostBody `json:"host"`
	}
	decodeInto(t, resp, &one)
	return one.Host.ID
}

func TestHostAgentFiles(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"etc/app.conf": "port = 80\n", "logo.png": "\x89PNG\r\n\x1a\n", "notes 笔记.md": "# 标题\n"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id := withFilesHost(t, e, home)
	root := e.rootSession()
	base := "/admin/v1/agent-hosts/" + strconv.FormatInt(id, 10) + "/agent/files"
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }

	// 缺省列家目录：目录在前。
	resp := e.do("GET", base, root, "")
	wantStatus(t, resp, http.StatusOK)
	var listing struct {
		Path    string `json:"path"`
		Parent  string `json:"parent"`
		Home    string `json:"home"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
			Size int64  `json:"size"`
			Mode string `json:"mode"`
		} `json:"entries"`
	}
	decodeInto(t, resp, &listing)
	if listing.Path != home || listing.Home != home || len(listing.Entries) != 3 || !listing.Entries[0].Dir || listing.Entries[0].Name != "etc" {
		t.Fatalf("家目录清单 = %+v", listing)
	}

	// 预览文本、二进制。
	var preview struct {
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		Binary    bool   `json:"binary"`
		Truncated bool   `json:"truncated"`
		Content   string `json:"content"`
	}
	resp = e.do("GET", base+"/preview"+q(filepath.Join(home, "notes 笔记.md")), root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &preview)
	if preview.Binary || preview.Content != "# 标题\n" || preview.Size != int64(len("# 标题\n")) {
		t.Fatalf("文本预览 = %+v", preview)
	}
	resp = e.do("GET", base+"/preview"+q(filepath.Join(home, "logo.png")), root, "")
	wantStatus(t, resp, http.StatusOK)
	preview.Content = "x"
	decodeInto(t, resp, &preview)
	if !preview.Binary || preview.Content != "" {
		t.Fatalf("二进制预览 = %+v", preview)
	}

	// 整份读取：图片内联、带沙箱 CSP；download=1 与非图片都是附件。
	resp = e.do("GET", base+"/raw"+q(filepath.Join(home, "logo.png")), root, "")
	wantStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("安全头 = %v", resp.Header)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	resp.Body.Close()
	if body.String() != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("原字节 = %q", body.String())
	}
	resp = e.do("GET", base+"/raw"+q(filepath.Join(home, "logo.png"))+"&download=1", root, "")
	wantStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatalf("download Content-Disposition = %q", got)
	}
	resp.Body.Close()
	resp = e.do("GET", base+"/raw"+q(filepath.Join(home, "notes 笔记.md")), root, "")
	wantStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("非图片的头 = %v", resp.Header)
	}
	resp.Body.Close()

	// 错误码。
	for _, c := range []struct {
		path   string
		status int
		code   string
	}{
		{base + q("relative"), http.StatusBadRequest, "invalid_path"},
		{base + q(filepath.Join(home, "nope")), http.StatusNotFound, "path_not_found"},
		{base + q(filepath.Join(home, "logo.png")), http.StatusBadRequest, "not_directory"},
		{base + "/preview" + q(filepath.Join(home, "etc")), http.StatusBadRequest, "is_directory"},
		{base + "/preview", http.StatusBadRequest, "invalid_path"},
		{"/admin/v1/agent-hosts/999/agent/files", http.StatusNotFound, "host_not_found"},
	} {
		resp := e.do("GET", c.path, root, "")
		wantStatus(t, resp, c.status)
		if code := errCode(t, resp); code != c.code {
			t.Errorf("%s 错误码 = %q，要 %q", c.path, code, c.code)
		}
	}
}

func TestHostAgentFilesRouteTiers(t *testing.T) {
	e := newEnv(t)
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range e.h.(tunnelctx.RouteLister).TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for _, p := range []string{"GET /admin/v1/agent-hosts/{id}/agent/files", "GET /admin/v1/agent-hosts/{id}/agent/files/preview",
		"GET /admin/v1/agent-hosts/{id}/agent/files/raw"} {
		if tiers[p] != tunnelctx.LANOnly {
			t.Errorf("%s 应只限 LAN，实际 %v", p, tiers[p])
		}
	}
}
