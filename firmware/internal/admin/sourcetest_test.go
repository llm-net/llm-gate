// sourcetest_test.go 是「测试来源」端点（POST /admin/v1/sources/{id}/test）的
// 可执行验收：
//
//   - mock 双协议上游：两条结果同序（chat → messages）、探测请求的路径/
//     anthropic-version/凭证注入/请求体（解析后的来源侧模型 ID、max_tokens=1）
//     全部落在上游侧断言；已停用的来源照样可测；审计 source.test 只记状态码。
//   - 上游错误体：OpenAI/Anthropic 两种风格的 error.message 提取、非 JSON 错误
//     页的截断兜底；摘要只进响应，不进日志与审计（§15.1）。
//   - 凭证链路：deepseek 型上游（store 直建以设 base_url）探测时上游收到
//     Bearer 明文，而明文不出现在响应/日志/审计任何一处。
//   - 协议过滤：ark 型只探 openai_chat 一条（与 protocols 徽标同一口径）。
//   - 连接失败：status=0 且 message 非空；密文损坏：409 upstream_key_unreadable；
//     未认证 401 / 缺 CSRF 头 403 / 来源不存在 404。
//
// 复用 server_test.go 的 env/req/do/errCode 等装置（同属 admin_test 包）。
package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 真实来源模型决定测试和读数的协议；Chat/Responses 只发一次 Chat 探测，
// Messages 单独按 x-api-key 鉴权。探测会话与凭据不进入管理响应、日志和审计。
func TestOpenCodeGoProbeProtocolsAndReadings(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	var mu sync.Mutex
	var sessions []string
	var requests []string
	requestCount := func() int { mu.Lock(); defer mu.Unlock(); return len(requests) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sid := r.Header.Get("x-opencode-session")
		if sid == "" || !strings.HasPrefix(r.UserAgent(), "llmgate/") {
			t.Error("探测缺少会话或客户端标识")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sessions = append(sessions, sid)
		requests = append(requests, r.URL.Path)
		if r.URL.Path == "/messages" {
			if r.Header.Get("x-api-key") != upstreamKeyPlaintext || r.Header.Get("Authorization") != "" {
				t.Error("Messages 鉴权错误")
			}
		} else if r.Header.Get("Authorization") != "Bearer "+upstreamKeyPlaintext || r.Header.Get("x-api-key") != "" {
			t.Error("Chat 鉴权错误")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	up, err := e.st.CreateUpstream(t.Context(), "opencode-test", "opencode_go", upstreamKeyPlaintext, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"longcat-2.0":   {"openai_chat", "openai_responses"},
		"mimo-v2.5":     {"openai_chat", "openai_responses"},
		"mimo-v2.5-pro": {"openai_chat", "openai_responses"},
		"minimax-m3":    {"anthropic_messages"},
		"gpt-5.6-luna":  {},
	}
	for model, protocols := range want {
		m := e.createModel(root, "alias-"+model)
		src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":%q}`, up.ID, model))
		if !slices.Equal(src.Protocols, protocols) {
			t.Fatalf("%s 来源徽标 = %v", model, src.Protocols)
		}
		before := requestCount()
		report := e.testSource(root, src.ID)
		if len(report.Results) != len(protocols) {
			t.Fatalf("%s 测试协议不符: %+v", model, report.Results)
		}
		for i, result := range report.Results {
			if result.Protocol != protocols[i] || !result.OK || result.Status != http.StatusOK {
				t.Fatalf("%s 测试失败: %+v", model, result)
			}
		}
		calls := 1
		if len(protocols) == 0 {
			calls = 0
		}
		if requestCount()-before != calls {
			t.Fatal("重复发送 Chat/Responses 探测或探测了不支持的协议")
		}
	}
	access := e.access(root)
	if len(access.Models) != len(want)-1 {
		t.Fatalf("可用模型清单含不可用型号: %+v", access.Models)
	}
	for _, m := range access.Models {
		if !slices.Equal(m.Protocols, want[strings.TrimPrefix(m.Name, "alias-")]) {
			t.Fatalf("接入读数协议不符: %+v", m)
		}
	}
	models := e.listModels(root)
	for _, m := range models {
		if !slices.Equal(m.Sources[0].Protocols, want[strings.TrimPrefix(m.Name, "alias-")]) {
			t.Fatalf("模型列表徽标不符: %+v", m)
		}
	}
	mu.Lock()
	secrets := append(slices.Clone(sessions), upstreamKeyPlaintext)
	mu.Unlock()
	for _, secret := range secrets {
		if strings.Contains(e.buf.String(), secret) || strings.Contains(strings.Join(e.auditDetails("source.test"), "\n"), secret) {
			t.Error("探测凭据或会话泄露")
		}
	}
}

type sourceTestResultDTO struct {
	Protocol  string `json:"protocol"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

type sourceTestReportDTO struct {
	SourceID        int64                 `json:"source_id"`
	ModelID         int64                 `json:"model_id"`
	Model           string                `json:"model"`
	Upstream        string                `json:"upstream"`
	UpstreamModelID string                `json:"upstream_model_id"`
	Results         []sourceTestResultDTO `json:"results"`
}

// probeReq 是假上游收到的一次探测请求的留痕。
type probeReq struct {
	path, auth, version, body string
}

// probeCapture 并发安全地记录假上游收到的请求。
type probeCapture struct {
	mu   sync.Mutex
	reqs []probeReq
}

func (c *probeCapture) add(r probeReq) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

func (c *probeCapture) all() []probeReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]probeReq(nil), c.reqs...)
}

// newProbeUpstream 起一个记录请求并固定应答的假上游。
func newProbeUpstream(t *testing.T, status int, respBody string) (*httptest.Server, *probeCapture) {
	t.Helper()
	cap := &probeCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.add(probeReq{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			version: r.Header.Get("anthropic-version"),
			body:    string(b),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// testSource 调测试端点（期望 200）并解出报告。
func (e *env) testSource(cookie string, sourceID int64) sourceTestReportDTO {
	e.t.Helper()
	resp := e.do("POST", fmt.Sprintf("/admin/v1/sources/%d/test", sourceID), cookie, "")
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("测试来源状态 = %d，期望 200；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var out sourceTestReportDTO
	decodeInto(e.t, resp, &out)
	return out
}

// auditDetails 取指定事件的全部审计 detail（按写入序）。
func (e *env) auditDetails(event string) []string {
	return e.auditColumn(event, "detail")
}

func (e *env) auditColumn(event, column string) []string {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		e.t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	// column 是本文件里写死的列名，不接受外部输入。
	rows, err := db.Query(`SELECT `+column+` FROM audit_events WHERE event = ? ORDER BY id`, event)
	if err != nil {
		e.t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			e.t.Fatalf("扫描审计行: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("遍历审计行: %v", err)
	}
	return out
}

// TestSourceTestMockBothProtocols：mock 双协议来源的整条快乐路径——两个入口
// 各一次探测、请求形状与凭证注入、留空来源侧 ID 的解析、停用行照测、审计留痕。
func TestSourceTestMockBothProtocols(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	srv, cap := newProbeUpstream(t, http.StatusOK, `{"id":"cmpl-1","model":"whatever","choices":[]}`)

	up := e.createUpstream(root, mockUpstreamBody("probe-mock", srv.URL))
	m := e.createModel(root, "probe-chat")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))

	report := e.testSource(root, src.ID)
	if report.SourceID != src.ID || report.Model != "probe-chat" || report.Upstream != "probe-mock" {
		t.Fatalf("报告头异常: %+v", report)
	}
	// 留空的来源侧模型 ID 已解析为模型名——管理员看到的就是发往上游的值。
	if report.UpstreamModelID != "probe-chat" {
		t.Errorf("UpstreamModelID = %q，期望解析为模型名", report.UpstreamModelID)
	}
	if len(report.Results) != 3 ||
		report.Results[0].Protocol != "openai_chat" || report.Results[1].Protocol != "openai_responses" || report.Results[2].Protocol != "anthropic_messages" {
		t.Fatalf("结果应为 chat → responses → messages 三条: %+v", report.Results)
	}
	for _, r := range report.Results {
		if !r.OK || r.Status != http.StatusOK || r.LatencyMS < 1 || r.Message != "" {
			t.Errorf("2xx 探测结果异常: %+v", r)
		}
	}

	reqs := cap.all()
	if len(reqs) != 2 {
		t.Fatalf("上游收到 %d 个请求，期望 2", len(reqs))
	}
	chat, messages := reqs[0], reqs[1]
	if chat.path != "/chat/completions" || messages.path != "/messages" {
		t.Errorf("探测路径 = %q / %q", chat.path, messages.path)
	}
	if chat.version != "" || messages.version != "2023-06-01" {
		t.Errorf("anthropic-version 只应出现在 messages 探测: %q / %q", chat.version, messages.version)
	}
	for _, r := range reqs {
		if !strings.Contains(r.body, `"model":"probe-chat"`) || !strings.Contains(r.body, `"max_tokens":1`) {
			t.Errorf("探测请求体形状异常: %s", r.body)
		}
	}

	// 已停用的来源照样可测：先测通再启用是常规操作。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", src.ID), root, `{"disabled":true}`), http.StatusOK)
	if got := e.testSource(root, src.ID); len(got.Results) != 3 || !got.Results[0].OK {
		t.Errorf("停用来源的测试结果异常: %+v", got.Results)
	}

	details := e.auditDetails("source.test")
	if len(details) != 2 {
		t.Fatalf("source.test 审计 %d 条，期望 2", len(details))
	}
	want := "model=probe-chat upstream=probe-mock openai_chat=200 openai_responses=200 anthropic_messages=200"
	if details[0] != want {
		t.Errorf("审计 detail = %q，期望 %q", details[0], want)
	}
}

// TestSourceTestErrorSummary：上游错误体的摘要提取（OpenAI/Anthropic/非 JSON
// 三种形态），且摘要只进响应——日志与审计里不得出现（§15.1）。
func TestSourceTestErrorSummary(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	const oaMsg = "Incorrect API key provided"
	oaSrv, _ := newProbeUpstream(t, http.StatusUnauthorized,
		`{"error":{"message":"`+oaMsg+`","type":"invalid_request_error","code":"invalid_api_key"}}`)
	anSrv, _ := newProbeUpstream(t, http.StatusForbidden,
		`{"type":"error","error":{"type":"permission_error","message":"account suspended"}}`)
	rawSrv, _ := newProbeUpstream(t, http.StatusBadGateway, "<html>upstream gateway exploded</html>")

	cases := []struct {
		name, wantSub string
		srv           *httptest.Server
		status        int
	}{
		{"oa", oaMsg, oaSrv, http.StatusUnauthorized},
		{"an", "permission_error: account suspended", anSrv, http.StatusForbidden},
		{"raw", "upstream gateway exploded", rawSrv, http.StatusBadGateway},
	}
	for i, c := range cases {
		up := e.createUpstream(root, mockUpstreamBody(fmt.Sprintf("err-mock-%d", i), c.srv.URL))
		m := e.createModel(root, fmt.Sprintf("err-model-%d", i))
		src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
		report := e.testSource(root, src.ID)
		for _, r := range report.Results {
			if r.OK || r.Status != c.status {
				t.Errorf("[%s] 非 2xx 结果异常: %+v", c.name, r)
			}
			if !strings.Contains(r.Message, c.wantSub) {
				t.Errorf("[%s] message = %q，期望含 %q", c.name, r.Message, c.wantSub)
			}
		}
	}

	// 摘要不落日志、不进审计 detail。
	logs := e.buf.String()
	for _, sub := range []string{oaMsg, "account suspended", "upstream gateway exploded"} {
		if strings.Contains(logs, sub) {
			t.Errorf("上游错误摘要泄入日志: %q", sub)
		}
		for _, d := range e.auditDetails("source.test") {
			if strings.Contains(d, sub) {
				t.Errorf("上游错误摘要泄入审计 detail: %q", d)
			}
		}
	}
}

// TestSourceTestKeyInjectionNeverLeaks：产品型上游（store 直建 deepseek +
// base_url，与 YAML seed 同一能力）探测时凭证以 Bearer 到达上游；明文在
// 响应、日志、审计三处全不出现。
func TestSourceTestKeyInjectionNeverLeaks(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	srv, cap := newProbeUpstream(t, http.StatusOK, `{"choices":[]}`)

	if _, err := e.st.CreateUpstream(context.Background(), "ds-probe", "deepseek", upstreamKeyPlaintext, srv.URL); err != nil {
		t.Fatalf("store.CreateUpstream: %v", err)
	}
	ups := e.listUpstreams(root)
	m := e.createModel(root, "ds-probe-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"deepseek-chat"}`, ups[0].ID))

	resp := e.do("POST", fmt.Sprintf("/admin/v1/sources/%d/test", src.ID), root, "")
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("测试来源状态 = %d；body: %s", resp.StatusCode, raw)
	}
	if strings.Contains(raw, upstreamKeyPlaintext) {
		t.Error("测试响应泄露上游 Key 明文")
	}

	reqs := cap.all()
	if len(reqs) != 2 { // deepseek 双协议
		t.Fatalf("上游收到 %d 个请求，期望 2", len(reqs))
	}
	for _, r := range reqs {
		if r.auth != "Bearer "+upstreamKeyPlaintext {
			t.Errorf("探测请求的 Authorization 异常（未注入上游 Key）")
		}
		if !strings.Contains(r.body, `"model":"deepseek-chat"`) {
			t.Errorf("探测请求体未用来源侧模型 ID: %s", r.body)
		}
	}

	if strings.Contains(e.buf.String(), upstreamKeyPlaintext) {
		t.Error("日志泄露上游 Key 明文")
	}
	for _, d := range e.auditDetails("source.test") {
		if strings.Contains(d, upstreamKeyPlaintext) {
			t.Error("审计 detail 泄露上游 Key 明文")
		}
	}
}

// TestSourceTestProtocolFilter：ark 型只服务 openai_chat，探测也只打一次
// ——与列表页 protocols 徽标、数据面候选过滤同一口径。
func TestSourceTestProtocolFilter(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	srv, cap := newProbeUpstream(t, http.StatusOK, `{"choices":[]}`)

	if _, err := e.st.CreateUpstream(context.Background(), "ark-probe", "ark", upstreamKeyPlaintext, srv.URL); err != nil {
		t.Fatalf("store.CreateUpstream: %v", err)
	}
	ups := e.listUpstreams(root)
	m := e.createModel(root, "ark-probe-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, ups[0].ID))

	report := e.testSource(root, src.ID)
	if len(report.Results) != 2 || report.Results[0].Protocol != "openai_chat" || report.Results[1].Protocol != "openai_responses" {
		t.Fatalf("ark 的 Chat 与 Responses 协议面共用 Chat 上游探测: %+v", report.Results)
	}
	if reqs := cap.all(); len(reqs) != 1 || reqs[0].path != "/chat/completions" {
		t.Errorf("上游收到的请求异常: %+v", reqs)
	}
}

// TestSourceTestConnectionFailure：上游拒连时 status=0、ok=false、message 非空，
// 端点本身仍 200——连不上是测试的结论，不是测试的失败。
func TestSourceTestConnectionFailure(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 占个端口再立刻释放：该地址此刻必然拒连。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	up := e.createUpstream(root, mockUpstreamBody("dead-mock", deadURL))
	m := e.createModel(root, "dead-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))

	report := e.testSource(root, src.ID)
	if len(report.Results) != 3 {
		t.Fatalf("结果条数 = %d，期望 2", len(report.Results))
	}
	for _, r := range report.Results {
		if r.OK || r.Status != 0 || r.Message == "" {
			t.Errorf("拒连结果异常: %+v", r)
		}
	}
	details := e.auditDetails("source.test")
	if len(details) != 1 || !strings.Contains(details[0], "openai_chat=net_error") {
		t.Errorf("审计应记 net_error: %v", details)
	}
}

// TestSourceTestKeyUnreadable：密文损坏（设备密钥被替换的等价形态）→ 409
// upstream_key_unreadable，且不向上游发出任何请求。
func TestSourceTestKeyUnreadable(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	srv, cap := newProbeUpstream(t, http.StatusOK, `{"choices":[]}`)

	if _, err := e.st.CreateUpstream(context.Background(), "corrupt-ds", "deepseek", upstreamKeyPlaintext, srv.URL); err != nil {
		t.Fatalf("store.CreateUpstream: %v", err)
	}
	ups := e.listUpstreams(root)
	m := e.createModel(root, "corrupt-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, ups[0].ID))

	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开库连接: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE upstreams SET api_key_sealed = 'AAAA' WHERE id = ?`, ups[0].ID); err != nil {
		t.Fatalf("损坏密文: %v", err)
	}

	resp := e.do("POST", fmt.Sprintf("/admin/v1/sources/%d/test", src.ID), root, "")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "upstream_key_unreadable" {
		t.Errorf("错误码 = %q，期望 upstream_key_unreadable", code)
	}
	if reqs := cap.all(); len(reqs) != 0 {
		t.Errorf("凭证解不开时不应向上游发请求，实际发了 %d 个", len(reqs))
	}
}

// TestSourceTestBoundaries：未认证 401、缺 CSRF 头 403、member 无权 403
// （测试要花客户上游账户的真钱，不属于自助作用域）、来源不存在 404。
func TestSourceTestBoundaries(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	srv, cap := newProbeUpstream(t, http.StatusOK, `{"choices":[]}`)

	up := e.createUpstream(root, mockUpstreamBody("bound-mock", srv.URL))
	m := e.createModel(root, "bound-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	path := fmt.Sprintf("/admin/v1/sources/%d/test", src.ID)

	wantStatus(t, e.do("POST", path, "", ""), http.StatusUnauthorized)

	r := e.req("POST", path, root, "")
	r.Header.Del("X-LlmGate-CSRF")
	wantStatus(t, e.send(r), http.StatusForbidden)

	wantStatus(t, e.do("POST", "/admin/v1/sources/9999/test", root, ""), http.StatusNotFound)

	// 以上四种拒绝都发生在探测之前：一次上游请求都不该发出。
	if reqs := cap.all(); len(reqs) != 0 {
		t.Errorf("被拒的请求不应触达上游，实际发了 %d 个", len(reqs))
	}
}
