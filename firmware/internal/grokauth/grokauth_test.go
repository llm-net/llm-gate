// grokauth 的可执行验收：auth.json 记录挑选与保真、§15.1 自遮蔽、设备码流的
// 三类答复（成功 / 还没批准 / 确定性拒绝）、刷新接线（表单形态、轮换落库、
// 不轮换沿用旧 refresh token）。
//
// Provider 状态机本体（single-flight、冷却、闩、世代去重）的回归网在
// internal/codexauth/token_test.go——它测的就是共用的 agentauth 核心，这里
// 只测 grok 侧接线，不重复。夹具全部是假凭据（仓库凭据纪律）。
package grokauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

const (
	fakeAccess  = "fake-grok-access-token"
	fakeRefresh = "fake-grok-refresh-token"
	fakeEmail   = "admin@example.invalid"
	fakeUserID  = "user-fake-1234"
)

// fakeJWT 造一个不验签场景够用的 JWT（header.payload.signature，签名是假的）。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

// fullAuthJSON 造一份官方形态的 ~/.grok/auth.json：OAuth 记录 + API密钥 记录 +
// 记录内一个本包不认识的键（保真断言用）。
func fullAuthJSON() string {
	return fmt.Sprintf(`{
	  %q: {
	    "key": %q,
	    "refresh_token": %q,
	    "auth_mode": "oidc",
	    "user_id": %q,
	    "email": %q,
	    "expires_at": "2026-08-12T03:04:05Z",
	    "create_time": "2026-08-11T00:00:00Z",
	    "oidc_issuer": "https://auth.x.ai",
	    "oidc_client_id": %q,
	    "coding_data_retention_opt_out": true
	  },
	  "xai::api_key": { "key": "xai-fake-api-key", "auth_mode": "api_key" }
	}`, scopeKeyFor(DefaultIssuer), fakeAccess, fakeRefresh, fakeUserID, fakeEmail, ClientID)
}

func TestParseAuthJSONPicksOAuthEntry(t *testing.T) {
	a, err := ParseAuthJSON(fullAuthJSON())
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	if a.AccessToken() != fakeAccess || a.refreshToken != fakeRefresh {
		t.Fatalf("令牌没解出来")
	}
	if a.Account() != fakeEmail {
		t.Fatalf("account = %q, 想要 email 优先 %q", a.Account(), fakeEmail)
	}
	if a.Expiry().IsZero() || a.LastRefresh().IsZero() {
		t.Fatalf("expires_at / create_time 没解出来")
	}

	// 回写保真：不认识的键原样带回，api_key 记录**不得**跟进来。
	blob, err := a.JSON()
	if err != nil {
		t.Fatalf("JSON(): %v", err)
	}
	if strings.Contains(blob, "xai-fake-api-key") || strings.Contains(blob, apiKeyScopeKey) {
		t.Fatalf("api_key 记录被顺手收走了: %s", blob)
	}
	for _, want := range []string{"coding_data_retention_opt_out", "oidc_client_id", fakeUserID} {
		if !strings.Contains(blob, want) {
			t.Fatalf("回写丢了 %q: %s", want, blob)
		}
	}
	// round-trip 再解一遍还是同一份。
	again, err := ParseAuthJSON(blob)
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if again.AccessToken() != fakeAccess || again.Account() != fakeEmail {
		t.Fatalf("round-trip 丢字段")
	}
}

func TestParseAuthJSONShapes(t *testing.T) {
	// 裸记录（人工单拎那一条）也认。
	bare := fmt.Sprintf(`{"key":%q,"refresh_token":%q,"email":%q}`, fakeAccess, fakeRefresh, fakeEmail)
	if a, err := ParseAuthJSON(bare); err != nil || a.AccessToken() != fakeAccess {
		t.Fatalf("裸记录: %v", err)
	}
	// 旧版 relay 键也认。
	legacy := fmt.Sprintf(`{%q:{"key":%q,"refresh_token":%q}}`, legacyScopeKey, fakeAccess, fakeRefresh)
	if a, err := ParseAuthJSON(legacy); err != nil || a.scopeKey != legacyScopeKey {
		t.Fatalf("legacy 键: %v", err)
	}
	// 只有 API密钥 登录：拒绝，且话说到点子上。
	apiOnly := `{"xai::api_key":{"key":"xai-fake","auth_mode":"api_key"}}`
	if _, err := ParseAuthJSON(apiOnly); err == nil || !strings.Contains(err.Error(), "API密钥") {
		t.Fatalf("api-key-only 该拒绝并指明原因, got %v", err)
	}
	// 缺 refresh_token：拒绝。
	noRefresh := fmt.Sprintf(`{%q:{"key":%q}}`, scopeKeyFor(DefaultIssuer), fakeAccess)
	if _, err := ParseAuthJSON(noRefresh); err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("缺 refresh_token 该拒绝, got %v", err)
	}
	// 空与非 JSON。
	if _, err := ParseAuthJSON("  "); err == nil {
		t.Fatal("空串该拒绝")
	}
	if _, err := ParseAuthJSON("not json"); err == nil {
		t.Fatal("非 JSON 该拒绝")
	}
}

// TestAuthMasksCredentials：%v / %+v / %#v / slog / json.Marshal 五条路都不得
// 吐出令牌（§15.1）。%#v 单独在列因为它走 GoStringer 不走 Stringer，且指针与
// 解引用值都要挡。DeviceLogin 的 device_code 同理。
func TestAuthMasksCredentials(t *testing.T) {
	a, err := ParseAuthJSON(fullAuthJSON())
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	enc, _ := json.Marshal(struct{ A Auth }{*a})
	for name, s := range map[string]string{
		"%v":         fmt.Sprintf("%v", a),
		"%+v":        fmt.Sprintf("%+v", *a),
		"%#v":        fmt.Sprintf("%#v", *a),
		"%#v指针":      fmt.Sprintf("%#v", a),
		"slog":       a.LogValue().String(),
		"jsonStruct": string(enc),
	} {
		if strings.Contains(s, fakeAccess) || strings.Contains(s, fakeRefresh) {
			t.Errorf("%s 泄露令牌: %s", name, s)
		}
	}
	dl := DeviceLogin{UserCode: "FAKE-CODE", deviceCode: "fake-device-code-secret"}
	for name, s := range map[string]string{
		"%v":   fmt.Sprintf("%v", dl),
		"%+v":  fmt.Sprintf("%+v", dl),
		"%#v":  fmt.Sprintf("%#v", dl),
		"slog": dl.LogValue().String(),
	} {
		if strings.Contains(s, "fake-device-code-secret") {
			t.Errorf("DeviceLogin %s 泄露 device_code: %s", name, s)
		}
	}
	var _ slog.LogValuer = Auth{}
	var _ fmt.GoStringer = Auth{}
	var _ slog.LogValuer = DeviceLogin{}
	var _ fmt.GoStringer = DeviceLogin{}
}

// stubIssuer 是假 auth.x.ai：设备码端点 + 令牌端点，可编程应答。
type stubIssuer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	tokenForms []url.Values
	// tokenRespond 决定令牌端点第 n 次调用的应答（从 1 起）。
	tokenRespond func(n int, w http.ResponseWriter)
	deviceCalls  int
}

func newStubIssuer(t *testing.T) *stubIssuer {
	t.Helper()
	si := &stubIssuer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+deviceCodePath, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.PostForm.Get("client_id"); got != ClientID {
			t.Errorf("device/code client_id = %q", got)
		}
		if got := r.PostForm.Get("scope"); got != Scope {
			t.Errorf("device/code scope = %q", got)
		}
		si.mu.Lock()
		si.deviceCalls++
		si.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"device_code":"fake-device-code-secret","user_code":"FAKE-1234",
		  "verification_uri":"https://accounts.x.ai/activate",
		  "verification_uri_complete":"https://accounts.x.ai/activate?user_code=FAKE-1234",
		  "expires_in":600,"interval":5}`)
	})
	mux.HandleFunc("POST "+tokenPath, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		si.mu.Lock()
		si.tokenForms = append(si.tokenForms, r.PostForm)
		n := len(si.tokenForms)
		respond := si.tokenRespond
		si.mu.Unlock()
		respond(n, w)
	})
	si.srv = httptest.NewServer(mux)
	t.Cleanup(si.srv.Close)
	return si
}

func (si *stubIssuer) lastForm() url.Values {
	si.mu.Lock()
	defer si.mu.Unlock()
	if len(si.tokenForms) == 0 {
		return nil
	}
	return si.tokenForms[len(si.tokenForms)-1]
}

func writeGrokTokens(w http.ResponseWriter, access, refresh, id string, expiresIn int64) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":%q,"token_type":"Bearer","expires_in":%d}`,
		access, refresh, id, expiresIn)
}

func writeOAuthErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

// TestDeviceLoginFlow：起会话 → 还没批准 → slow_down → 批准换出句柄。
// 断言换码请求形态（RFC 8628 三参）与身份提取（id_token 的 email/sub）。
func TestDeviceLoginFlow(t *testing.T) {
	si := newStubIssuer(t)
	si.tokenRespond = func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			writeOAuthErr(w, http.StatusBadRequest, "authorization_pending")
		case 2:
			writeOAuthErr(w, http.StatusBadRequest, "slow_down")
		default:
			id := fakeJWT(t, map[string]any{"sub": fakeUserID, "email": fakeEmail})
			writeGrokTokens(w, fakeAccess, fakeRefresh, id, 3600)
		}
	}
	c := NewClient(si.srv.URL)
	login, err := c.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if login.UserCode != "FAKE-1234" || login.VerificationURI == "" || login.deviceCode == "" {
		t.Fatalf("设备码字段不全: %+v", login.UserCode)
	}
	if _, err := c.CompleteDeviceLogin(context.Background(), login); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("第一次该是还没批准, got %v", err)
	}
	if _, err := c.CompleteDeviceLogin(context.Background(), login); !errors.Is(err, ErrSlowDown) {
		t.Fatalf("第二次该是 slow_down, got %v", err)
	}
	auth, err := c.CompleteDeviceLogin(context.Background(), login)
	if err != nil {
		t.Fatalf("第三次该成功: %v", err)
	}
	form := si.lastForm()
	if form.Get("grant_type") != deviceGrantType || form.Get("device_code") != "fake-device-code-secret" ||
		form.Get("client_id") != ClientID {
		t.Fatalf("换码表单形态不对: %v", form)
	}
	if auth.AccessToken() != fakeAccess || auth.Account() != fakeEmail {
		t.Fatalf("句柄内容不对: account=%q", auth.Account())
	}
	// 合成的句柄要能 round-trip，且带上规范键与身份字段。
	blob, err := auth.JSON()
	if err != nil {
		t.Fatalf("JSON(): %v", err)
	}
	if !strings.Contains(blob, scopeKeyFor(si.srv.URL)) || !strings.Contains(blob, fakeUserID) {
		t.Fatalf("合成句柄形态不对: %s", blob)
	}
	if _, err := ParseAuthJSON(blob); err != nil {
		t.Fatalf("合成句柄解不回来: %v", err)
	}
}

// TestDeviceLoginRejected：access_denied 是确定性拒绝（会话该被调用方清掉）；
// 没有 refresh_token 的应答也要当场拦下。
func TestDeviceLoginRejected(t *testing.T) {
	si := newStubIssuer(t)
	si.tokenRespond = func(n int, w http.ResponseWriter) {
		writeOAuthErr(w, http.StatusBadRequest, "access_denied")
	}
	c := NewClient(si.srv.URL)
	login, err := c.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	_, err = c.CompleteDeviceLogin(context.Background(), login)
	if !agentauth.Deterministic(err) {
		t.Fatalf("access_denied 该是确定性拒绝, got %v", err)
	}

	si.tokenRespond = func(n int, w http.ResponseWriter) {
		writeGrokTokens(w, fakeAccess, "", "", 3600)
	}
	if _, err := c.CompleteDeviceLogin(context.Background(), login); err == nil ||
		!strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("缺 refresh_token 该拦下, got %v", err)
	}

	// 过期会话不打网络。
	dead := &DeviceLogin{ExpiresAt: time.Now().Add(-time.Minute)}
	if _, err := c.CompleteDeviceLogin(context.Background(), dead); err == nil ||
		!strings.Contains(err.Error(), "超时") {
		t.Fatalf("过期会话该本地拒绝, got %v", err)
	}
}

// TestRefreshWiring：经 agentauth.Provider 走一整圈刷新，断言 grok 侧接线——
// 表单形态（grant_type/refresh_token/client_id、**没有 scope**，同官方 CLI 的
// refresh_tokens_once）、轮换落库（OnRotate 拿到的新句柄带新 refresh token）、
// 应答不带 refresh_token 时沿用旧的。
func TestRefreshWiring(t *testing.T) {
	si := newStubIssuer(t)
	si.tokenRespond = func(n int, w http.ResponseWriter) {
		switch n {
		case 1: // 轮换
			writeGrokTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
		default: // 不轮换
			writeGrokTokens(w, "fake-access-gen3", "", "", 3600)
		}
	}
	c := NewClient(si.srv.URL)
	auth, err := ParseAuthJSON(fullAuthJSON())
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	var rotated []string
	p := NewProvider(c, auth, agentauth.ProviderOptions{
		OnRotate: func(_ context.Context, blob string) error {
			rotated = append(rotated, blob)
			return nil
		},
	})
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("第一次刷新: %v", err)
	}
	form := si.lastForm()
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != fakeRefresh ||
		form.Get("client_id") != ClientID {
		t.Fatalf("刷新表单形态不对: %v", form)
	}
	if got := form.Get("scope"); got != "" {
		t.Fatalf("刷新不该带 scope（官方 CLI 同形）, got %q", got)
	}
	if len(rotated) != 1 {
		t.Fatalf("OnRotate 次数 = %d", len(rotated))
	}
	gen2, err := ParseAuthJSON(rotated[0])
	if err != nil {
		t.Fatalf("落库句柄解不开: %v", err)
	}
	if gen2.AccessToken() != "fake-access-gen2" || gen2.refreshToken != "fake-refresh-gen2" {
		t.Fatalf("落库的不是新一代")
	}
	if gen2.Account() != fakeEmail {
		t.Fatalf("刷新不该丢账号身份: %q", gen2.Account())
	}

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("第二次刷新: %v", err)
	}
	gen3, err := ParseAuthJSON(rotated[1])
	if err != nil {
		t.Fatalf("第二代落库句柄解不开: %v", err)
	}
	if gen3.refreshToken != "fake-refresh-gen2" {
		t.Fatalf("应答不带 refresh_token 时该沿用旧的, got %q", gen3.refreshToken)
	}
}

// TestRefreshDeterministicLatch：invalid_grant 落闩（ErrAuthExpired）+
// OnAuthExpired 通知——grok 侧接线复用 agentauth 的判定，这里只验一圈。
func TestRefreshDeterministicLatch(t *testing.T) {
	si := newStubIssuer(t)
	si.tokenRespond = func(n int, w http.ResponseWriter) {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant")
	}
	c := NewClient(si.srv.URL)
	auth, err := ParseAuthJSON(fullAuthJSON())
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	var expired int
	p := NewProvider(c, auth, agentauth.ProviderOptions{
		OnAuthExpired: func(context.Context, error) { expired++ },
	})
	if err := p.Refresh(context.Background()); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("invalid_grant 该翻成 ErrAuthExpired, got %v", err)
	}
	if expired != 1 || !p.AuthExpired() {
		t.Fatalf("闩没落下: expired=%d", expired)
	}
}

// TestGrokDeviceFlowProbe 是**出网**探针（缺省 skip）：验证 auth.x.ai 的设备码
// 端点还在、还收本 client_id（docs/firmware-agents-grok.md 的登录形态结论）。
// 哪天 xAI 撤掉设备码流，这条会失败，届时回文档改结论。
//
// 跑法：GROK_LOGIN_PROBE=1 go test ./internal/grokauth -run Probe -v
// 只起一次会话、不批准、不消费任何令牌。
func TestGrokDeviceFlowProbe(t *testing.T) {
	if os.Getenv("GROK_LOGIN_PROBE") != "1" {
		t.Skip("出网探针，GROK_LOGIN_PROBE=1 才跑（全仓测试必须离线可跑）")
	}
	c := NewClient("")
	login, err := c.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("auth.x.ai 设备码端点不可用（形态变了？回文档改结论）: %v", err)
	}
	t.Logf("device flow OK: user_code=%s verification_uri=%s expires=%s",
		login.UserCode, login.VerificationURI, time.Until(login.ExpiresAt))
}
