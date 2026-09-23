package store

// workspaces 仓储验收：建行 / 列表 / 点查 / 删的形状、同主机重名冲突、随主机级联删除、
// 凭证删掉只置空、入参白名单。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWorkspaceLifecycle(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	host, err := s.CreateAgentHost(ctx, NewAgentHost{Kind: AgentHostKindWorker, Address: "192.168.1.10", Port: 22, Username: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	cred := newGitCredential(t, s, "github.com", "octocat")

	if list, err := s.ListWorkspaces(ctx); err != nil || len(list) != 0 {
		t.Fatalf("初始清单 = %+v（err=%v）", list, err)
	}
	w, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID, Name: "demo", Path: "/home/pi/workspaces/demo",
		RepoURL: "https://github.com/octocat/hello.git", Branch: "main", CredentialID: cred.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if len(w.ID) != 26 || w.HostID != host.ID || w.Name != "demo" || w.Path != "/home/pi/workspaces/demo" ||
		w.RepoURL != "https://github.com/octocat/hello.git" || w.Branch != "main" || w.CredentialID != cred.ID {
		t.Fatalf("新行 = %+v", w)
	}
	if got, err := s.GetWorkspaceByName(ctx, host.ID, "demo"); err != nil || got.ID != w.ID {
		t.Fatalf("按名点查 = %+v（err=%v）", got, err)
	}
	if _, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID, Name: "demo", Path: "/x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重名应 ErrConflict，得到 %v", err)
	}
	if _, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID + 99, Name: "other", Path: "/x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("主机不存在应 ErrConflict（外键），得到 %v", err)
	}
	// 空目录、无凭证的空间。
	empty, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID, Name: "scratch", Path: "/home/pi/workspaces/scratch"})
	if err != nil || empty.RepoURL != "" || empty.CredentialID != "" {
		t.Fatalf("空目录空间 = %+v（err=%v）", empty, err)
	}

	// 凭证删掉：空间还在，凭证引用置空。
	if err := s.DeleteCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkspace(ctx, w.ID); err != nil || got.CredentialID != "" {
		t.Fatalf("删凭证后 = %+v（err=%v）", got, err)
	}
	list, err := s.ListWorkspaces(ctx)
	if err != nil || len(list) != 2 || list[0].ID != w.ID || list[1].ID != empty.ID {
		t.Fatalf("清单 = %+v（err=%v）", list, err)
	}

	if err := s.DeleteWorkspace(ctx, empty.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ctx, empty.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，得到 %v", err)
	}
	// 主机解除纳管：空间级联删除。
	if err := s.DeleteAgentHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspace(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("主机删除后空间应级联删除，得到 %v", err)
	}
}

func TestWorkspaceNormalizers(t *testing.T) {
	for _, name := range []string{"demo", "a", "my-app_1.2", "Abc123456789012345678901"} {
		if _, err := NormalizeWorkspaceName(name); err != nil {
			t.Errorf("名称 %q 应合法：%v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "-x", "a b", "a/b", "a'b", `a"b`, "a$b", "Abc1234567890123456789012", "中文"} {
		if _, err := NormalizeWorkspaceName(name); !errors.Is(err, ErrInvalidWorkspace) {
			t.Errorf("名称 %q 应拒绝，得到 %v", name, err)
		}
	}
	for _, u := range []string{"", "https://github.com/o/r.git", "http://10.0.0.1:3000/o/r", "ssh://git@github.com/o/r.git", "git@gitee.com:o/r.git"} {
		if _, err := NormalizeRepoURL(u); err != nil {
			t.Errorf("地址 %q 应合法：%v", u, err)
		}
	}
	for _, u := range []string{"-https://x", "https://user:pw@github.com/o/r", "https://x/a b", "https://x/a'b", "https://x/$HOME", "ftp://x/y", "github.com/o/r", "https://x/`id`"} {
		if _, err := NormalizeRepoURL(u); !errors.Is(err, ErrInvalidWorkspace) {
			t.Errorf("地址 %q 应拒绝，得到 %v", u, err)
		}
	}
	for _, b := range []string{"", "main", "feature/x-1", "v1.2.3"} {
		if _, err := NormalizeBranch(b); err != nil {
			t.Errorf("分支 %q 应合法：%v", b, err)
		}
	}
	for _, b := range []string{"-x", "a..b", "a/", "a.lock", "a//b", "a/.b", "a b", "a'b", "a$b"} {
		if _, err := NormalizeBranch(b); !errors.Is(err, ErrInvalidWorkspace) {
			t.Errorf("分支 %q 应拒绝，得到 %v", b, err)
		}
	}
}

// 创作工作空间：没有主机、没有仓库；设备上重名冲突；对话 / 指令 / 事件 / 文件附注随空间级联删除。
func TestStudioWorkspaceLifecycle(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	ws, err := s.CreateWorkspace(ctx, NewWorkspace{Kind: WorkspaceKindStudio, Name: "poster", Path: "/var/lib/llmgate/workspaces/x", Template: "short-drama"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws.Kind != WorkspaceKindStudio || ws.HostID != 0 || !ws.IsStudio() || ws.Template != "short-drama" {
		t.Fatalf("新行 = %+v", ws)
	}
	if got, err := s.GetWorkspaceByName(ctx, 0, "poster"); err != nil || got.ID != ws.ID {
		t.Fatalf("按名点查 = %+v（err=%v）", got, err)
	}
	if _, err := s.CreateWorkspace(ctx, NewWorkspace{Kind: WorkspaceKindStudio, Name: "poster", Path: "/y", Template: "general"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重名应 ErrConflict，得到 %v", err)
	}
	for _, bad := range []NewWorkspace{
		{Kind: WorkspaceKindStudio, Name: "a", Path: "/p", HostID: -1, Template: "general"},
		{Kind: WorkspaceKindStudio, Name: "a", Path: "/p", RepoURL: "https://github.com/x/y.git", Template: "general"},
		// 创作类型必填且只能是小写字母 / 数字 / 连字符。
		{Kind: WorkspaceKindStudio, Name: "a", Path: "/p"},
		{Kind: WorkspaceKindStudio, Name: "a", Path: "/p", Template: "Short_Drama"},
		{Kind: WorkspaceKindDev, Name: "a", Path: "/p"},
		{Kind: "other", Name: "a", Path: "/p"},
	} {
		if _, err := s.CreateWorkspace(ctx, bad); !errors.Is(err, ErrInvalidWorkspace) {
			t.Fatalf("%+v 应 ErrInvalidWorkspace，得到 %v", bad, err)
		}
	}
	// 旧形态：不带 Kind 即开发工作空间，需要主机。
	host, err := s.CreateAgentHost(ctx, NewAgentHost{Kind: AgentHostKindWorker, Address: "10.0.0.2", Port: 22, Username: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID, Name: "poster", Path: "/home/pi/workspaces/poster"})
	if err != nil || dev.Kind != WorkspaceKindDev || dev.Template != "" {
		t.Fatalf("开发工作空间 = %+v（err=%v）", dev, err)
	}
	// 开发工作空间没有创作类型。
	if _, err := s.CreateWorkspace(ctx, NewWorkspace{HostID: host.ID, Name: "typed", Path: "/home/pi/workspaces/typed", Template: "general"}); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("开发工作空间带创作类型应 ErrInvalidWorkspace，得到 %v", err)
	}
	// 主机上的创作工作空间：与同主机上的开发工作空间共用名字空间（同一个 ~/workspaces/<name>）。
	if _, err := s.CreateWorkspace(ctx, NewWorkspace{Kind: WorkspaceKindStudio, HostID: host.ID, Name: "poster", Path: "/home/pi/workspaces/poster", Template: "general"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("主机上重名应 ErrConflict，得到 %v", err)
	}
	onHost, err := s.CreateWorkspace(ctx, NewWorkspace{Kind: WorkspaceKindStudio, HostID: host.ID, Name: "studio-on-host", Path: "/home/pi/workspaces/studio-on-host", Template: "general"})
	if err != nil || !onHost.IsStudio() || !onHost.OnHost() || onHost.HostID != host.ID || onHost.Template != "general" {
		t.Fatalf("主机上的创作工作空间 = %+v（err=%v）", onHost, err)
	}
	if got, err := s.GetWorkspaceByName(ctx, host.ID, "studio-on-host"); err != nil || got.ID != onHost.ID {
		t.Fatalf("按主机与名字点查 = %+v（err=%v）", got, err)
	}
	if err := s.DeleteWorkspace(ctx, onHost.ID); err != nil {
		t.Fatal(err)
	}

	chat, err := s.CreateStudioChat(ctx, NewStudioChat{WorkspaceID: ws.ID, Title: "海报", Engine: "codex", KeyID: 1, KeyDisplay: "sk…1", Model: "m", Instructions: "i"})
	if err != nil {
		t.Fatalf("CreateStudioChat: %v", err)
	}
	if _, err := s.CreateStudioChat(ctx, NewStudioChat{WorkspaceID: "nope", KeyID: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空间不存在应 ErrNotFound，得到 %v", err)
	}
	run, err := s.CreateStudioRun(ctx, NewStudioRun{WorkspaceID: ws.ID, ChatID: chat.ID, Text: "画一张海报", ImageCount: 1, Engine: "codex", Model: "m"})
	if err != nil || run.Status != AgentRunQueued {
		t.Fatalf("CreateStudioRun = %+v（err=%v）", run, err)
	}
	if _, err := s.SetStudioRunStatus(ctx, run.ID, AgentRunRunning, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStudioRunUsage(ctx, run.ID, 10, 3); err != nil {
		t.Fatal(err)
	}
	done, err := s.SetStudioRunStatus(ctx, run.ID, AgentRunSucceeded, "", time.Now())
	if err != nil || done.StartedAt == nil || done.FinishedAt == nil || done.InputTokens != 10 || done.OutputTokens != 3 {
		t.Fatalf("终态 = %+v（err=%v）", done, err)
	}
	for i, kind := range []string{StudioEventUser, StudioEventTool, StudioEventGenerate, StudioEventAssistant} {
		if _, err := s.AppendStudioEvent(ctx, StudioEvent{WorkspaceID: ws.ID, ChatID: chat.ID, RunID: run.ID, Kind: kind, Body: fmt.Sprint(i)}); err != nil {
			t.Fatalf("AppendStudioEvent: %v", err)
		}
	}
	all, err := s.ListStudioEvents(ctx, StudioEventQuery{ChatID: chat.ID})
	if err != nil || len(all) != 4 || all[0].Kind != StudioEventUser || all[3].Kind != StudioEventAssistant {
		t.Fatalf("事件 = %+v（err=%v）", all, err)
	}
	after, err := s.ListStudioEvents(ctx, StudioEventQuery{ChatID: chat.ID, AfterID: all[1].ID})
	if err != nil || len(after) != 2 || after[0].ID != all[2].ID {
		t.Fatalf("AfterID = %+v（err=%v）", after, err)
	}
	only, err := s.ListStudioEvents(ctx, StudioEventQuery{ChatID: chat.ID, Kinds: []string{StudioEventGenerate}})
	if err != nil || len(only) != 1 || only[0].Kind != StudioEventGenerate {
		t.Fatalf("Kinds = %+v（err=%v）", only, err)
	}
	if chats, err := s.ListStudioChats(ctx, ws.ID); err != nil || len(chats) != 1 || chats[0].ID != chat.ID {
		t.Fatalf("对话清单 = %+v（err=%v）", chats, err)
	}

	f, err := s.UpsertStudioFile(ctx, StudioFile{WorkspaceID: ws.ID, Name: "hero.png", Kind: StudioFileImage, Mime: "image/png", Bytes: 12, Width: 4, Height: 3,
		Origin: StudioFileOriginGenerated, Provider: "codex", Model: "gpt-image-2", Prompt: "海报", Params: `{"aspect_ratio":"16:9"}`, ChatID: chat.ID, RunID: run.ID})
	if err != nil || f.Name != "hero.png" || f.Width != 4 || f.Prompt != "海报" {
		t.Fatalf("UpsertStudioFile = %+v（err=%v）", f, err)
	}
	if _, err := s.UpsertStudioFile(ctx, StudioFile{WorkspaceID: ws.ID, Name: "hero.png", Kind: StudioFileImage, Bytes: 20, Origin: StudioFileOriginUpload}); err != nil {
		t.Fatalf("覆盖: %v", err)
	}
	if got, err := s.GetStudioFile(ctx, ws.ID, "hero.png"); err != nil || got.Bytes != 20 || got.Origin != StudioFileOriginUpload || got.Prompt != "" {
		t.Fatalf("覆盖后 = %+v（err=%v）", got, err)
	}
	if err := s.RenameStudioFile(ctx, ws.ID, "hero.png", "cover.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetStudioFile(ctx, ws.ID, "hero.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改名后原名应 ErrNotFound，得到 %v", err)
	}
	if files, err := s.ListStudioFiles(ctx, ws.ID); err != nil || len(files) != 1 || files[0].Name != "cover.png" {
		t.Fatalf("文件清单 = %+v（err=%v）", files, err)
	}
	if err := s.DeleteStudioFile(ctx, ws.ID, "cover.png"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStudioFile(ctx, ws.ID, "cover.png"); err != nil {
		t.Fatalf("重复删除应算成功，得到 %v", err)
	}

	// 删对话：指令与事件级联。
	chat2, _ := s.CreateStudioChat(ctx, NewStudioChat{WorkspaceID: ws.ID, KeyID: 1})
	run2, _ := s.CreateStudioRun(ctx, NewStudioRun{WorkspaceID: ws.ID, ChatID: chat2.ID, Text: "x"})
	if err := s.DeleteStudioChat(ctx, chat2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetStudioRun(ctx, run2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删对话后指令应 ErrNotFound，得到 %v", err)
	}
	if err := s.DeleteStudioChat(ctx, chat2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删对话应 ErrNotFound，得到 %v", err)
	}
	// 删空间：对话 / 指令 / 事件 / 文件附注全部级联。
	if _, err := s.UpsertStudioFile(ctx, StudioFile{WorkspaceID: ws.ID, Name: "a.txt", Kind: StudioFileText, Origin: StudioFileOriginAgent}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetStudioChat(ctx, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删空间后对话应 ErrNotFound，得到 %v", err)
	}
	if _, err := s.GetStudioRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删空间后指令应 ErrNotFound，得到 %v", err)
	}
	if evs, _ := s.ListStudioEvents(ctx, StudioEventQuery{ChatID: chat.ID}); len(evs) != 0 {
		t.Fatalf("删空间后事件应清空，得到 %d 条", len(evs))
	}
	if files, _ := s.ListStudioFiles(ctx, ws.ID); len(files) != 0 {
		t.Fatalf("删空间后文件附注应清空，得到 %d 条", len(files))
	}
	if list, err := s.ListWorkspaces(ctx); err != nil || len(list) != 1 || list[0].ID != dev.ID {
		t.Fatalf("清单 = %+v（err=%v）", list, err)
	}
}

// 迁移 0054：已有对话行里的「生成模型」一节裁掉（有模型 / 没模型两种形态），其余原样；
// 新表与归档列可用。
// 0055：存量创作工作空间的创作类型补成 general，开发工作空间保持空。
func TestMigration0055StudioWorkspaceTemplate(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 54)
	for _, row := range []string{
		`INSERT INTO agent_hosts (id, kind, name, address, port, username, status, created_at, updated_at) VALUES (7, 'worker', 'pi', '10.0.0.7', 22, 'pi', 'ready', '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
		`INSERT INTO workspaces (id, kind, name, path, created_at, updated_at) VALUES ('ws1', 'studio', 'poster', '/x', '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
		`INSERT INTO workspaces (id, kind, host_id, name, path, created_at, updated_at) VALUES ('ws2', 'dev', 7, 'code', '/home/pi/workspaces/code', '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
	} {
		if _, err := db.Exec(row); err != nil {
			t.Fatalf("插入存量行: %v\n%s", err, row)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if ws, err := s.GetWorkspace(ctx, "ws1"); err != nil || ws.Template != "general" {
		t.Fatalf("存量创作工作空间 = %+v（err=%v）", ws, err)
	}
	if ws, err := s.GetWorkspace(ctx, "ws2"); err != nil || ws.Template != "" {
		t.Fatalf("存量开发工作空间 = %+v（err=%v）", ws, err)
	}
}

func TestMigration0054StudioMediaConfig(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 53)
	if _, err := db.Exec(`INSERT INTO workspaces (id, kind, name, path, created_at, updated_at) VALUES ('ws1', 'studio', 'poster', '/x', '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("插入存量工作空间: %v", err)
	}
	head, tail := "你是创作智能体。\n\n## 工作空间\n- 名称：poster\n\n", "## 工作方式\n- 先看素材再动手\n\n## 当前目录\n（目录是空的。）\n"
	for id, mid := range map[string]string{
		"with":    "## 可用的生成模型（按对话选定的 API 密钥）\n- `gpt-image-2`（图像）\n\n",
		"without": "## 生成模型\n- 这把密钥当前没有可用的图像 / 视频生成模型\n\n",
		"plain":   "",
	} {
		if _, err := db.Exec(`INSERT INTO studio_chats (id, workspace_id, instructions, created_at, updated_at) VALUES (?, 'ws1', ?, '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
			id, head+mid+tail); err != nil {
			t.Fatalf("插入存量对话 %s: %v", id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{"with", "without", "plain"} {
		chat, err := s.GetStudioChat(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if chat.Instructions != head+tail || chat.ArchivedAt != nil {
			t.Errorf("对话 %s 迁移后: archived=%v\n%s", id, chat.ArchivedAt, chat.Instructions)
		}
	}

	// 白名单整份替换、按模型名列出、随工作空间级联；不存在的工作空间 ErrNotFound。
	if err := s.ReplaceStudioMediaModels(ctx, "ws1", []StudioMediaModel{{Model: "b", Usage: "草图"}, {Model: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceStudioMediaModels(ctx, "ws1", []StudioMediaModel{{Model: "c", Usage: "成稿"}, {Model: "a"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListStudioMediaModels(ctx, "ws1")
	if err != nil || len(got) != 2 || got[0] != (StudioMediaModel{Model: "a"}) || got[1] != (StudioMediaModel{Model: "c", Usage: "成稿"}) {
		t.Fatalf("ListStudioMediaModels = %+v, %v", got, err)
	}
	if err := s.ReplaceStudioMediaModels(ctx, "nope", []StudioMediaModel{{Model: "a"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的工作空间应 ErrNotFound，得到 %v", err)
	}

	// 归档：记时刻，重复归档保持原时刻。
	at := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	if err := s.ArchiveStudioChat(ctx, "plain", at); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveStudioChat(ctx, "plain", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if chat, _ := s.GetStudioChat(ctx, "plain"); chat.ArchivedAt == nil || !chat.ArchivedAt.Equal(at) {
		t.Fatalf("归档时刻 = %v", chat.ArchivedAt)
	}
	if err := s.ArchiveStudioChat(ctx, "nope", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("归档不存在的对话应 ErrNotFound，得到 %v", err)
	}
	if err := s.DeleteWorkspace(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListStudioMediaModels(ctx, "ws1"); len(got) != 0 {
		t.Fatalf("删空间后白名单应级联清空：%+v", got)
	}
}
