package admin

// 智能体——工作节点的「工具配置」（internal/devhost/tools.go）：
//
//	GET    /admin/v1/agent-hosts/{id}/tools                 经 SSH 探一次 git / tmux / 创作工具 / gate 的读数（LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/git/install     用主机的包管理器装 git（要 root）       （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/tmux/install    用主机的包管理器装 tmux（要 root）      （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/studio/{tool}/install  创作工具安装排进主机队列（202）    （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/gate/install    把安装 gate 排进这台主机的队列（202）    （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/gate/update     gate update：不动地址与 API 密钥        （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/gate/config     gate bootstrap <地址> [Key]：重新设置   （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/gate/uninstall  gate uninstall --yes                    （LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/dev/{tool}/{action}
//	       把 gate <tool> install | update | disconnect | uninstall --yes 排进这台主机的队列（202）（LAN）
//	POST   /admin/v1/agent-hosts/{id}/tools/jobs           一次排进多个动作（一键安装 / 升级全部）（LAN）
//	GET    /admin/v1/agent-hosts/{id}/tools/jobs           队列快照：排队 / 执行中 / 最近结束的动作     （LAN）
//	GET    /admin/v1/agent-hosts/{id}/tools/jobs/wait      陪等：?revision= 变化或 30 秒即回一帧        （LAN）
//	DELETE /admin/v1/agent-hosts/{id}/tools/jobs           清掉已结束的历史                             （LAN）
//	DELETE /admin/v1/agent-hosts/{id}/tools/jobs/{job}     取消排队 / 执行中的动作，或移除一条历史      （LAN）
//
// 开发工具的动作与安装 gate 都由设备后台逐个执行（internal/devhost/devtooljobs.go）：页面离开
// 不打断，回来读一次快照再接着陪等；每个动作结束时这里记一条审计（agent_host.dev_tool，安装
// gate 是 agent_host.gate_install）。安装 gate 的地址 / Key 形态在入队时就验（400），主机上
// 没有 curl、首次安装没选 Key 这类要连上主机才知道的，落在那条记录的 error 里。
//
// devd 的安装 / 检查 / 卸载仍在 devconsole.go，同一页面一起呈现。读数不落库，每次都探；
// 受控纳管的主机答 409 host_kind_not_allowed。全部端点恒 LANOnly：它们在另一台机器上
// 装软件，读数那条也要开 SSH 连接。
//
// §15.1：安装或设置 gate 时选中的密钥由设备解封一次明文、经 SSH 会话交给主机，不回响应、不进
// 日志、不进审计 detail（审计只记密钥标签与前缀，并另记一条 key.reveal）；sudo 口令同
// devd 安装，用完即弃。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/gatehelper"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventHostGitInstall    = "agent_host.git_install"
	EventHostTmuxInstall   = "agent_host.tmux_install"
	EventHostStudioTool    = "agent_host.studio_tool"
	EventHostGateInstall   = "agent_host.gate_install"
	EventHostGateUpdate    = "agent_host.gate_update"
	EventHostGateConfig    = "agent_host.gate_config"
	EventHostGateUninstall = "agent_host.gate_uninstall"
	EventHostDevTool       = "agent_host.dev_tool"
)

// hostToolsJSON 是一台工作节点的工具配置读数。
type hostToolsJSON struct {
	Host           agentHostJSON  `json:"host"`
	Git            hostPkgJSON    `json:"git"`
	Tmux           hostPkgJSON    `json:"tmux"`
	Studio         hostStudioJSON `json:"studio"`
	PackageManager string         `json:"package_manager,omitempty"`
	Curl           bool           `json:"curl"`
	Gate           hostGateJSON   `json:"gate"`
	// DevTools 是 gate 管的六个开发工具，按 devd.DevTools 的顺序恒六项。
	DevTools []hostDevToolJSON `json:"dev_tools"`
	// GateVersion / DaemonVersion 是固件内嵌的 gate 与 devd 版本；空 = 本固件没有制品。
	GateVersion   string `json:"gate_version,omitempty"`
	DaemonVersion string `json:"daemon_version,omitempty"`
	// Output 只在安装 gate 之后带回安装脚本的最后几行（不含 Key）。
	Output string `json:"output,omitempty"`
}

// hostPkgJSON 是经包管理器安装的程序（git / tmux / ffmpeg）的读数。
type hostPkgJSON struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
}

type hostStudioJSON struct {
	FFmpeg struct {
		hostPkgJSON
		FFprobe   bool `json:"ffprobe"`
		H264      bool `json:"h264"`
		AAC       bool `json:"aac"`
		Subtitles bool `json:"subtitles"`
	} `json:"ffmpeg"`
	FontsCJK struct {
		Count  int    `json:"count"`
		Family string `json:"family"`
	} `json:"fonts_cjk"`
	ImageMagick hostPkgJSON `json:"imagemagick"`
	Ready       bool        `json:"ready"`
}

func studioToolsJSON(st devhost.StudioTools) hostStudioJSON {
	var out hostStudioJSON
	out.FFmpeg.hostPkgJSON = hostPkgJSON{Installed: st.FFmpeg.Installed, Version: st.FFmpeg.Version}
	out.FFmpeg.FFprobe, out.FFmpeg.H264, out.FFmpeg.AAC, out.FFmpeg.Subtitles = st.FFmpeg.FFprobe, st.FFmpeg.H264, st.FFmpeg.AAC, st.FFmpeg.Subtitles
	out.FontsCJK.Count, out.FontsCJK.Family = st.FontsCJK.Count, st.FontsCJK.Family
	out.ImageMagick = hostPkgJSON{Installed: st.ImageMagick.Installed, Version: st.ImageMagick.Version}
	out.Ready = st.Ready
	return out
}

type hostGateJSON struct {
	Installed  bool   `json:"installed"`
	Path       string `json:"path,omitempty"`
	Version    string `json:"version,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Configured bool   `json:"configured"`
}

// hostDevToolJSON 是主机上一个开发工具的读数：linked = gate 配置里有它的关联；origin 为
// gate（受管安装）或 external（使用者自己装的）；found 是 PATH 上的同名程序（未关联时有意义）。
type hostDevToolJSON struct {
	Name    string `json:"name"`
	Linked  bool   `json:"linked"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Origin  string `json:"origin,omitempty"`
	Found   string `json:"found,omitempty"`
}

func (s *Server) hostToolsPayload(ctx context.Context, tools *devhost.Tools, output string) (*hostToolsJSON, error) {
	cert, err := s.hosts.CurrentCertificate(ctx)
	if err != nil {
		return nil, err
	}
	devTools := make([]hostDevToolJSON, 0, len(tools.DevTools))
	for _, d := range tools.DevTools {
		devTools = append(devTools, hostDevToolJSON{Name: d.Name, Linked: d.Linked, Path: d.Path, Version: d.Version, Origin: d.Origin, Found: d.Found})
	}
	return &hostToolsJSON{
		Host:           hostJSON(*tools.Host, cert),
		DevTools:       devTools,
		Git:            hostPkgJSON{Installed: tools.Git.Installed, Version: tools.Git.Version},
		Tmux:           hostPkgJSON{Installed: tools.Tmux.Installed, Version: tools.Tmux.Version},
		Studio:         studioToolsJSON(tools.Studio),
		PackageManager: tools.PackageManager,
		Curl:           tools.Curl,
		Gate: hostGateJSON{Installed: tools.Gate.Installed, Path: tools.Gate.Path, Version: tools.Gate.Version,
			BaseURL: tools.Gate.BaseURL, Configured: tools.Gate.Configured},
		GateVersion:   gatehelper.Version(),
		DaemonVersion: s.devHosts.DaemonVersionFor(tools.Host),
		Output:        output,
	}, nil
}

func (s *Server) writeHostTools(w http.ResponseWriter, r *http.Request, tools *devhost.Tools, output string) {
	payload, err := s.hostToolsPayload(r.Context(), tools, output)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleHostTools(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	tools, err := s.devHosts.Tools(r.Context(), id)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeHostTools(w, r, tools, "")
}

// handleHostPackageInstall 用主机的包管理器装 git 或 tmux（name 取自 devhost.Packages）。
func (s *Server) handleHostPackageInstall(name, event string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDevHosts(w) {
			return
		}
		id, ok := agentHostID(w, r)
		if !ok {
			return
		}
		var req devdRequest
		if !decodeJSONOptional(w, r, &req) {
			return
		}
		tools, err := s.devHosts.InstallPackage(r.Context(), devhost.PackageInstallRequest{ID: id, Name: name, Password: req.Password})
		if err != nil {
			s.writeDevHostError(w, r, err)
			return
		}
		h := tools.Host
		version := tools.Git.Version
		if name == "tmux" {
			version = tools.Tmux.Version
		}
		s.audit(r.Context(), store.AuditEvent{
			Event: event, Entity: agentHostEntity(h.ID),
			Detail:   fmt.Sprintf("在 %s@%s:%d 安装 %s（%s，%s）", h.Username, h.Address, h.Port, name, tools.PackageManager, version),
			RemoteIP: remoteIP(r),
		})
		s.writeHostTools(w, r, tools, "")
	}
}

// hostGateInstallRequest 是安装 gate 的入参。KeyID 非零时设备在入队那一刻解封那把密钥的明文，
// 由队列在动作开始时交给主机；首次安装必须给，重装可省略（沿用主机上已保存的 Key）。
type hostGateInstallRequest struct {
	BaseURL string `json:"base_url"`
	KeyID   int64  `json:"key_id"`
}

func (s *Server) handleHostGateInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req hostGateInstallRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	spec := devhost.GateInstallSpec{BaseURL: req.BaseURL, RemoteIP: remoteIP(r)}
	if req.KeyID != 0 {
		// 先看主机行：受控纳管或不存在的主机不该让密钥解封一次。
		if err := s.devHosts.CheckWorker(r.Context(), id); err != nil {
			s.writeDevHostError(w, r, err)
			return
		}
		k, plaintext, ok := s.revealKeyForHost(w, r, req.KeyID, id, "安装 gate")
		if !ok {
			return
		}
		spec.Key, spec.KeyLabel = plaintext, k.Label
	}
	st, err := s.devHosts.EnqueueGateInstall(r.Context(), id, spec)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusAccepted, st)
}

// revealKeyForHost 解封一把要交给主机上 gate 的密钥：停用的 409 key_disabled，没留明文的
// 409 plaintext_unavailable / plaintext_unreadable；成功另记一条 key.reveal 审计（只记标签与
// 前缀、用途）。明文只在返回值里，调用方用完即弃。
func (s *Server) revealKeyForHost(w http.ResponseWriter, r *http.Request, keyID, hostID int64, purpose string) (*store.APIKey, string, bool) {
	k, ok := s.keyForWrite(w, r, keyID)
	if !ok {
		return nil, "", false
	}
	if k.Disabled {
		writeError(w, http.StatusConflict, "key_disabled", "这把 Key 已停用：gate 用它无法通过设备验证，请换一把或先启用。")
		return nil, "", false
	}
	plaintext, err := s.st.GetAPIKeyPlaintext(r.Context(), k.ID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrKeyPlaintextMissing):
			writeError(w, http.StatusConflict, "plaintext_unavailable", "该 Key 签发于旧版本，明文未留存，无法写入 gate；请新建一把替换")
		case errors.Is(err, store.ErrKeyPlaintextUnreadable):
			writeError(w, http.StatusConflict, "plaintext_unreadable", "明文解密失败（设备密钥被替换或密文损坏），无法写入 gate；请新建一把替换")
		default:
			s.writeKeyError(w, r, err)
		}
		return nil, "", false
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventKeyReveal, Entity: entityKey(k.ID),
		Detail:   fmt.Sprintf("label=%s display_prefix=%s 用于在主机 %d %s", k.Label, k.DisplayPrefix, hostID, purpose),
		RemoteIP: remoteIP(r),
	})
	return k, plaintext, true
}

func (s *Server) handleHostGateUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	res, err := s.devHosts.UpdateGate(r.Context(), id)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	h := res.Tools.Host
	s.audit(r.Context(), store.AuditEvent{
		Event: EventHostGateUpdate, Entity: agentHostEntity(h.ID),
		Detail:   fmt.Sprintf("在 %s@%s:%d 升级 gate 到 %s", h.Username, h.Address, h.Port, res.Tools.Gate.Version),
		RemoteIP: remoteIP(r),
	})
	s.writeHostTools(w, r, res.Tools, res.Output)
}

// hostGateConfigRequest 是「设置 gate」的入参：地址必填；KeyID 为 0 即沿用主机上已保存的 Key。
type hostGateConfigRequest struct {
	BaseURL string `json:"base_url"`
	KeyID   int64  `json:"key_id"`
}

func (s *Server) handleHostGateConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req hostGateConfigRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	label, plaintext := "", ""
	if req.KeyID != 0 {
		// 先看主机行：受控纳管或不存在的主机不该让密钥解封一次。
		if err := s.devHosts.CheckWorker(r.Context(), id); err != nil {
			s.writeDevHostError(w, r, err)
			return
		}
		k, secret, ok := s.revealKeyForHost(w, r, req.KeyID, id, "设置 gate")
		if !ok {
			return
		}
		label, plaintext = k.Label, secret
	}
	res, err := s.devHosts.ConfigureGate(r.Context(), devhost.GateConfigRequest{ID: id, BaseURL: req.BaseURL, Key: plaintext})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	h := res.Tools.Host
	detail := fmt.Sprintf("在 %s@%s:%d 设置 gate 的设备地址 %s", h.Username, h.Address, h.Port, res.Tools.Gate.BaseURL)
	if label != "" {
		detail += "，写入密钥 " + label
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventHostGateConfig, Entity: agentHostEntity(h.ID), Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeHostTools(w, r, res.Tools, res.Output)
}

func (s *Server) handleHostGateUninstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	tools, err := s.devHosts.UninstallGate(r.Context(), id)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	h := tools.Host
	s.audit(r.Context(), store.AuditEvent{
		Event: EventHostGateUninstall, Entity: agentHostEntity(h.ID),
		Detail: fmt.Sprintf("从 %s@%s:%d 卸载 gate", h.Username, h.Address, h.Port), RemoteIP: remoteIP(r),
	})
	s.writeHostTools(w, r, tools, "")
}

// ---- 开发工具动作队列 ----

// hostDevToolJobJSON 是队列里一个动作的读数。
type hostDevToolJobJSON struct {
	ID     int64  `json:"id"`
	Tool   string `json:"tool"`
	Action string `json:"action"`
	// Status：queued / running / succeeded / failed / cancelled。
	Status string `json:"status"`
	// Stage 只在执行中有：connecting / probing / running / verifying。
	Stage string `json:"stage,omitempty"`
	// Percent 是从 gate 进度行读出的百分比；缺席 = 还没有。
	Percent *int `json:"percent,omitempty"`
	// Line 是 gate 最近吐的一行；Output 是最近几行。
	Line       string `json:"line,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty" i18n:"text"`
	ErrorCode  string `json:"error_code,omitempty"`
	Version    string `json:"version,omitempty"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// hostDevToolJobsJSON 是一台主机队列的一帧。Tools 是最近一次动作结束时复核到的整份读数
// （ToolsAt 是那一刻），页面据此换读数、不必再探一次；没跑过任何动作时缺席。
type hostDevToolJobsJSON struct {
	Revision int64                `json:"revision"`
	Jobs     []hostDevToolJobJSON `json:"jobs"`
	Tools    *hostToolsJSON       `json:"tools,omitempty"`
	ToolsAt  string               `json:"tools_at,omitempty"`
}

func devToolJobJSON(j devhost.DevToolJob) hostDevToolJobJSON {
	out := hostDevToolJobJSON{
		ID: j.ID, Tool: j.Tool, Action: string(j.Action), Status: j.Status, Stage: j.Stage,
		Line: j.Line, Output: j.Output, Error: j.Error, ErrorCode: j.ErrorCode, Version: j.Version,
		CreatedAt: j.CreatedAt.UTC().Format(time.RFC3339),
	}
	if j.Percent >= 0 {
		p := j.Percent
		out.Percent = &p
	}
	if !j.StartedAt.IsZero() {
		out.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if !j.FinishedAt.IsZero() {
		out.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) writeDevToolJobs(w http.ResponseWriter, r *http.Request, status int, st devhost.DevToolQueueState) {
	out := hostDevToolJobsJSON{Revision: st.Revision, Jobs: make([]hostDevToolJobJSON, 0, len(st.Jobs))}
	for _, j := range st.Jobs {
		out.Jobs = append(out.Jobs, devToolJobJSON(j))
	}
	if st.Tools != nil {
		payload, err := s.hostToolsPayload(r.Context(), st.Tools, "")
		if err != nil {
			s.writeAgentHostError(w, r, err)
			return
		}
		out.Tools = payload
		out.ToolsAt = st.ToolsAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, status, out)
}

// handleHostDevTool 把对 gate 管的一个开发工具的动作排进队列：`gate <tool> install | update |
// disconnect | uninstall --yes`，由设备后台以 SSH 用户身份在主机上跑；工具名与动作只认词汇表
// （400 invalid_host）。不经手任何密钥：安装物由主机上的 gate 凭它已保存的 Key 经设备取得。
// 答 202 与队列快照。
func (s *Server) handleHostDevTool(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	spec := devhost.DevToolSpec{Tool: r.PathValue("tool"), Action: devhost.DevToolAction(r.PathValue("action")), RemoteIP: remoteIP(r)}
	st, err := s.devHosts.EnqueueDevTools(r.Context(), id, []devhost.DevToolSpec{spec})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusAccepted, st)
}

// handleHostStudioToolInstall only queues work; the HTTP request never waits for a package manager.
func (s *Server) handleHostStudioToolInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	tool := "studio/" + r.PathValue("tool")
	if !devhost.IsStudioTool(tool) {
		writeError(w, http.StatusBadRequest, "invalid_host", "不认识的创作工具")
		return
	}
	var req devdRequest
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	st, err := s.devHosts.EnqueueToolJobs(r.Context(), id, []devhost.DevToolSpec{{Tool: tool, Action: devhost.DevToolInstall, RemoteIP: remoteIP(r)}}, req.Password)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusAccepted, st)
}

// hostDevToolBatchRequest 是一次排进多个动作的入参（一键安装 / 升级全部）。
type hostDevToolBatchRequest struct {
	Password string `json:"password"`
	Jobs     []struct {
		Tool   string `json:"tool"`
		Action string `json:"action"`
	} `json:"jobs"`
}

func (s *Server) handleHostDevToolBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req hostDevToolBatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Jobs) == 0 || len(req.Jobs) > 32 {
		writeError(w, http.StatusBadRequest, "invalid_request", "请给出 1–32 个要执行的动作。")
		return
	}
	specs := make([]devhost.DevToolSpec, 0, len(req.Jobs))
	for _, j := range req.Jobs {
		specs = append(specs, devhost.DevToolSpec{Tool: j.Tool, Action: devhost.DevToolAction(j.Action), RemoteIP: remoteIP(r)})
	}
	st, err := s.devHosts.EnqueueToolJobs(r.Context(), id, specs, req.Password)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusAccepted, st)
}

func (s *Server) handleHostDevToolJobs(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	if err := s.devHosts.CheckWorker(r.Context(), id); err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusOK, s.devHosts.DevToolJobs(id))
}

func (s *Server) handleHostDevToolJobsWait(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	revision, err := queryInt64(r, "revision")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "revision 须是整数")
		return
	}
	if err := s.devHosts.CheckWorker(r.Context(), id); err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	st, err := s.devHosts.WaitDevToolJobs(r.Context(), id, revision)
	if err != nil {
		// 页面关了：不写任何东西。
		return
	}
	s.writeDevToolJobs(w, r, http.StatusOK, st)
}

func (s *Server) handleHostDevToolJobCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("job"), 10, 64)
	if err != nil || jobID <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "动作 id 须是正整数")
		return
	}
	st, err := s.devHosts.CancelDevToolJob(id, jobID)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeDevToolJobs(w, r, http.StatusOK, st)
}

func (s *Server) handleHostDevToolJobsClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	s.writeDevToolJobs(w, r, http.StatusOK, s.devHosts.ClearDevToolJobs(id))
}

// auditDevToolJob 在一个动作结束时记审计：只记工具、动作、结果与版本（§15.1，输出不进 detail）。
// 「安装 gate」记 agent_host.gate_install（成功时带版本、设备地址与密钥标签，明文不在这里）。
func (s *Server) auditDevToolJob(job devhost.DevToolJob, tools *devhost.Tools) {
	where := fmt.Sprintf("主机 %d", job.HostID)
	if tools != nil && tools.Host != nil {
		where = fmt.Sprintf("%s@%s:%d", tools.Host.Username, tools.Host.Address, tools.Host.Port)
	}
	if devhost.IsStudioTool(job.Tool) {
		s.audit(context.Background(), store.AuditEvent{Event: EventHostStudioTool, Entity: agentHostEntity(job.HostID),
			Detail: fmt.Sprintf("主机 %d 创作工具 %s：%s", job.HostID, job.Tool, job.Status), RemoteIP: job.RemoteIP})
		return
	}
	var detail string
	if job.Tool == devhost.GateJobTool {
		switch job.Status {
		case devhost.DevToolJobSucceeded:
			base := ""
			if tools != nil {
				base = tools.Gate.BaseURL
			}
			detail = fmt.Sprintf("在 %s 安装 gate %s（设备地址 %s）", where, job.Version, base)
			if job.KeyLabel != "" {
				detail += "，写入密钥 " + job.KeyLabel
			}
		case devhost.DevToolJobCancelled:
			detail = fmt.Sprintf("在 %s 安装 gate：已取消", where)
		default:
			detail = fmt.Sprintf("在 %s 安装 gate 失败：%s", where, job.ErrorCode)
		}
		s.audit(context.Background(), store.AuditEvent{
			Event: EventHostGateInstall, Entity: agentHostEntity(job.HostID), Detail: detail, RemoteIP: job.RemoteIP,
		})
		return
	}
	switch job.Status {
	case devhost.DevToolJobSucceeded:
		detail = fmt.Sprintf("在 %s 执行 gate %s %s（%s）", where, job.Tool, job.Action, job.Version)
	case devhost.DevToolJobCancelled:
		detail = fmt.Sprintf("在 %s 执行 gate %s %s：已取消", where, job.Tool, job.Action)
	default:
		detail = fmt.Sprintf("在 %s 执行 gate %s %s 失败：%s", where, job.Tool, job.Action, job.ErrorCode)
	}
	s.audit(context.Background(), store.AuditEvent{
		Event: EventHostDevTool, Entity: agentHostEntity(job.HostID), Detail: detail, RemoteIP: job.RemoteIP,
	})
}
