package admin_test

// 设备铭牌端点的验收：固件版本与板卡型号免会话可读，不带设备身份。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

// TestVersionIsAnonymous：GET /admin/v1/version 不需要会话——登录页的规格铭牌
// 要印它，而那一页在会话之前。型号来自装配期注入的 boardinfo 识别结果。
func TestVersionIsAnonymous(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "2608221732-2d81"
	t.Cleanup(func() { buildinfo.Version = old })

	e := newEnv(t)
	resp := e.do("GET", "/admin/v1/version", "", "")
	wantStatus(t, resp, http.StatusOK)

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	resp.Body.Close()
	if got["version"] != "2608221732-2d81" {
		t.Fatalf("version = %v，期望 2608221732-2d81", got["version"])
	}
	if got["hardware_model"] != testHardwareModel {
		t.Fatalf("hardware_model = %v，期望 %s", got["hardware_model"], testHardwareModel)
	}
	// 未登录方只能读到两项公开铭牌事实，不能带出序列号、SID、MAC 等身份。
	if len(got) != 2 {
		t.Fatalf("免会话端点多回了字段：%+v", got)
	}
}

// TestVersionOnlyAnonymousBesidesLogin：白名单只多了这一条——其余 /admin/v1
// 端点匿名访问仍是 401（withSession 的既有纪律，加端点不该在它上面开洞）。
func TestVersionOnlyAnonymousBesidesLogin(t *testing.T) {
	e := newEnv(t)
	// 同路径的写方法不在白名单里：白名单钉的是 method+path，不是路径前缀。
	for _, m := range []string{"POST", "DELETE"} {
		if resp := e.do(m, "/admin/v1/version", "", "{}"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("匿名 %s /admin/v1/version = %d，期望 401", m, resp.StatusCode)
		}
	}
}
