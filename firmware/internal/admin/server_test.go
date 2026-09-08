// server_test.go 是管理监听器的可执行验收：会话认证边界（/admin/v1/* 除登录
// 外一律要会话）、CSRF 三重简版的两道显式防线（X-LlmGate-CSRF 头 + JSON
// Content-Type）、Key 明文的两条外流通道、禁用/删除 Key 后数据面摘要查找立即
// 失效，以及管理面日志对口令/会话令牌/Key 明文的 §15.1 脱敏。
package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 直读审计表用（与板上走查的 sqlite3 同视角）

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/auth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/netconfig"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/sysinfo"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	// rootPass 是用例自己设的设备口令（不用出厂默认值：那条路径由
	// TestDefaultPasswordCanLogIn 单独验）。
	rootPass = "root-pass-1234"
	// testListenPort 是测试环境的明文监听端口（不真监听，只进配置）：取个
	// 非缺省值，接入读数里回的必须是它。
	testListenPort = 8123
	// testHardwareModel 是管理 API 的板卡型号注入值；测试环境不读宿主机
	// /sys，响应必须逐字回这串。
	testHardwareModel = "test-board"
)

// env 是一次装配好的管理面测试环境：真实 store 与真实 auth，日志写入 buf
// 供脱敏断言（debug 级别：脱敏在最宽松级别也必须成立）。
//
// **库里出厂是空的**：出厂默认口令由装配层显式播种（生产在 runGatewayd，
// 用例在 rootSession / TestDefaultPasswordCanLogIn），env 自己不播。
type env struct {
	t   *testing.T
	h   http.Handler
	st  *store.Store
	dir string
	buf *bytes.Buffer
	// meter 是与管理面共用的那台计量器（iteration-9）：用量端点的用例往它
	// 里 Record 样本，再从端点把账读回来。时区固定 UTC，区间边界才可复现。
	meter *usage.Meter
	// srv 留给需要在装配后注入可选能力的用例（如官网数据文件来源）。h 是它的
	// Handler：路由绑的是方法值，注入后即时生效，不必重建 handler。
	srv *admin.Server
	// as 是与管理面共用的登录核心；用例靠它走生产那条播种路径
	// （auth.Service.EnsureDefaultPassword），验「新设备开机就能登进来」。
	as *auth.Service
	// egress 是与管理面共用的出站代理策略（egress_test.go 用它读生效快照）。
	egress *egress.Manager
}

func newEnv(t *testing.T) *env { return newEnvOpt(t, envOpt{meter: true}) }

// newEnvNoMeter 是「计量未装配」的环境（gatewayd 没接计量器时的形态）：用量
// 端点答 503，密钥列表整块不给 spend——两处都必须是「说没有」而不是回 0。
func newEnvNoMeter(t *testing.T) *env { return newEnvOpt(t, envOpt{}) }

// envOpt 选装 env 是否接计量器。
type envOpt struct {
	meter bool
}

func newEnvOpt(t *testing.T, o envOpt) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	buf := &bytes.Buffer{}
	logger := logging.New(buf, slog.LevelDebug)
	as, err := auth.New(context.Background(), st, dir, logger)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	// 管理面不再自持监听器（单端口决策）：cfg 喂上游探测的超时缺省，外加两个
	// 监听端口——接入读数要如实回显它们。端口刻意取非缺省值，好让测试分得清
	// 「真的读了配置」与「恰好撞上 80/443」。
	cfg := &config.Config{Listen: fmt.Sprintf("0.0.0.0:%d", testListenPort)}
	// 采集器/记录器与 gatewayd 同构接线；记录器不 Run——历史端点对「开着但
	// 还没有数据」的响应形状也因此被测到。
	sysCol := sysinfo.NewCollector()
	rec := sysinfo.NewRecorder(sysCol, dir, config.DefaultHistoryDays, logger)
	// 网络配置管理器注入「环境不支持」Runner：测试环境不真跑 nmcli，
	// 网络端点的降级形状（supported=false / network_unsupported）顺带被测到。
	netMgr := netconfig.NewManager(logger, nil, netconfig.WithRunner(
		func(ctx context.Context, args ...string) (string, string, error) {
			return "", "", exec.ErrNotFound
		}))
	// 出站代理策略与 gatewayd 同构接线：设置存 st（密封走设备密钥），显式测试的目标
	// 指向不存在的本地端口——用例只验策略、审计与脱敏，不出网。
	eg := egress.NewManager(egress.Options{Settings: st, Logger: logger, TestTarget: "https://127.0.0.1:9/"})
	if err := eg.Load(context.Background()); err != nil {
		t.Fatalf("egress.Load: %v", err)
	}
	srv := admin.New(cfg, logger, st, as, sysCol, rec, netMgr, testHardwareModel, eg)
	e := &env{t: t, st: st, dir: dir, buf: buf, srv: srv, as: as, egress: eg}
	if o.meter {
		// 用量读数出口与 gatewayd 同款注入（探针 nil：管理面这边不跑懒对账）。
		e.meter = usage.NewMeter(st, config.DefaultUsageDays, time.UTC, nil, logger)
		srv.SetUsageReader(e.meter)
	}
	e.h = srv.Handler()
	return e
}

// req 构造请求；变更方法自动携带 X-LlmGate-CSRF: 1 与 JSON Content-Type
// （CSRF 反向用例在返回的请求上改写对应头）。
func (e *env) req(method, path, cookie, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if method != http.MethodGet {
		r.Header.Set("X-LlmGate-CSRF", "1")
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: admin.SessionCookieName, Value: cookie})
	}
	return r
}

func (e *env) send(r *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec.Result()
}

func (e *env) do(method, path, cookie, body string) *http.Response {
	return e.send(e.req(method, path, cookie, body))
}

// rootSession 把设备口令设成 rootPass 并登录，返回会话 Cookie 值。
//
// 直接落库而不走播种：用例要一个自己说了算的口令，出厂默认那条路径由
// TestDefaultPasswordCanLogIn 单独验。
func (e *env) rootSession() string {
	e.t.Helper()
	hash, err := auth.HashPassword(rootPass)
	if err != nil {
		e.t.Fatalf("HashPassword: %v", err)
	}
	if err := e.st.SetSetting(context.Background(), store.SettingAdminPasswordHash, hash); err != nil {
		e.t.Fatalf("设置设备口令: %v", err)
	}
	return e.login(rootPass)
}

// login 登录并返回会话 Cookie 值（期望成功）。
func (e *env) login(password string) string {
	e.t.Helper()
	resp := e.do("POST", "/admin/v1/login", "", `{"password":"`+password+`"}`)
	if resp.StatusCode != http.StatusNoContent {
		e.t.Fatalf("login 状态 = %d，期望 204；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	c := sessionCookie(resp)
	if c == "" {
		e.t.Fatal("login 成功但未下发会话 Cookie")
	}
	return c
}

// createKey 经 API 签发一把 Key（期望 201），返回元数据与明文。
func (e *env) createKey(cookie, label string) (keyDTO, string) {
	e.t.Helper()
	resp := e.do("POST", "/admin/v1/keys", cookie, fmt.Sprintf(`{"label":%q}`, label))
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("创建 Key 状态 = %d，期望 201；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var out struct {
		Key       keyDTO `json:"key"`
		Plaintext string `json:"plaintext"`
	}
	decodeInto(e.t, resp, &out)
	return out.Key, out.Plaintext
}

type keyDTO struct {
	ID                 int64  `json:"id"`
	Label              string `json:"label"`
	DisplayPrefix      string `json:"display_prefix"`
	DisplayLast4       string `json:"display_last4"`
	Disabled           bool   `json:"disabled"`
	PlaintextAvailable bool   `json:"plaintext_available"`
	// 密钥级限额（null = 不限）。
	BudgetDayMicro        *int64 `json:"budget_day_micro"`
	BudgetWeekMicro       *int64 `json:"budget_week_micro"`
	BudgetMonthMicro      *int64 `json:"budget_month_micro"`
	RPMLimit              *int64 `json:"rpm_limit"`
	MeteredAllowanceMicro int64  `json:"metered_allowance_micro"`
	// Spend 是当前自然日/周/月的已消费额，与上面三条预算成对读。
	// 整块为 nil = 计量未装配。
	Spend *struct {
		DayMicro   int64 `json:"day_micro"`
		WeekMicro  int64 `json:"week_micro"`
		MonthMicro int64 `json:"month_micro"`
	} `json:"spend"`
}

func sessionCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookieName && c.MaxAge >= 0 && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return string(b)
}

func decodeInto(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	raw := readAll(t, resp)
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		t.Fatalf("解析响应 %q: %v", raw, err)
	}
}

// errCode 断言响应是统一 JSON 错误体 {"error":{"code","message"}} 并返回 code。
func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeInto(t, resp, &body)
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatal("响应不是统一错误体（缺 error.code/error.message）")
	}
	return body.Error.Code
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("状态 = %d，期望 %d；body: %s", resp.StatusCode, want, readAll(t, resp))
	}
}

func digestOf(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ---- 验收用例 ----

// TestDefaultPasswordCanLogIn：**新设备开机就能登进来**。出厂默认口令由装配层
// 播种（生产是 runGatewayd 里那一句 EnsureDefaultPassword），管理台第一屏就是
// 登录页，中间没有其他初始化页面。登录成功回 204——设备没有「我是谁」可答。
func TestDefaultPasswordCanLogIn(t *testing.T) {
	e := newEnv(t)
	if err := e.as.EnsureDefaultPassword(context.Background()); err != nil {
		t.Fatalf("EnsureDefaultPassword: %v", err)
	}

	resp := e.do("POST", "/admin/v1/login", "", `{"password":"`+auth.DefaultPassword+`"}`)
	wantStatus(t, resp, http.StatusNoContent)
	cookie := sessionCookie(resp)
	if cookie == "" {
		t.Fatal("登录成功但未下发会话 Cookie")
	}
	if body := readAll(t, resp); strings.TrimSpace(body) != "" {
		t.Errorf("login 不该回 body，得 %q", body)
	}
	// 会话真能用：探针 204，管理页读数照常。
	wantStatus(t, e.do("GET", "/admin/v1/session", cookie, ""), http.StatusNoContent)
	wantStatus(t, e.do("GET", "/admin/v1/keys", cookie, ""), http.StatusOK)
	// 错口令仍是 401 invalid_credentials（不区分原因）。
	bad := e.do("POST", "/admin/v1/login", "", `{"password":"wrong-password"}`)
	wantStatus(t, bad, http.StatusUnauthorized)
	if got := errCode(t, bad); got != "invalid_credentials" {
		t.Errorf("错口令 error.code = %q，期望 invalid_credentials", got)
	}
}

// TestSessionCookieSecureFollowsRequestTLS：HTTP/HTTPS 共用 handler，但只有请求
// 实际经 TLS 到达设备时 Cookie 才带 Secure；伪造转发头不能改变结果。
func TestSessionCookieSecureFollowsRequestTLS(t *testing.T) {
	e := newEnv(t)
	e.rootSession()
	loginBody := `{"password":"` + rootPass + `"}`

	httpReq := e.req(http.MethodPost, "/admin/v1/login", "", loginBody)
	httpReq.Header.Set("X-Forwarded-Proto", "https")
	httpResp := e.send(httpReq)
	wantStatus(t, httpResp, http.StatusNoContent)
	if c := sessionCookieByName(t, httpResp); c.Secure {
		t.Error("明文请求不应因 X-Forwarded-Proto 获得 Secure Cookie")
	}

	httpsReq := e.req(http.MethodPost, "/admin/v1/login", "", loginBody)
	httpsReq.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	httpsResp := e.send(httpsReq)
	wantStatus(t, httpsResp, http.StatusNoContent)
	if c := sessionCookieByName(t, httpsResp); !c.Secure {
		t.Error("TLS 请求下发的会话 Cookie 缺 Secure")
	}
}

func sessionCookieByName(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookieName {
			return c
		}
	}
	t.Fatal("响应未下发管理会话 Cookie")
	return nil
}

// TestRetiredEndpointsAreGone：旧初始化/设备绑定端点，以及随「用户」概念一起
// 退场的用户管理与 /me 自助子树，都已整体删除。
//
// 匿名调用得到的是 **401 而不是 404**：/admin/v1/ 下的未知路径先过会话中间件
// 再落 404，不给未认证方一张"这台设备有哪些端点"的探测面（withSession 的既有
// 纪律，删端点不该在它上面开一个洞）。带会话时才是统一 JSON 404。
func TestRetiredEndpointsAreGone(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/admin/v1/setup", ""},
		{"POST", "/admin/v1/setup", `{"code":"AAAA-AAAA-AAAA","name":"x","password":"long-enough-pass"}`},
		{"GET", "/admin/v1/cloud-gate", ""},
		{"POST", "/admin/v1/cloud-gate/refresh", `{}`},
		{"GET", "/admin/v1/onboarding", ""},
		{"POST", "/admin/v1/onboarding/enroll", `{"serial_no":"AAAA-BBBB","setup_code":"AAAA-AAAA-AAAA"}`},
		// 0019 起「用户」整个概念退场。
		{"GET", "/admin/v1/users", ""},
		{"POST", "/admin/v1/users", `{"name":"x","role":"admin","password":"long-enough-pass"}`},
		{"PATCH", "/admin/v1/users/1", `{"disabled":true}`},
		{"DELETE", "/admin/v1/users/1", ""},
		{"POST", "/admin/v1/users/1/password", `{"password":"long-enough-pass"}`},
		{"POST", "/admin/v1/users/1/reserve", `{"delta_micro":1}`},
		{"GET", "/admin/v1/me", ""},
		{"PATCH", "/admin/v1/me", `{"nickname":"x"}`},
		{"POST", "/admin/v1/me/password", `{"old_password":"a","new_password":"b"}`},
		{"GET", "/admin/v1/me/keys", ""},
		{"POST", "/admin/v1/me/keys", `{"label":"x"}`},
		{"DELETE", "/admin/v1/me/keys/1", ""},
		{"GET", "/admin/v1/me/usage", ""},
	} {
		resp := e.do(tc.method, tc.path, "", tc.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("匿名 %s %s = %d，期望 401（不给未认证方探测面）", tc.method, tc.path, resp.StatusCode)
		}
		resp = e.do(tc.method, tc.path, root, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("带会话 %s %s = %d，期望 404", tc.method, tc.path, resp.StatusCode)
		} else if got := errCode(t, resp); got != "not_found" {
			t.Errorf("%s %s error.code = %q，期望 not_found", tc.method, tc.path, got)
		}
	}
}

// TestUnauthenticatedRejected：无会话（或伪造会话）访问业务接口一律 401
// 统一错误体；未知 /admin/v1 路径同样先认证再 404。
func TestUnauthenticatedRejected(t *testing.T) {
	e := newEnv(t)
	e.rootSession()

	cases := []struct{ method, path string }{
		{"GET", "/admin/v1/session"},
		{"GET", "/admin/v1/keys"},
		{"POST", "/admin/v1/keys"},
		{"PATCH", "/admin/v1/keys/1"},
		{"DELETE", "/admin/v1/keys/1"},
		{"POST", "/admin/v1/keys/1/plaintext"},
		{"POST", "/admin/v1/password"},
		{"POST", "/admin/v1/logout"},
		{"GET", "/admin/v1/usage"},
		{"GET", "/admin/v1/endpoints"},
		{"GET", "/admin/v1/apps"},
		{"GET", "/admin/v1/upstreams"},
		{"GET", "/admin/v1/models"},
		{"GET", "/admin/v1/system"},
		{"GET", "/admin/v1/no-such-endpoint"},
	}
	for _, c := range cases {
		for _, cookie := range []string{"", "forged-session-token"} {
			resp := e.do(c.method, c.path, cookie, "{}")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s（cookie=%q）状态 = %d，期望 401", c.method, c.path, cookie, resp.StatusCode)
			}
			if got := errCode(t, resp); got != "unauthorized" {
				t.Fatalf("%s %s error.code = %q，期望 unauthorized", c.method, c.path, got)
			}
		}
	}
}

// TestAuthedUnknownPathIs404：带会话访问未知管理路径得到统一 JSON 404。
func TestAuthedUnknownPathIs404(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	resp := e.do("GET", "/admin/v1/no-such-endpoint", cookie, "")
	wantStatus(t, resp, http.StatusNotFound)
	if got := errCode(t, resp); got != "not_found" {
		t.Errorf("error.code = %q，期望 not_found", got)
	}
}

// TestCSRFGuards：变更请求缺 X-LlmGate-CSRF 头 → 403，Content-Type 非 JSON → 415；
// 对免会话端点（login）与业务端点同等生效。
func TestCSRFGuards(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	// 缺自定义头（login，无会话也拦）。
	r := e.req("POST", "/admin/v1/login", "", `{"password":"b"}`)
	r.Header.Del("X-LlmGate-CSRF")
	resp := e.send(r)
	wantStatus(t, resp, http.StatusForbidden)
	if got := errCode(t, resp); got != "csrf_required" {
		t.Errorf("error.code = %q，期望 csrf_required", got)
	}

	// Content-Type 非 JSON。
	r = e.req("POST", "/admin/v1/login", "", `{"password":"b"}`)
	r.Header.Set("Content-Type", "text/plain")
	resp = e.send(r)
	wantStatus(t, resp, http.StatusUnsupportedMediaType)
	if got := errCode(t, resp); got != "unsupported_media_type" {
		t.Errorf("error.code = %q，期望 unsupported_media_type", got)
	}

	// 带会话的业务变更同样强制。
	r = e.req("PATCH", "/admin/v1/keys/1", cookie, `{"disabled":true}`)
	r.Header.Del("X-LlmGate-CSRF")
	resp = e.send(r)
	wantStatus(t, resp, http.StatusForbidden)

	// GET 不受 CSRF 头约束。
	wantStatus(t, e.do("GET", "/admin/v1/keys", cookie, ""), http.StatusOK)
}

// TestSystemStatus：管理员可读设备状态快照；响应至少携带采样时间戳。
// 各读数节是否出现取决于运行平台（darwin 开发机没有 /proc，只剩时间戳），
// 数值口径由 internal/sysinfo 的夹具测试钉死，这里只验端点契约。
func TestSystemStatus(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	resp := e.do("GET", "/admin/v1/system", root, "")
	wantStatus(t, resp, http.StatusOK)
	var body struct {
		System struct {
			SampledAt time.Time `json:"sampled_at"`
			WindowMS  int64     `json:"window_ms"`
		} `json:"system"`
		HardwareModel string `json:"hardwareModel"`
	}
	decodeInto(t, resp, &body)
	if body.System.SampledAt.IsZero() {
		t.Error("sampled_at 缺失")
	}
	if body.System.WindowMS < 0 {
		t.Errorf("window_ms = %d", body.System.WindowMS)
	}
	if body.HardwareModel != testHardwareModel {
		t.Errorf("hardwareModel = %q，期望 %q", body.HardwareModel, testHardwareModel)
	}
}

// TestSystemHistory：历史端点契约——range 校验、缺省 6h、响应形状（记录器
// 未运行时 enabled 但无数据点）。序列内容口径由 internal/sysinfo 钉死。
func TestSystemHistory(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	resp := e.do("GET", "/admin/v1/system/history?range=7d", root, "")
	wantStatus(t, resp, http.StatusOK)
	var body struct {
		History struct {
			Enabled         bool   `json:"enabled"`
			Range           string `json:"range"`
			IntervalSeconds int    `json:"interval_seconds"`
			RetentionDays   int    `json:"retention_days"`
		} `json:"history"`
	}
	decodeInto(t, resp, &body)
	if !body.History.Enabled || body.History.Range != "7d" ||
		body.History.IntervalSeconds != 600 || body.History.RetentionDays != config.DefaultHistoryDays {
		t.Errorf("history = %+v", body.History)
	}

	// 缺省 range = 6h；空序列必须是 []（不是 null）——前端按数组遍历。
	resp = e.do("GET", "/admin/v1/system/history", root, "")
	wantStatus(t, resp, http.StatusOK)
	var rawBody struct {
		History struct {
			Range           string          `json:"range"`
			IntervalSeconds int             `json:"interval_seconds"`
			Points          json.RawMessage `json:"points"`
		} `json:"history"`
	}
	decodeInto(t, resp, &rawBody)
	if rawBody.History.Range != "6h" || rawBody.History.IntervalSeconds != 60 {
		t.Errorf("缺省 range = %+v", rawBody.History)
	}
	if string(rawBody.History.Points) != "[]" {
		t.Errorf("空序列 points = %s，期望 []", rawBody.History.Points)
	}

	resp = e.do("GET", "/admin/v1/system/history?range=1y", root, "")
	wantStatus(t, resp, http.StatusBadRequest)
}

// TestKeyLifecycle：签发 → 列表 → 禁用 → 删除，全走同一组端点（0019 起没有
// 「属主」这一维，管理端与自助端合并成一组）。
func TestKeyLifecycle(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	k, plaintext := e.createKey(cookie, "我的笔记本")
	if !strings.HasPrefix(plaintext, "sk_") {
		t.Fatalf("明文前缀 = %q，期望 sk_ 开头", plaintext)
	}
	if len(plaintext) != len("sk_")+43 {
		t.Errorf("明文长度 = %d，期望 %d（sk_ + 43 位 base62）", len(plaintext), len("sk_")+43)
	}
	if k.DisplayPrefix != plaintext[:12] || k.DisplayLast4 != plaintext[len(plaintext)-4:] {
		t.Errorf("展示串与明文不对应: %+v", k)
	}
	if !k.PlaintextAvailable {
		t.Error("创建响应 plaintext_available 应为 true")
	}
	// 数据面摘要点查立即命中（withAuth 走的就是这条路）。
	a, err := e.st.LookupKeyByDigest(context.Background(), digestOf(plaintext))
	if err != nil || a.KeyDisabled {
		t.Fatalf("LookupKeyByDigest = %+v (err=%v)", a, err)
	}

	// 列表：raw body 既不含明文也不含摘要，但带展示串与当前已用额。
	resp := e.do("GET", "/admin/v1/keys", cookie, "")
	wantStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	if strings.Contains(raw, plaintext) {
		t.Error("Key 列表响应泄露了明文")
	}
	if strings.Contains(raw, digestOf(plaintext)) {
		t.Error("Key 列表响应泄露了摘要（无展示必要）")
	}
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("解码列表: %v", err)
	}
	if len(list.Keys) != 1 || list.Keys[0].ID != k.ID || list.Keys[0].DisplayPrefix != k.DisplayPrefix {
		t.Fatalf("列表异常: %+v", list.Keys)
	}
	if list.Keys[0].Spend == nil {
		t.Error("接了计量却没给 spend（列表要能把「花了多少 / 能花多少」摆在一起）")
	}

	// 删除 → 摘要点查落空；重复删除 404。
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/keys/%d", k.ID), cookie, ""), http.StatusNoContent)
	if _, err := e.st.LookupKeyByDigest(context.Background(), digestOf(plaintext)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("已删除的 Key 摘要仍能点查到: %v", err)
	}
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/keys/%d", k.ID), cookie, ""), http.StatusNotFound)
	wantStatus(t, e.do("DELETE", "/admin/v1/keys/9999", cookie, ""), http.StatusNotFound)
}

// TestKeysListWithoutMeter：没接计量器时密钥列表整块不给 spend——必须是
// 「说没有」而不是回一排 0（回 0 会把「没接线」读成「没花钱」）。
func TestKeysListWithoutMeter(t *testing.T) {
	e := newEnvNoMeter(t)
	cookie := e.rootSession()
	e.createKey(cookie, "k")

	resp := e.do("GET", "/admin/v1/keys", cookie, "")
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	decodeInto(t, resp, &list)
	if len(list.Keys) != 1 || list.Keys[0].Spend != nil {
		t.Errorf("计量未装配时不该给 spend: %+v", list.Keys)
	}
}

// TestKeyPlaintextReveal：复制端点（POST /admin/v1/keys/{id}/plaintext）全态：
// 明文与签发一致；禁用的 Key 照样可复制（禁用挡数据面，不挡管理员看自己的
// 凭据）；不存在的 id 404；旧行（0012 之前签发、无封存明文）409
// plaintext_unavailable；列表只以 plaintext_available 布尔预告，响应体永不含明文。
func TestKeyPlaintextReveal(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	created, plaintext := e.createKey(cookie, "reveal-me")

	reveal := func() *http.Response {
		return e.do("POST", fmt.Sprintf("/admin/v1/keys/%d/plaintext", created.ID), cookie, "")
	}
	resp := reveal()
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	decodeInto(t, resp, &out)
	if out.Plaintext != plaintext {
		t.Errorf("复制回的明文与签发不一致: %q", out.Plaintext)
	}

	// 禁用后照样可复制。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", created.ID), cookie, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, reveal(), http.StatusOK)

	// 不存在的 id → 404。
	wantStatus(t, e.do("POST", "/admin/v1/keys/9999/plaintext", cookie, ""), http.StatusNotFound)

	// 模拟旧行：0012 之前签发的 Key 封存列恒空串。
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开直查连接: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE api_keys SET plaintext_sealed = '' WHERE id = ?`, created.ID); err != nil {
		t.Fatalf("清空封存明文: %v", err)
	}
	resp = reveal()
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "plaintext_unavailable" {
		t.Errorf("旧行错误码 = %q，期望 plaintext_unavailable", got)
	}

	// 列表：布尔预告可复制性，且体内永不含明文。
	resp = e.do("GET", "/admin/v1/keys", cookie, "")
	wantStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	if strings.Contains(raw, plaintext) {
		t.Error("列表响应泄露明文")
	}
	var list struct {
		Keys []keyDTO `json:"keys"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("解码列表: %v", err)
	}
	if len(list.Keys) != 1 || list.Keys[0].PlaintextAvailable {
		t.Errorf("旧行列表 plaintext_available 应为 false: %+v", list.Keys)
	}
}

// TestKeyIssueValidation：label 超长被拒；不带 label 也能签（标签可空）。
func TestKeyIssueValidation(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	long := strings.Repeat("标", 129)
	resp := e.do("POST", "/admin/v1/keys", cookie, fmt.Sprintf(`{"label":%q}`, long))
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "bad_request" {
		t.Errorf("error.code = %q，期望 bad_request", got)
	}
	wantStatus(t, e.do("POST", "/admin/v1/keys", cookie, `{}`), http.StatusCreated)
}

// TestDisableKeyDataPlane：禁用 Key 对数据面摘要查找即时生效；启用即时恢复。
func TestDisableKeyDataPlane(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	k, plaintext := e.createKey(cookie, "k")

	lookup := func() *store.KeyAuth {
		t.Helper()
		a, err := e.st.LookupKeyByDigest(context.Background(), digestOf(plaintext))
		if err != nil {
			t.Fatalf("LookupKeyByDigest: %v", err)
		}
		return a
	}
	if lookup().KeyDisabled {
		t.Fatal("新签发的 Key 不应是禁用态")
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", k.ID), cookie, `{"disabled":true}`), http.StatusOK)
	if !lookup().KeyDisabled {
		t.Error("禁用 Key 后 KeyAuth.KeyDisabled 应为 true")
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", k.ID), cookie, `{"disabled":false}`), http.StatusOK)
	if lookup().KeyDisabled {
		t.Error("启用后 KeyAuth.KeyDisabled 应回 false")
	}
}

// TestPasswordChange：改登录口令 → 验旧口令、清空全部旧会话、为当前浏览器
// 轮换新会话（无感续用）、新口令可登录、弱口令被策略拒绝。
func TestPasswordChange(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	// 第二个浏览器：改密后它必须被踢掉。
	other := e.login(rootPass)

	// 旧口令不对 → 400。
	resp := e.do("POST", "/admin/v1/password", cookie,
		`{"old_password":"wrong-password","new_password":"brand-new-pass"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "invalid_old_password" {
		t.Errorf("error.code = %q，期望 invalid_old_password", got)
	}

	// 弱新口令 → 400 password_policy。
	resp = e.do("POST", "/admin/v1/password", cookie,
		`{"old_password":"`+rootPass+`","new_password":"short"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "password_policy" {
		t.Errorf("error.code = %q，期望 password_policy", got)
	}

	// 正常改密：204 + 轮换 Cookie。
	resp = e.do("POST", "/admin/v1/password", cookie,
		`{"old_password":"`+rootPass+`","new_password":"brand-new-pass"}`)
	wantStatus(t, resp, http.StatusNoContent)
	rotated := sessionCookie(resp)
	if rotated == "" || rotated == cookie {
		t.Fatal("改密后应下发一个新的会话 Cookie")
	}
	wantStatus(t, e.do("GET", "/admin/v1/session", rotated, ""), http.StatusNoContent)
	// 旧会话（本浏览器的与另一个浏览器的）全部失效。
	wantStatus(t, e.do("GET", "/admin/v1/session", cookie, ""), http.StatusUnauthorized)
	wantStatus(t, e.do("GET", "/admin/v1/session", other, ""), http.StatusUnauthorized)
	// 旧口令登不进来，新口令可以。
	wantStatus(t, e.do("POST", "/admin/v1/login", "", `{"password":"`+rootPass+`"}`), http.StatusUnauthorized)
	e.login("brand-new-pass")
}

// TestLogout：登出删除会话（旧 Cookie 立即 401）并清除浏览器 Cookie。
func TestLogout(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	resp := e.do("POST", "/admin/v1/logout", cookie, "")
	wantStatus(t, resp, http.StatusNoContent)
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("登出响应应下发过期 Cookie 清除浏览器态")
	}
	wantStatus(t, e.do("GET", "/admin/v1/session", cookie, ""), http.StatusUnauthorized)
}

// TestAuditTrail：默认口令播种、登录失败、改密、登出与 Key 全部变更都追加进
// 审计表（entity 形如 key:N / device:password；detail 不含口令/令牌/Key 明文）。
// 直接以只读 SQL 读库验证——store 依约不提供审计读改接口。
func TestAuditTrail(t *testing.T) {
	e := newEnv(t)
	// 播种要排在设口令之前：库里已有口令之后 EnsureDefaultPassword 什么都不做。
	if err := e.as.EnsureDefaultPassword(context.Background()); err != nil {
		t.Fatalf("EnsureDefaultPassword: %v", err)
	}
	root := e.rootSession()
	// 一次失败登录 + 各类变更。
	e.do("POST", "/admin/v1/login", "", `{"password":"wrong-pass-123"}`)
	k, plaintext := e.createKey(root, "audit-key")
	doomed, _ := e.createKey(root, "doomed-key")
	wantStatus(t, e.do("POST", fmt.Sprintf("/admin/v1/keys/%d/plaintext", k.ID), root, ""), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", k.ID), root, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", k.ID), root,
		`{"budget_day_micro":1000000}`), http.StatusOK)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/keys/%d", doomed.ID), root, ""), http.StatusNoContent)
	resp := e.do("POST", "/admin/v1/password", root,
		`{"old_password":"`+rootPass+`","new_password":"new-pass-12345"}`)
	wantStatus(t, resp, http.StatusNoContent)
	rotated := sessionCookie(resp)
	wantStatus(t, e.do("POST", "/admin/v1/logout", rotated, ""), http.StatusNoContent)

	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	type auditRow struct{ event, entity, detail string }
	var got []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.event, &a.entity, &a.detail); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		got = append(got, a)
		// §15.1 延伸：审计 detail 不得含口令/Key 明文/会话令牌。
		for _, secret := range []string{rootPass, "wrong-pass-123", "new-pass-12345", plaintext, root, rotated} {
			if secret != "" && strings.Contains(a.detail, secret) {
				t.Errorf("审计 detail 泄露敏感值（event=%s）", a.event)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计行: %v", err)
	}
	want := map[string]string{ // event → 期望的 entity 前缀（空 = 不校验）
		"password.seed":     "device:password",
		"login.failure":     "",
		"login.success":     "device:password",
		"password.change":   "device:password",
		"key.create":        fmt.Sprintf("key:%d", k.ID),
		"key.reveal":        fmt.Sprintf("key:%d", k.ID),
		"key.disable":       fmt.Sprintf("key:%d", k.ID),
		"key.limits_update": fmt.Sprintf("key:%d", k.ID),
		"key.delete":        fmt.Sprintf("key:%d", doomed.ID),
		"logout":            "device:password",
	}
	for event, entityPrefix := range want {
		found := false
		for _, a := range got {
			if a.event == event && (entityPrefix == "" || strings.HasPrefix(a.entity, entityPrefix)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("审计表缺少事件 %s（entity 前缀 %q）；已有: %+v", event, entityPrefix, got)
		}
	}
}

// TestLogRedaction：管理面日志（debug 级别）不含口令、会话令牌与 Key 明文。
// 一次错口令尝试也走一遍，错误路径与成功路径同一纪律。
func TestLogRedaction(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	wantStatus(t, e.do("POST", "/admin/v1/login", "",
		`{"password":"wrong-pass-9999"}`), http.StatusUnauthorized)
	second := e.login(rootPass)
	_, plaintext := e.createKey(root, "some-key")
	wantStatus(t, e.do("POST", "/admin/v1/logout", second, ""), http.StatusNoContent)

	logs := e.buf.String()
	for what, secret := range map[string]string{
		"口令":     rootPass,
		"错误的口令":  "wrong-pass-9999",
		"会话令牌 A": root,
		"会话令牌 B": second,
		"Key 明文": plaintext,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("日志泄露%s（§15.1）", what)
		}
	}
}
