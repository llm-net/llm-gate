package store

// 主机智能体仓储验收：档案读写、指令状态机的时刻列、事件的三种分页与种类过滤，
// 以及解除纳管时三张表随主机行级联删除。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAgentHostProfileRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "10.0.0.7")

	p, err := s.GetAgentHostProfile(ctx, h.ID)
	if err != nil || p.Content != "" || !p.UpdatedAt.IsZero() {
		t.Fatalf("空档案 = %+v, %v", p, err)
	}
	at := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	if _, err := s.SetAgentHostProfile(ctx, h.ID, "# 板子\n- 角色：网关", "agent", at); err != nil {
		t.Fatalf("SetAgentHostProfile: %v", err)
	}
	if _, err := s.SetAgentHostProfile(ctx, h.ID, "# 板子 v2", "admin", at.Add(time.Minute)); err != nil {
		t.Fatalf("SetAgentHostProfile(2): %v", err)
	}
	p, err = s.GetAgentHostProfile(ctx, h.ID)
	if err != nil || p.Content != "# 板子 v2" || p.UpdatedBy != "admin" || !p.UpdatedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("档案 = %+v, %v", p, err)
	}
	if _, err := s.SetAgentHostProfile(ctx, 999, "x", "admin", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主机 err = %v", err)
	}
}

func TestAgentHostRunLifecycle(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "10.0.0.8")
	t0 := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	r1, err := s.CreateAgentHostRun(ctx, NewAgentHostRun{HostID: h.ID, Text: "看看磁盘", ImageCount: 1, Engine: "codex_app_server", Model: "gpt-5.5", CreatedAt: t0})
	if err != nil {
		t.Fatalf("CreateAgentHostRun: %v", err)
	}
	if r1.Status != AgentRunQueued || r1.ImageCount != 1 || r1.StartedAt != nil || r1.FinishedAt != nil || len(r1.ID) != 26 {
		t.Fatalf("新指令 = %+v", r1)
	}
	r2, err := s.CreateAgentHostRun(ctx, NewAgentHostRun{HostID: h.ID, Text: "再看看内存", CreatedAt: t0.Add(time.Second)})
	if err != nil {
		t.Fatalf("CreateAgentHostRun(2): %v", err)
	}
	if _, err := s.CreateAgentHostRun(ctx, NewAgentHostRun{HostID: 999, Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主机 err = %v", err)
	}

	active, err := s.ListActiveAgentHostRuns(ctx)
	if err != nil || len(active) != 2 || active[0].ID != r1.ID {
		t.Fatalf("未完成指令 = %+v, %v", active, err)
	}
	got, err := s.SetAgentHostRunStatus(ctx, r1.ID, AgentRunRunning, "", t0.Add(2*time.Second))
	if err != nil || got.Status != AgentRunRunning || got.StartedAt == nil || !got.StartedAt.Equal(t0.Add(2*time.Second)) || got.FinishedAt != nil {
		t.Fatalf("running = %+v, %v", got, err)
	}
	if err := s.SetAgentHostRunUsage(ctx, r1.ID, 1200, 80); err != nil {
		t.Fatalf("SetAgentHostRunUsage: %v", err)
	}
	got, err = s.SetAgentHostRunStatus(ctx, r1.ID, AgentRunSucceeded, "", t0.Add(9*time.Second))
	if err != nil || got.Status != AgentRunSucceeded || got.FinishedAt == nil || !got.FinishedAt.Equal(t0.Add(9*time.Second)) ||
		got.StartedAt == nil || !got.StartedAt.Equal(t0.Add(2*time.Second)) || got.InputTokens != 1200 || got.OutputTokens != 80 {
		t.Fatalf("succeeded = %+v, %v", got, err)
	}
	got, err = s.SetAgentHostRunStatus(ctx, r2.ID, AgentRunCancelled, "管理员取消", t0.Add(3*time.Second))
	if err != nil || got.Status != AgentRunCancelled || got.Error != "管理员取消" || got.StartedAt != nil || got.FinishedAt == nil {
		t.Fatalf("cancelled = %+v, %v", got, err)
	}
	if _, err := s.SetAgentHostRunStatus(ctx, "nope", AgentRunFailed, "x", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的指令 err = %v", err)
	}
	if _, err := s.SetAgentHostRunStatus(ctx, r2.ID, "weird", "", t0); err == nil {
		t.Fatal("非法状态该被拒")
	}
	list, err := s.ListAgentHostRuns(ctx, h.ID, 10)
	if err != nil || len(list) != 2 || list[0].ID != r2.ID || list[1].ID != r1.ID {
		t.Fatalf("倒序列表 = %+v, %v", list, err)
	}
	active, err = s.ListActiveAgentHostRuns(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("终态后未完成指令 = %+v, %v", active, err)
	}
}

func TestAgentHostEventsPagingAndCascade(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "10.0.0.9")
	other := newHost(t, s, "10.0.0.10")
	t0 := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	kinds := []string{AgentEventUser, AgentEventCommand, AgentEventAssistant, AgentEventCommand, AgentEventProfile}
	var ids []int64
	for i, k := range kinds {
		code := i
		ev, err := s.AppendAgentHostEvent(ctx, AgentHostEvent{HostID: h.ID, RunID: "run1", At: t0.Add(time.Duration(i) * time.Second),
			Kind: k, Title: "t", Body: "b", Meta: `{"n":1}`, ExitCode: &code, DurationMs: int64(i * 10)})
		if err != nil {
			t.Fatalf("AppendAgentHostEvent(%d): %v", i, err)
		}
		ids = append(ids, ev.ID)
	}
	if _, err := s.AppendAgentHostEvent(ctx, AgentHostEvent{HostID: other.ID, Kind: AgentEventUser, Body: "别家的"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAgentHostEvent(ctx, AgentHostEvent{HostID: 999, Kind: AgentEventUser}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主机 err = %v", err)
	}

	all, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID})
	if err != nil || len(all) != 5 || all[0].ID != ids[0] || all[4].ID != ids[4] {
		t.Fatalf("全部（升序）= %+v, %v", all, err)
	}
	if all[3].ExitCode == nil || *all[3].ExitCode != 3 || all[3].DurationMs != 30 || all[3].Meta != `{"n":1}` || !all[3].At.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("第 4 条 = %+v", all[3])
	}
	recent, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, Limit: 2})
	if err != nil || len(recent) != 2 || recent[0].ID != ids[3] || recent[1].ID != ids[4] {
		t.Fatalf("最近两条 = %+v, %v", recent, err)
	}
	after, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, AfterID: ids[2]})
	if err != nil || len(after) != 2 || after[0].ID != ids[3] {
		t.Fatalf("after = %+v, %v", after, err)
	}
	before, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, BeforeID: ids[3], Limit: 2})
	if err != nil || len(before) != 2 || before[0].ID != ids[1] || before[1].ID != ids[2] {
		t.Fatalf("before = %+v, %v", before, err)
	}
	ops, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, Kinds: []string{AgentEventCommand, AgentEventProfile}})
	if err != nil || len(ops) != 3 || ops[0].Kind != AgentEventCommand || ops[2].Kind != AgentEventProfile {
		t.Fatalf("操作日志 = %+v, %v", ops, err)
	}

	// 解除纳管：档案、指令与事件随主机行级联删除，别家的不受影响。
	if _, err := s.SetAgentHostProfile(ctx, h.ID, "x", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentHostRun(ctx, NewAgentHostRun{HostID: h.ID, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgentHost(ctx, h.ID); err != nil {
		t.Fatalf("DeleteAgentHost: %v", err)
	}
	gone, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID})
	if err != nil || len(gone) != 0 {
		t.Fatalf("删主机后事件 = %+v, %v", gone, err)
	}
	if runs, err := s.ListAgentHostRuns(ctx, h.ID, 10); err != nil || len(runs) != 0 {
		t.Fatalf("删主机后指令 = %+v, %v", runs, err)
	}
	if p, err := s.GetAgentHostProfile(ctx, h.ID); err != nil || p.Content != "" {
		t.Fatalf("删主机后档案 = %+v, %v", p, err)
	}
	kept, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: other.ID})
	if err != nil || len(kept) != 1 {
		t.Fatalf("别家的事件 = %+v, %v", kept, err)
	}
}

func TestAgentHostChats(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "10.0.0.11")
	t0 := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	c1, err := s.CreateAgentHostChat(ctx, NewAgentHostChat{HostID: h.ID, Title: "装 Docker", Engine: "codex_app_server", KeyID: 7,
		KeyDisplay: "sk_abc…wxyz", Model: "gpt-5.5", Effort: "high", Instructions: "你是…", CreatedAt: t0})
	if err != nil {
		t.Fatalf("CreateAgentHostChat: %v", err)
	}
	if c1.ID == "" || c1.HostID != h.ID || c1.Title != "装 Docker" || c1.KeyID != 7 || c1.KeyDisplay != "sk_abc…wxyz" || c1.Model != "gpt-5.5" ||
		c1.Effort != "high" || c1.Instructions != "你是…" || !c1.CreatedAt.Equal(t0) || !c1.UpdatedAt.Equal(t0) {
		t.Fatalf("对话 = %+v", c1)
	}
	if _, err := s.CreateAgentHostChat(ctx, NewAgentHostChat{HostID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主机 err = %v", err)
	}
	c2, err := s.CreateAgentHostChat(ctx, NewAgentHostChat{HostID: h.ID, CreatedAt: t0.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	// 列表按活动时刻倒序；改标题 / 提交指令刷新活动时刻。
	list, err := s.ListAgentHostChats(ctx, h.ID)
	if err != nil || len(list) != 2 || list[0].ID != c2.ID || list[1].ID != c1.ID {
		t.Fatalf("列表 = %+v, %v", list, err)
	}
	if err := s.SetAgentHostChatTitle(ctx, c1.ID, "装 Docker 与 compose", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentHostChatTitle(ctx, "nope", "x", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改不存在的标题 err = %v", err)
	}
	list, _ = s.ListAgentHostChats(ctx, h.ID)
	if list[0].ID != c1.ID || list[0].Title != "装 Docker 与 compose" {
		t.Fatalf("改标题后列表 = %+v", list)
	}
	if err := s.TouchAgentHostChat(ctx, c2.ID, t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListAgentHostChats(ctx, c2.HostID)
	if list[0].ID != c2.ID {
		t.Fatalf("刷新后列表 = %+v", list)
	}

	// 指令与事件按对话归属；主机级事件 chat_id 为空。
	r1, err := s.CreateAgentHostRun(ctx, NewAgentHostRun{HostID: h.ID, ChatID: c1.ID, Text: "一"})
	if err != nil || r1.ChatID != c1.ID {
		t.Fatalf("指令 = %+v, %v", r1, err)
	}
	for _, ev := range []AgentHostEvent{
		{HostID: h.ID, ChatID: c1.ID, RunID: r1.ID, Kind: AgentEventUser, Body: "一"},
		{HostID: h.ID, ChatID: c1.ID, RunID: r1.ID, Kind: AgentEventCommand, Title: "uname -a"},
		{HostID: h.ID, ChatID: c1.ID, RunID: r1.ID, Kind: AgentEventAssistant, Body: "好了"},
		{HostID: h.ID, ChatID: c2.ID, Kind: AgentEventUser, Body: "二"},
		{HostID: h.ID, Kind: AgentEventProfile, Title: "admin", Body: "# x"},
	} {
		if _, err := s.AppendAgentHostEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	chat1, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, ChatID: c1.ID})
	if err != nil || len(chat1) != 3 || chat1[0].ChatID != c1.ID || chat1[2].Kind != AgentEventAssistant {
		t.Fatalf("对话 1 事件 = %+v, %v", chat1, err)
	}
	all, err := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID})
	if err != nil || len(all) != 5 {
		t.Fatalf("主机全部事件 = %+v, %v", all, err)
	}

	// 删对话：指令与回复没了，命令 / 指令记录改挂主机；另一个对话不受影响。
	if err := s.DeleteAgentHostChat(ctx, c1.ID); err != nil {
		t.Fatalf("DeleteAgentHostChat: %v", err)
	}
	if err := s.DeleteAgentHostChat(ctx, c1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("再删 err = %v", err)
	}
	if _, err := s.GetAgentHostRun(ctx, r1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删对话后指令 err = %v", err)
	}
	if got, _ := s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID, ChatID: c1.ID}); len(got) != 0 {
		t.Fatalf("删对话后仍有归属事件: %+v", got)
	}
	all, _ = s.ListAgentHostEvents(ctx, AgentHostEventQuery{HostID: h.ID})
	kinds := ""
	for _, ev := range all {
		kinds += ev.Kind + ","
	}
	if kinds != "user,command,user,profile," || all[0].ChatID != "" || all[1].ChatID != "" || all[2].ChatID != c2.ID {
		t.Fatalf("删对话后主机事件 = %+v", all)
	}
	list, _ = s.ListAgentHostChats(ctx, h.ID)
	if len(list) != 1 || list[0].ID != c2.ID {
		t.Fatalf("删后列表 = %+v", list)
	}
	// 解除纳管：对话随主机级联删除。
	if err := s.DeleteAgentHost(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAgentHostChat(ctx, c2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删主机后对话 err = %v", err)
	}
}
