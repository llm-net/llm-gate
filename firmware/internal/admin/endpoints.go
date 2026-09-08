package admin

// 接入读数端点：GET /admin/v1/endpoints 回模型路由页要的全部事实——这台设备的
// 内网地址、管理员登记的外网映射，以及现在能调哪些模型。
//
// 为什么要有它：接入指引要给出客户端该填的地址，而浏览器只知道自己用的那
// 一个——从域名打开管理台的看不到内网 IP，设备有两块网卡时也只看得到自己走
// 的那块，端口更是只对当前这条路径成立。地址是设备的事实，问设备。
//
// 两条由设备给出的路径互不替代：
//   - 内网 IP addresses：每块 up 的网卡一条，恒走明文端口 HTTPPort。IP 上
//     没有能通过校验的证书（证书只覆盖域名），所以这条路径不给 https 写法。
//   - 外网映射 ExternalURL：管理员在自己的网络边界配置后登记，设备只记录与
//     回显，不建立也不探测链路（见 external.go）。
//
// 主机名（mDNS 的 <hostname>.local）曾经也在这个读数里，2026-08-07 产品方
// 明确不要：内网一路只给 IP 一种写法。别再加回来。
//
// 同一份读数有两个出口：管理员会话下的 GET /admin/v1/endpoints（本文件，模型清单
// 是全设备的），以及数据面凭 API 密钥自证的 GET /gate-helper/v1/endpoints
//（keyaccess.go，模型清单按这把 Key 的 API模型范围裁剪、不带订阅读数）。两者共用
// 下面的组装函数，地址口径只此一份。安全上它只读、不接受任何入参、不含任何用户
// 数据：局域网地址与端口是调用方已经连上的这台设备自己的事实；外网映射是管理员
// 主动公布的地址；模型名则是客户端可见的原始名（数据面 /v1/models 拿 API密钥 就
// 能读到同一份）。
//
// 资源约定（与「零轮询」规则配套）：一次 net.Interfaces（netlink 一问一答）、
// 一次 settings 单键点查、两次模型目录查询（文本与视频/图片各一，板上规模是
// 几行）、一次 Agents 订阅查询（每 provider 单账户，当前至多四行），无 exec、
// 无 /proc 遍历；页面打开取一次。只读端点，不写审计。

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// accessAddress 是设备的一个对外 IPv4 地址。
type accessAddress struct {
	Host      string `json:"host"`
	Interface string `json:"interface"`
	// Primary 标记「本次请求就是从这个地址进来的」：它对当前这个浏览器
	// 一定可达，界面把它排在最前、示例代码也优先用它。
	Primary bool `json:"primary"`
}

// servableModelJSON 是一个「客户端现在就能调用的模型」：客户端可见的原始
// 模型名，加它能服务的入口协议。Protocols 恒非 nil，可能为空数组——那意味着
// 这个名字列得出但三个协议面都不可用（来源的上游类型不服务任何入口），界面
// 照实标出来，胜过让人拿它去试。
type servableModelJSON struct {
	Name      string   `json:"name"`
	Protocols []string `json:"protocols"`
	// 仅供管理员顶栏汇总，不序列化到管理员或 Key 持有人的模型清单。
	billingModes map[string]bool
}

// endpointsJSON 是「怎么连到这台设备」的读数（见文件头）。
type endpointsJSON struct {
	Addresses []accessAddress `json:"addresses"`
	// HTTPPort 是明文监听端口（cfg.listen）；内网 IP 直连恒走它。
	HTTPPort int `json:"http_port,omitempty"`
	// ExternalURL 是当前生效的公网接入基址（external.go：外网映射登记的地址，
	// 或 Cloudflare Tunnel 启用后的 https://<hostname>）；未配置时不出现。它是
	// 公开的设备级接入配置，不含网络边界的凭据或实现细节。
	ExternalURL string `json:"external_url,omitempty"`
	// LanDomainURL 是内网域名 HTTPS 地址（landomain.go：证书覆盖域名且监听在跑时才公布，
	// 形如 https://<label>.llm.net）。它解析到设备的内网地址，只在内网可达。
	LanDomainURL string `json:"lan_domain_url,omitempty"`
}

// aigcModelJSON 是一个「客户端现在就能提交的视频/图像模型」（使用API页的
// 最小可见性）。模型的协议面就是客户端契约：客户端必须按该面源头厂商的官方
// 文档构造请求，所以 api（协议标识，如 minimax_video）与 protocol_face（路径
// 首段，如 minimax）**对 member 可见**——使用API页要靠它们渲染正确的官方路径
// 与示例。仍然不进响应的是上游**账户**：账户名、账户数量、来源侧模型 ID
// （§11.1 的本义未变）。
type aigcModelJSON struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // video | image
	// API 是该模型的协议面标识（kind 圈内其来源真正服务的那一个）；
	// 空 = 没有任何来源服务该 kind 的协议面（此时 Available 必为假）。
	API string `json:"api,omitempty"`
	// ProtocolFace 是 API 所属厂商的路径首段（config.ProtocolFace）；随 API 同空同有。
	ProtocolFace string `json:"protocol_face,omitempty"`
	// Available 为假 = 名字列得出但入口进不去（来源全挂在不服务该 kind 的
	// 上游类型上），提交一律 404。界面照实标出来，胜过让人拿它去试。
	Available    bool `json:"available"`
	billingModes map[string]bool
}

type apiModelCountJSON struct {
	Text int `json:"text"`
	AIGC int `json:"aigc"`
}

// agentsAccessJSON 是「这台设备现在能不能当某种 agent 后端用」的读数（迭代 11
// Phase 5；2026-08-12 起每个 provider 一项，「使用Agent」页要的全部事实）。
//
// 对 member 开放的判据与 aigc_models 同一条：这是**设备能力**，不是用户数据。
// 三个键各自的理由——Provider 是这一项说的是哪一种 agent（codex / grok /
// claude / cursor，界面据此渲染；与有没有连上无关，所以四项恒在场），
// Available 是「现在能不能用」（member 只需要知道这个，修不修得好是管理员的
// 事），DefaultModel 要抄进用户电脑上的 ~/.codex/config.toml 或
// ~/.grok/config.toml，页面生成启动命令时直接用；Cursor 透明代理恒为空。
//
// **每项只有这三个键**：label、account_id、状态、最近刷新时间都是订阅账号自身
// 的信息，属管理员视角（GET /admin/v1/agent-accounts，admin-only），不进这份
// 全员可读的读数；auth.json 与任何令牌更是连管理视图都没有（§15.1）。
type agentsAccessJSON struct {
	// Provider ∈ codex | grok | claude | cursor。
	Provider string `json:"provider"`
	// Available：存在一份已连接、启用且状态 active 的订阅。auth_expired 视同
	// 不可用——与数据面 /v1/responses 的分岔同一口径（见 gateway 的
	// agentCredential：disabled 与未连接对调用方是同一件事）。
	Available bool `json:"available"`
	// DefaultModel 是管理员为这份订阅设的默认模型；未设、当前不可用或 provider
	// 为 Cursor 时为空——Cursor 的模型由 cursor-agent 自行发现和选择。
	DefaultModel string `json:"default_model"`
}

// endpointsResponse 是接入指引的一次性完整读数。
type endpointsResponse struct {
	Endpoints endpointsJSON       `json:"endpoints"`
	Models    []servableModelJSON `json:"models"`
	// AIGCModels 是视频/图片模型清单；多数设备没有这类模型，空清单不出现。
	AIGCModels []aigcModelJSON `json:"aigc_models,omitempty"`
	// 按可用来源的账号计费模式分别去重；同一模型可在两组各计一次。
	APIModelCounts map[string]apiModelCountJSON `json:"api_model_counts"`
	// Agents 是订阅代理的可用性读数（迭代 11；2026-08-12 起为**数组**，恒含
	// codex、grok、claude、cursor 四项、顺序固定）。**恒在场**（不 omitempty）：「没连订阅」
	// 是这块读数最常见也最要紧的一种答案，缺省会逼前端把「读不到」与「没连」
	// 当成一回事。
	Agents []agentsAccessJSON `json:"agents"`
	// FirmwareVersion 是固件唯一的版本名称（buildinfo）。它搭这趟车而不是
	// 自成一次请求，是为了守住外壳顶栏「挂载时只发一个请求」那条纪律
	// （web/ui/src/features/layout/topbar.tsx 开头的取数纪律）。
	FirmwareVersion string `json:"firmware_version"`
	// HardwareModel 是 boardinfo 识别出的本机技术型号代号；与固件版本搭同一
	// 趟读数，顶栏不为设备型号另发请求。空串表示当前平台无法识别。
	HardwareModel string `json:"hardware_model"`
	// 管理员自定义的本地显示名称，不进入公开铭牌或 Key 持有人的接入读数。
	DeviceName string `json:"device_name"`
}

// endpointsSnapshot 组装地址读数：本机网卡 IPv4 + 明文端口 + 外网映射。
// settings 点查失败只降级掉外网映射，不能拖垮其余接入指引。
func (s *Server) endpointsSnapshot(r *http.Request) endpointsJSON {
	return endpointsJSON{
		Addresses:    localIPv4s(servedIP(r)),
		HTTPPort:     s.httpPort,
		ExternalURL:  s.effectiveExternalURL(r.Context()),
		LanDomainURL: s.effectiveLanDomainURL(r),
	}
}

// servableModels 汇总「客户端现在就能调用的模型」：与数据面 /v1/models 同一
// 口径（store 侧同一组谓词，恒为文本口径——kind=text 钉在 SQL 里，视频/图片
// 模型不进这份读数，它们走 aigcModels 那份），每个模型标出它能服务的入口
// 协议——同一模型的多条来源取并集，因为客户端看不见来源，只看得见「这个
// 名字能不能在这个入口用」。
// scope 是单把 Key 的 API模型范围（数据面 /v1/models 用的同一份谓词）：范围外的
// 模型整个不出现——对那把 Key 它就是不存在；零值 scope 放行全部（管理员视角）。
// 读失败只记一条 warn 并回空清单：地址部分照常可用，指引不至于整页打不开。
func (s *Server) servableModels(ctx context.Context, scope store.KeyAPIModelScope) []servableModelJSON {
	rows, err := s.st.ListServableModelSources(ctx)
	if err != nil {
		s.log.Warn("读取可用模型清单失败，接入读数暂不含它", "err", err.Error())
		return nil
	}
	names := make([]string, 0, len(rows))
	doc, _ := s.effectivePlatformModels(ctx)
	protos := make(map[string]map[string]bool, len(rows))
	billing := make(map[string]map[string]bool, len(rows))
	for _, row := range rows {
		if !scope.Allows(row.ModelID) {
			continue
		}
		set, ok := protos[row.ModelName]
		if !ok {
			set = make(map[string]bool, 2)
			protos[row.ModelName] = set
			billing[row.ModelName] = make(map[string]bool)
			names = append(names, row.ModelName)
		}
		// 这条读数只有文本模型（见上），协议圈定恒为文本三个协议面；来源能力再
		// 与模型的调用入口开关求交集——关掉的入口对客户端就是不存在，绝不
		// 在这份成员可读的清单里替它做广告。
		for _, p := range sourceProtocols(doc, store.ModelKindText, row.ModelName, row.UpstreamModelID, row.UpstreamType, row.UpstreamCatalogID, row.UpstreamBaseURL) {
			if p == config.ProtocolOpenAIChat && !row.EntryOpenAI {
				continue
			}
			if p == config.ProtocolOpenAIResponses && !row.EntryResponses {
				continue
			}
			if p == config.ProtocolAnthropicMessages && !row.EntryAnthropic {
				continue
			}
			set[p] = true
			billing[row.ModelName][apiBillingMode(row.UpstreamBillingMode)] = true
		}
	}
	out := make([]servableModelJSON, 0, len(names))
	for _, name := range names {
		m := servableModelJSON{Name: name, Protocols: []string{}, billingModes: billing[name]}
		// 协议顺序恒定（chat 在前，与页面上三个协议面同序）：来源怎么排都不该
		// 改变读数。
		for _, p := range []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages} {
			if protos[name][p] {
				m.Protocols = append(m.Protocols, p)
			}
		}
		if len(m.Protocols) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// aigcModels 汇总视频/图像模型清单（store 谓词与文本读数同款：三层都启用；
// kind 显式圈定 video|image）。available 按协议表现算：该模型任一来源的上游
// 类型服务其 kind 的入口协议即为真，api 记下那个协议面的标识（同格式约束由
// 管理写入侧保证，多来源必同面——取第一个命中即可）。scope 同 servableModels：
// 视频/图片模型也受 Key 的 API模型范围约束（数据面提交时同一份谓词）。读失败同
// servableModels 只降级：记一条 warn、这一节缺省。
func (s *Server) aigcModels(ctx context.Context, scope store.KeyAPIModelScope) []aigcModelJSON {
	rows, err := s.st.ListServableAIGCSources(ctx)
	if err != nil {
		s.log.Warn("读取视频/图片模型清单失败，接入读数暂不含它", "err", err.Error())
		return nil
	}
	var out []aigcModelJSON
	idx := make(map[string]int, len(rows))
	for _, row := range rows {
		if !scope.Allows(row.ModelID) {
			continue
		}
		i, ok := idx[row.ModelName]
		if !ok {
			i = len(out)
			idx[row.ModelName] = i
			out = append(out, aigcModelJSON{Name: row.ModelName, Kind: row.Kind, billingModes: make(map[string]bool)})
		}
		if ps := servableProtocols(row.Kind, row.UpstreamType, row.UpstreamBaseURL); len(ps) > 0 {
			if out[i].API == "" {
				out[i].API = ps[0]
				out[i].ProtocolFace = config.ProtocolFace(ps[0])
			}
			out[i].Available = true
			out[i].billingModes[apiBillingMode(row.UpstreamBillingMode)] = true
		}
	}
	return out
}

// 与模型接入页 apiBillingPage 同口径：subscription 归订阅，usage/none 归按量。
func apiBillingMode(mode string) string {
	if mode == "subscription" {
		return "subscription"
	}
	return "usage"
}

func apiModelCounts(models []servableModelJSON, aigc []aigcModelJSON) map[string]apiModelCountJSON {
	counts := map[string]apiModelCountJSON{"usage": {}, "subscription": {}}
	for _, model := range models {
		for mode := range model.billingModes {
			count := counts[mode]
			count.Text++
			counts[mode] = count
		}
	}
	for _, model := range aigc {
		for mode := range model.billingModes {
			count := counts[mode]
			count.AIGC++
			counts[mode] = count
		}
	}
	return counts
}

// agentsAccess 报告各订阅代理此刻可不可用（迭代 11；每个 provider 一项，
// 顺序固定 codex → grok → claude → cursor，四项恒在场——「能代理哪几种 agent」
// 是设备能力，与连没连上无关）。
//
// 判据与数据面逐字一致：该 provider 有一行且状态 active 才算可用，disabled 与
// auth_expired 都算不可用（前者是管理员的决定，后者要管理员重新登录——对
// 一个只想跑 agent 的成员来说，两者都是「现在用不了」）。
//
// 读失败同 servableModels 只降级：记一条 warn 并按不可用回答。把整页指引拖垮
// 是更坏的结果，而「说它不可用」在读不到状态时是安全方向的那一侧。
func (s *Server) agentsAccess(ctx context.Context) []agentsAccessJSON {
	out := []agentsAccessJSON{
		{Provider: store.AgentProviderCodex},
		{Provider: store.AgentProviderGrok},
		{Provider: store.AgentProviderClaude},
		{Provider: store.AgentProviderCursor},
	}
	accts, err := s.st.ListAgentAccounts(ctx)
	if err != nil {
		s.log.Warn("读取 Agents 订阅状态失败，接入读数按不可用回答", "err", err.Error())
		return out
	}
	for i := range out {
		entry := &out[i]
		for j := range accts {
			a := &accts[j]
			if a.Provider != entry.Provider || a.Status != store.AgentStatusActive {
				continue
			}
			entry.Available = true
			if entry.Provider != store.AgentProviderCursor {
				entry.DefaultModel = a.DefaultModel
			}
			break
		}
	}
	return out
}

// endpointsSnapshotFull 组装完整接入读数（地址 + 可用模型 + 视频/图片模型 +
// Agents 可用性）。
func (s *Server) endpointsSnapshotFull(r *http.Request) endpointsResponse {
	// 管理员视角不套 Key 范围：零值 scope 放行全部模型。
	models := s.servableModels(r.Context(), store.KeyAPIModelScope{})
	aigc := s.aigcModels(r.Context(), store.KeyAPIModelScope{})
	name, err := s.st.DeviceName(r.Context())
	if err != nil {
		s.log.Warn("读取设备名失败", "err", err.Error())
	}
	return endpointsResponse{
		Endpoints:       s.endpointsSnapshot(r),
		Models:          models,
		AIGCModels:      aigc,
		APIModelCounts:  apiModelCounts(models, aigc),
		Agents:          s.agentsAccess(r.Context()),
		FirmwareVersion: buildinfo.Version,
		HardwareModel:   s.hardwareModel,
		DeviceName:      name,
	}
}

func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.endpointsSnapshotFull(r))
}

// listenPort 取监听地址的端口号；解析不出（空配置或异常值）回 0，界面据此
// 退回浏览器当前端口。板上恒有值：config.applyDefaults 补缺省、validate 保证
// 是合法 host:port。
func listenPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// servedIP 取本次连接落在本机哪个地址上（net/http 把监听侧地址放进 ctx）。
// 测试用 httptest.NewRequest 构造的请求没有这个值，返回 nil 即「无主地址」。
func servedIP(r *http.Request) net.IP {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return nil
	}
	if ta, ok := addr.(*net.TCPAddr); ok {
		return ta.IP
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// localIPv4s 枚举本机可供客户端直连的 IPv4 地址：只要 up 且非回环的网卡，
// 只取全局单播地址（IsGlobalUnicast 已排除回环/链路本地/组播/未指定）。
//
// 只报 IPv4 是取舍不是限制——监听套接字本身是双栈（Go 对 0.0.0.0:80 建的是
// v6only=0 的 [::] 套接字），IPv6 客户端连得上。但板上 Wi-Fi 现下只有
// fe80:: 链路本地地址，写进 URL 要带 zone（http://[fe80::1%25wlan0]），客户端
// 支持参差；而使用API页是给客户看的接入指引，多一个填不对的地址不如不给。
// primary 排前，其余按网卡名与地址排序，保证读数稳定。
func localIPv4s(primary net.IP) []accessAddress {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]accessAddress, 0, len(ifaces))
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || !ip4.IsGlobalUnicast() {
				continue
			}
			out = append(out, accessAddress{
				Host:      ip4.String(),
				Interface: ifc.Name,
				Primary:   primary != nil && ip4.Equal(primary),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		if out[i].Interface != out[j].Interface {
			return out[i].Interface < out[j].Interface
		}
		return out[i].Host < out[j].Host
	})
	return out
}
