package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 任务行编码给页面时，data URI 形式的 media_url 只留头不带正文：列表 JSON 不背 base64。
func TestMediaJobJSONStripsDataURIPayload(t *testing.T) {
	task := MediaJob{ID: "X", Provider: "codex", Kind: "image", Model: "m", Status: "succeeded", MediaURL: "data:image/png;base64,QUJDQUJD", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	for _, v := range []any{task, &task} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"media_url":"data:image/png;base64,"`) || strings.Contains(string(raw), "QUJD") {
			t.Fatalf("JSON = %s", raw)
		}
	}
	task.MediaURL = "https://example.invalid/v.mp4"
	raw, _ := json.Marshal(task)
	if !strings.Contains(string(raw), `"media_url":"https://example.invalid/v.mp4"`) {
		t.Fatalf("平台 URL 应原样编码: %s", raw)
	}
}

// 完成时间在首次写成终态时记下，之后的写回（补缩略图等）不改它；未到终态为空。
func TestMediaJobFinishedAtSetOnce(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	task := MediaJob{ID: "01FIN000000000000000000000", Provider: "grok", Kind: "image", Model: "m", Prompt: "p", Status: MediaStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMediaJob(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMediaJob(ctx, task.ID)
	if got.FinishedAt != nil {
		t.Fatalf("running 任务不该有完成时间: %v", got.FinishedAt)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "finished_at") {
		t.Fatalf("未完成任务 JSON 不该带 finished_at: %s", raw)
	}
	got.Status = "succeeded"
	if err := st.UpdateMediaJob(ctx, *got); err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetMediaJob(ctx, task.ID)
	if first.FinishedAt == nil {
		t.Fatal("写成终态后应有完成时间")
	}
	time.Sleep(1100 * time.Millisecond)
	first.ThumbFile = "x.thumb.jpg"
	if err := st.UpdateMediaJob(ctx, *first); err != nil {
		t.Fatal(err)
	}
	second, _ := st.GetMediaJob(ctx, task.ID)
	if !second.FinishedAt.Equal(*first.FinishedAt) || !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("完成时间应保持 %v，得到 %v（更新时间 %v → %v）", first.FinishedAt, second.FinishedAt, first.UpdatedAt, second.UpdatedAt)
	}
	// 启动判失败同样记完成时间。
	stale := MediaJob{ID: "01FIN000000000000000000001", Provider: "grok", Kind: "image", Model: "m", Prompt: "p", Status: MediaStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMediaJob(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FailRunningMediaJobs(ctx, "中断"); err != nil {
		t.Fatal(err)
	}
	if failed, _ := st.GetMediaJob(ctx, stale.ID); failed.FinishedAt == nil {
		t.Fatal("启动判失败的任务应有完成时间")
	}
}

// 改表前的行把输入形态与 operation 混在 params 里：读取时挪到 Inputs / Operation，其余键原样。
func TestMediaJobLegacyParamsDecoded(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := fmtTime(time.Now())
	if _, err := st.db.ExecContext(ctx, `INSERT INTO media_jobs (id, provider, backend, kind, model, status, params, created_at, updated_at)
		VALUES ('01LEGACY000000000000000000', 'grok', 'grok', 'video', 'grok-imagine-video', 'succeeded',
		'{"operation":"extend","duration":6,"source_video":true,"reference_images":2}', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	j, err := st.GetMediaJob(ctx, "01LEGACY000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if j.Operation != MediaOpExtend || j.Origin != MediaOriginPage {
		t.Fatalf("operation=%q origin=%q", j.Operation, j.Origin)
	}
	if len(j.Params) != 1 || j.Params["duration"] != float64(6) {
		t.Fatalf("params = %v", j.Params)
	}
	if j.Inputs[MediaRoleSourceVideo] != 1 || j.Inputs[MediaRoleReferenceImages] != 2 || len(j.Inputs) != 2 {
		t.Fatalf("inputs = %v", j.Inputs)
	}
}

// 页面列表只列 origin=page；并发计数跨来源合计；清空不动 studio / cli 的任务。
func TestMediaJobOriginFiltering(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	jobs := []MediaJob{
		{ID: "01ORIGIN00000000000000000A", Origin: MediaOriginPage, Backend: "grok", Kind: "image", Model: "m", Status: MediaStatusRunning, KeyID: 7, CreatedAt: now},
		{ID: "01ORIGIN00000000000000000B", Origin: MediaOriginStudio, Owner: `{"workspace_id":"w"}`, Backend: "grok", Kind: "image", Model: "m", Status: MediaStatusSucceeded, KeyID: 7, CreatedAt: now},
		{ID: "01ORIGIN00000000000000000C", Origin: MediaOriginCLI, Backend: "codex", Kind: "image", Model: "m", Status: MediaStatusQueued, KeyID: 7, BatchID: "B", CreatedAt: now,
			Params: map[string]any{"aspect_ratio": "16:9"}, Inputs: map[string]int{MediaRoleReferenceImages: 2}},
	}
	if err := st.CreateMediaJobs(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if page, _ := st.ListPageMediaJobs(ctx); len(page) != 1 || page[0].ID != jobs[0].ID {
		t.Fatalf("页面列表 = %+v", page)
	}
	if mine, _ := st.ListPageMediaJobsByKey(ctx, 7); len(mine) != 1 {
		t.Fatalf("持有人列表 = %+v", mine)
	}
	if n, _ := st.CountActiveMediaJobsByKey(ctx, 7); n != 2 {
		t.Fatalf("未到终态 = %d，应为 2（page running + cli queued）", n)
	}
	if done, _ := st.ListTerminalMediaJobsByOrigin(ctx, MediaOriginStudio); len(done) != 1 || done[0].Owner != `{"workspace_id":"w"}` {
		t.Fatalf("studio 终态 = %+v", done)
	}
	cli, err := st.GetMediaJob(ctx, jobs[2].ID)
	if err != nil || cli.BatchID != "B" || cli.Params["aspect_ratio"] != "16:9" || cli.Inputs[MediaRoleReferenceImages] != 2 || cli.Provider != "codex" {
		t.Fatalf("cli 任务 = %+v, %v", cli, err)
	}
	if n, _ := st.DeletePageMediaJobs(ctx); n != 1 {
		t.Fatalf("清空删了 %d 条", n)
	}
	if _, err := st.GetMediaJob(ctx, jobs[1].ID); err != nil {
		t.Fatal("studio 任务不该被页面清空删掉")
	}
	// 同批落库是一个事务：主键冲突时一条不留。
	dup := []MediaJob{{ID: "01ORIGIN00000000000000000D", Backend: "grok", Kind: "image", Model: "m", Status: MediaStatusRunning}, {ID: jobs[1].ID, Backend: "grok", Kind: "image", Model: "m", Status: MediaStatusRunning}}
	if err := st.CreateMediaJobs(ctx, dup); err == nil {
		t.Fatal("主键冲突应失败")
	}
	if _, err := st.GetMediaJob(ctx, dup[0].ID); err == nil {
		t.Fatal("失败的批次不该留下任何一条")
	}
}
