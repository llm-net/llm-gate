package admin_test

// 工作节点「工具配置」端点验收：路由暴露档位、受控纳管主机被拒、探测读数、装 git、装（走
// 队列）/ 升级 / 设置 / 卸载 gate（密钥明文只经设备解封交给假主机，不回响应、不进队列帧、
// 不进日志）与开发工具队列。

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

type hostToolsBody struct {
	Host struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
	} `json:"host"`
	Git struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version"`
	} `json:"git"`
	Tmux struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version"`
	} `json:"tmux"`
	Studio struct {
		FFmpeg struct {
			Installed bool   `json:"installed"`
			Version   string `json:"version"`
			FFprobe   bool   `json:"ffprobe"`
			H264      bool   `json:"h264"`
			AAC       bool   `json:"aac"`
			Subtitles bool   `json:"subtitles"`
		} `json:"ffmpeg"`
		FontsCJK struct {
			Count  int    `json:"count"`
			Family string `json:"family"`
		} `json:"fonts_cjk"`
		ImageMagick struct {
			Installed bool   `json:"installed"`
			Version   string `json:"version"`
		} `json:"imagemagick"`
		Ready bool `json:"ready"`
	} `json:"studio"`
	PackageManager string `json:"package_manager"`
	Curl           bool   `json:"curl"`
	Gate           struct {
		Installed  bool   `json:"installed"`
		Path       string `json:"path"`
		Version    string `json:"version"`
		BaseURL    string `json:"base_url"`
		Configured bool   `json:"configured"`
	} `json:"gate"`
	DevTools []struct {
		Name    string `json:"name"`
		Linked  bool   `json:"linked"`
		Path    string `json:"path"`
		Version string `json:"version"`
		Origin  string `json:"origin"`
		Found   string `json:"found"`
	} `json:"dev_tools"`
	GateVersion string `json:"gate_version"`
	Output      string `json:"output"`
}

type devToolJobBody struct {
	ID        int64  `json:"id"`
	Tool      string `json:"tool"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Percent   *int   `json:"percent"`
	Line      string `json:"line"`
	Output    string `json:"output"`
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
	Version   string `json:"version"`
}

type devToolJobsBody struct {
	Revision int64            `json:"revision"`
	Jobs     []devToolJobBody `json:"jobs"`
	Tools    *hostToolsBody   `json:"tools"`
	ToolsAt  string           `json:"tools_at"`
}

// settleDevToolJobs 陪等到队列里没有排队 / 执行中的动作，回最后一帧。
func settleDevToolJobs(t *testing.T, e *env, session, id string, revision int64) devToolJobsBody {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		response := e.do("GET", "/admin/v1/agent-hosts/"+id+"/tools/jobs/wait?revision="+strconv.FormatInt(revision, 10), session, "")
		wantStatus(t, response, http.StatusOK)
		var st devToolJobsBody
		decodeInto(t, response, &st)
		revision = st.Revision
		active := false
		for _, j := range st.Jobs {
			if j.Status == "queued" || j.Status == "running" {
				active = true
			}
		}
		if !active {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("队列 20 秒没跑完：%+v", st.Jobs)
		}
	}
}

func TestHostToolsExposure(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 不暴露路由表")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	if _, found := tiers["POST /admin/v1/agent-hosts/{id}/tools/ffmpeg/install"]; found {
		t.Fatal("standalone FFmpeg install route still registered")
	}
	for _, pattern := range []string{
		"GET /admin/v1/agent-hosts/{id}/tools",
		"POST /admin/v1/agent-hosts/{id}/tools/git/install",
		"POST /admin/v1/agent-hosts/{id}/tools/tmux/install",
		"POST /admin/v1/agent-hosts/{id}/tools/studio/{tool}/install",
		"POST /admin/v1/agent-hosts/{id}/tools/gate/install",
		"POST /admin/v1/agent-hosts/{id}/tools/gate/update",
		"POST /admin/v1/agent-hosts/{id}/tools/gate/config",
		"POST /admin/v1/agent-hosts/{id}/tools/gate/uninstall",
		"POST /admin/v1/agent-hosts/{id}/tools/dev/{tool}/{action}",
		"POST /admin/v1/agent-hosts/{id}/tools/jobs",
		"GET /admin/v1/agent-hosts/{id}/tools/jobs",
		"GET /admin/v1/agent-hosts/{id}/tools/jobs/wait",
		"DELETE /admin/v1/agent-hosts/{id}/tools/jobs",
		"DELETE /admin/v1/agent-hosts/{id}/tools/jobs/{job}",
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != tunnelctx.LANOnly {
			t.Fatalf("%s 应是 LANOnly：%v", pattern, tier)
		}
	}
}

func TestHostStudioToolsBatch(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev", Pkg: "apt-get"})
	id := enrollDevHost(t, e, root, host, "worker", "dev", true)
	base := "/admin/v1/agent-hosts/" + id + "/tools"
	wantStatus(t, e.do("POST", base+"/studio/unknown/install", root, `{}`), http.StatusBadRequest)
	wantStatus(t, e.do("POST", base+"/jobs", root, `{"jobs":[{"tool":"studio/ffmpeg","action":"install"},{"tool":"studio/fonts-cjk","action":"update"}]}`), http.StatusBadRequest)
	var frame devToolJobsBody
	response := e.do("GET", base+"/jobs", root, "")
	decodeInto(t, response, &frame)
	if len(frame.Jobs) != 0 {
		t.Fatal("invalid batch partly queued")
	}
	wantStatus(t, e.do("POST", base+"/jobs", root, `{"jobs":[{"tool":"studio/ffmpeg","action":"install"},{"tool":"studio/fonts-cjk","action":"install"}]}`), http.StatusAccepted)
	frame = settleDevToolJobs(t, e, root, id, 0)
	if frame.Tools == nil || !frame.Tools.Studio.Ready || frame.Tools.Studio.ImageMagick.Installed || frame.Tools.Studio.FontsCJK.Count != 1 {
		t.Fatalf("required install: %+v", frame)
	}
	for _, job := range frame.Jobs {
		if job.Status != "succeeded" {
			t.Fatalf("job: %+v", job)
		}
	}
	wantStatus(t, e.do("POST", base+"/studio/imagemagick/install", root, `{}`), http.StatusAccepted)
	frame = settleDevToolJobs(t, e, root, id, 0)
	if frame.Tools == nil || !frame.Tools.Studio.ImageMagick.Installed || !frame.Tools.Studio.Ready {
		t.Fatalf("optional install: %+v", frame)
	}
	response = e.do("GET", base, root, "")
	wantStatus(t, response, http.StatusOK)
	var tools hostToolsBody
	decodeInto(t, response, &tools)
	if !tools.Studio.Ready || !tools.Studio.FFmpeg.FFprobe || !tools.Studio.FFmpeg.H264 || !tools.Studio.FFmpeg.AAC || !tools.Studio.FFmpeg.Subtitles {
		t.Fatalf("live readings: %+v", tools.Studio)
	}
}

func TestHostStudioToolPasswordAndAudit(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev", Pkg: "apt-get", Command: func(command, stdin string) (string, int, bool) {
		if strings.Contains(command, "apt-get install") {
			// Simulate a misbehaving remote command echoing stdin: it must not reach the UI/logs.
			return "package failed " + stdin, 1, true
		}
		return "", 0, false
	}})
	id := enrollDevHost(t, e, root, host, "worker", "dev", false)
	base := "/admin/v1/agent-hosts/" + id + "/tools"
	wantStatus(t, e.do("POST", base+"/studio/fonts-cjk/install", root, `{}`), http.StatusBadGateway)
	wantStatus(t, e.do("POST", base+"/jobs", root, `{"password":"`+devPassword+`","jobs":[{"tool":"studio/fonts-cjk","action":"install"}]}`), http.StatusAccepted)
	st := settleDevToolJobs(t, e, root, id, 0)
	if st.Jobs[0].Status != "failed" || st.Tools == nil {
		t.Fatalf("failed install: %+v", st)
	}
	response := e.do("GET", base+"/jobs", root, "")
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), devPassword) || strings.Contains(e.buf.String(), devPassword) {
		t.Fatal("password in queue response/log")
	}
	for _, command := range host.Execs() {
		if strings.Contains(command.Command, devPassword) {
			t.Fatal("password in argv")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, event := range e.audits() {
			if strings.Contains(event.Detail, devPassword) {
				t.Fatal("password in audit")
			}
			if event.Event == "agent_host.studio_tool" {
				found = true
				if !strings.Contains(event.Detail, "studio/fonts-cjk") || !strings.Contains(event.Detail, "failed") || strings.Contains(event.Detail, "package failed") {
					t.Fatal("invalid audit detail")
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing studio audit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostToolsLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev", Pkg: "apt-get", DevToolsFound: map[string]string{"claude": "/usr/local/bin/claude"}})
	id := enrollDevHost(t, e, root, host, "worker", "dev", true)

	var tools hostToolsBody
	response := e.do("GET", "/admin/v1/agent-hosts/"+id+"/tools", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if tools.Host.Kind != "worker" || tools.Git.Installed || tools.Studio.FFmpeg.Installed || tools.PackageManager != "apt-get" || !tools.Curl || tools.Gate.Installed {
		t.Fatalf("初始读数 = %+v", tools)
	}

	// 装 git（免密 sudo，不要口令）。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/git/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if !tools.Git.Installed || tools.Git.Version == "" {
		t.Fatalf("装 git 后 = %+v", tools.Git)
	}
	if tools.Tmux.Installed {
		t.Fatalf("tmux 不该被一起装上：%+v", tools.Tmux)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/tmux/install", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if !tools.Tmux.Installed || tools.Tmux.Version == "" || host.Tmux() == "" {
		t.Fatalf("装 tmux 后 = %+v", tools.Tmux)
	}

	if tools.Studio.FFmpeg.Installed {
		t.Fatal("安装 git / tmux 不该装上 FFmpeg")
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/studio/ffmpeg/install", root, `{}`)
	wantStatus(t, response, http.StatusAccepted)
	studioJobs := settleDevToolJobs(t, e, root, id, 0)
	if studioJobs.Tools == nil || !studioJobs.Tools.Studio.FFmpeg.Installed || studioJobs.Jobs[0].Status != "succeeded" || studioJobs.Tools.Studio.Ready {
		t.Fatalf("FFmpeg alone: %+v", studioJobs)
	}
	wantStatus(t, e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, ""), http.StatusOK)

	// 装 gate 走队列（202）：地址非法 → 400、不入队；首次没选密钥 → 入队后在执行时失败
	// （invalid_host）；选可用的 → 明文交给主机、不回响应、不进队列帧；成功那帧带着复核读数。
	var jobs devToolJobsBody
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/install", root, `{"base_url":"box"}`)
	wantStatus(t, response, http.StatusBadRequest)
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/install", root, `{"base_url":"http://192.168.1.10"}`)
	wantStatus(t, response, http.StatusAccepted)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Tool != "gate" || jobs.Jobs[0].Action != "install" {
		t.Fatalf("入队后 = %+v", jobs.Jobs)
	}
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Status != "failed" || jobs.Jobs[0].ErrorCode != devhost.CodeInvalidHost || host.Gate().Version != "" {
		t.Fatalf("首次没选密钥 = %+v / %+v", jobs.Jobs, host.Gate())
	}
	key, plaintext := e.createKey(root, "工作节点")
	keyID := strconv.FormatInt(key.ID, 10)
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/install", root, `{"base_url":"http://192.168.1.10","key_id":`+keyID+`}`)
	wantStatus(t, response, http.StatusAccepted)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 2 || jobs.Jobs[1].Tool != "gate" || (jobs.Jobs[1].Status != "queued" && jobs.Jobs[1].Status != "running") {
		t.Fatalf("入队后 = %+v", jobs.Jobs)
	}
	// 同一台主机已有排队 / 执行中的安装：再入队被忽略。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/install", root, `{"base_url":"http://192.168.1.10","key_id":`+keyID+`}`)
	wantStatus(t, response, http.StatusAccepted)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 2 {
		t.Fatalf("重复入队后 = %+v", jobs.Jobs)
	}
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 2 || jobs.Jobs[1].Status != "succeeded" || jobs.Jobs[1].Version == "" || jobs.Jobs[1].Output == "" {
		t.Fatalf("装 gate 后队列 = %+v", jobs.Jobs)
	}
	if jobs.Tools == nil || !jobs.Tools.Gate.Installed || !jobs.Tools.Gate.Configured || jobs.Tools.Gate.BaseURL != "http://192.168.1.10" || jobs.Tools.Gate.Version == "" {
		t.Fatalf("装 gate 后读数 = %+v", jobs.Tools)
	}
	tools = *jobs.Tools
	if got := host.Gate(); got.LastKey != plaintext {
		t.Fatalf("主机拿到的 Key 不是选中那把（LastKey=%q）", got.LastKey)
	}
	if strings.Contains(jobs.Jobs[1].Output, plaintext) || strings.Contains(jobs.Jobs[1].Line, plaintext) {
		t.Fatal("队列帧里带了 Key 明文")
	}
	if strings.Contains(e.buf.String(), plaintext) {
		t.Fatal("Key 明文写进了日志")
	}
	// 重装：不选密钥，沿用主机上的。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, "")
	wantStatus(t, response, http.StatusOK)
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/install", root, `{"base_url":"https://box.example"}`)
	wantStatus(t, response, http.StatusAccepted)
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Status != "succeeded" || jobs.Tools == nil || jobs.Tools.Gate.BaseURL != "https://box.example" || host.Gate().LastKey != "" {
		t.Fatalf("重装后 = %+v / %+v", jobs, host.Gate())
	}
	tools = *jobs.Tools
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, "")
	wantStatus(t, response, http.StatusOK)
	// gate 管的开发工具：读数恒五项；动作排进队列（202）、由设备后台跑，陪等到结束后队列帧里
	// 带着复核读数：装 codex（受管）、关联 PATH 上的 claude（external）、卸载 codex；词汇表之外
	// 400、不入队；输出与审计都不含 Key。
	if len(tools.DevTools) != 6 || tools.DevTools[0].Name != "codex" || tools.DevTools[0].Linked || tools.DevTools[1].Found != "/usr/local/bin/claude" {
		t.Fatalf("开发工具读数 = %+v", tools.DevTools)
	}
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 0 {
		t.Fatalf("清历史后队列 = %+v", jobs)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/dev/codex/install", root, `{}`)
	wantStatus(t, response, http.StatusAccepted)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Tool != "codex" || jobs.Jobs[0].Action != "install" || (jobs.Jobs[0].Status != "queued" && jobs.Jobs[0].Status != "running") {
		t.Fatalf("入队后 = %+v", jobs.Jobs)
	}
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Status != "succeeded" || jobs.Jobs[0].Version == "" || jobs.Jobs[0].Output == "" || jobs.Jobs[0].Percent == nil || *jobs.Jobs[0].Percent != 100 {
		t.Fatalf("装 codex 后队列 = %+v", jobs.Jobs)
	}
	if jobs.Tools == nil || !jobs.Tools.DevTools[0].Linked || jobs.Tools.DevTools[0].Origin != "gate" || jobs.Tools.DevTools[0].Version == "" || jobs.ToolsAt == "" {
		t.Fatalf("装 codex 后读数 = %+v", jobs.Tools)
	}
	// 一次排多个：关联 claude、再装一次 codex（幂等：gate 说已关联）；同一动作重复入队被忽略。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, `{"jobs":[{"tool":"claude","action":"install"},{"tool":"codex","action":"install"},{"tool":"claude","action":"install"}]}`)
	wantStatus(t, response, http.StatusAccepted)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 3 {
		t.Fatalf("批量入队后 = %+v", jobs.Jobs)
	}
	jobs = settleDevToolJobs(t, e, root, id, 0)
	for _, j := range jobs.Jobs {
		if j.Status != "succeeded" {
			t.Fatalf("批量结果 = %+v", jobs.Jobs)
		}
	}
	if jobs.Tools == nil || !jobs.Tools.DevTools[1].Linked || jobs.Tools.DevTools[1].Origin != "external" {
		t.Fatalf("关联 claude 后 = %+v", jobs.Tools.DevTools)
	}
	// 清历史，再卸 codex。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 0 {
		t.Fatalf("清历史后 = %+v", jobs.Jobs)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/dev/codex/uninstall", root, `{}`)
	wantStatus(t, response, http.StatusAccepted)
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Status != "succeeded" || jobs.Tools == nil || jobs.Tools.DevTools[0].Linked || !jobs.Tools.DevTools[1].Linked {
		t.Fatalf("卸载 codex 后 = %+v / %+v", jobs.Jobs, jobs.Tools)
	}
	// gate 拒绝的动作：升级外部安装的 claude → 队列里一条 failed、带 host_command_failed。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/dev/claude/update", root, `{}`)
	wantStatus(t, response, http.StatusAccepted)
	jobs = settleDevToolJobs(t, e, root, id, 0)
	if len(jobs.Jobs) != 2 || jobs.Jobs[1].Status != "failed" || jobs.Jobs[1].ErrorCode != devhost.CodeCommandFailed || jobs.Jobs[1].Error == "" {
		t.Fatalf("升级外部安装 = %+v", jobs.Jobs)
	}
	// 移除一条历史。
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs/"+strconv.FormatInt(jobs.Jobs[1].ID, 10), root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 1 {
		t.Fatalf("移除历史后 = %+v", jobs.Jobs)
	}
	response = e.do("DELETE", "/admin/v1/agent-hosts/"+id+"/tools/jobs/999999", root, "")
	wantStatus(t, response, http.StatusBadRequest)
	for _, bad := range []string{"/tools/dev/vim/install", "/tools/dev/codex/purge"} {
		response = e.do("POST", "/admin/v1/agent-hosts/"+id+bad, root, `{}`)
		wantStatus(t, response, http.StatusBadRequest)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, `{"jobs":[{"tool":"codex","action":"install"},{"tool":"vim","action":"install"}]}`)
	wantStatus(t, response, http.StatusBadRequest)
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/tools/jobs", root, "")
	decodeInto(t, response, &jobs)
	if len(jobs.Jobs) != 1 {
		t.Fatalf("整批被拒后不该有新动作入队：%+v", jobs.Jobs)
	}
	if strings.Contains(e.buf.String(), plaintext) {
		t.Fatal("Key 明文写进了日志")
	}
	// 主机页读数以队列帧里的复核读数为准；再探一次应一致。
	response = e.do("GET", "/admin/v1/agent-hosts/"+id+"/tools", root, "")
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if tools.DevTools[0].Linked || !tools.DevTools[1].Linked {
		t.Fatalf("再探读数 = %+v", tools.DevTools)
	}
	// 升级：gate update，地址与密钥不动。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/update", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if got := host.Gate(); got.Version != "fake-gate-updated" || got.Base != "https://box.example" || got.LastKey != "" || tools.Gate.BaseURL != "https://box.example" {
		t.Fatalf("升级后 = %+v / %+v", got, tools.Gate)
	}
	// 设置 gate：地址非法 → 400；换地址 + 换一把密钥 → 明文交给主机、不回响应、不进日志。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/config", root, `{"base_url":"box"}`)
	wantStatus(t, response, http.StatusBadRequest)
	key2, plaintext2 := e.createKey(root, "工作节点-2")
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/config", root, `{"base_url":"http://192.168.1.20","key_id":`+strconv.FormatInt(key2.ID, 10)+`}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if !tools.Gate.Configured || tools.Gate.BaseURL != "http://192.168.1.20" || tools.Output == "" {
		t.Fatalf("设置后 = %+v / %q", tools.Gate, tools.Output)
	}
	if got := host.Gate(); got.LastKey != plaintext2 || got.Base != "http://192.168.1.20" {
		t.Fatalf("主机拿到的 Key 不是新选的那把（%+v）", got)
	}
	if strings.Contains(tools.Output, plaintext2) || strings.Contains(e.buf.String(), plaintext2) {
		t.Fatal("Key 明文进了响应或日志")
	}
	// 只换地址：不选密钥，沿用主机上已保存的。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/config", root, `{"base_url":"https://box.example"}`)
	wantStatus(t, response, http.StatusOK)
	if got := host.Gate(); got.LastKey != "" || got.Base != "https://box.example" || !got.HasKey {
		t.Fatalf("只换地址后 = %+v", got)
	}
	// 卸载。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/uninstall", root, `{}`)
	wantStatus(t, response, http.StatusOK)
	decodeInto(t, response, &tools)
	if tools.Gate.Installed || host.Gate().Version != "" {
		t.Fatalf("卸载后 = %+v", tools.Gate)
	}
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/uninstall", root, `{}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != devhost.CodeToolMissing {
		t.Fatalf("错误码 = %q", code)
	}
	// 没装 gate 时设置：409 tool_missing。
	response = e.do("POST", "/admin/v1/agent-hosts/"+id+"/tools/gate/config", root, `{"base_url":"https://box.example","key_id":`+keyID+`}`)
	wantStatus(t, response, http.StatusConflict)
	if code := errCode(t, response); code != devhost.CodeToolMissing {
		t.Fatalf("错误码 = %q", code)
	}
}

func TestHostToolsRefusedForManagedHost(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	host := withDevHosts(t, e, devhosttest.Options{Username: "dev", Pkg: "apt-get"})
	id := enrollDevHost(t, e, root, host, "managed", "dev", true)
	for _, ep := range []struct{ method, path, body string }{
		{"GET", "/tools", ""},
		{"POST", "/tools/git/install", `{}`},
		{"POST", "/tools/tmux/install", `{}`},
		{"POST", "/tools/studio/ffmpeg/install", `{}`},
		{"POST", "/tools/gate/install", `{"base_url":"http://h"}`},
		{"POST", "/tools/gate/update", `{}`},
		{"POST", "/tools/gate/config", `{"base_url":"http://h","key_id":1}`},
		{"POST", "/tools/gate/uninstall", `{}`},
		{"POST", "/tools/dev/codex/install", `{}`},
	} {
		response := e.do(ep.method, "/admin/v1/agent-hosts/"+id+ep.path, root, ep.body)
		wantStatus(t, response, http.StatusConflict)
		if code := errCode(t, response); code != devhost.CodeKindNotAllowed {
			t.Fatalf("%s %s 错误码 = %q", ep.method, ep.path, code)
		}
	}
}
