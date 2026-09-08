// images_internal_test.go 钉死图片入口的包内口径：观察器把厂商 usage 原文、
// 出图张数与请求 size 参数记进 reqInfo 内存点位（账单事实，iteration-9 消费），
// 非流式与流式两种形态都覆盖；observe 挂在 forward→commitResponse 的 2xx 载荷
// 解析处（接线由 TestImageObserverWiring 走真实 handler 证实，"非 2xx 不观测"
// 自 iteration-9 起由 commitResponse 的结构保证，见 TestImageObserverNo2xx）。
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// decodeObj 是用例里"上游发来的一帧载荷"——与 proxy.go 交给观察器的东西同构
// （UseNumber，数字保原字面量）。
func decodeObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("用例载荷不是 JSON 对象: %v", err)
	}
	return m
}

func TestImageUsageObserver(t *testing.T) {
	const okBody = `{"model":"m","created":1,` +
		`"data":[{"url":"https://cdn/a.png","size":"2048x2048"},{"error":{"code":"x","message":"y"}}],` +
		`"usage":{"generated_images":1,"output_tokens":16464,"total_tokens":16464}}`

	cases := []struct {
		name     string
		payload  map[string]any
		frames   []string // 依次喂给观察器的载荷（流式即多帧）
		count    int
		reqSize  string
		usageHas string // 期望 usage 原文包含的片段；空 = 期望未记
	}{
		{"2xx出图", map[string]any{"size": "2K"}, []string{okBody}, 2, "2K", `"output_tokens":16464`},
		{"无size参数", map[string]any{}, []string{okBody}, 2, "", `"generated_images":1`},
		{"usage为null不记", map[string]any{"size": "2K"},
			[]string{`{"data":[{"url":"u"}],"usage":null}`}, 1, "2K", ""},
		// 流式出图：逐张事件计数，usage 只挂在完成事件上——这一路不覆盖
		// 就是整条流式出图漏账。
		{"流式逐张与完成事件", map[string]any{"size": "2K"}, []string{
			`{"type":"image_generation.partial_succeeded","image_index":0,"url":"https://cdn/a.png"}`,
			`{"type":"image_generation.partial_succeeded","image_index":1,"url":"https://cdn/b.png"}`,
			`{"type":"image_generation.completed","model":"m","usage":{"generated_images":2,"output_tokens":32928}}`,
		}, 2, "2K", `"generated_images":2`},
		// 只出了一张就失败：失败事件不计张数，也没有 usage。
		{"流式部分失败", map[string]any{}, []string{
			`{"type":"image_generation.partial_succeeded","image_index":0,"url":"https://cdn/a.png"}`,
			`{"type":"image_generation.partial_failed","image_index":1,"error":{"code":"x"}}`,
		}, 1, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := &reqInfo{}
			obs := imageUsageObserver(info, c.payload)
			for _, f := range c.frames {
				obs(decodeObj(t, f))
			}
			if info.imageCount != c.count {
				t.Errorf("imageCount = %d，期望 %d", info.imageCount, c.count)
			}
			if info.imageReqSize != c.reqSize {
				t.Errorf("imageReqSize = %q，期望 %q", info.imageReqSize, c.reqSize)
			}
			if c.usageHas == "" {
				if info.imageUsage != "" {
					t.Errorf("imageUsage = %q，期望未记", info.imageUsage)
				}
			} else if !strings.Contains(info.imageUsage, c.usageHas) {
				t.Errorf("imageUsage = %q，期望含 %q（厂商原文）", info.imageUsage, c.usageHas)
			}
		})
	}
}

// runImageWire 走真实 handler 的图片全路径：假上游按 respond 回话，
// 直调 handler（绕过认证中间件，reqInfo 手工注入）后交回本请求的 reqInfo
// 与响应记录器。
func runImageWire(t *testing.T, respond http.HandlerFunc) (*reqInfo, *httptest.ResponseRecorder) {
	t.Helper()
	upstream := httptest.NewServer(respond)
	t.Cleanup(upstream.Close)

	cfg := &config.Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), LogLevel: "debug"}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := st.CreateUpstream(t.Context(), "ark-img", config.UpstreamArk, "k", upstream.URL)
	if err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	m, err := st.CreateModel(t.Context(), "seedream-wire", store.ModelKindImage, "")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := st.CreateModelSource(t.Context(), m.ID, up.ID, "doubao-seedream-4-0-250828", 100); err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	s := New(cfg, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, nil)

	info := &reqInfo{id: "wire-test"}
	r := httptest.NewRequest("POST", "/ark/api/v3/images/generations",
		strings.NewReader(`{"model":"seedream-wire","prompt":"p","size":"1024x1024"}`))
	r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info))
	w := httptest.NewRecorder()
	s.handleArkImagesGenerations(w, r)
	return info, w
}

// TestImageObserverWiring：真实 handler 全路径——httptest 假上游出图后，
// 观察器经 forward→commitResponse 的 observe 钩子把账单事实记进本请求的
// reqInfo（响应侧 model 回写照常发生，观测拿的是改写前的载荷）。
func TestImageObserverWiring(t *testing.T) {
	info, w := runImageWire(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"doubao-seedream-4-0-250828","created":2,`+
			`"data":[{"url":"https://cdn/a.png","size":"2048x2048"}],`+
			`"usage":{"generated_images":1,"output_tokens":100,"total_tokens":100}}`)
	})

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model":"seedream-wire"`) {
		t.Errorf("响应 model 未回写: %s", w.Body.String())
	}
	if info.imageCount != 1 || info.imageReqSize != "1024x1024" {
		t.Errorf("账单事实未记全: count=%d reqSize=%q", info.imageCount, info.imageReqSize)
	}
	if !strings.Contains(info.imageUsage, `"output_tokens":100`) {
		t.Errorf("imageUsage = %q，期望厂商 usage 原文", info.imageUsage)
	}
	if info.attempts != 1 || info.upstream != "ark-img" {
		t.Errorf("选路字段未记: attempts=%d upstream=%q", info.attempts, info.upstream)
	}
	// 记账维度：图片入口 + 模型 kind 与目录价（选路时回填的记账时点价）。
	if info.bill.entry != "image" || info.bill.model != "seedream-wire" ||
		info.bill.kind != store.ModelKindImage || info.bill.upstreamType != config.UpstreamArk {
		t.Errorf("记账维度未记全: %+v", info.bill)
	}
}

// TestImageObserverNo2xx：上游错误体不观测——observe 只挂在 2xx 载荷解析处，
// 错误体一律原样透传（commitResponse 的结构保证，不靠观察器自己判状态码）。
func TestImageObserverNo2xx(t *testing.T) {
	info, w := runImageWire(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// 错误体里放一个同名 usage 字段：观测若不看状态码就会误记。
		io.WriteString(w, `{"error":{"code":"e","message":"m"},"usage":{"generated_images":9}}`)
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400 原样透传", w.Code)
	}
	if info.imageUsage != "" || info.imageCount != 0 || info.imageReqSize != "" {
		t.Errorf("非 2xx 不应观测: usage=%q count=%d size=%q",
			info.imageUsage, info.imageCount, info.imageReqSize)
	}
}
