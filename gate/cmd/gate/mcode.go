package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const mcodeProviderID = "custom_provider:llmgate"

// MCode reads custom_provider credentials literally; api-key-env in its CLI
// copies the environment value into config.yaml rather than storing a reference.
// JSON is valid YAML. This entire generated file is gate-owned, including when
// MCode rewrites it as YAML. Sessions and other profile files remain separate.
func mcodeConfig(base, key string, tool runtimeTool) ([]byte, error) {
	models := make(map[string]any, len(tool.Models))
	order := make([]string, 0, len(tool.Models))
	for _, model := range tool.Models {
		if strings.TrimSpace(model.Name) == "" || model.Source != "catalog" {
			return nil, errors.New("MiniMax Code 收到无效目录模型")
		}
		if _, exists := models[model.Name]; exists {
			continue
		}
		models[model.Name] = map[string]any{"name": gateModelDisplayName(model.Name)}
		order = append(order, model.Name)
	}
	if _, ok := models[tool.DefaultModel]; !ok || tool.DefaultModel == "" {
		return nil, errors.New("MiniMax Code 配置缺少可见默认模型")
	}
	return json.MarshalIndent(map[string]any{
		"defaultModel": mcodeProviderID + "/" + tool.DefaultModel,
		"custom_provider": map[string]any{"llmgate": map[string]any{
			"kind": "custom", "name": "LLM Gate", "enabled": true,
			"api":     "openai-completions",
			"options": map[string]string{"baseURL": base + "/v1", "apiKey": key},
			"models":  models, "model_order": order,
		}},
		"telemetry": map[string]bool{"enabled": false},
		"prompt":    map[string]bool{"autoUpdate": false},
	}, "", "  ")
}

func mcodeEnv(env []string, dir string) []string {
	clean := make([]string, 0, len(env)+3)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		if strings.HasPrefix(name, "MCODE_") || strings.HasPrefix(name, "MINIMAX_") ||
			strings.HasPrefix(name, "MAVIS_") || strings.HasPrefix(name, "__MAVIS_") {
			continue
		}
		clean = append(clean, kv)
	}
	return append(clean, "MINIMAX_DATA_DIR="+dir, "MAVIS_DATA_DIR="+dir, "MCODE_DISABLE_TELEMETRY=1")
}

func (a *app) prepareMCode(path string, tool runtimeTool) error {
	body, err := mcodeConfig(a.cfg.BaseURL, a.cfg.APIKey, tool)
	if err != nil {
		return err
	}
	dir := a.derivedDir("mcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := noLinkPath(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := securePath(dir, true); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := noLinkPath(configPath); err != nil {
		return err
	}
	previous, err := readSmallFile(configPath)
	if err != nil {
		return err
	}
	if err := atomicWrite(configPath, append(body, '\n'), 0o600); err != nil {
		return err
	}
	// Initialize and validate the actual isolated profile in the launch
	// directory. This command performs no inference; output is never logged.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := mcodeCommand(ctx, path, "provider", "list", "--json")
	cmd.Env = mcodeEnv(installerEnv(), dir)
	cmd.Dir, _ = os.Getwd()
	var output mcodeCheckOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err == nil && validMCodeSnapshot(output.Bytes(), a.cfg.BaseURL, tool) {
		return atomicWrite(configPath, append(body, '\n'), 0o600)
	}
	// Restore the prior configuration exactly on any failed check.
	if previous == nil {
		err = os.Remove(configPath)
	} else {
		err = atomicWrite(configPath, previous, 0o600)
	}
	if err != nil {
		return errors.New("MiniMax Code 自检失败，且无法恢复原派生配置")
	}
	return &cliUnusableError{tool: "MiniMax Code", path: path, version: "unknown",
		reason: "无法读取 gate 派生模型配置；可执行 gate mcode update --adopt 使用 gate 管理的程序与 Node.js 运行时"}
}

type mcodeCheckOutput struct{ bytes.Buffer }

func (b *mcodeCheckOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2<<20 {
		return 0, errors.New("MiniMax Code 自检输出超过限制")
	}
	return b.Buffer.Write(p)
}

func validMCodeSnapshot(body []byte, base string, tool runtimeTool) bool {
	var snapshot struct {
		Providers []struct {
			ID        string `json:"providerId"`
			API       string `json:"apiFormat"`
			BaseURL   string `json:"baseUrl"`
			Enabled   bool   `json:"enabled"`
			HasAPIKey bool   `json:"hasApiKey"`
			Models    []struct {
				ID       string `json:"modelId"`
				Selected bool   `json:"selected"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(body, &snapshot) != nil {
		return false
	}
	want := map[string]bool{}
	for _, m := range tool.Models {
		want[m.Name] = true
	}
	for _, provider := range snapshot.Providers {
		if provider.ID != mcodeProviderID {
			continue
		}
		if provider.API != "openai-completions" || provider.BaseURL != base+"/v1" ||
			!provider.Enabled || !provider.HasAPIKey || len(provider.Models) != len(want) {
			return false
		}
		selected := false
		for _, model := range provider.Models {
			if !want[model.ID] {
				return false
			}
			delete(want, model.ID)
			if model.Selected {
				if model.ID != tool.DefaultModel {
					return false
				}
				selected = true
			}
		}
		return selected && len(want) == 0
	}
	return false
}

// Windows CreateProcess cannot execute .cmd directly. Official and npm installs
// provide an adjacent PowerShell launcher; -File preserves arguments without
// interpolating user text into a shell command expression.
func mcodeCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	program, argv := mcodeCommandArgs(path, runtime.GOOS, args)
	return exec.CommandContext(ctx, program, argv...)
}

func mcodeCommandArgs(path, goos string, args []string) (string, []string) {
	if goos == "windows" && strings.EqualFold(filepath.Ext(path), ".cmd") {
		script := strings.TrimSuffix(path, filepath.Ext(path)) + ".ps1"
		return "powershell.exe", append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}, args...)
	}
	return path, args
}
