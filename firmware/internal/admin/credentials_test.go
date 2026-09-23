package admin_test

// 「智能体 → 凭证管理」端点验收：路由暴露档位、增删改查的形状、入参校验与重复、令牌永不
// 回响应 / 进日志 / 进审计（§15.1）。

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

const testCredentialToken = "ghp_TESTTOKEN0123456789abcdef"

type credentialDTO struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Username   string `json:"username"`
	SecretHint string `json:"secret_hint"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type credentialsBody struct {
	Credentials []credentialDTO `json:"credentials"`
	Kinds       []struct {
		Kind  string   `json:"kind"`
		Hosts []string `json:"hosts"`
	} `json:"kinds"`
}

func TestCredentialsExposure(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 不暴露路由表")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for pattern, want := range map[string]tunnelctx.Exposure{
		"GET /admin/v1/credentials":         tunnelctx.Admin,
		"POST /admin/v1/credentials":        tunnelctx.LANOnly,
		"PATCH /admin/v1/credentials/{id}":  tunnelctx.LANOnly,
		"DELETE /admin/v1/credentials/{id}": tunnelctx.LANOnly,
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != want {
			t.Fatalf("%s 档位 = %v，期望 %v", pattern, tier, want)
		}
	}
}

func TestCredentialsRequireSession(t *testing.T) {
	e := newEnv(t)
	for _, ep := range []struct{ method, path string }{
		{"GET", "/admin/v1/credentials"},
		{"POST", "/admin/v1/credentials"},
		{"PATCH", "/admin/v1/credentials/x"},
		{"DELETE", "/admin/v1/credentials/x"},
	} {
		wantStatus(t, e.do(ep.method, ep.path, "", `{}`), http.StatusUnauthorized)
	}
}

func TestCredentialsLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	var list credentialsBody
	response := e.do("GET", "/admin/v1/credentials", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &list)
	if len(list.Credentials) != 0 || len(list.Kinds) != 1 || list.Kinds[0].Kind != "git" ||
		strings.Join(list.Kinds[0].Hosts, ",") != "github.com,gitee.com" {
		t.Fatalf("初始读数 = %+v", list)
	}

	// 添加：类型 git、站点 github.com。
	var one struct {
		Credential credentialDTO `json:"credential"`
	}
	response = e.do("POST", "/admin/v1/credentials", root,
		`{"kind":"git","name":"工作账号","host":"github.com","username":"octocat","secret":"`+testCredentialToken+`"}`)
	wantStatus(t, response, http.StatusCreated)
	decodeInto(t, response, &one)
	c := one.Credential
	if c.ID == "" || c.Kind != "git" || c.Name != "工作账号" || c.Host != "github.com" || c.Username != "octocat" ||
		c.SecretHint != testCredentialToken[len(testCredentialToken)-4:] || c.CreatedAt == "" || c.UpdatedAt == "" {
		t.Fatalf("新行 = %+v", c)
	}

	// 同站点同账号重复 → 409；gitee.com 是另一份。
	response = e.do("POST", "/admin/v1/credentials", root,
		`{"kind":"git","host":"github.com","username":"octocat","secret":"another-token-value"}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != "credential_exists" {
		t.Fatalf("错误码 = %q", code)
	}
	response = e.do("POST", "/admin/v1/credentials", root,
		`{"kind":"git","host":"gitee.com","username":"octocat","secret":"gitee-private-token-0001"}`)
	wantStatus(t, response, http.StatusCreated)

	// 入参校验 → 400 invalid_credential。
	for name, body := range map[string]string{
		"缺类型":   `{"host":"github.com","username":"u","secret":"x"}`,
		"类型不认识": `{"kind":"ssh","host":"github.com","username":"u","secret":"x"}`,
		"站点不认识": `{"kind":"git","host":"gitlab.com","username":"u","secret":"x"}`,
		"缺用户名":  `{"kind":"git","host":"github.com","secret":"x"}`,
		"缺令牌":   `{"kind":"git","host":"github.com","username":"u"}`,
	} {
		response = e.do("POST", "/admin/v1/credentials", root, body)
		wantStatus(t, response, http.StatusBadRequest)
		if code := errCode(t, response); code != "invalid_credential" {
			t.Fatalf("%s：错误码 = %q", name, code)
		}
	}
	// 未知字段照旧 400 bad_request（decodeJSON 的通用规则）。
	response = e.do("POST", "/admin/v1/credentials", root, `{"kind":"git","host":"github.com","username":"u","secret":"x","token":"y"}`)
	wantStatus(t, response, http.StatusBadRequest)

	// 列表两行，按创建顺序。
	response = e.do("GET", "/admin/v1/credentials", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &list)
	if len(list.Credentials) != 2 || list.Credentials[0].ID != c.ID || list.Credentials[1].Host != "gitee.com" {
		t.Fatalf("清单 = %+v", list.Credentials)
	}

	// 修改：只改名称与账号，令牌留空不换。
	response = e.do("PATCH", "/admin/v1/credentials/"+c.ID, root, `{"name":"备用","username":"octocat-2","secret":""}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &one)
	if one.Credential.Name != "备用" || one.Credential.Username != "octocat-2" || one.Credential.SecretHint != c.SecretHint || one.Credential.Host != "github.com" {
		t.Fatalf("改后 = %+v", one.Credential)
	}
	if sec, err := e.st.GetCredentialSecret(context.Background(), c.ID); err != nil || sec.Plaintext() != testCredentialToken {
		t.Fatalf("不换令牌时令牌变了：%v", err)
	}
	// 换令牌与站点。
	next := "glpat-REPLACED9876543210"
	response = e.do("PATCH", "/admin/v1/credentials/"+c.ID, root, `{"host":"gitee.com","username":"someone-else","secret":"`+next+`"}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &one)
	if one.Credential.Host != "gitee.com" || one.Credential.SecretHint != next[len(next)-4:] {
		t.Fatalf("换令牌后 = %+v", one.Credential)
	}
	if sec, err := e.st.GetCredentialSecret(context.Background(), c.ID); err != nil || sec.Plaintext() != next {
		t.Fatalf("换令牌后解封 = %v", err)
	}
	// 改成与另一行撞车 → 409；改成不认识的站点 → 400；不存在 → 404。
	response = e.do("PATCH", "/admin/v1/credentials/"+c.ID, root, `{"username":"octocat"}`)
	wantStatus(t, response, http.StatusConflict)
	response = e.do("PATCH", "/admin/v1/credentials/"+c.ID, root, `{"host":"example.com"}`)
	wantStatus(t, response, http.StatusBadRequest)
	response = e.do("PATCH", "/admin/v1/credentials/does-not-exist", root, `{"name":"x"}`)
	wantStatus(t, response, http.StatusNotFound)
	if code := errCode(t, response); code != "credential_not_found" {
		t.Fatalf("错误码 = %q", code)
	}

	// 删除。
	response = e.do("DELETE", "/admin/v1/credentials/"+c.ID, root, "")
	wantStatus(t, response, http.StatusNoContent)
	response = e.do("DELETE", "/admin/v1/credentials/"+c.ID, root, "")
	wantStatus(t, response, http.StatusNotFound)
	response = e.do("GET", "/admin/v1/credentials", root, "")
	decodeInto(t, response, &list)
	if len(list.Credentials) != 1 || list.Credentials[0].Host != "gitee.com" {
		t.Fatalf("删后清单 = %+v", list.Credentials)
	}

	// §15.1：令牌不回响应、不进日志、不进审计。
	for _, secret := range []string{testCredentialToken, next, "gitee-private-token-0001", "another-token-value"} {
		if strings.Contains(e.buf.String(), secret) {
			t.Fatalf("令牌 %q 写进了日志", secret[:4])
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `SELECT event, entity, detail FROM audit_events WHERE event LIKE 'credential.%' ORDER BY id`)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var event, entity, detail string
		if err := rows.Scan(&event, &entity, &detail); err != nil {
			t.Fatalf("读审计: %v", err)
		}
		if strings.Contains(detail, testCredentialToken) || strings.Contains(detail, next) {
			t.Fatalf("审计 detail 带了令牌：%s", detail)
		}
		if !strings.HasPrefix(entity, "credential:") {
			t.Fatalf("审计实体 = %q", entity)
		}
		events = append(events, event)
	}
	if got := strings.Join(events, ","); got != "credential.create,credential.create,credential.update,credential.update,credential.delete" {
		t.Fatalf("审计事件 = %s", got)
	}
}
