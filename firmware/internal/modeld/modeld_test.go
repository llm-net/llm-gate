package modeld

// 离线验收：假算力服务器（httptest）跑队列与调度，对外 API 的令牌闸，模型缓存的拉取 / 核对 /
// 删除，安装布局，以及用 /bin/sh 写的假 codex app-server / 假 claude 跑引擎会话。全程不出网。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	pollEvery = 30 * time.Millisecond
	s, err := New(Options{StateDir: t.TempDir(), Home: t.TempDir(), User: "tester", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	admin := httptest.NewServer(s.Handler())
	t.Cleanup(func() { admin.Close(); s.Stop() })
	return s, admin.URL
}

func call(t *testing.T, base, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// fakeBackend 是一台假算力服务器：接任务、按 polls 次查询后成功。
type fakeBackend struct {
	mu    sync.Mutex
	tasks map[string]int
	polls int
	seen  []string
	fail  bool
	delay time.Duration
}

func (f *fakeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":["h3-video"],"slots":2}`))
	})
	mux.HandleFunc("POST /v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"gpu on fire"}}`))
			return
		}
		f.seen = append(f.seen, in.ID)
		f.tasks["b-"+in.ID] = 0
		w.WriteHeader(202)
		w.Write([]byte(`{"id":"b-` + in.ID + `","status":"queued"}`))
	})
	mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(f.delay)
		f.mu.Lock()
		defer f.mu.Unlock()
		n, ok := f.tasks[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		f.tasks[r.PathValue("id")] = n + 1
		if n+1 >= f.polls {
			w.Write([]byte(`{"status":"succeeded","output":{"url":"http://x/out.mp4"}}`))
			return
		}
		w.Write([]byte(`{"status":"running"}`))
	})
	mux.HandleFunc("DELETE /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.tasks, r.PathValue("id"))
		f.mu.Unlock()
		w.Write([]byte(`{"status":"cancelled"}`))
	})
	return mux
}

func waitTask(t *testing.T, admin, id string, want string) TaskView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, raw := call(t, admin, "GET", "/v1/tasks/"+id, nil)
		var out struct {
			Task TaskView `json:"task"`
		}
		json.Unmarshal(raw, &out)
		if out.Task.Status == want {
			return out.Task
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务 %s 状态 = %+v，想要 %s", id, out.Task, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestQueueDispatchAndLoadBalance(t *testing.T) {
	_, admin := newTestServer(t)
	fb := &fakeBackend{tasks: map[string]int{}, polls: 2}
	bsrv := httptest.NewServer(fb.handler())
	defer bsrv.Close()

	// 没有算力服务器时提交被拒。
	status, raw := call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "h3-video", "input": map[string]any{"prompt": "x"}})
	if status != 503 || !strings.Contains(string(raw), CodeNoBackend) {
		t.Fatalf("无算力服务器提交 = %d %s", status, raw)
	}
	// 登记一台：立刻探活，自述带回来。
	status, raw = call(t, admin, "POST", "/v1/backends", map[string]any{"name": "gpu-1", "base_url": bsrv.URL + "/", "models": []string{"h3-video"}, "slots": 1})
	if status != 201 {
		t.Fatalf("登记 = %d %s", status, raw)
	}
	var created struct {
		Backend BackendView `json:"backend"`
	}
	json.Unmarshal(raw, &created)
	if created.Backend.Status != "ready" || created.Backend.ReportedSlots != 2 || created.Backend.BaseURL != bsrv.URL {
		t.Fatalf("登记读数 = %+v", created.Backend)
	}
	// 同地址重复登记被拒。
	if status, _ = call(t, admin, "POST", "/v1/backends", map[string]any{"base_url": bsrv.URL}); status != 409 {
		t.Fatalf("重复登记 = %d", status)
	}
	// 不承载的模型被拒。
	if status, _ = call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "other"}); status != 503 {
		t.Fatalf("不承载的模型 = %d", status)
	}
	// 提交两个任务：槽位只有 1，第二个先排队；高优先级的后来者插队。
	status, raw = call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "h3-video", "input": map[string]any{"prompt": "a"}})
	if status != 202 {
		t.Fatalf("提交 = %d %s", status, raw)
	}
	var one struct {
		Task TaskView `json:"task"`
	}
	json.Unmarshal(raw, &one)
	first := one.Task.ID
	fb.mu.Lock()
	fb.polls = 100 // 第一个任务一直跑着
	fb.mu.Unlock()
	waitTask(t, admin, first, TaskRunning)
	_, raw = call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "h3-video", "priority": 0})
	json.Unmarshal(raw, &one)
	low := one.Task.ID
	_, raw = call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "h3-video", "priority": 5})
	json.Unmarshal(raw, &one)
	high := one.Task.ID
	if v := waitTask(t, admin, high, TaskQueued); v.Position != 1 {
		t.Fatalf("高优先级位次 = %+v", v)
	}
	if v := waitTask(t, admin, low, TaskQueued); v.Position != 2 {
		t.Fatalf("低优先级位次 = %+v", v)
	}
	// 取消执行中的第一个：算力服务器收到 DELETE，队列往下走，高优先级先派。
	if status, raw = call(t, admin, "DELETE", "/v1/tasks/"+first, nil); status != 200 {
		t.Fatalf("取消 = %d %s", status, raw)
	}
	fb.mu.Lock()
	fb.polls = 2
	fb.mu.Unlock()
	waitTask(t, admin, high, TaskSucceeded)
	done := waitTask(t, admin, low, TaskSucceeded)
	if string(done.Output) == "" || !strings.Contains(string(done.Output), "out.mp4") || done.BackendName != "gpu-1" {
		t.Fatalf("结果 = %+v", done)
	}
	fb.mu.Lock()
	order := append([]string{}, fb.seen...)
	fb.mu.Unlock()
	if len(order) != 3 || order[0] != first || order[1] != high || order[2] != low {
		t.Fatalf("派发顺序 = %v（first=%s high=%s low=%s）", order, first, high, low)
	}
	// 读数与清理。
	_, raw = call(t, admin, "GET", "/v1/tasks", nil)
	var list struct {
		Tasks []TaskView `json:"tasks"`
		Stats QueueStats `json:"stats"`
	}
	json.Unmarshal(raw, &list)
	if list.Stats.Succeeded != 2 || list.Stats.Cancelled != 1 || len(list.Tasks) != 3 {
		t.Fatalf("统计 = %+v", list.Stats)
	}
	// 重试一条已结束的：新任务同入参。
	if status, _ = call(t, admin, "POST", "/v1/tasks/"+low+"/retry", nil); status != 202 {
		t.Fatalf("重试 = %d", status)
	}
	_, raw = call(t, admin, "DELETE", "/v1/tasks", nil)
	if !strings.Contains(string(raw), `"removed":3`) && !strings.Contains(string(raw), `"removed":4`) {
		t.Fatalf("清理 = %s", raw)
	}
	// 有任务在跑的算力服务器不能移除；没有了才行。
	call(t, admin, "GET", "/v1/summary", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, raw = call(t, admin, "DELETE", "/v1/backends/"+created.Backend.ID, nil)
		if status == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != 200 {
		t.Fatalf("移除 = %d %s", status, raw)
	}
}

func TestDispatchFailureRetriesOnAnotherBackend(t *testing.T) {
	_, admin := newTestServer(t)
	bad := &fakeBackend{tasks: map[string]int{}, polls: 1, fail: true}
	good := &fakeBackend{tasks: map[string]int{}, polls: 1}
	badSrv, goodSrv := httptest.NewServer(bad.handler()), httptest.NewServer(good.handler())
	defer badSrv.Close()
	defer goodSrv.Close()
	call(t, admin, "POST", "/v1/backends", map[string]any{"name": "bad", "base_url": badSrv.URL, "slots": 4})
	call(t, admin, "POST", "/v1/backends", map[string]any{"name": "good", "base_url": goodSrv.URL, "slots": 1})
	_, raw := call(t, admin, "POST", "/v1/tasks", map[string]any{"model": "any"})
	var one struct {
		Task TaskView `json:"task"`
	}
	json.Unmarshal(raw, &one)
	done := waitTask(t, admin, one.Task.ID, TaskSucceeded)
	if done.BackendName != "good" || done.Attempts < 1 {
		t.Fatalf("应换一台成功 = %+v", done)
	}
	_, raw = call(t, admin, "GET", "/v1/backends", nil)
	var list struct {
		Backends []BackendView `json:"backends"`
	}
	json.Unmarshal(raw, &list)
	for _, b := range list.Backends {
		if b.Name == "bad" && (b.Status != "error" || b.Failed == 0) {
			t.Fatalf("坏机器应标异常 = %+v", b)
		}
	}
}

func TestPublicAPIRequiresToken(t *testing.T) {
	s, admin := newTestServer(t)
	fb := &fakeBackend{tasks: map[string]int{}, polls: 1}
	bsrv := httptest.NewServer(fb.handler())
	defer bsrv.Close()
	call(t, admin, "POST", "/v1/backends", map[string]any{"base_url": bsrv.URL, "models": []string{"h3-video"}})
	// 开对外 API。
	status, raw := call(t, admin, "PUT", "/v1/api", map[string]any{"listen": "127.0.0.1:0"})
	if status != 200 {
		t.Fatalf("开 API = %d %s", status, raw)
	}
	addr := s.APIAddr()
	if addr == "" {
		t.Fatal("对外 API 没起来")
	}
	pub := "http://" + addr
	// 没有令牌：503；签发之后没带：401。
	if status, _ = call(t, pub, "GET", "/api/v1/models", nil); status != 503 {
		t.Fatalf("无令牌时 = %d", status)
	}
	status, raw = call(t, admin, "POST", "/v1/tokens", map[string]any{"name": "worker-1"})
	if status != 201 {
		t.Fatalf("签发 = %d %s", status, raw)
	}
	var tok struct {
		Token     TokenView `json:"token"`
		Plaintext string    `json:"plaintext"`
	}
	json.Unmarshal(raw, &tok)
	if !strings.HasPrefix(tok.Plaintext, tokenPrefix) || tok.Token.Prefix != tok.Plaintext[:10] {
		t.Fatalf("令牌 = %+v %s", tok.Token, tok.Plaintext)
	}
	// 摘要落盘、明文不落盘。
	st, _ := os.ReadFile(filepath.Join(s.stateDir, "state.json"))
	if strings.Contains(string(st), tok.Plaintext) || !strings.Contains(string(st), tok.Token.ID) {
		t.Fatal("state.json 里不该有明文")
	}
	if status, _ = call(t, pub, "GET", "/api/v1/models", nil); status != 401 {
		t.Fatalf("不带令牌 = %d", status)
	}
	do := func(method, path string, body any, token string) (int, []byte) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, pub+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	if status, _ = do("GET", "/api/v1/models", nil, "msk_wrong"); status != 401 {
		t.Fatalf("错令牌 = %d", status)
	}
	status, raw = do("GET", "/api/v1/models", nil, tok.Plaintext)
	if status != 200 || !strings.Contains(string(raw), "h3-video") {
		t.Fatalf("模型清单 = %d %s", status, raw)
	}
	status, raw = do("POST", "/api/v1/tasks", map[string]any{"model": "h3-video", "input": map[string]any{"p": 1}}, tok.Plaintext)
	if status != 202 {
		t.Fatalf("API 提交 = %d %s", status, raw)
	}
	var one struct {
		Task TaskView `json:"task"`
	}
	json.Unmarshal(raw, &one)
	waitTask(t, admin, one.Task.ID, TaskSucceeded)
	status, raw = do("GET", "/api/v1/tasks/"+one.Task.ID, nil, tok.Plaintext)
	if status != 200 || !strings.Contains(string(raw), `"source":"api"`) {
		t.Fatalf("API 查询 = %d %s", status, raw)
	}
	// 另一把令牌看不到这个任务。
	_, raw = call(t, admin, "POST", "/v1/tokens", map[string]any{"name": "other"})
	var other struct {
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(raw, &other)
	if status, _ = do("GET", "/api/v1/tasks/"+one.Task.ID, nil, other.Plaintext); status != 404 {
		t.Fatalf("跨令牌查询 = %d", status)
	}
	// 吊销后失效；关掉监听。
	call(t, admin, "DELETE", "/v1/tokens/"+tok.Token.ID, nil)
	if status, _ = do("GET", "/api/v1/models", nil, tok.Plaintext); status != 401 {
		t.Fatalf("吊销后 = %d", status)
	}
	call(t, admin, "PUT", "/v1/api", map[string]any{"listen": ""})
	if s.APIAddr() != "" {
		t.Fatal("对外 API 应已关闭")
	}
	if status, _ = call(t, admin, "PUT", "/v1/api", map[string]any{"listen": "nope"}); status != 400 {
		t.Fatalf("坏地址 = %d", status)
	}
}

func TestCachePull(t *testing.T) {
	s, admin := newTestServer(t)
	payload := bytes.Repeat([]byte("model-bytes "), 4096)
	sum := sha256.Sum256(payload)
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(404)
			return
		}
		w.Write(payload)
	}))
	defer fs.Close()
	waitPull := func(id, want string) Pull {
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, raw := call(t, admin, "GET", "/v1/cache/pulls", nil)
			var out struct {
				Pulls []Pull `json:"pulls"`
			}
			json.Unmarshal(raw, &out)
			for _, p := range out.Pulls {
				if p.ID == id && p.Status == want {
					return p
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("拉取 %s 没到 %s：%+v", id, want, out.Pulls)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	status, raw := call(t, admin, "POST", "/v1/cache/pull", map[string]any{"url": fs.URL + "/weights.bin", "sha256": hex.EncodeToString(sum[:])})
	if status != 202 {
		t.Fatalf("拉取 = %d %s", status, raw)
	}
	var one struct {
		Pull Pull `json:"pull"`
	}
	json.Unmarshal(raw, &one)
	p := waitPull(one.Pull.ID, "succeeded")
	if p.Received != int64(len(payload)) {
		t.Fatalf("字节数 = %+v", p)
	}
	_, raw = call(t, admin, "GET", "/v1/cache", nil)
	var listing struct {
		Files []CacheFile `json:"files"`
		Stats CacheStats  `json:"stats"`
	}
	json.Unmarshal(raw, &listing)
	if len(listing.Files) != 1 || listing.Files[0].Name != "weights.bin" || listing.Files[0].SHA256 != hex.EncodeToString(sum[:]) || listing.Stats.Bytes != int64(len(payload)) {
		t.Fatalf("清单 = %+v", listing)
	}
	// 同名再拉被拒；SHA 不符判失败且不留文件；404 判失败；坏名字被拒。
	if status, _ = call(t, admin, "POST", "/v1/cache/pull", map[string]any{"url": fs.URL + "/weights.bin"}); status != 409 {
		t.Fatalf("同名 = %d", status)
	}
	_, raw = call(t, admin, "POST", "/v1/cache/pull", map[string]any{"url": fs.URL + "/w2.bin", "sha256": strings.Repeat("0", 64)})
	json.Unmarshal(raw, &one)
	if p = waitPull(one.Pull.ID, "failed"); !strings.Contains(p.Error, "SHA-256") {
		t.Fatalf("SHA 不符 = %+v", p)
	}
	if _, err := os.Stat(filepath.Join(s.cache.dir, "w2.bin")); err == nil {
		t.Fatal("SHA 不符的文件不该留下")
	}
	_, raw = call(t, admin, "POST", "/v1/cache/pull", map[string]any{"url": fs.URL + "/missing"})
	json.Unmarshal(raw, &one)
	waitPull(one.Pull.ID, "failed")
	if status, _ = call(t, admin, "POST", "/v1/cache/pull", map[string]any{"url": fs.URL + "/x", "name": "../etc"}); status != 400 {
		t.Fatalf("坏名字 = %d", status)
	}
	// 按原字节读回（带 Range）。
	req, _ := http.NewRequest("GET", admin+"/v1/cache/files/weights.bin", nil)
	req.Header.Set("Range", "bytes=0-11")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(got) != "model-bytes " {
		t.Fatalf("Range 读 = %d %q", resp.StatusCode, got)
	}
	// 删除。
	if status, _ = call(t, admin, "DELETE", "/v1/cache/weights.bin", nil); status != 200 {
		t.Fatalf("删除 = %d", status)
	}
	if status, _ = call(t, admin, "DELETE", "/v1/cache/weights.bin", nil); status != 404 {
		t.Fatalf("再删 = %d", status)
	}
}

func TestInstallLayout(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	dir := t.TempDir()
	res, err := Install(InstallOptions{ConfigDir: dir, User: u.Username, Listen: "-", StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(filepath.Join(dir, "config.json"))
	if err != nil || cfg.Socket != DefaultSocket || cfg.User != u.Username || cfg.Listen != "" || cfg.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("config = %+v %v", cfg, err)
	}
	unit, _ := os.ReadFile(res.UnitPath)
	for _, want := range []string{"User=" + u.Username, "RuntimeDirectory=llmgate-modeld", "ExecStart=" + DefaultBinPath + " serve --config-dir " + dir, "Restart=always"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("unit 缺 %q:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "StateDirectory=") {
		t.Fatal("非缺省状态目录不该写 StateDirectory")
	}
	if info, _ := os.Stat(filepath.Join(dir, "config.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json 权限 = %v", info.Mode())
	}
	// 幂等 + 卸载。
	if _, err := Install(InstallOptions{ConfigDir: dir, User: u.Username}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("配置目录应已删除")
	}
	if !strings.Contains(UnitFile(DefaultBinPath, DefaultConfigDir, "x", DefaultStateDir), "StateDirectory=llmgate-modeld") {
		t.Fatal("缺省状态目录应写 StateDirectory")
	}
}

// fakeClaude 是用 sh 写的假 claude：应答 initialize，收到 user 消息后吐一段回复与 result。
const fakeClaude = `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"initialize"'*)
      id=$(printf '%s' "$line" | sed 's/.*"request_id":"\([^"]*\)".*/\1/')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$id" ;;
    *'"type":"user"'*)
      printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}\n'
      printf '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}\n'
      printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"hi\\n","is_error":false}]}}\n'
      printf '{"type":"assistant","message":{"content":[{"type":"text","text":"hello from claude"}]}}\n'
      printf '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":5,"output_tokens":3}}\n' ;;
  esac
done
`

// fakeCodex 是用 sh 写的假 codex app-server：应答 initialize / thread/start / turn/start，然后吐
// agentMessage 与 turn/completed。
const fakeCodex = `#!/bin/sh
[ "$1" = "app-server" ] || exit 9
[ -f "$CODEX_HOME/config.toml" ] || exit 8
grep -q 'experimental_bearer_token = "sk_test"' "$CODEX_HOME/config.toml" || exit 7
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{"userAgent":"fake"}}\n' "$id" ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"th1"}}}\n' "$id" ;;
    *'"method":"turn/start"'*)
      printf '{"id":%s,"result":{"turn":{"id":"tu1"}}}\n' "$id"
      printf '{"method":"item/agentMessage/delta","params":{"delta":"hi "}}\n'
      printf '{"method":"item/completed","params":{"item":{"type":"commandExecution","command":"/bin/bash -lc '"'"'ls'"'"'","aggregatedOutput":"a b","exitCode":0}}}\n'
      printf '{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"hi from codex"}}}\n'
      printf '{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":7,"outputTokens":2}}}}\n'
      printf '{"method":"turn/completed","params":{"turn":{"status":"completed"}}}\n' ;;
  esac
done
`

func writeFakeBinary(t *testing.T, name, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runSessionTest(t *testing.T, engine, script, wantText string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	s, admin := newTestServer(t)
	bin := writeFakeBinary(t, engine, script)
	workdir := t.TempDir()
	status, raw := call(t, admin, "POST", "/v1/engines/sessions", map[string]any{
		"engine": engine, "binary": bin, "workdir": workdir, "api_base": "http://10.0.0.1/", "api_key": "sk_test", "model": "m1", "instructions": "be nice",
	})
	if status != 201 {
		t.Fatalf("起会话 = %d %s", status, raw)
	}
	var created struct {
		Session SessionView `json:"session"`
	}
	json.Unmarshal(raw, &created)
	sid := created.Session.ID
	if created.Session.Status != "idle" || created.Session.Workdir != workdir {
		t.Fatalf("会话 = %+v", created.Session)
	}
	// 密钥只在实例目录里、0600；不在进程环境里。
	entries, _ := os.ReadDir(filepath.Join(s.stateDir, "engine"))
	if len(entries) != 1 {
		t.Fatalf("实例目录 = %v", entries)
	}
	inst := filepath.Join(s.stateDir, "engine", entries[0].Name())
	found := false
	filepath.Walk(inst, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "sk_test") {
				found = true
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("%s 权限 = %v", p, info.Mode())
				}
			}
		}
		return nil
	})
	if !found {
		t.Fatal("实例目录里没有密钥文件")
	}
	sess := s.engines.get(sid)
	for _, kv := range sess.cmd.Env {
		if strings.Contains(kv, "sk_test") {
			t.Fatalf("密钥进了环境变量：%s", kv)
		}
	}
	// 下一条指令、陪等事件。
	if status, raw = call(t, admin, "POST", "/v1/engines/sessions/"+sid+"/turns", map[string]any{"text": "say hi"}); status != 202 {
		t.Fatalf("指令 = %d %s", status, raw)
	}
	if status, _ = call(t, admin, "POST", "/v1/engines/sessions/"+sid+"/turns", map[string]any{"text": "again"}); status != 409 && status != 202 {
		t.Fatalf("并发指令 = %d", status)
	}
	var events []Event
	after := int64(0)
	deadline := time.Now().Add(10 * time.Second)
	done := false
	for !done && time.Now().Before(deadline) {
		_, raw = call(t, admin, "GET", "/v1/engines/sessions/"+sid+"/events?after="+itoa(after)+"&wait=1", nil)
		var out struct {
			Events []Event `json:"events"`
		}
		json.Unmarshal(raw, &out)
		for _, e := range out.Events {
			after = e.Seq
			events = append(events, e)
			if e.Type == "done" {
				done = true
			}
		}
	}
	var sawMsg, sawCmd, sawUsage bool
	for _, e := range events {
		switch e.Type {
		case "message":
			sawMsg = sawMsg || e.Text == wantText
		case "command":
			sawCmd = sawCmd || (e.ExitCode != nil && *e.ExitCode == 0)
		case "usage":
			sawUsage = sawUsage || e.InputTok > 0
		case "done":
			if e.Outcome != "completed" {
				t.Fatalf("结局 = %+v", e)
			}
		}
	}
	if !sawMsg || !sawCmd || !sawUsage {
		t.Fatalf("事件不全 msg=%v cmd=%v usage=%v：%+v", sawMsg, sawCmd, sawUsage, events)
	}
	_, raw = call(t, admin, "GET", "/v1/engines/sessions/"+sid, nil)
	json.Unmarshal(raw, &created)
	if created.Session.Status != "idle" || created.Session.InputTokens == 0 {
		t.Fatalf("指令后会话 = %+v", created.Session)
	}
	// 关闭：进程退、实例目录删。
	if status, _ = call(t, admin, "DELETE", "/v1/engines/sessions/"+sid, nil); status != 200 {
		t.Fatalf("关闭 = %d", status)
	}
	if _, err := os.Stat(inst); !os.IsNotExist(err) {
		t.Fatal("实例目录应已删除")
	}
	select {
	case <-sess.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI 进程没退出")
	}
	if status, _ = call(t, admin, "GET", "/v1/engines/sessions/"+sid, nil); status != 404 {
		t.Fatalf("关闭后查询 = %d", status)
	}
}

func TestClaudeSession(t *testing.T) {
	runSessionTest(t, EngineClaude, fakeClaude, "hello from claude")
}
func TestCodexSession(t *testing.T) { runSessionTest(t, EngineCodex, fakeCodex, "hi from codex") }

func TestSessionCreateErrors(t *testing.T) {
	_, admin := newTestServer(t)
	if status, _ := call(t, admin, "POST", "/v1/engines/sessions", map[string]any{"engine": "x", "api_base": "http://a", "api_key": "k"}); status != 400 {
		t.Fatalf("坏引擎 = %d", status)
	}
	if status, raw := call(t, admin, "POST", "/v1/engines/sessions", map[string]any{"engine": "codex", "binary": "/nonexistent/codex", "api_base": "http://a", "api_key": "k"}); status != 501 || !strings.Contains(string(raw), CodeEngineMissing) {
		t.Fatalf("缺程序 = %d %s", status, raw)
	}
	if status, _ := call(t, admin, "POST", "/v1/engines/sessions", map[string]any{"engine": "codex", "binary": "/bin/sh", "api_base": "ftp://a", "api_key": "k"}); status != 400 {
		t.Fatalf("坏地址 = %d", status)
	}
	_, raw := call(t, admin, "GET", "/v1/engines", nil)
	var info EnginesInfo
	json.Unmarshal(raw, &info)
	if len(info.Engines) != 2 {
		t.Fatalf("引擎清单 = %+v", info)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Options{StateDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	admin := httptest.NewServer(s.Handler())
	call(t, admin.URL, "POST", "/v1/backends", map[string]any{"name": "gpu", "base_url": "http://127.0.0.1:1", "models": []string{"m"}, "slots": 3})
	call(t, admin.URL, "POST", "/v1/tokens", map[string]any{"name": "t"})
	admin.Close()
	s2, err := New(Options{StateDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.backendViews(); len(got) != 1 || got[0].Name != "gpu" || got[0].Slots != 3 || got[0].Status != "unknown" {
		t.Fatalf("重启后算力服务器 = %+v", got)
	}
	if s2.st.tokenCount() != 1 {
		t.Fatal("重启后令牌丢了")
	}
	_ = context.Background()
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
