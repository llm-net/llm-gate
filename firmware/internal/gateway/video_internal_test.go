// video_internal_test.go 钉死视频任务面的包内口径：「仅签名查询串变化不算
// URL 变更」判定（等值观测零写的 SD 卡纪律靠它成立），以及 minimax 适配器的
// 旁路解析（{task:{…}} 拆包、usage 原文字节、取消响应 action 判定、查询响应
// model 回显改写）。
package gateway

import "testing"

// TestSameURLIgnoringQuery 钉死「仅签名查询串变化不算 URL 变更」的判定：
// scheme/host/path 任一不同都算变更；解析失败按变更处理（宁多写不错存）。
func TestSameURLIgnoringQuery(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"https://cdn.example.com/a.mp4?sig=1", "https://cdn.example.com/a.mp4?sig=2", true},
		{"https://cdn.example.com/a.mp4", "https://cdn.example.com/a.mp4?sig=1", true},
		{"https://cdn.example.com/a.mp4?sig=1", "https://cdn.example.com/b.mp4?sig=1", false},
		{"https://cdn-a.example.com/a.mp4", "https://cdn-b.example.com/a.mp4", false},
		{"http://cdn.example.com/a.mp4", "https://cdn.example.com/a.mp4", false},
		{"", "https://cdn.example.com/a.mp4", false},
		{"https://cdn.example.com/a.mp4", "", false},
		{"://bad", "://bad", false},
	}
	for _, c := range cases {
		if got := sameURLIgnoringQuery(c.a, c.b); got != c.same {
			t.Errorf("sameURLIgnoringQuery(%q, %q) = %v，期望 %v", c.a, c.b, got, c.same)
		}
	}
}

// TestMinimaxParseTaskResponse：{task:{…}} 拆包、状态直通、usage 原文字节
// （不经 map 往返）、任务级 code/message 仅在 failed 时载入（code 数字/字符串
// 两种字面都收）。
func TestMinimaxParseTaskResponse(t *testing.T) {
	ad := minimaxVideoAdapter{}

	obs, err := ad.parseTaskResponse([]byte(
		`{"task":{"id":"1","status":"succeeded","content":{"url":"https://cdn/a.mp4?sig=1"},` +
			`"usage":{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}}}`))
	if err != nil {
		t.Fatalf("parseTaskResponse: %v", err)
	}
	if obs.Status != "succeeded" || obs.VideoURL != "https://cdn/a.mp4?sig=1" {
		t.Errorf("观测异常: %+v", obs)
	}
	if obs.UsageJSON != `{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}` {
		t.Errorf("usage 未存原文字节: %q", obs.UsageJSON)
	}

	obs, err = ad.parseTaskResponse([]byte(`{"task":{"id":"1","status":"failed","code":1026,"message":"sensitive"}}`))
	if err != nil {
		t.Fatalf("parseTaskResponse(failed): %v", err)
	}
	if obs.ErrorCode != "1026" || obs.ErrorMessage != "sensitive" {
		t.Errorf("任务级错误未按原字面载入: %+v", obs)
	}

	if _, err := ad.parseTaskResponse([]byte(`{"nope":true}`)); err == nil {
		t.Error("缺 task.status 的响应应报解析错误")
	}
}

// TestMinimaxParseCancelResponse：action=cancelled 才算本次取消；deleted 与
// 解析不动都按「没取消」（观测旁路宁少写不错写）。
func TestMinimaxParseCancelResponse(t *testing.T) {
	ad := minimaxVideoAdapter{}
	cases := []struct {
		body      string
		cancelled bool
	}{
		{`{"task_id":"1","action":"cancelled","status":"cancelled"}`, true},
		{`{"task_id":"1","action":"deleted","status":"succeeded"}`, false},
		{`not-json`, false},
		{`{}`, false},
	}
	for _, c := range cases {
		if got := ad.parseCancelResponse([]byte(c.body)); got != c.cancelled {
			t.Errorf("parseCancelResponse(%q) = %v，期望 %v", c.body, got, c.cancelled)
		}
	}
}

// TestMinimaxRewriteTaskModel：task.model 与客户端可见名不同才改写；缺
// task/model 或已一致不动。
func TestMinimaxRewriteTaskModel(t *testing.T) {
	ad := minimaxVideoAdapter{}
	m := map[string]any{"task": map[string]any{"model": "side-id", "status": "running"}}
	if !ad.rewriteTaskModel(m, "MiniMax-H3") {
		t.Fatal("来源侧回显应被改写")
	}
	if got := m["task"].(map[string]any)["model"]; got != "MiniMax-H3" {
		t.Errorf("改写后 model = %v", got)
	}
	if ad.rewriteTaskModel(m, "MiniMax-H3") {
		t.Error("已一致时不该报改写")
	}
	if ad.rewriteTaskModel(map[string]any{"ok": true}, "x") {
		t.Error("缺 task 时不该报改写")
	}
}
