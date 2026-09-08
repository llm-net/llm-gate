// llmgate 是产品的单一 multi-call 二进制：systemd 以不同子命令、不同用户与能力
// 把它启动为 gatewayd / updated 两个进程，另提供本地 boardinfo 诊断命令。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/auth"
	"github.com/llm-net/llm-gate/firmware/internal/boardinfo"
	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/landomain"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/mihomo"
	"github.com/llm-net/llm-gate/firmware/internal/netconfig"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/sysinfo"
	"github.com/llm-net/llm-gate/firmware/internal/update"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const usageText = `llmgate — AI API 网关 multi-call 二进制

用法:
  llmgate <子命令> [参数]

子命令:
  gatewayd    API 网关服务（--config <path>）
  boardinfo   输出已脱敏的板卡信息 JSON（需 root）
  updated     固件升级引擎（root systemd 服务；见 docs/firmware-update.md）

通用参数:
  --version   输出版本信息
  --help      输出本清单
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "gatewayd":
		os.Exit(runGatewayd(os.Args[2:]))
	case "boardinfo":
		os.Exit(runBoardinfo(os.Args[2:]))
	case "updated":
		os.Exit(runUpdated(os.Args[2:]))
	case "--version", "version":
		fmt.Println(buildinfo.String())
	case "--help", "-h", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "llmgate: 未知子命令 %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
}

// runBoardinfo 输出已脱敏的板卡信息 JSON。SID 不因任何命令行选项暴露。
func runBoardinfo(args []string) int {
	fset := flag.NewFlagSet("boardinfo", flag.ContinueOnError)
	boardSerial := fset.String("board-serial", "", "板卡序列号，仅用于没有板卡 EEPROM 的型号（如 rk3576-evb1）；同型号每块板必须不同")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	// Collect 的错误只含路径、字段名与形状，不含 SID 或其片段。
	info, err := boardinfo.NewCollector().Collect(*boardSerial)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate boardinfo: %v\n", err)
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(os.Stderr, "提示：板卡 EEPROM 只有 root 可读，请改用 `sudo llmgate boardinfo`")
		}
		return 1
	}

	out, err := info.MaskedJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate boardinfo: 序列化板卡信息: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

// runGatewayd 是网关服务入口：config → logging → store（SQLite 迁移）→ YAML
// 导入（上游/模型 seed + api_keys 幂等）→ auth → 单监听器启动链（2026-08-06
// 决策：数据面与管理面同端口，管理面 handler 挂进 gateway.Server 按路径前缀
// 分发）。配置缺失或非法时启动即退出；SIGTERM/SIGINT 触发优雅停机（drain
// 在途请求）。
func runGatewayd(args []string) int {
	fs := flag.NewFlagSet("gatewayd", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径（YAML）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// config.Load 保证错误信息可读且不含敏感值（涉及 Key 的错误已经 RedactKey）。
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}
	level, err := logging.ParseLevel(cfg.LogLevel)
	if err != nil { // config.Load 已校验过 log_level，此为防御分支
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}
	// 日志走 stderr：板上由 systemd 收进 journal。
	logger := logging.New(os.Stderr, level)

	// 数据目录：store.Open 要求目录已存在；板上由部署创建（/var/lib/llmgate，
	// MkdirAll 幂等空操作且不改既有权限），本机 dev 路径（./data，gitignored）
	// 在此兜底创建。
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: 创建数据目录 %s: %v\n", cfg.DataDir, err)
		return 1
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}
	defer st.Close()

	// systemd stop 发 SIGTERM，本地调试 Ctrl-C 发 SIGINT，都走优雅停机。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// YAML 导入表落 SQLite，按「上游/模型 → Key」定序。上游与模型两段是
	// **seed**：只在两表皆空时导入一次（iteration-5 决策 6b），此后一切经管理
	// 界面——管理台删掉的行不会被下次重启的 YAML 复活。客户端 Key 段仍按摘要
	// 幂等导入（iteration-4 决策 7：已存在摘要整条跳过、停用不复活）。
	if err := gateway.ImportConfigCatalog(ctx, st, cfg.Upstreams, cfg.LogicalModels, logger); err != nil {
		// 导入器的错误只含上游/模型名与库约束文本，不含凭证物料。
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}
	if _, _, err := gateway.ImportConfigKeys(ctx, st, cfg.APIKeys, logger); err != nil {
		// ImportConfigKeys 的错误经 RedactKey 指认条目，不含 Key 明文。
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}

	// 出站代理策略（docs-dev/firmware-egress-proxy.md）：数据库与 device-key 可用后构造
	// 唯一的 egress.Manager，管理面与数据面共享。缺省全直连；凭据解封失败只让选了代理的
	// 流量失败关闭（不降级直连），网关与本地管理台照常。
	egressMgr := egress.NewManager(egress.Options{Settings: st, Logger: logger})
	if err := egressMgr.Load(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: 读取出站代理设置: %v\n", err)
		return 1
	}

	// 型号只识别一次：有板卡档案的设备拿到型号代号；识别不出（x86-64 云主机、
	// ARM64 通用主机、开发机）就是通用主机——型号不是设备身份，空型号只让界面
	// 省略「设备型号」，固件检查照常按本机平台筛索引、只取不限型号的发布项。
	hardwareModel, err := boardinfo.NewCollector().Model()
	if err != nil {
		logger.Info("未识别出板卡型号，按通用主机运行", "platform", officialsite.HostPlatform(), "err", err.Error())
		hardwareModel = ""
	}
	siteClient := officialsite.NewClient(cfg.OfficialSite.EffectiveBaseURL(),
		officialsite.FixedModel(hardwareModel), officialsite.WithEgress(egressMgr))

	// auth.New 顺带完成启动侧过期会话清理，并删除旧版本遗留的明文初始化码文件。
	authSvc, err := auth.New(ctx, st, cfg.DataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}
	// 库里还没有登录口令时播下出厂默认口令，让新设备开机即可登录本地管理台。
	// 播不下去就启动失败：无人能登录的设备没有继续启动的意义。已有口令则一个
	// 字节都不动（见 EnsureDefaultPassword）。
	if err := authSvc.EnsureDefaultPassword(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "llmgate gatewayd: %v\n", err)
		return 1
	}

	// 设备状态：即时采集器 + 历史记录器（分钟采样、10 分钟归档、按天保留，
	// history_days: 0 时记录器整体停摆）。两者共享，记录器顺带给即时页喂
	// 差值基点。
	sysCol := sysinfo.NewCollector()
	recorder := sysinfo.NewRecorder(sysCol, cfg.DataDir, cfg.HistoryDaysOrDefault(), logger)

	// 设备设置——网络配置状态机（nmcli/NetworkManager）：应用与回滚发生在
	// 请求生命周期之外（定时器），审计经此回调补记。
	netMgr := netconfig.NewManager(logger, func(event, detail string) {
		if err := st.AppendAudit(context.Background(), store.AuditEvent{
			Event: event, Entity: "system:network", Detail: detail,
		}); err != nil {
			logger.Error("审计写入失败", "event", event, "error", err.Error())
		}
	})

	// 固件升级管理器（docs/firmware-update.md）：advisory 情报、固件包
	// staging（云下载与手动上传汇合同一条校验）与升级引擎（llmgate updated，
	// UDS）的调用转发。引擎 socket 不可达时管理台如实降级——安装/回退答
	// engine_unavailable，检查/下载/上传照常；真正动系统的只有引擎进程。
	updateSocket := cfg.UpdateSocket
	if updateSocket == "" {
		updateSocket = updated.DefaultSocket
	}
	engineClient := updated.NewClient(updateSocket)
	updateMgr := update.NewManager(cfg.DataDir, siteClient, engineClient, logger)

	// 管理面只输出 handler，监听与停机统一归数据面 Server（单端口决策）。
	adminSrv := admin.New(cfg, logger, st, authSvc, sysCol, recorder, netMgr, hardwareModel, egressMgr)
	// 数据升级、固件升级与推荐应用都匿名读取同一个静态官网。
	adminSrv.SetCatalogSource(siteClient)
	adminSrv.SetAppsSource(siteClient)
	adminSrv.SetFirmwareUpdater(updateMgr)
	srv := gateway.New(cfg, logger, st, gateway.NewStoreKeyAuthorizer(st, logger), adminSrv.Handler(), egressMgr)
	// Agents 自检（迭代 11）：管理台那颗「自检」按钮要刷新的，是数据面进程内
	// 那**同一份**订阅令牌状态。盒子是这份轮换型 refresh token 的唯一刷新者
	// （决策 1），管理面自己造第二个 Provider 就会与数据面互废世代——所以刷新
	// 的执行体在数据面，管理面只经这条注入去调它。漏注入不静默：自检端点答
	// 503 而不是假装刷过。
	adminSrv.SetAgentTokens(srv)
	quotaMgr := agentquota.NewManager(st, srv.FetchAgentQuota)
	adminSrv.SetAgentQuota(quotaMgr)
	srv.SetAgentQuota(quotaMgr)
	// 反向的那一条（2026-08-19）：GET /agents/v1/models 要列「已连接订阅带来
	// 的文本模型」，而「哪个名字属于哪份订阅」的判据要读模型目录数据并对账，
	// 那套解析与所有权判定只该有一份（internal/admin/agentmodels.go）——所以
	// 读数的执行体在管理面，数据面只经这条注入去取。漏注入不炸：那条端点答
	// 空列表，模型选择器空着，/agents/v1/responses 照常。
	srv.SetAgentModels(adminSrv)
	// 模型能力与平台身份读取同一份生效数据目录；数据升级后的 effort、思维链
	// 回放和 Codex 选择器元数据在下一次请求即时生效。
	srv.SetPlatformModels(adminSrv)
	// 凭 Key 自证的接入读数（GET /gate-helper/v1/endpoints）同理：地址枚举与
	// 「按 Key 能调哪些模型」的组装只有管理面那一份，数据面经这条注入去取。
	// 漏注入不静默：那条端点答 503。
	srv.SetKeyAccess(adminSrv)
	srv.SetDevToolPolicy(&devtoolpolicy.Resolver{
		Store:          st,
		AgentModels:    adminSrv.AgentSubscriptionModels,
		PlatformModels: adminSrv.EffectivePlatformModels,
	})
	// Cloudflare Tunnel 公网接入（docs-dev/firmware-cloudflare-tunnel.md）：管理器
	// 拿官网读签名清单、拿升级引擎装组件与启停 connector、拿网关开关专用 origin
	// socket。管理面暴露的闸门是「口令不是出厂缺省值」，网关闸门与管理器共用同一
	// 判据（读不出口令时失败关闭）。
	adminGate := func(ctx context.Context) bool {
		isDefault, err := authSvc.PasswordIsDefault(ctx)
		return err == nil && !isDefault
	}
	srv.SetTunnelAdminGate(func() bool { return adminGate(context.Background()) })
	cfMgr := cloudflared.NewManager(cloudflared.Options{
		DataDir:   cfg.DataDir,
		Settings:  st,
		Website:   siteClient,
		Engine:    engineClient,
		Listener:  srv,
		Logger:    logger,
		AdminGate: adminGate,
	})
	adminSrv.SetCloudflareTunnel(cfMgr)
	// 板上代理内核（internal/mihomo，docs-dev/firmware-egress-proxy.md §8）：清单来自官网、
	// 制品来自 Mihomo 官方 release（经 component_artifacts 分类）、订阅拉取经 proxy_subscription
	// 分类；内核由升级引擎启停，起来后把出站代理 profile 指向 127.0.0.1 的 SOCKS 端口。
	coreMgr := mihomo.NewManager(mihomo.Options{
		DataDir:  cfg.DataDir,
		Settings: st,
		Website:  siteClient,
		Engine:   engineClient,
		Egress:   egressMgr,
		Fetch:    mihomo.NewFetchClient(egressMgr),
		Logger:   logger,
		Keys:     cloudflared.TrustedKeys(),
	})
	adminSrv.SetProxyCore(coreMgr)
	// 内网域名（internal/landomain）：经 LLM Gate官网账号关联申领 <label>.llm.net、
	// 由官网按配额选择 CA 用 DNS-01 签发证书；私钥与 HTTPS 监听只在设备上。官网不可达
	// 只让 HTTPS 入口不可用，纯 IP 网关与本地管理台不受影响。
	lanMgr := landomain.New(landomain.Options{
		DataDir:  cfg.DataDir,
		Settings: st,
		Site:     landomain.NewSiteClient(cfg.OfficialSite.EffectiveBaseURL(), landomain.WithSiteEgress(egressMgr)),
		Listener: srv,
		Logger:   logger,
		Model:    hardwareModel,
		Version:  buildinfo.Version,
	})
	srv.EnableDomainTLS(lanMgr)
	adminSrv.SetLanDomain(lanMgr)

	// 用量计量与人民币预算（迭代 9）。三方接线同样是装配期一次性注入：
	// 计量器拿 gateway 当懒对账的探针（现查厂商任务的适配器在那边），数据面
	// 拿它做记账与准入，管理面拿它出读数。**探针漏注入是个静默故障**——弃轮询
	// 的视频任务永远不入账，唯一痕迹是启动日志的 lazy_settle=false。
	meter := usage.NewMeter(st, cfg.UsageDaysOrDefault(), nil, srv, logger)
	srv.EnableMetering(meter)
	adminSrv.SetUsageReader(meter)
	// 先播一次种再开门：预算计数器从库里恢复「今天/本月已经花了多少」之前，
	// 准入是按 0 已用额判的（等于当天预算凭空翻倍）。Run 自己也会播（失败可
	// 重试），这里只是把那个窗口关到最小。
	if err := meter.Seed(ctx); err != nil {
		// 播不出来不拦启动：计量照跑，只是已用额从本进程启动时刻起算。
		logger.Warn("用量预算播种失败，稍后由计量协程重试", "err", err.Error())
	}
	// Agent 订阅模型按模型目录数据收敛一次（2026-08-15）：固件升级换来更新的
	// 内嵌基线、或上一次收敛中途失败时，开机就对齐。同步进行不拦启动——它只读
	// 本地库与内嵌/已存的目录文件，不出网；失败只记日志，下一次触发再收敛。
	adminSrv.SyncAgentModels(ctx)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go authSvc.RunSessionCleanup(runCtx)
	// 外部额度同步复用取消信号，不延长网关的停止窗口；OAuth 唯一刷新者
	// 自有刷新超时，不能让它把仅供读数的后台任务变成停机前置条件。
	go quotaMgr.Run(runCtx)
	// 已启用的 Cloudflare Tunnel 开机恢复：先建 origin socket 再让引擎确认 connector。
	// 之后每天匿名检查一次签名组件清单（自动更新缺省开启，管理员可关）。任何失败
	// 只让公网入口不可用，不影响网关与本地管理台。
	go func() {
		cfMgr.Resume(runCtx)
		first := time.NewTimer(10 * time.Minute)
		defer first.Stop()
		select {
		case <-runCtx.Done():
			return
		case <-first.C:
			cfMgr.AutoUpdate(runCtx)
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				cfMgr.AutoUpdate(runCtx)
			}
		}
	}()
	// 内置代理内核：开机恢复已启用的内核（同配置在跑不重启），之后每天刷新一次订阅。
	go func() {
		coreMgr.Resume(runCtx)
		ticker := time.NewTicker(mihomo.RefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				coreMgr.AutoRefresh(runCtx)
			}
		}
	}()
	// 内网域名维护：开机恢复 HTTPS 监听，之后每半天同步解析地址、到期前续期。
	go lanMgr.Run(runCtx)
	// aigc 任务行保留期清理（迭代 8）：任务行是短期账单事实与列表数据，
	// 14 天后随既有清理节奏（每小时）删除。
	go gateway.RunAIGCTaskPrune(runCtx, st, logger)
	// 数据升级启动即检查一次，此后每小时匿名读取官网静态 JSON。失败只影响
	// 数据新鲜度，不影响网关与本地管理台。
	go func() {
		adminSrv.AutoSyncData(runCtx)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				adminSrv.AutoSyncData(runCtx)
			}
		}
	}()

	// 记录器与计量器停机都要把内存里未落盘的那一段写出去，退出前必须等它们
	// 收尾。两者都吃 runCtx：SIGTERM 会同时取消它与 srv.Run 的 drain，于是
	// 收尾冲刷（≤5s）与 drain（≤15s）**并行**跑完——整个停机仍在 15s 内，
	// 稳在 systemd 的 TimeoutStopSec=20 之内。代价是 drain 期间才收尾的请求
	// 记不进账本（至多一个冲刷窗口的量，方向与掉电丢账一致：只会少记）。
	var bgWG sync.WaitGroup
	bgWG.Add(2)
	go func() {
		defer bgWG.Done()
		recorder.Run(runCtx)
	}()
	go func() {
		defer bgWG.Done()
		meter.Run(runCtx)
	}()

	exitErr := srv.Run(runCtx)
	cancel()
	bgWG.Wait()
	if exitErr != nil {
		// Run 的错误只含监听地址/网络错误，不含敏感值。
		logger.Error("gatewayd 异常退出", "err", exitErr.Error())
		return 1
	}
	return 0
}

// runUpdated 是升级引擎入口（root systemd 服务 llmgate-updated，
// docs/firmware-update.md）：加载 gatewayd 配置取健康探针端口与数据目录 →
// 恢复崩溃残留任务 → UDS 监听服务 gatewayd 的安装/回退请求。配置读不到时
// 降级运行（无 DB 备份、健康探针按 80 端口），引擎的其余能力不受影响——
// 宁可少一层保险也不能让升级引擎起不来。
func runUpdated(args []string) int {
	fs := flag.NewFlagSet("updated", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/llmgate/gatewayd.yaml", "gatewayd 配置文件路径（读 listen 与 data_dir）")
	socket := fs.String("socket", updated.DefaultSocket, "UDS 监听路径")
	socketGroup := fs.String("socket-group", "llmgate", "socket 属组（gatewayd 的服务用户组；系统里没有则跳过）")
	stateDir := fs.String("state-dir", updated.DefaultStateDir, "引擎状态目录（prev 槽、DB 备份、任务状态）")
	installPath := fs.String("install-path", updated.DefaultInstallPath, "固件二进制安装路径")
	gatewaydUnit := fs.String("gatewayd-unit", updated.DefaultGatewaydUnit, "网关 systemd 单元名")
	selfUnit := fs.String("self-unit", updated.DefaultSelfUnit, "引擎自己的 systemd 单元名（空 = 升级后不自重启）")
	componentsDir := fs.String("components-dir", updated.DefaultComponentsDir, "第三方组件（cloudflared）A/B slot 根目录")
	tunnelUnit := fs.String("tunnel-unit", updated.DefaultTunnelUnit, "Cloudflare Tunnel connector 的 systemd 单元名")
	tunnelUser := fs.String("tunnel-user", updated.DefaultTunnelUser, "connector 运行用户（运行期 token 文件属主）")
	tunnelRuntimeDir := fs.String("tunnel-runtime-dir", updated.DefaultTunnelRuntimeDir, "connector 运行期目录（token 文件所在，tmpfs）")
	proxyUnit := fs.String("proxy-unit", updated.DefaultProxyUnit, "板上代理内核（Mihomo）的 systemd 单元名")
	proxyUser := fs.String("proxy-user", updated.DefaultProxyUser, "代理内核运行用户（运行期配置属主）")
	proxyRuntimeDir := fs.String("proxy-runtime-dir", updated.DefaultProxyRuntimeDir, "代理内核运行期目录（配置文件所在，tmpfs）")
	logLevel := fs.String("log-level", "info", "日志级别（debug|info|warn|error）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate updated: %v\n", err)
		return 1
	}
	logger := logging.New(os.Stderr, level).With("srv", "updated")

	dataDir := ""
	healthURL := "http://127.0.0.1:80/healthz"
	if cfg, err := config.Load(*configPath); err == nil {
		dataDir = cfg.DataDir
		if _, port, perr := net.SplitHostPort(cfg.Listen); perr == nil {
			healthURL = "http://127.0.0.1:" + port + "/healthz"
		}
	} else {
		// config.Load 的错误已脱敏。
		logger.Warn("读取 gatewayd 配置失败，降级运行（无数据库备份、健康探针按 80 端口）", "err", err.Error())
	}

	engine, err := updated.New(updated.Options{
		StateDir:     *stateDir,
		InstallPath:  *installPath,
		GatewaydUnit: *gatewaydUnit,
		SelfUnit:     *selfUnit,
		DataDir:      dataDir,
		Sys:          updated.ExecSystemctl{},
		Health:       updated.HTTPHealthProber(healthURL),
		Logger:       logger,
		// 第三方组件与 connector（components.go）：形状校验、自述版本与就绪探针
		// 都用生产实现；组件更新时先起新版本、就绪才算换成，否则切回旧 slot。
		ComponentsDir:    *componentsDir,
		TunnelUnit:       *tunnelUnit,
		TunnelUser:       *tunnelUser,
		TunnelRuntimeDir: *tunnelRuntimeDir,
		TunnelReady:      updated.HTTPReadyProber(updated.DefaultTunnelReadyURL),
		// 板上代理内核（proxycore.go）：配置由 gatewayd 生成、经 UDS 交来；引擎让内核先自检
		// 再起 unit，就绪以本机 SOCKS 端口可连为准。
		ProxyUnit:        *proxyUnit,
		ProxyUser:        *proxyUser,
		ProxyRuntimeDir:  *proxyRuntimeDir,
		ProxyReady:       updated.TCPReadyProber(updated.DefaultProxyReadyAddr),
		ProxyConfigCheck: updated.RunMihomoConfigTest(*proxyUser),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate updated: %v\n", err)
		return 1
	}
	engine.ResumeIfNeeded()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := updated.NewServer(engine, logger)
	ln, err := srv.Listen(*socket, *socketGroup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate updated: %v\n", err)
		return 1
	}
	logger.Info("固件升级引擎已启动", "socket", *socket, "install_path", *installPath,
		"gatewayd_unit", *gatewaydUnit, "version", buildinfo.Version)
	if err := srv.Serve(ctx, ln); err != nil {
		logger.Error("升级引擎异常退出", "err", err.Error())
		return 1
	}
	logger.Info("固件升级引擎已停止")
	return 0
}
