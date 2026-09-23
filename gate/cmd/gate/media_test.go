package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeMediaJobA = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	fakeMediaJobB = "01JBBBBBBBBBBBBBBBBBBBBBBB"
)

// fakeMediaDevice 是 /gate-helper/v1/media/* 的假设备：记录每个请求，
// 任务在第二次陪等时离开 running。
type fakeMediaDevice struct {
	t  *testing.T
	mu sync.Mutex

	calls      []string // "METHOD path"
	submits    []map[string]any
	waits      map[string]int
	jobIDs     []string
	finalState string
	finalError string
	submitCode int
	submitBody string
	retryAfter string
}

func (d *fakeMediaDevice) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			d.t.Errorf("%s %s: Authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if got := r.Header.Get("X-LLMGate-Client"); got != "gate/"+version {
			d.t.Errorf("%s %s: X-LLMGate-Client=%q", r.Method, r.URL.Path, got)
		}
		if strings.Contains(r.URL.String(), fakeKey) {
			d.t.Errorf("Key 出现在 URL: %s", r.URL)
		}
		rel, ok := strings.CutPrefix(r.URL.Path, "/gate-helper/v1/media")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		d.calls = append(d.calls, r.Method+" "+rel)
		w.Header().Set("Content-Type", "application/json")
		job := func(id, status string) string {
			b, _ := json.Marshal(map[string]any{"id": id, "status": status, "model": "fake-video", "error": d.errorFor(status), "media_file": id + ".mp4"})
			return string(b)
		}
		switch {
		case r.Method == http.MethodGet && rel == "/models":
			io.WriteString(w, fakeMediaModels)
		case r.Method == http.MethodPost && rel == "/jobs":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				d.t.Errorf("提交体不是 JSON: %v", err)
			}
			d.submits = append(d.submits, body)
			if d.submitCode != 0 {
				if d.retryAfter != "" {
					w.Header().Set("Retry-After", d.retryAfter)
				}
				w.WriteHeader(d.submitCode)
				io.WriteString(w, d.submitBody)
				return
			}
			var jobs []string
			for _, id := range d.jobIDs {
				jobs = append(jobs, job(id, "running"))
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"batch_id":"","jobs":[`+strings.Join(jobs, ",")+`]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(rel, "/wait"):
			id := strings.TrimSuffix(strings.TrimPrefix(rel, "/jobs/"), "/wait")
			d.waits[id]++
			if d.waits[id] < 2 {
				io.WriteString(w, job(id, "running"))
				return
			}
			io.WriteString(w, job(id, d.finalState))
		case r.Method == http.MethodGet && strings.HasSuffix(rel, "/download"):
			id := strings.TrimSuffix(strings.TrimPrefix(rel, "/jobs/"), "/download")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.mp4"`)
			io.WriteString(w, "media:"+id)
		case r.Method == http.MethodGet && strings.HasPrefix(rel, "/jobs/"):
			io.WriteString(w, job(strings.TrimPrefix(rel, "/jobs/"), d.finalState))
		case r.Method == http.MethodDelete && strings.HasPrefix(rel, "/jobs/"):
			io.WriteString(w, `{"deleted":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"没有这个路径"}}`)
		}
	})
}

func (d *fakeMediaDevice) errorFor(status string) string {
	if status == "failed" || status == "expired" {
		return d.finalError
	}
	return ""
}

func (d *fakeMediaDevice) count(call string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c == call {
			n++
		}
	}
	return n
}

const fakeMediaModels = `{"running_per_key":3,"models":[{"id":"fake-video","backend":"grok","kind":"video","billing":"subscription","note":"","available":true,"reason_code":"","reason":"","operations":[{"name":"generate","label":"生成","inputs":[{"role":"first_frame","label":"首帧","max":1,"max_bytes":10485760,"formats":["png","jpeg"],"required":false}],"prompt_optional_with":["first_frame"],"params":[{"name":"duration","type":"integer","label":"时长","min":1,"max":15,"unit":"秒"},{"name":"aspect_ratio","type":"enum","values":["1:1","16:9"]},{"name":"voices","type":"strings","max_items":3}]}]},{"id":"fake-image","backend":"ark_image","kind":"image","billing":"metered","available":false,"reason_code":"model_not_found","reason":"模型未启用","operations":[]}]}`

type mediaTestEnv struct {
	dev      *fakeMediaDevice
	a        *app
	out, err *bytes.Buffer
	dir      string
}

func newMediaTestEnv(t *testing.T, jobIDs ...string) *mediaTestEnv {
	t.Helper()
	oldInterval, oldRetry := mediaWaitMinInterval, mediaWaitRetryDelay
	mediaWaitMinInterval, mediaWaitRetryDelay = time.Millisecond, time.Millisecond
	t.Cleanup(func() { mediaWaitMinInterval, mediaWaitRetryDelay = oldInterval, oldRetry })
	if len(jobIDs) == 0 {
		jobIDs = []string{fakeMediaJobA}
	}
	dev := &fakeMediaDevice{t: t, waits: map[string]int{}, jobIDs: jobIDs, finalState: "succeeded"}
	srv := httptest.NewServer(dev.handler())
	t.Cleanup(srv.Close)
	env := &mediaTestEnv{dev: dev, out: &bytes.Buffer{}, err: &bytes.Buffer{}, dir: t.TempDir()}
	t.Chdir(env.dir)
	cfg := emptyConfig()
	cfg.BaseURL, cfg.APIKey = srv.URL, fakeKey
	env.a = &app{root: t.TempDir(), cfg: cfg, hc: newHTTPClient(), out: env.out, err: env.err}
	return env
}

func (e *mediaTestEnv) run(t *testing.T, in io.Reader, args ...string) int {
	t.Helper()
	for _, arg := range args {
		if strings.Contains(arg, fakeKey) {
			t.Fatalf("测试把 Key 放进了 argv: %q", arg)
		}
	}
	code := e.a.media(args, in)
	for name, text := range map[string]string{"stdout": e.out.String(), "stderr": e.err.String()} {
		if strings.Contains(text, fakeKey) {
			t.Fatalf("Key 出现在 %s: %s", name, text)
		}
	}
	return code
}

func TestMediaParamsInferTypesAndRepeatedKeysBecomeArrays(t *testing.T) {
	got, err := mediaParams([]string{
		"generate_audio=true", "watermark=false", "duration=6", "seed=-1", "resolution=720p",
		"voices=a,b", "tags=x", "tags=7", "empty=", "ratio=16:9", "big=99999999999999999999", "float=1.5", "expr=a=b",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"generate_audio": true, "watermark": false, "duration": int64(6), "seed": int64(-1), "resolution": "720p",
		"voices": "a,b", "tags": []string{"x", "7"}, "empty": "", "ratio": "16:9",
		"big": "99999999999999999999", "float": "1.5", "expr": "a=b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params=%#v\nwant %#v", got, want)
	}
	for _, bad := range []string{"noequals", "=v"} {
		if _, err := mediaParams([]string{bad}); err == nil {
			t.Errorf("mediaParams(%q) 应报错", bad)
		}
	}
	if got, err := mediaParams(nil); err != nil || got != nil {
		t.Fatalf("空参数=(%v,%v)", got, err)
	}
}

func TestMediaGenerateSubmitsWaitsDownloadsAndDeletes(t *testing.T) {
	env := newMediaTestEnv(t)
	png := filepath.Join(t.TempDir(), "first.PNG")
	ref1 := filepath.Join(t.TempDir(), "a.jpg")
	ref2 := filepath.Join(t.TempDir(), "b.webp")
	for _, p := range []string{png, ref1, ref2} {
		if err := os.WriteFile(p, []byte("img:"+filepath.Base(p)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code := env.run(t, nil, "generate", "--model", "fake-video", "--operation=edit", "--prompt", "一只猫",
		"--first-frame", png, "--ref", ref1, "--ref", ref2,
		"--param", "duration=6", "--param", "generate_audio=true", "--param", "voices=a", "--param", "voices=b", "--count", "1")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, env.err.String())
	}
	body := env.dev.submits[0]
	if body["model"] != "fake-video" || body["operation"] != "edit" || body["prompt"] != "一只猫" || body["count"] != float64(1) {
		t.Fatalf("提交体=%v", body)
	}
	if _, ok := body["key_id"]; ok {
		t.Fatal("持有人端点不该带 key_id")
	}
	wantParams := map[string]any{"duration": float64(6), "generate_audio": true, "voices": []any{"a", "b"}}
	if !reflect.DeepEqual(body["params"], wantParams) {
		t.Fatalf("params=%#v", body["params"])
	}
	inputs := body["inputs"].(map[string]any)
	wantFirst := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("img:first.PNG"))
	if inputs["first_frame"] != wantFirst {
		t.Fatalf("first_frame=%v", inputs["first_frame"])
	}
	refs := inputs["reference_images"].([]any)
	if len(refs) != 2 || !strings.HasPrefix(refs[0].(string), "data:image/jpeg;base64,") || !strings.HasPrefix(refs[1].(string), "data:image/webp;base64,") {
		t.Fatalf("reference_images=%v", refs)
	}
	wantCalls := []string{
		"POST /jobs",
		"POST /jobs/" + fakeMediaJobA + "/wait",
		"POST /jobs/" + fakeMediaJobA + "/wait",
		"GET /jobs/" + fakeMediaJobA + "/download",
		"DELETE /jobs/" + fakeMediaJobA,
	}
	if !reflect.DeepEqual(env.dev.calls, wantCalls) {
		t.Fatalf("calls=%v", env.dev.calls)
	}
	got, err := os.ReadFile(filepath.Join(env.dir, fakeMediaJobA+".mp4"))
	if err != nil || string(got) != "media:"+fakeMediaJobA {
		t.Fatalf("下载文件=(%q,%v)", got, err)
	}
	if strings.TrimSpace(env.out.String()) != fakeMediaJobA+".mp4" {
		t.Fatalf("stdout=%q", env.out.String())
	}
}

func TestMediaGenerateKeepDoesNotDelete(t *testing.T) {
	env := newMediaTestEnv(t)
	if code := env.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x", "--keep", "--out", "cat"); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, env.err.String())
	}
	if n := env.dev.count("DELETE /jobs/" + fakeMediaJobA); n != 0 {
		t.Fatalf("--keep 仍删除了 %d 次", n)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "cat.mp4")); err != nil {
		t.Fatal(err)
	}
}

func TestMediaGenerateNoWaitOnlyPrintsIDs(t *testing.T) {
	env := newMediaTestEnv(t, fakeMediaJobA, fakeMediaJobB)
	if code := env.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x", "--count", "2", "--no-wait"); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, env.err.String())
	}
	if env.out.String() != fakeMediaJobA+"\n"+fakeMediaJobB+"\n" {
		t.Fatalf("stdout=%q", env.out.String())
	}
	if !reflect.DeepEqual(env.dev.calls, []string{"POST /jobs"}) {
		t.Fatalf("calls=%v", env.dev.calls)
	}
	entries, _ := os.ReadDir(env.dir)
	if len(entries) != 0 {
		t.Fatalf("--no-wait 不该落文件: %v", entries)
	}
}

func TestMediaDownloadNamesNeverOverwrite(t *testing.T) {
	env := newMediaTestEnv(t, fakeMediaJobA, fakeMediaJobB)
	if err := os.WriteFile(filepath.Join(env.dir, "shot.mp4"), []byte("mine"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := env.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x", "--count", "2", "--out", "shot"); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, env.err.String())
	}
	for name, want := range map[string]string{
		"shot.mp4": "mine", "shot-2.mp4": "media:" + fakeMediaJobA, "shot-3.mp4": "media:" + fakeMediaJobB,
	} {
		got, err := os.ReadFile(filepath.Join(env.dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s=(%q,%v), want %q", name, got, err, want)
		}
	}
	if env.out.String() != "shot-2.mp4\nshot-3.mp4\n" {
		t.Fatalf("stdout=%q", env.out.String())
	}
	// --out 已带同一扩展名时不叠成 .mp4.mp4。
	f, path, err := mediaCreateUnique("clip.MP4", ".mp4")
	if err != nil || path != "clip.mp4" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	f.Close()
}

func TestMediaFailedJobExitsNonZeroAndIsDeleted(t *testing.T) {
	env := newMediaTestEnv(t)
	env.dev.finalState, env.dev.finalError = "failed", "平台拒绝了这条提示词"
	if code := env.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x"); code == 0 {
		t.Fatal("失败任务应让退出码非 0")
	}
	if !strings.Contains(env.err.String(), "平台拒绝了这条提示词") || !strings.Contains(env.err.String(), fakeMediaJobA) {
		t.Fatalf("stderr=%q", env.err.String())
	}
	if env.dev.count("GET /jobs/"+fakeMediaJobA+"/download") != 0 || env.dev.count("DELETE /jobs/"+fakeMediaJobA) != 1 {
		t.Fatalf("calls=%v", env.dev.calls)
	}
	entries, _ := os.ReadDir(env.dir)
	if len(entries) != 0 {
		t.Fatalf("失败任务不该落文件: %v", entries)
	}

	kept := newMediaTestEnv(t)
	kept.dev.finalState, kept.dev.finalError = "failed", "boom"
	if code := kept.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x", "--keep"); code == 0 {
		t.Fatal("失败任务应让退出码非 0")
	}
	if kept.dev.count("DELETE /jobs/"+fakeMediaJobA) != 0 {
		t.Fatal("--keep 不该删除失败任务")
	}
}

func TestMediaSubmitErrorShowsMessageCodeAndRetryAfter(t *testing.T) {
	env := newMediaTestEnv(t)
	env.dev.submitCode, env.dev.retryAfter = http.StatusTooManyRequests, "12"
	env.dev.submitBody = `{"error":{"code":"media_busy","message":"并发名额不够"}}`
	if code := env.run(t, nil, "generate", "--model", "fake-video", "--prompt", "x"); code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"并发名额不够", "media_busy", "12 秒"} {
		if !strings.Contains(env.err.String(), want) {
			t.Errorf("stderr 缺 %q: %s", want, env.err.String())
		}
	}
	if len(env.dev.calls) != 1 {
		t.Fatalf("calls=%v", env.dev.calls)
	}
}

func TestMediaPromptSources(t *testing.T) {
	env := newMediaTestEnv(t)
	promptFile := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(promptFile, []byte("  来自文件\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := env.run(t, strings.NewReader("不该读到"), "generate", "--model", "m", "--prompt-file", promptFile, "--no-wait"); code != 0 {
		t.Fatal(env.err.String())
	}
	if code := env.run(t, strings.NewReader("来自标准输入\n"), "generate", "--model", "m", "--no-wait"); code != 0 {
		t.Fatal(env.err.String())
	}
	if code := env.run(t, nil, "generate", "--model", "m", "--no-wait"); code != 0 {
		t.Fatal(env.err.String())
	}
	if env.dev.submits[0]["prompt"] != "来自文件" || env.dev.submits[1]["prompt"] != "来自标准输入" {
		t.Fatalf("submits=%v", env.dev.submits)
	}
	if _, ok := env.dev.submits[2]["prompt"]; ok {
		t.Fatalf("没有提示词来源时不该带 prompt: %v", env.dev.submits[2])
	}
	if code := env.run(t, nil, "generate", "--model", "m", "--prompt", "a", "--prompt-file", promptFile); code != 2 {
		t.Fatalf("两个提示词来源同给 code=%d", code)
	}
}

func TestMediaGenerateRejectsBadArgumentsBeforeAnyRequest(t *testing.T) {
	env := newMediaTestEnv(t)
	txt := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(txt, []byte("x"), 0600)
	for _, args := range [][]string{
		{"generate", "--prompt", "x"},
		{"generate", "--model", "m", "--count", "0"},
		{"generate", "--model", "m", "--param", "bad"},
		{"generate", "--model", "m", "--sk", "whatever"},
		{"generate", "--model", "m", "--insecure"},
		{"generate", "--model", "m", "--no-wait", "--keep"},
		{"generate", "--model", "m", "stray"},
		{"wait"},
		{"status", "../config"},
		{"nope"},
	} {
		if code := env.run(t, nil, args...); code != 2 {
			t.Errorf("%v code=%d, want 2", args, code)
		}
	}
	if code := env.run(t, nil, "generate", "--model", "m", "--first-frame", txt, "--prompt", "x"); code != 1 {
		t.Errorf("不支持的扩展名 code=%d", code)
	}
	if len(env.dev.calls) != 0 {
		t.Fatalf("参数错误不该发请求: %v", env.dev.calls)
	}
}

func TestMediaWaitStatusCancel(t *testing.T) {
	env := newMediaTestEnv(t)
	if code := env.run(t, nil, "wait", fakeMediaJobA, "--out", "done"); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, env.err.String())
	}
	if _, err := os.Stat(filepath.Join(env.dir, "done.mp4")); err != nil {
		t.Fatal(err)
	}
	if env.dev.count("DELETE /jobs/"+fakeMediaJobA) != 1 {
		t.Fatalf("calls=%v", env.dev.calls)
	}
	env.out.Reset()
	if code := env.run(t, nil, "status", fakeMediaJobB); code != 0 {
		t.Fatal(env.err.String())
	}
	if env.out.String() != fakeMediaJobB+"\tsucceeded\tfake-video\n" {
		t.Fatalf("status=%q", env.out.String())
	}
	env.out.Reset()
	if code := env.run(t, nil, "cancel", fakeMediaJobB); code != 0 {
		t.Fatal(env.err.String())
	}
	if env.dev.count("DELETE /jobs/"+fakeMediaJobB) != 1 || !strings.Contains(env.out.String(), fakeMediaJobB) {
		t.Fatalf("calls=%v out=%q", env.dev.calls, env.out.String())
	}
}

func TestMediaModelsTableAndJSON(t *testing.T) {
	env := newMediaTestEnv(t)
	if code := env.run(t, nil, "models", "--json"); code != 0 {
		t.Fatal(env.err.String())
	}
	if env.out.String() != fakeMediaModels+"\n" {
		t.Fatalf("--json 应原样输出设备响应: %q", env.out.String())
	}
	env.out.Reset()
	if code := env.run(t, nil, "models"); code != 0 {
		t.Fatal(env.err.String())
	}
	for _, want := range []string{
		"fake-video  video  subscription  可用",
		"fake-image  image  metered  不可用：模型未启用（model_not_found）",
		"generate（生成）", "带 first_frame 时可省",
		"first_frame  最多 1 个  png/jpeg  每个不超过 10 MiB",
		"duration  integer  1–15 秒", "aspect_ratio  enum  1:1 | 16:9", "voices  strings  最多 3 项",
	} {
		if !strings.Contains(env.out.String(), want) {
			t.Errorf("表格缺 %q:\n%s", want, env.out.String())
		}
	}
}

func TestMediaRequiresSavedDeviceAndRunDispatches(t *testing.T) {
	a := &app{root: t.TempDir(), cfg: emptyConfig(), hc: newHTTPClient(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if code := a.media([]string{"models"}, nil); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if commandMutates([]string{"media", "generate", "--model", "m"}) {
		t.Fatal("gate media 不该占用本机操作锁")
	}
	t.Setenv("GATE_CONFIG_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"media", "help"}, nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "gate media generate") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	var help bytes.Buffer
	printHelp(&help)
	if !strings.Contains(help.String(), "gate media") {
		t.Fatal("帮助缺少 gate media")
	}
}
