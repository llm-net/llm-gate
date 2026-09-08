package egress_test

// 互联网出口绊线：扫描固件全部生产 Go 文件里的 http.Client{} / http.Transport{} 复合字面量、
// http.DefaultClient / http.DefaultTransport / http.ProxyFromEnvironment / http.Get 等旁路，
// 只允许出现在下面登记过的构造点。没有显式 Transport 的 http.Client 会用 DefaultTransport
// ——它读 HTTP_PROXY 等环境变量，是绕过出站策略的暗门；本测试比搜 `Proxy: nil` 可靠。
//
// 新增互联网客户端时：经 egress.TransportFor 接入对应 scope，再把构造点登记进 allowed
// 并写明理由。本地 UDS / 127.0.0.1 探针逐项白名单，也要写明为什么它们不是互联网出口。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowed 是「文件路径#函数名」→ 理由。路径相对 firmware 模块根。
var allowed = map[string]string{
	// 出站策略本身。
	"internal/egress/manager.go#Test": "显式连通性测试：Transport 直接挂当前快照的 SOCKS5 拨号器",
	// 各 scope 的批准构造点（都经 egress.TransportFor 接入）。
	"internal/upstream/upstream.go#NewHTTPClient":                "model_api：模型/订阅数据面与管理面探测",
	"internal/officialsite/client.go#newTransport":               "official_site：官网 JSON / 透传 / 固件下载的基底 Transport",
	"internal/officialsite/client.go#newJSONClient":              "official_site：经 siteTransport 接入",
	"internal/officialsite/client.go#newPassthroughClient":       "official_site：经 siteTransport 接入",
	"internal/officialsite/firmware.go#FetchFirmware":            "official_site：经 siteTransport 接入",
	"internal/officialsite/components.go#FetchComponentArtifact": "component_artifacts：经 TransportFor 接入",
	"internal/codexauth/codexauth.go#newHTTPClient":              "agent_auth：OpenAI OAuth",
	"internal/grokauth/grokauth.go#newHTTPClient":                "agent_auth：xAI OAuth",
	"internal/agentquota/client.go#NewClient":                    "agent_auth：固定官方订阅额度接口，复用设备凭据",
	"internal/claudeauth/client.go#NewClient":                    "agent_auth：Claude OAuth 续期，固定官方地址且不跟随重定向",
	"internal/codexhelper/cli.go#newCLIHTTPClient":               "cli_artifacts：Codex 安装物透传",
	"internal/grokhelper/cli.go#newCLIHTTPClient":                "cli_artifacts：Grok 安装物透传",
	"internal/claudehelper/cli.go#newCLIHTTPClient":              "cli_artifacts：Claude Code 安装物透传",
	"internal/cursorhelper/cli.go#newCLIHTTPClient":              "cli_artifacts：Cursor 安装物透传",
	"internal/opencodehelper/cli.go#newCLIHTTPClient":            "cli_artifacts：OpenCode 安装物透传",
	"internal/landomain/site.go#NewSiteClient":                   "official_site：设备关联 / 域名 / 证书接口",
	"internal/mihomo/manager.go#NewFetchClient":                  "proxy_subscription：Clash 订阅拉取",
	// 有意直连或只走本机的例外。
	"internal/cloudflared/manager.go#NewManager":     "公网探测有意直连：验证的是本地网络能否经公网到达自己的 Tunnel 主机名，与独立进程 connector 同一视角",
	"internal/cloudflared/manager.go#probeReady":     "127.0.0.1 上的 connector metrics，本机回环",
	"internal/cloudflared/manager.go#probeOrigin":    "origin unix socket 自连，本机",
	"internal/updated/client.go#NewClient":           "升级引擎 UDS 客户端，本机",
	"internal/updated/systemctl.go#HTTPHealthProber": "gatewayd /healthz 回环探针，本机",
	"internal/updated/components.go#HTTPReadyProber": "cloudflared /ready 回环探针，本机",
}

// forbiddenSelectors 是任何生产文件都不该出现的旁路。
var forbiddenSelectors = map[string]bool{
	"http.DefaultClient":        true,
	"http.DefaultTransport":     true,
	"http.ProxyFromEnvironment": true,
	"http.Get":                  true,
	"http.Head":                 true,
	"http.Post":                 true,
	"http.PostForm":             true,
}

func TestInternetEgressTripwire(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("模块根定位失败（期望 %s/go.mod）: %v", root, err)
	}
	seen := map[string]bool{}
	var violations []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "web" || name == "node_modules" || name == "testdata" || name == "bin" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "tools/") || strings.HasPrefix(rel, "internal/egress/egresstest/") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		httpName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "net/http" {
				httpName = "http"
				if imp.Name != nil {
					httpName = imp.Name.Name
				}
			}
		}
		if httpName == "" {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == httpName && (sel.Sel.Name == "Client" || sel.Sel.Name == "Transport") {
							key := rel + "#" + fn.Name.Name
							seen[key] = true
							if _, ok := allowed[key]; !ok {
								violations = append(violations, key+"（http."+sel.Sel.Name+"{} 未登记）")
							}
						}
					}
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == httpName {
						if forbiddenSelectors["http."+x.Sel.Name] {
							violations = append(violations, rel+"#"+fn.Name.Name+"（使用 http."+x.Sel.Name+"）")
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range allowed {
		if !seen[key] {
			violations = append(violations, key+"（登记项已不存在，请从 allowed 删除）")
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("互联网出口绊线：\n  %s", strings.Join(violations, "\n  "))
	}
}
