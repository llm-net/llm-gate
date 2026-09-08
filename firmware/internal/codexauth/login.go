package codexauth

// 管理员登录：授权码 + PKCE + **粘贴回调地址**（迭代 11 决策 4，备选①升为主路径，
// 理由与实测证据见包注释）。
//
// 一次登录分三步，中间那步不在盒子上：
//
//	① 盒子 [Client.NewLogin]：生成 code_verifier 与 state，拼出 authorize URL。
//	② 管理员的浏览器打开它，完成 OpenAI 登录与授权，最后被重定向到
//	   http://localhost:1455/auth/callback?code=…&state=…——那台机器上没有
//	   任何东西监听 1455，页面必然打不开，**这是预期结果**：要的就是地址栏里
//	   那条整串 URL。管理员把它整条粘回面板。
//	③ 盒子 [Client.ExchangeAuthCode]：核对 state、用 code + code_verifier
//	   换回整份 auth.json。
//
// 为什么这条路成立而设备码流不成立：盒子从头到尾**只打令牌端点**，authorize
// 端点由人的浏览器打开。前者实测对普通 HTTP 客户端正常答 JSON，后者对非浏览器
// 客户端一律 Cloudflare 质询——设备码流恰恰要求盒子自己去打一个「给人看」的
// 端点，而那个端点在这个签发方上根本不存在。
//
// PKCE 在这里挡的是什么：授权码经由**人手复制**在网络之外传一段，
// code_verifier 只在盒子内存里，所以即便那条 URL 被人瞥见（截图、共享屏幕、
// 浏览器历史），拿到 code 的人也换不出令牌。state 挡的是另一件事——管理员被
// 诱导粘贴**别人的**回调地址，那会把盒子接到攻击者的 ChatGPT 账号上。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// codex 在 authorize 上额外带的两个非标准参数，本包照带——盒子在这条链路上
// 扮演的就是 codex 那个 client_id：
//
//   - id_token_add_organizations=true 让 id_token 带上
//     https://api.openai.com/auth.chatgpt_account_id，**代理注入
//     ChatGPT-Account-ID 靠的就是它**，少了它整条代理路会 401；
//   - codex_cli_simplified_flow=true 让浏览器授权完直接落到回调地址，
//     而不是停在一个还要再点一下的成功页——这一步只影响管理员看到什么，
//     不影响协议。
const (
	paramAddOrganizations = "id_token_add_organizations"
	paramSimplifiedFlow   = "codex_cli_simplified_flow"
)

// LoginTTL 是一次登录会话的有效期：管理员在浏览器里完成登录、再把地址粘回来，
// 十分钟足够，超过就重来（重来无代价——[Client.NewLogin] 只是一次本地随机数）。
// 上限存在的理由是 code_verifier 不该在盒子内存里无限期躺着。
const LoginTTL = 10 * time.Minute

// Login 是一次在飞的登录会话。**含 code_verifier，是密钥物料**：不落库、
// 不进日志、不进 API 响应，只在管理面进程内存里活到换码或过期
// （Phase 4 的会话单件）。
type Login struct {
	// AuthorizeURL 是要交给管理员在浏览器里打开的地址。里面只有公开参数与
	// 一个 code_challenge（verifier 的 SHA-256），可以显示、可以复制。
	AuthorizeURL string
	// State 是本次会话的防混淆随机串，回调里必须原样带回。
	State string
	// CreatedAt 是会话起点，配合 [LoginTTL] 判过期。
	CreatedAt time.Time

	// verifier 是 PKCE 的 code_verifier。未导出且不进 String——见类型注释。
	verifier string
}

// Expired 报告这次登录会话是否已经超时。
func (l *Login) Expired(now time.Time) bool {
	return now.Sub(l.CreatedAt) > LoginTTL
}

// String 自遮蔽：Login 带 code_verifier，%v/%+v 都不该把它带出去（§15.1）。
// 值接收者，理由同 [Auth.String]。
func (l Login) String() string {
	return "codexauth.Login{state=" + l.State + ", verifier 已遮蔽}"
}

// GoString 挡 %#v（走 fmt.GoStringer 不走 Stringer），LogValue 挡 slog——
// 与 [Auth] 同一套三件套（全局硬规则「凭据包装类型三件套」）。
func (l Login) GoString() string { return l.String() }

// LogValue 让 slog 拿到的也是遮蔽后的值。
func (l Login) LogValue() slog.Value { return slog.StringValue(l.String()) }

// NewLogin 起一次登录会话：生成 code_verifier 与 state，拼出 authorize URL。
// 纯本地动作，不出网——出网发生在管理员的浏览器里，以及后面的换码那一步。
func (c *Client) NewLogin() (*Login, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, errors.New("生成登录随机数失败")
	}
	state, err := randomURLSafe(24)
	if err != nil {
		return nil, errors.New("生成登录随机数失败")
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", Scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set(paramAddOrganizations, "true")
	q.Set(paramSimplifiedFlow, "true")

	return &Login{
		AuthorizeURL: c.issuer + authorizePath + "?" + q.Encode(),
		State:        state,
		CreatedAt:    c.now(),
		verifier:     verifier,
	}, nil
}

// ExchangeAuthCode 用管理员粘回来的整条回调地址换取句柄。
//
// callbackURL 收得宽（人从地址栏复制来的东西：可能带首尾空白、可能没有 scheme），
// 但**state 必须对上**：对不上一律拒绝，不去打令牌端点。那不是苛刻——state 对不上
// 只有两种可能，粘错了（重来即可），或者这条 URL 根本不是这台盒子发起的那次登录
// （那正是要挡的事）。
func (c *Client) ExchangeAuthCode(ctx context.Context, login *Login, callbackURL string) (*Auth, error) {
	if login == nil {
		return nil, errors.New("登录会话不存在或已过期，请重新发起登录")
	}
	if login.Expired(c.now()) {
		return nil, fmt.Errorf("登录会话已超过 %d 分钟，请重新发起登录", int(LoginTTL.Minutes()))
	}
	q, err := callbackQuery(callbackURL)
	if err != nil {
		return nil, err
	}
	// 回调自己带 error 时优先报它：那是 OpenAI 明确拒绝了这次授权
	// （管理员点了「取消」是最常见的一种），拿着这条 URL 去换码只会得到
	// 一个更难懂的错误。
	if e := safeCode(q.Get("error")); e != "" {
		return nil, fmt.Errorf("OpenAI 拒绝了这次授权（%s），请重新发起登录", e)
	}
	code := q.Get("code")
	if code == "" {
		return nil, errors.New("回调地址里没有 code：请把浏览器地址栏里的整条 URL 粘贴进来")
	}
	if q.Get("state") != login.State {
		return nil, errors.New("回调地址与本次登录不匹配（state 不一致）：" +
			"请确认粘贴的是刚才这次登录跳转到的地址，或重新发起登录")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	// redirect_uri 在换码时要原样再报一次（OAuth 的规定），且必须与 authorize
	// 时逐字节一致，否则一律 invalid_grant。
	form.Set("redirect_uri", RedirectURI)
	form.Set("code_verifier", login.verifier)

	tok, err := c.postToken(ctx, form, phaseExchange)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		// 没有 refresh_token 的句柄一小时后就废，而盒子是长期代理。
		// 这通常意味着 scope 里的 offline_access 没生效——落库前就拦下来，
		// 别造一台明天自己坏掉的设备。
		return nil, errors.New("OpenAI 没有返回 refresh_token（这份订阅凭据无法长期使用），请重新发起登录")
	}
	return newAuthFromTokens(tok, c.now()), nil
}

// callbackQuery 从管理员粘来的东西里取出查询参数。
//
// 宽容两件事，因为这是一条**人手复制**的路：首尾空白，以及没有 scheme 的写法
// （"localhost:1455/auth/callback?code=…" 这种——url.Parse 会把 localhost 当
// scheme，RawQuery 反而是空的）。除此之外不猜：只给一个裸 code 也不认，
// 因为那样就没有 state 可核对了。
func callbackQuery(raw string) (url.Values, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("请粘贴浏览器地址栏里的整条回调地址")
	}
	i := strings.Index(s, "?")
	if i < 0 {
		return nil, errors.New("这不像一条回调地址：请把浏览器地址栏里带 ?code=… 的整条 URL 粘贴进来")
	}
	query := s[i+1:]
	// 片段（#…）不属于查询串。不切掉的话它会粘在最后一个参数值尾巴上，
	// 于是 code 带了一截垃圾去换码，换回来的是一句看不懂的 invalid_grant——
	// 病因离现象很远，而这是一条人从地址栏复制来的路。
	if j := strings.Index(query, "#"); j >= 0 {
		query = query[:j]
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return nil, errors.New("回调地址无法解析：请重新复制浏览器地址栏里的整条 URL")
	}
	return q, nil
}

// randomURLSafe 取 n 字节强随机并编成 base64url（无填充）。
// n=32 得到 43 个字符，正好落在 RFC 7636 对 code_verifier 的 43–128 位要求内。
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
