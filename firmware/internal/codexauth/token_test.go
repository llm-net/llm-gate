// token_test.go 是迭代 11 Phase 2 可执行验收的主体：single-flight 刷新、
// 新世代重新落库（OnRotate）、两类失败两种反应（决策 5）。
//
// 全部对着一个**假 OpenAI 令牌端点**跑，离线可用——全仓 go test 不出网
// （受限现场、CI、smoke 都不出网）。真实端点的探针在 codexauth_test.go，
// 门控在 CODEX_LOGIN_PROBE=1。
package codexauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenServer 是假令牌端点：记账 + 可编程应答。
type tokenServer struct {
	t     *testing.T
	srv   *httptest.Server
	calls atomic.Int64
	// forms 留下每次收到的表单，供断言请求形态。
	mu    sync.Mutex
	forms []url.Values
	// respond 决定每一次调用的应答；n 是第几次（从 1 起）。
	respond func(n int64, w http.ResponseWriter)
}

func newTokenServer(t *testing.T, respond func(n int64, w http.ResponseWriter)) *tokenServer {
	t.Helper()
	ts := &tokenServer{t: t, respond: respond}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t.Errorf("打到了意料之外的路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("请求体不是表单: %v", err)
		}
		if got := r.PostForm.Get("client_id"); got != ClientID {
			t.Errorf("client_id = %q", got)
		}
		ts.mu.Lock()
		ts.forms = append(ts.forms, r.PostForm)
		ts.mu.Unlock()
		n := ts.calls.Add(1)
		ts.respond(n, w)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) lastForm() url.Values {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.forms) == 0 {
		return nil
	}
	return ts.forms[len(ts.forms)-1]
}

// writeTokens 写一条成功应答。
func writeTokens(w http.ResponseWriter, access, refresh, id string, expiresIn int64) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":%q,"token_type":"Bearer","expires_in":%d}`,
		access, refresh, id, expiresIn)
}

// newTestProvider 造一个指向假端点的 provider；auth 的 access_token 到期时刻
// 由 exp 决定（零值 = 不带 exp claim = 到期时刻未知）。
func newTestProvider(t *testing.T, ts *tokenServer, exp time.Time, opts ProviderOptions) (*Provider, *Client) {
	t.Helper()
	claims := map[string]any{"sub": "user_fake"}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	access := fakeJWT(t, claims)
	blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":%q,"account_id":%q}}`,
		access, fakeRefresh, fakeIDToken(t, fakeAccount), fakeAccount)
	auth, err := ParseAuthJSON(blob)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	c := NewClient(ts.srv.URL)
	return NewProvider(c, auth, opts), c
}

// TestProviderSingleFlightRefresh：N 个并发请求撞上同一个过期令牌，
// 只允许触发**一次**刷新，其余复用同一个新令牌（决策 5）。
//
// 这条不是性能优化：refresh token 是轮换型的，第二次刷新会拿着已经被消费掉的
// 那一份去换，必失败——并发刷新等于自己把自己的句柄废掉。
func TestProviderSingleFlightRefresh(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		// 拖一下，把并发窗口拉开：没有 single-flight 的实现会在这里挤进来。
		time.Sleep(30 * time.Millisecond)
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{})

	const n = 32
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = p.Current(context.Background())
		}()
	}
	wg.Wait()

	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("刷新了 %d 次，只该刷新 1 次", calls)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 个请求出错: %v", i, errs[i])
		}
		if got[i] != "fake-access-gen2" {
			t.Fatalf("第 %d 个请求拿到 %q，想要新令牌", i, got[i])
		}
	}
	// 刷新请求形态：拿旧 refresh token 去换，带 scope（offline_access 在里面）。
	form := ts.lastForm()
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != fakeRefresh {
		t.Errorf("没有拿旧 refresh token 去换")
	}
	if !strings.Contains(form.Get("scope"), "offline_access") {
		t.Errorf("scope 里没有 offline_access: %q", form.Get("scope"))
	}
	// 刷新完再取，纯内存读，不再打端点。
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("刷新后再取: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("刷新后再取又打了端点（共 %d 次）", calls)
	}
}

// TestProviderRotatePersisted：刷新成功后必须把**新一代整份 auth.json**
// 交给 OnRotate 落库。不落库的后果是重启后拿着已被消费的 refresh token，必 401。
func TestProviderRotatePersisted(t *testing.T) {
	newID := fakeIDToken(t, "acct_fake_rotated")
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", newID, 3600)
	})
	var (
		mu      sync.Mutex
		rotated []string
	)
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnRotate: func(_ context.Context, authJSON string) error {
			mu.Lock()
			rotated = append(rotated, authJSON)
			mu.Unlock()
			return nil
		},
	})
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(rotated) != 1 {
		t.Fatalf("OnRotate 调了 %d 次，想要 1 次", len(rotated))
	}
	next, err := ParseAuthJSON(rotated[0])
	if err != nil {
		t.Fatalf("落库的 auth.json 解不开: %v", err)
	}
	if next.AccessToken() != "fake-access-gen2" || next.refreshToken != "fake-refresh-gen2" {
		t.Fatalf("落库的不是新一代令牌")
	}
	if next.AccountID() != "acct_fake_rotated" {
		t.Errorf("account_id 没跟着新 id_token 走: %q", next.AccountID())
	}
	if next.LastRefresh().IsZero() {
		t.Errorf("last_refresh 没盖上")
	}
	if ProviderAccountID(p) != "acct_fake_rotated" {
		t.Errorf("provider 内存里的 account_id 没更新: %q", ProviderAccountID(p))
	}
}

// TestProviderKeepsOldRefreshTokenWhenNotReissued：刷新应答可以不重发
// refresh_token / id_token，缺就沿用旧的——覆盖成空串等于把句柄丢了。
func TestProviderKeepsOldRefreshTokenWhenNotReissued(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fake-access-gen2","expires_in":3600}`)
	})
	var rotated string
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnRotate: func(_ context.Context, authJSON string) error { rotated = authJSON; return nil },
	})
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	next, err := ParseAuthJSON(rotated)
	if err != nil {
		t.Fatalf("落库的 auth.json 解不开: %v", err)
	}
	if next.refreshToken != fakeRefresh {
		t.Fatalf("refresh token 被覆盖了")
	}
	if next.AccountID() != fakeAccount {
		t.Fatalf("account_id 被覆盖了: %q", next.AccountID())
	}
}

// TestProviderDeterministicRejectionLatches：确定性拒绝（4xx）标失效、
// 通知上层、**停止自动重试**（决策 5）。用的是实测形态——过期 refresh token
// 回的是 401 + 嵌套错误体 code=token_expired，既不是 400 也不是 invalid_grant。
func TestProviderDeterministicRejectionLatches(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Could not validate your token.","type":"invalid_request_error","param":null,"code":"token_expired"}}`)
	})
	var expiredCalls atomic.Int64
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnAuthExpired: func(context.Context, error) { expiredCalls.Add(1) },
	})

	_, err := p.Current(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("想要 ErrAuthExpired，得到 %v", err)
	}
	var oe *OAuthError
	if !errors.As(err, &oe) {
		t.Fatalf("错误里没保住 OAuthError: %v", err)
	}
	if oe.Status != http.StatusUnauthorized || oe.Code != "token_expired" {
		t.Errorf("OAuthError = %+v", oe)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("错误消息不该是多行（要进界面）: %q", err.Error())
	}
	assertNoSecrets(t, err.Error())

	// 闩落下后不再打端点，连着问三次也一样。
	for range 3 {
		if _, err := p.Current(context.Background()); !errors.Is(err, ErrAuthExpired) {
			t.Fatalf("闩落下后想要 ErrAuthExpired，得到 %v", err)
		}
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("确定性拒绝后又去重试了（共 %d 次）", calls)
	}
	if n := expiredCalls.Load(); n != 1 {
		t.Fatalf("OnAuthExpired 调了 %d 次，想要 1 次", n)
	}
	if !p.AuthExpired() {
		t.Errorf("AuthExpired() 应为真")
	}

	// Reset（管理员重新登录/粘贴导入）解闩，此后可以再试。
	blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q}}`, "fake-access-relogin", fakeRefresh)
	fresh, err := ParseAuthJSON(blob)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	p.Reset(fresh)
	if p.AuthExpired() {
		t.Fatalf("Reset 没解开失效闩")
	}
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Reset 后取令牌: %v", err)
	}
	if tok != "fake-access-relogin" {
		t.Fatalf("Reset 后拿到 %q", tok)
	}
}

// TestProviderTransientFailureRetries：网络错/5xx 是另一类——保留现有令牌，
// 下次请求再试（决策 5）。把它误判成确定性拒绝，会让一次上游抖动逼管理员
// 白重新登录一遍。
func TestProviderTransientFailureRetries(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	var expiredCalls atomic.Int64
	p, c := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnAuthExpired: func(context.Context, error) { expiredCalls.Add(1) },
	})
	base := time.Now()
	c.now = func() time.Time { return base }

	_, err := p.Current(context.Background())
	if err == nil {
		t.Fatal("5xx 想要报错")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("5xx 不该判成登录失效: %v", err)
	}
	if p.AuthExpired() || expiredCalls.Load() != 0 {
		t.Fatalf("5xx 不该落失效闩")
	}
	assertNoSecrets(t, err.Error())

	// 越过冷却期（冷却本身在 TestProviderRefreshFailureCooldown 单独钉）。
	c.now = func() time.Time { return base.Add(refreshFailCooldown + time.Second) }
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("重试想要成功: %v", err)
	}
	if tok != "fake-access-gen2" {
		t.Fatalf("重试拿到 %q", tok)
	}
	if calls := ts.calls.Load(); calls != 2 {
		t.Fatalf("打了 %d 次端点，想要 2 次（首次失败 + 重试）", calls)
	}

	// 429 与 408 是 4xx 里的两个例外，同样按可重试处理。
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout} {
		oe := &OAuthError{Phase: phaseRefresh, Status: status, Code: "x"}
		if oe.Deterministic() {
			t.Errorf("HTTP %d 不该判为确定性拒绝", status)
		}
	}
}

// TestProviderWAFPageDoesNotLatch：**不是签发方给的 4xx** 不该逼管理员重新登录。
// 这条用的是真实形态——auth.openai.com 同域的 authorize 端点对非浏览器客户端
// 就是回一张 403 的 Cloudflare 质询页（包注释里记着），令牌端点哪天被同样对待
// 不是空想。判据是「对面有没有说 OAuth」：HTML 里没有错误码。
func TestProviderWAFPageDoesNotLatch(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Just a moment...</title></head><body></body></html>`)
	})
	var expiredCalls atomic.Int64
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnAuthExpired: func(context.Context, error) { expiredCalls.Add(1) },
	})
	_, err := p.Current(context.Background())
	if err == nil {
		t.Fatal("想要报错")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("质询页不该判成登录失效: %v", err)
	}
	if p.AuthExpired() || expiredCalls.Load() != 0 {
		t.Fatal("质询页不该落失效闩——那要管理员白重新登录一遍")
	}
}

// TestProviderInvalidateForcesRefresh：没到期但被上游 401 拒了的令牌，
// Invalidate 之后必须刷新（代理侧「401 → Invalidate → 重试一次」那条路）。
func TestProviderInvalidateForcesRefresh(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	p, _ := newTestProvider(t, ts, time.Now().Add(time.Hour), ProviderOptions{})

	first, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 0 {
		t.Fatalf("没到期的令牌不该触发刷新（打了 %d 次）", calls)
	}
	p.Invalidate(first)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Invalidate 后取令牌: %v", err)
	}
	if tok != "fake-access-gen2" || ts.calls.Load() != 1 {
		t.Fatalf("Invalidate 后没刷新: tok=%q calls=%d", tok, ts.calls.Load())
	}
}

// TestProviderInvalidateIsGenerationScoped：令牌被吊销时是 N 个在飞请求同时
// 拿到 401。第一个换来新令牌之后，**后到的那些说的都是上一代**，不该把刚换来的
// 这一代也作废——否则 N 个请求换 N 代，每代还各带一次落库，正是本包处处在防的
// 「按请求速率烧世代」。
func TestProviderInvalidateIsGenerationScoped(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, fmt.Sprintf("fake-access-gen%d", n+1), "", "", 3600)
	})
	p, _ := newTestProvider(t, ts, time.Now().Add(time.Hour), ProviderOptions{})

	gen1, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	// 32 个请求都拿着 gen1 挨了 401，各自交回自己用的那一个。
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Invalidate(gen1)
			if _, err := p.Current(context.Background()); err != nil {
				t.Errorf("Current: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("一代令牌被 N 个 401 换成了 %d 代", calls)
	}

	// 交回一个早已不是当前世代的令牌：无事发生。
	p.Invalidate(gen1)
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("陈旧世代不该触发刷新（共 %d 次）", calls)
	}
	// 空串同理（调用方没记住自己用的是哪一个时的安全默认：什么都不做）。
	p.Invalidate("")
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("空令牌不该触发刷新（共 %d 次）", calls)
	}
}

// TestProviderRefreshFailureCooldown：刷新在持锁期间完成，所以云端不可达时
// 每个排队的请求都各打一次 30s 的网络 = 第 K 个要等 K×30s。冷却期把这条长队
// 压成「一次真尝试 + 一串立即的明确报错」。
func TestProviderRefreshFailureCooldown(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	p, c := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{})
	base := time.Now()
	c.now = func() time.Time { return base }

	first, err := p.Current(context.Background())
	if err == nil {
		t.Fatalf("想要 5xx 报错，得到 %q", first)
	}
	for range 10 {
		_, again := p.Current(context.Background())
		if again == nil {
			t.Fatal("冷却期内想要立刻报错")
		}
		if again.Error() != err.Error() {
			t.Fatalf("冷却期内该原样回上次的错误，得到 %v", again)
		}
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("冷却期内又打了网络（共 %d 次）", calls)
	}

	// 冷却结束后照常重试并成功。
	c.now = func() time.Time { return base.Add(refreshFailCooldown + time.Second) }
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("冷却结束后想要重试成功: %v", err)
	}
	if tok != "fake-access-gen2" || ts.calls.Load() != 2 {
		t.Fatalf("冷却结束后没重试: tok=%q calls=%d", tok, ts.calls.Load())
	}
}

// TestProviderUnknownExpiryTreatedValid：exp claim 解不出来时按「有效」处理。
//
// 反方向（按过期处理）的代价是每次进程重启都白烧掉一代轮换型 refresh token；
// 这个方向的代价只是多打一次 401，而代理那侧本来就有 Invalidate + 重试一次。
func TestProviderUnknownExpiryTreatedValid(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		t.Errorf("不该刷新")
		writeTokens(w, "x", "y", "", 3600)
	})
	p, _ := newTestProvider(t, ts, time.Time{}, ProviderOptions{})
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 0 {
		t.Fatalf("到期时刻未知时不该刷新（打了 %d 次）", calls)
	}
}

// TestProviderExpirySkew：expires_in 要减去提前量——卡着到期时刻用令牌
// 会换来一个本可避免的 401。
func TestProviderExpirySkew(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, fmt.Sprintf("fake-access-gen%d", n+1), "", "", 3600)
	})
	p, c := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{})
	base := time.Now()
	c.now = func() time.Time { return base }
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	// expires_in=3600s，减去 60s 提前量 → 3540s 处才该重刷。
	c.now = func() time.Time { return base.Add(3500 * time.Second) }
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("到期前不该重刷（共 %d 次）", calls)
	}
	c.now = func() time.Time { return base.Add(3541 * time.Second) }
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 2 {
		t.Fatalf("过了提前量该重刷（共 %d 次）", calls)
	}
}

// TestProviderShortLivedTokenDoesNotLoop：任何一种「拿到即过期」的算法都会
// 变成按请求速率刷新，也就是按请求速率烧掉轮换型 refresh token 的世代。
// minTokenValidity 那道下限是总闸，两个入口都要挡住：
//
//	① expires_in 比提前量还短；② 应答不带 expires_in，而 JWT 的 exp 只剩几十秒。
func TestProviderShortLivedTokenDoesNotLoop(t *testing.T) {
	base := time.Now()
	for _, name := range []string{"expires_in 比提前量还短", "没有 expires_in 且 exp 将尽"} {
		t.Run(name, func(t *testing.T) {
			// 两种应答都表达「这令牌只剩 10 秒」，一种用 expires_in，
			// 一种只在 JWT 的 exp claim 里说。
			shortJWT := fakeJWT(t, map[string]any{"exp": base.Add(10 * time.Second).Unix()})
			ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
				if name == "expires_in 比提前量还短" {
					writeTokens(w, fmt.Sprintf("fake-access-gen%d", n+1), "", "", 10)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"access_token":%q}`, shortJWT)
			})
			p, c := newTestProvider(t, ts, base.Add(-time.Hour), ProviderOptions{})
			c.now = func() time.Time { return base }
			if _, err := p.Current(context.Background()); err != nil {
				t.Fatalf("Current: %v", err)
			}
			// 下限保证刚换来的令牌至少还能用 minTokenValidity。
			for _, at := range []time.Duration{0, time.Second, minTokenValidity - time.Second} {
				c.now = func() time.Time { return base.Add(at) }
				if _, err := p.Current(context.Background()); err != nil {
					t.Fatalf("Current: %v", err)
				}
			}
			if calls := ts.calls.Load(); calls != 1 {
				t.Fatalf("短寿命令牌把刷新打成了循环（共 %d 次）", calls)
			}
			c.now = func() time.Time { return base.Add(minTokenValidity + time.Second) }
			if _, err := p.Current(context.Background()); err != nil {
				t.Fatalf("Current: %v", err)
			}
			if calls := ts.calls.Load(); calls != 2 {
				t.Fatalf("过了下限该重刷（共 %d 次）", calls)
			}
		})
	}
}

// TestProviderResetSameGenerationIsNoOp（迭代 11 Phase 4 外部评审）：把**同一代**
// 句柄再 Reset 一次必须是真正的空操作，不能只是"看起来一样"。
//
// 这条路每次刷新之后都会走到：OnRotate 落库抬高了行的 updated_at，数据面下一个
// 请求于是走"世代前进"那一支，把库里读回来的同一代再 Reset 一次。而 reset 里
// 的到期时刻只从 JWT 的 exp 算，[minTokenValidity] 那道下限与 expires_in 都不
// 在场——本用例造的正是那种令牌（JWT 的 exp 已在过去，签发方却说 expires_in
// 还有一小时：设备时钟快于签发方就是这个形状）。少了短路，Reset 之后它当场
// 又算过期，于是**每个请求刷一代**，把轮换型 refresh token 按请求速率烧掉。
func TestProviderResetSameGenerationIsNoOp(t *testing.T) {
	base := time.Now()
	// 新一代 access_token 是一个 exp 已过期的 JWT，但签发方说它还有一小时——
	// 设备时钟快于签发方（板子无 RTC、现场封了 NTP）就是这个形状。
	staleJWT := fakeJWT(t, map[string]any{"exp": base.Add(-time.Hour).Unix()})
	var rotated string
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, staleJWT, fmt.Sprintf("fake-refresh-gen%d", n+1), "", 3600)
	})
	p, c := newTestProvider(t, ts, base.Add(-time.Hour), ProviderOptions{
		OnRotate: func(_ context.Context, authJSON string) error {
			rotated = authJSON // 这就是落库的那一份，数据面下一个请求读回来的也是它
			return nil
		},
	})
	c.now = func() time.Time { return base }
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("首次取令牌应刷新一次，实际 %d 次", calls)
	}

	// 数据面看到 updated_at 前进 → 用库里那一份 Reset（同一代）。
	same, err := ParseAuthJSON(rotated)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	p.Reset(same)

	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("同一代 Reset 之后又刷新了（共 %d 次）：到期时刻的下限被 reset 抹掉了，"+
			"每个请求烧一代 refresh token", calls)
	}

	// 真正换了一代（不同的 access_token）时，Reset 照常整体重置。
	// sub 得取个不一样的值：claim 相同的 fakeJWT 是同一个串，那就成了上面
	// 那条同一代短路，测不到这一半。
	next, err := ParseAuthJSON(fmt.Sprintf(
		`{"tokens":{"access_token":%q,"refresh_token":"fake-refresh-relogin"}}`,
		fakeJWT(t, map[string]any{"sub": "user_relogin", "exp": base.Add(-time.Hour).Unix()})))
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	p.Reset(next)
	if _, err := p.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if calls := ts.calls.Load(); calls != 2 {
		t.Fatalf("重新登录换来的过期令牌该触发一次刷新（共 %d 次）", calls)
	}
}

// TestProviderRotatePersistFailureStillServes：落库失败不该让本次请求失败——
// 令牌已经在手且可用，回绝请求既救不回被消费掉的那一代，也毫无意义。
func TestProviderRotatePersistFailureStillServes(t *testing.T) {
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnRotate: func(context.Context, string) error { return errors.New("库写失败") },
	})
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("落库失败不该让取令牌失败: %v", err)
	}
	if tok != "fake-access-gen2" {
		t.Fatalf("拿到 %q", tok)
	}
}

// TestProviderRefreshSurvivesCallerCancel：等在同一次刷新后面的请求可能有
// 一个先撤了，刷新本身不该跟着被撤——轮换型 refresh token 半路被撤，
// 谁也说不清刚才那次到底消费掉没有。
func TestProviderRefreshSurvivesCallerCancel(t *testing.T) {
	started := make(chan struct{})
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		close(started)
		time.Sleep(60 * time.Millisecond)
		writeTokens(w, "fake-access-gen2", "fake-refresh-gen2", "", 3600)
	})
	var rotated atomic.Int64
	p, _ := newTestProvider(t, ts, time.Now().Add(-time.Hour), ProviderOptions{
		OnRotate: func(context.Context, string) error { rotated.Add(1); return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Current(ctx)
	}()
	<-started
	cancel()
	<-done

	// 刷新照样跑完并落了库：下一次取到的是新令牌，且没有再打一次端点。
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if tok != "fake-access-gen2" {
		t.Fatalf("拿到 %q，想要刷新后的新令牌", tok)
	}
	if calls := ts.calls.Load(); calls != 1 {
		t.Fatalf("打了 %d 次端点，想要 1 次", calls)
	}
	if rotated.Load() != 1 {
		t.Fatalf("OnRotate 调了 %d 次", rotated.Load())
	}
}
