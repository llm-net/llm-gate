package admin_test

// 「智能体 → 主机/SoC」端点验收：路由暴露档位、会话/CSRF、错误映射，以及一条
// 从生成证书到解除纳管的完整路径（假主机在进程内跑真 SSH，不出网）。

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

const hostPassword = "n0t-a-real-password"

type agentCertBody struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	CreatedAt   string `json:"created_at"`
	FileName    string `json:"file_name"`
}

type agentHostBody struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	Address            string `json:"address"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	Status             string `json:"status"`
	CertCurrent        bool   `json:"cert_current"`
	KeyFingerprint     string `json:"key_fingerprint"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
	SudoNoPasswd       bool   `json:"sudo_nopasswd"`
	System             string `json:"system"`
	LastError          string `json:"last_error"`
	LastCheckedAt      string `json:"last_checked_at"`
}

type agentHostsBody struct {
	Certificate *agentCertBody  `json:"certificate"`
	Hosts       []agentHostBody `json:"hosts"`
}

// withAgentHosts 给这套环境接上纳管管理器，并把拨号恒指向那台假主机。
func withAgentHosts(t *testing.T, e *env, opt agenthosttest.Options) *agenthosttest.Server {
	t.Helper()
	if opt.Password == "" {
		opt.Password = hostPassword
	}
	srv := agenthosttest.New(t, opt)
	e.srv.SetAgentHosts(agenthost.New(agenthost.Options{Store: e.st, Dial: srv.Dial}))
	return srv
}

func TestAgentHostsUnavailableWithoutManager(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	response := e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusServiceUnavailable)
	if code := errCode(t, response); code != "agent_hosts_unavailable" {
		t.Fatalf("错误码 = %q", code)
	}
}

func TestAgentHostsLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	addr, port := host.Addr()

	// 还没生成证书：读数不带证书，添加主机被拒。
	var snapshot agentHostsBody
	response := e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &snapshot)
	if snapshot.Certificate != nil || len(snapshot.Hosts) != 0 {
		t.Fatalf("初始读数 = %+v", snapshot)
	}
	body := `{"name":"板子一","kind":"worker","address":"` + addr + `","port":` + strconv.Itoa(port) + `,"username":"admin","password":"` + hostPassword + `","configure_sudo":true}`
	response = e.do("POST", "/admin/v1/agent-hosts", root, body)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != agenthost.CodeCertificateMissing {
		t.Fatalf("错误码 = %q", code)
	}

	// 生成证书：公钥是 authorized_keys 单行，带下载文件名。
	response = e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var created struct {
		Certificate agentCertBody `json:"certificate"`
	}
	decodeInto(t, response, &created)
	if !strings.HasPrefix(created.Certificate.PublicKey, "ssh-ed25519 ") ||
		!strings.HasPrefix(created.Certificate.Fingerprint, "SHA256:") ||
		created.Certificate.FileName == "" {
		t.Fatalf("证书 = %+v", created.Certificate)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	wantStatus(t, response, http.StatusConflict)

	// 添加主机：装公钥、配免密 sudo，再用证书复验。
	response = e.do("POST", "/admin/v1/agent-hosts", root, body)
	wantStatus(t, response, http.StatusOK)
	var one struct {
		Host agentHostBody `json:"host"`
	}
	decodeInto(t, response, &one)
	if one.Host.Status != "ready" || !one.Host.CertCurrent || !one.Host.SudoNoPasswd {
		t.Fatalf("主机 = %+v", one.Host)
	}
	if one.Host.KeyFingerprint != created.Certificate.Fingerprint {
		t.Fatalf("已装指纹 = %q", one.Host.KeyFingerprint)
	}
	if one.Host.Kind != "worker" {
		t.Fatalf("类型 = %q", one.Host.Kind)
	}
	if !strings.HasPrefix(one.Host.HostKeyFingerprint, "SHA256:") || one.Host.System == "" {
		t.Fatalf("主机读数缺字段 = %+v", one.Host)
	}
	if !host.HasKey(created.Certificate.PublicKey) {
		t.Fatal("公钥没装到主机上")
	}

	// 同一台重复添加：409，不建第二行。
	response = e.do("POST", "/admin/v1/agent-hosts", root, body)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != agenthost.CodeHostExists {
		t.Fatalf("错误码 = %q", code)
	}

	// 改名、检查。
	response = e.do("PATCH", "/admin/v1/agent-hosts/"+itoa(one.Host.ID), root, `{"name":"客厅板子"}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &one)
	if one.Host.Name != "客厅板子" {
		t.Fatalf("名称 = %q", one.Host.Name)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+itoa(one.Host.ID)+"/check", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &one)
	if one.Host.Status != "ready" || one.Host.LastCheckedAt == "" {
		t.Fatalf("检查后 = %+v", one.Host)
	}

	// 轮换证书：新公钥下发到这台主机，旧的被摘掉。
	response = e.do("POST", "/admin/v1/agent-hosts/certificate/rotate", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var rotated struct {
		Certificate agentCertBody `json:"certificate"`
		Results     []struct {
			ID    int64  `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
		Hosts []agentHostBody `json:"hosts"`
	}
	decodeInto(t, response, &rotated)
	if rotated.Certificate.Fingerprint == created.Certificate.Fingerprint {
		t.Fatal("证书没换")
	}
	if len(rotated.Results) != 1 || !rotated.Results[0].OK {
		t.Fatalf("下发结果 = %+v", rotated.Results)
	}
	if len(rotated.Hosts) != 1 || !rotated.Hosts[0].CertCurrent {
		t.Fatalf("下发后读数 = %+v", rotated.Hosts)
	}
	if !host.HasKey(rotated.Certificate.PublicKey) || host.HasKey(created.Certificate.PublicKey) {
		t.Fatal("主机上的公钥没换干净")
	}

	// 解除纳管：默认把公钥从主机上摘掉。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+itoa(one.Host.ID), root, `{}`)
	wantStatus(t, response, http.StatusOK)
	var removed struct {
		KeyRemoved bool `json:"key_removed"`
	}
	decodeInto(t, response, &removed)
	if !removed.KeyRemoved || host.HasKey(rotated.Certificate.PublicKey) {
		t.Fatal("公钥没从主机上摘掉")
	}
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &snapshot)
	if len(snapshot.Hosts) != 0 {
		t.Fatalf("删除后仍有主机 = %+v", snapshot.Hosts)
	}

	// §15.1：口令不进日志、不进审计、不回响应。
	if strings.Contains(e.buf.String(), hostPassword) {
		t.Fatal("主机口令写进了日志")
	}
	if strings.Contains(e.buf.String(), "PRIVATE KEY") {
		t.Fatal("证书私钥写进了日志")
	}
}

func TestAgentHostBadPasswordMapsTo401(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withAgentHosts(t, e, agenthosttest.Options{Username: "admin", Password: "another"})
	addr, port := host.Addr()
	e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	response := e.do("POST", "/admin/v1/agent-hosts", root,
		`{"name":"板子","kind":"worker","address":"`+addr+`","port":`+strconv.Itoa(port)+`,"username":"admin","password":"`+hostPassword+`","configure_sudo":true}`)
	wantStatus(t, response, http.StatusUnauthorized)
	if code := errCode(t, response); code != agenthost.CodeAuthFailed {
		t.Fatalf("错误码 = %q", code)
	}
	// 401 是「那台主机拒绝了这把口令」，不是管理会话失效：行仍在，带着原因。
	response = e.do("GET", "/admin/v1/agent-hosts", root, "")
	wantStatus(t, response, http.StatusOK)
	var snapshot agentHostsBody
	decodeInto(t, response, &snapshot)
	if len(snapshot.Hosts) != 1 || snapshot.Hosts[0].Status != "error" || snapshot.Hosts[0].LastError == "" {
		t.Fatalf("读数 = %+v", snapshot.Hosts)
	}
}

func TestAgentHostMissingPasswordRejected(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	response := e.do("POST", "/admin/v1/agent-hosts", root,
		`{"name":"板子","kind":"worker","address":"10.0.0.9","port":22,"username":"admin","password":"","configure_sudo":true}`)
	wantStatus(t, response, http.StatusBadRequest)
	if code := errCode(t, response); code != "invalid_host" {
		t.Fatalf("错误码 = %q", code)
	}
}

func TestAgentHostUnknownID(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	e.do("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	for _, path := range []string{"/admin/v1/agent-hosts/404/check", "/admin/v1/agent-hosts/404/push"} {
		response := e.do("POST", path, root, `{}`)
		wantStatus(t, response, http.StatusNotFound)
		if code := errCode(t, response); code != "host_not_found" {
			t.Fatalf("%s 错误码 = %q", path, code)
		}
	}
}

func TestAgentHostRoutesRequireSessionAndCSRF(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	withAgentHosts(t, e, agenthosttest.Options{Username: "admin"})
	for _, path := range []string{"/admin/v1/agent-hosts", "/admin/v1/agent-hosts/certificate"} {
		response := e.do("POST", path, "", `{}`)
		wantStatus(t, response, http.StatusUnauthorized)
	}
	response := e.do("GET", "/admin/v1/agent-hosts", "", "")
	wantStatus(t, response, http.StatusUnauthorized)

	request := e.req("POST", "/admin/v1/agent-hosts/certificate", root, `{}`)
	request.Header.Del("X-LlmGate-CSRF")
	if got := e.send(request).StatusCode; got != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头时状态 = %d", got)
	}
}

func TestAgentHostWriteRoutesAreLANOnly(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 应实现 RouteLister")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	if tiers["GET /admin/v1/agent-hosts"] != tunnelctx.Admin {
		t.Fatalf("读数应是 Admin 档: %v", tiers["GET /admin/v1/agent-hosts"])
	}
	seen := 0
	for pattern, tier := range tiers {
		if !strings.Contains(pattern, "/admin/v1/agent-hosts") || strings.HasPrefix(pattern, "GET ") {
			continue
		}
		seen++
		if tier != tunnelctx.LANOnly {
			t.Errorf("%s 应只限 LAN，实际 %v", pattern, tier)
		}
	}
	if seen == 0 {
		t.Fatal("没找到任何写端点")
	}
}
