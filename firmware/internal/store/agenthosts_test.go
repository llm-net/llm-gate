package store

// agent_hosts 仓储验收。纳管流程（SSH、装公钥、免密 sudo）在 internal/agenthost，
// 这里只钉仓储行为：
//
//   - 新行恒以 error 起步，连接三元组唯一；
//   - 一次会话的观测值整组写回，读得回同一份；
//   - 改名与删除的未命中一律 ErrNotFound；
//   - 表里没有凭据列——口令不入库是端到端纪律，仓储这层先钉住形状。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newHost(t *testing.T, s *Store, address string) *AgentHost {
	t.Helper()
	h, err := s.CreateAgentHost(context.Background(), NewAgentHost{
		Name: "板子", Kind: AgentHostKindWorker, Address: address, Port: 22, Username: "admin",
	})
	if err != nil {
		t.Fatalf("CreateAgentHost: %v", err)
	}
	return h
}

func TestAgentHostCreateListAndConflict(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()

	hosts, err := s.ListAgentHosts(ctx)
	if err != nil {
		t.Fatalf("ListAgentHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("初始清单 = %+v", hosts)
	}

	h := newHost(t, s, "192.168.1.10")
	if h.Status != AgentHostStatusError || h.SudoNoPasswd || h.KeyFingerprint != "" {
		t.Fatalf("新行 = %+v：证书没装上之前不该是可用状态", h)
	}
	if h.Kind != AgentHostKindWorker || !h.AllowsDevd() {
		t.Fatalf("新行类型 = %q", h.Kind)
	}
	if _, err := s.CreateAgentHost(ctx, NewAgentHost{
		Kind: AgentHostKindManaged, Address: "192.168.1.10", Port: 22, Username: "admin",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("同一连接三元组应冲突：%v", err)
	}
	// 换端口或换用户就是另一台可纳管的目标。
	if _, err := s.CreateAgentHost(ctx, NewAgentHost{Kind: AgentHostKindManaged, Address: "192.168.1.10", Port: 2222, Username: "admin"}); err != nil {
		t.Fatalf("换端口: %v", err)
	}
	if _, err := s.CreateAgentHost(ctx, NewAgentHost{Kind: AgentHostKindWorker, Address: "192.168.1.10", Port: 22, Username: "pi"}); err != nil {
		t.Fatalf("换用户: %v", err)
	}
	if hosts, err = s.ListAgentHosts(ctx); err != nil || len(hosts) != 3 {
		t.Fatalf("清单 = %+v（err=%v）", hosts, err)
	}
	// 类型按行读回：受控纳管不能装 devd。
	if hosts[1].Kind != AgentHostKindManaged || hosts[1].AllowsDevd() || hosts[2].Kind != AgentHostKindWorker {
		t.Fatalf("清单类型 = %q / %q", hosts[1].Kind, hosts[2].Kind)
	}

	for _, bad := range []NewAgentHost{
		{Kind: AgentHostKindWorker, Address: "", Username: "admin", Port: 22},
		{Kind: AgentHostKindWorker, Address: "h", Username: "", Port: 22},
		{Kind: AgentHostKindWorker, Address: "h", Username: "admin", Port: 0},
		{Kind: AgentHostKindWorker, Address: "h", Username: "admin", Port: 70000},
		// 类型必填且只认词汇表里的值（CHECK 约束之前仓储先拒）。
		{Address: "h", Username: "admin", Port: 22},
		{Kind: "owner", Address: "h", Username: "admin", Port: 22},
	} {
		if _, err := s.CreateAgentHost(ctx, bad); err == nil {
			t.Fatalf("%+v 应被拒", bad)
		}
	}
}

func TestAgentHostStateRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "192.168.1.10")

	at := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	got, err := s.SetAgentHostState(ctx, AgentHostState{
		ID: h.ID, HostKey: "ssh-ed25519 AAAAhost", KeyFingerprint: "SHA256:abc",
		SudoNoPasswd: true, System: "Linux 6.12 aarch64", Status: AgentHostStatusReady, CheckedAt: at,
	})
	if err != nil {
		t.Fatalf("SetAgentHostState: %v", err)
	}
	if got.Status != AgentHostStatusReady || !got.SudoNoPasswd || got.KeyFingerprint != "SHA256:abc" ||
		got.HostKey != "ssh-ed25519 AAAAhost" || got.System != "Linux 6.12 aarch64" {
		t.Fatalf("写回后 = %+v", got)
	}
	if !got.LastCheckedAt.Equal(at) {
		t.Fatalf("检查时刻 = %v", got.LastCheckedAt)
	}
	reread, err := s.GetAgentHost(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetAgentHost: %v", err)
	}
	if *reread != *got {
		t.Fatalf("重读 = %+v，写回 = %+v", reread, got)
	}

	// 失败写回把原因留在行里，成功过的读数（指纹、系统）由调用方决定是否保留。
	failed, err := s.SetAgentHostState(ctx, AgentHostState{
		ID: h.ID, HostKey: got.HostKey, KeyFingerprint: got.KeyFingerprint,
		Status: AgentHostStatusError, LastError: "连接超时",
	})
	if err != nil {
		t.Fatalf("SetAgentHostState: %v", err)
	}
	if failed.Status != AgentHostStatusError || failed.LastError != "连接超时" || failed.SudoNoPasswd {
		t.Fatalf("失败写回 = %+v", failed)
	}

	if _, err := s.SetAgentHostState(ctx, AgentHostState{ID: h.ID, Status: "whatever"}); err == nil {
		t.Fatal("非法状态应被拒")
	}
	if _, err := s.SetAgentHostState(ctx, AgentHostState{ID: 4242, Status: AgentHostStatusReady}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的行应 ErrNotFound：%v", err)
	}
}

func TestAgentHostRenameAndDelete(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h := newHost(t, s, "192.168.1.10")

	renamed, err := s.RenameAgentHost(ctx, h.ID, "客厅开发板")
	if err != nil {
		t.Fatalf("RenameAgentHost: %v", err)
	}
	if renamed.Name != "客厅开发板" || renamed.Address != h.Address {
		t.Fatalf("改名后 = %+v", renamed)
	}
	if cleared, err := s.RenameAgentHost(ctx, h.ID, ""); err != nil || cleared.Name != "" {
		t.Fatalf("清空名称 = %+v（err=%v）", cleared, err)
	}
	if _, err := s.RenameAgentHost(ctx, 4242, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的行应 ErrNotFound：%v", err)
	}

	if err := s.DeleteAgentHost(ctx, h.ID); err != nil {
		t.Fatalf("DeleteAgentHost: %v", err)
	}
	if _, err := s.GetAgentHost(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后仍读得到：%v", err)
	}
	if err := s.DeleteAgentHost(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound：%v", err)
	}
}

// 守护进程观测值：整组写回、状态词汇表、卸载归零、未命中 ErrNotFound。
func TestAgentHostDevdRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	h, err := s.CreateAgentHost(ctx, NewAgentHost{Kind: AgentHostKindWorker, Address: "10.0.0.8", Port: 22, Username: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Devd.Installed() || h.Devd.Status != DevdStatusNone {
		t.Fatalf("新行守护进程 = %+v", h.Devd)
	}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	got, err := s.SetAgentHostDevd(ctx, AgentHostDevdState{
		ID: h.ID, Version: "dev", Home: "/home/dev", Tmux: true, Status: DevdStatusReady, CheckedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := got.Devd
	if !d.Installed() || d.Status != DevdStatusReady || d.Version != "dev" || !d.Tmux || d.Home != "/home/dev" || !d.CheckedAt.Equal(at) {
		t.Fatalf("写回 = %+v", d)
	}
	if _, err := s.SetAgentHostDevd(ctx, AgentHostDevdState{ID: h.ID, Status: DevdStatusNone}); err == nil {
		t.Fatal("「没装」不该经 SetAgentHostDevd 写")
	}
	if _, err := s.SetAgentHostDevd(ctx, AgentHostDevdState{ID: 999, Status: DevdStatusError}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中 = %v", err)
	}
	// 主机自身的状态写回不动守护进程那组列。
	if got, err = s.SetAgentHostState(ctx, AgentHostState{ID: h.ID, Status: AgentHostStatusReady}); err != nil || got.Devd.Version != "dev" {
		t.Fatalf("主机状态写回后守护进程 = %+v, %v", got.Devd, err)
	}
	if got, err = s.ClearAgentHostDevd(ctx, h.ID); err != nil || got.Devd.Installed() || got.Devd.Version != "" || got.Devd.Home != "" || !got.Devd.CheckedAt.IsZero() {
		t.Fatalf("归零 = %+v, %v", got.Devd, err)
	}
	if _, err := s.ClearAgentHostDevd(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("归零未命中 = %v", err)
	}
}

// 表里不该有任何凭据列：口令不入库这条纪律先在 schema 上钉住。
func TestAgentHostTableHasNoCredentialColumn(t *testing.T) {
	s, _ := mustOpen(t)
	rows, err := s.db.QueryContext(context.Background(), `SELECT name FROM pragma_table_info('agent_hosts')`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch name {
		case "password", "password_sealed", "secret", "private_key", "auth_json_sealed":
			t.Fatalf("agent_hosts 不该有凭据列 %q", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}
