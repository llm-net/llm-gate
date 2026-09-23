package devhost_test

// 模型服务节点：安装 modeld（而不是 devd）、透传到 modeld、类型闸。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/modeld"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestModelServiceInstallsModeld(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", Arch: "x86_64"})
	ctx := context.Background()
	row := f.enrollKind(t, store.AgentHostKindModelService, "dev", true)
	if !row.IsModelService() || !row.AllowsDaemon() || row.AllowsDevd() || !row.AllowsTools() || row.DaemonName() != "modeld" {
		t.Fatalf("类型能力 = %+v", row)
	}
	// 模型服务透传在没装之前答没装；工作节点的透传对它同样没装。
	if _, err := f.dev.ProxyModelService(ctx, row.ID, http.MethodGet, "summary", nil, nil, ""); code(t, err) != devhost.CodeNotInstalled {
		t.Fatalf("未装透传 = %v", err)
	}
	h, err := f.dev.Install(ctx, devhost.InstallRequest{ID: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Devd.Installed() || h.Devd.Status != store.DevdStatusReady || h.Devd.Version != "fake" {
		t.Fatalf("守护进程 = %+v", h.Devd)
	}
	// 装的是 llmgate-modeld：安装脚本与转发目标都是 modeld 的。
	sawInstall := false
	for _, ex := range f.host.Execs() {
		if strings.Contains(ex.Command, modeld.DefaultBinPath+" install") {
			sawInstall = true
		}
		if strings.Contains(ex.Command, "llmgate-devd") {
			t.Fatalf("模型服务节点不该碰 devd：%q", ex.Command)
		}
	}
	if !sawInstall {
		t.Fatal("没看到 modeld 的安装命令")
	}
	if socks := f.host.Sockets(); len(socks) == 0 || socks[len(socks)-1] != modeld.DefaultSocket {
		t.Fatalf("转发目标 = %v", socks)
	}
	// 透传到 modeld：自述与一屏读数。
	resp, err := f.dev.ProxyModelService(ctx, row.ID, http.MethodGet, "summary", nil, nil, "")
	if err != nil || resp.Status != 200 {
		t.Fatalf("summary = %+v %v", resp, err)
	}
	var sum struct {
		Info struct {
			Daemon string `json:"daemon"`
		} `json:"info"`
	}
	json.Unmarshal(resp.Body, &sum)
	if sum.Info.Daemon != "modeld" {
		t.Fatalf("summary 体 = %s", resp.Body)
	}
	resp, err = f.dev.ProxyModelService(ctx, row.ID, http.MethodPost, "backends", nil, strings.NewReader(`{"name":"gpu","base_url":"http://127.0.0.1:1","slots":2}`), "application/json")
	if err != nil || resp.Status != 201 {
		t.Fatalf("登记算力服务器 = %+v %v", resp, err)
	}
	// 检查与卸载沿用同一套。
	if h, err = f.dev.Check(ctx, row.ID); err != nil || h.Devd.Status != store.DevdStatusReady {
		t.Fatalf("check = %+v %v", h, err)
	}
	if h, err = f.dev.Uninstall(ctx, devhost.UninstallRequest{ID: row.ID}); err != nil || h.Devd.Installed() {
		t.Fatalf("uninstall = %+v %v", h, err)
	}
	if f.host.Installed() {
		t.Fatal("主机上应已卸载")
	}
}

func TestModelServiceProxyRefusedForWorker(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "dev", Arch: "x86_64"})
	ctx := context.Background()
	row := f.enroll(t, "dev", true)
	if _, err := f.dev.Install(ctx, devhost.InstallRequest{ID: row.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dev.ProxyModelService(ctx, row.ID, http.MethodGet, "summary", nil, nil, ""); code(t, err) != devhost.CodeKindNotAllowed {
		t.Fatalf("工作节点的模型服务透传应被拒 = %v", err)
	}
	if f.dev.ModelDaemonVersion() != "test" || f.dev.DaemonVersionFor(row) != "test" {
		t.Fatalf("版本 = %q %q", f.dev.ModelDaemonVersion(), f.dev.DaemonVersionFor(row))
	}
}
