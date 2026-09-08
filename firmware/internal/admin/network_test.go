// 设备设置——网络配置端点的管理面走查：权限边界（member 403）、开发环境
// 降级形状（supported=false）、netconfig 哨兵错误到统一错误体的映射。
// 状态机与解析的行为验收在 internal/netconfig 包内。
package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNetworkEndpoints(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	// 无会话：默认拒绝。
	if resp := e.do("GET", "/admin/v1/system/network", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话读网络配置状态 = %d，期望 401", resp.StatusCode)
	}

	// 有会话：测试环境 Runner 恒报「无 nmcli」，形状为 supported=false + reason。
	resp := e.do("GET", "/admin/v1/system/network", root, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读网络配置状态 = %d，期望 200；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var got struct {
		Network struct {
			Supported            bool   `json:"supported"`
			Reason               string `json:"reason"`
			ConfirmWindowSeconds int    `json:"confirm_window_seconds"`
		} `json:"network"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if got.Network.Supported || got.Network.Reason == "" || got.Network.ConfirmWindowSeconds <= 0 {
		t.Fatalf("降级形状错误：%+v", got.Network)
	}

	// 提交：先撞语法校验（400），语法过了撞环境不支持（409）。
	resp = e.do("PUT", "/admin/v1/system/network", root,
		`{"device":"wlan0","method":"manual","address":"999.1.1.1","prefix":24}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, resp) != "invalid_network_config" {
		t.Fatalf("非法地址应 400 invalid_network_config，得到 %d", resp.StatusCode)
	}
	resp = e.do("PUT", "/admin/v1/system/network", root,
		`{"device":"wlan0","method":"manual","address":"192.168.50.102","prefix":24,"gateway":"192.168.50.1","dns":["192.168.53.1"]}`)
	if resp.StatusCode != http.StatusConflict || errCode(t, resp) != "network_unsupported" {
		t.Fatalf("环境不支持应 409 network_unsupported，得到 %d", resp.StatusCode)
	}

	// 确认：无进行中的变更。
	resp = e.do("POST", "/admin/v1/system/network/confirm", root, `{}`)
	if resp.StatusCode != http.StatusConflict || errCode(t, resp) != "no_pending_change" {
		t.Fatalf("无变更确认应 409 no_pending_change，得到 %d", resp.StatusCode)
	}
}
