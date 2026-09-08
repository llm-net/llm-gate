package updated

// 真实实现：exec systemctl 与 HTTP 健康探针。测试一律用假实现，这两个只在
// runUpdated（板上）装配。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

// ExecSystemctl 经 exec 调 systemctl（引擎以 root 运行，无需 sudo/polkit）。
type ExecSystemctl struct{}

func (ExecSystemctl) Run(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		// systemctl 的输出只有单元名与状态词，可安全入错误串。
		return fmt.Errorf("systemctl %s: %v（%s）", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (ExecSystemctl) IsActive(ctx context.Context, unit string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", unit).CombinedOutput()
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		return "", err
	}
	// is-active 对非 active 状态退出码非零，但 stdout 里带着状态词——那才是
	// 我们要的答案。
	return state, nil
}

// Show 读单元属性：`systemctl show <unit> -p A -p B` 逐行输出 `A=值`。
func (ExecSystemctl) Show(ctx context.Context, unit string, props ...string) (map[string]string, error) {
	args := []string{"show", unit}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %v", unit, err)
	}
	values := make(map[string]string, len(props))
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	return values, nil
}

// HTTPHealthProber 构造对 gatewayd /healthz 的探针。
func HTTPHealthProber(url string) HealthProber {
	// 本机 127.0.0.1 探针：Transport 显式直连（不读 HTTP_PROXY 等环境代理）。
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New("healthz 非 200")
		}
		return nil
	}
}
