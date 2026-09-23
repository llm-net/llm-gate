// llmgate-modeld 是「智能体 → 主机/SoC」模型服务节点上运行的守护进程 modeld：管理面只在本机
// Unix socket 上监听（设备凭访问证书经 SSH 连到它），对外 API 在一个 TCP 端口上以令牌服务。它
// 管理多台算力服务器、排队与调度推理任务、缓存模型文件，并能在本机直接拉起 Codex App Server /
// Claude Code（大模型 API 从设备走）。独立的纯标准库二进制（不是 llmgate multi-call 的子命令）：
// 固件为 linux/amd64 与 linux/arm64 各内嵌一份，安装时按目标架构下发。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"syscall"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/modeld"
)

const usageText = `llmgate-modeld — LLM Gate 模型服务守护进程 modeld

用法:
  llmgate-modeld serve     [--config-dir /etc/llmgate-modeld]
  llmgate-modeld install   --user <用户名> [--socket /run/llmgate-modeld/modeld.sock] [--listen 0.0.0.0:8790|-]
                           [--state-dir /var/lib/llmgate-modeld] [--config-dir DIR] [--no-systemd]
  llmgate-modeld uninstall [--config-dir DIR] [--no-systemd]
  llmgate-modeld --version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "--version", "version":
		fmt.Println("llmgate-modeld " + buildinfo.Version)
	case "--help", "-h", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "llmgate-modeld: 未知子命令 %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("config-dir", modeld.DefaultConfigDir, "配置目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	lay := modeld.LayoutIn(*dir)
	cfg, err := modeld.ReadConfig(lay.Config)
	if err != nil {
		logger.Error("读取配置失败", "error", err.Error())
		return 1
	}
	home, _ := os.UserHomeDir()
	username, shell := cfg.User, os.Getenv("SHELL")
	if u, err := user.Current(); err == nil {
		if username == "" {
			username = u.Username
		}
		if home == "" {
			home = u.HomeDir
		}
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	srv, err := modeld.New(modeld.Options{StateDir: cfg.StateDir, Home: home, User: username, Shell: shell, Version: buildinfo.Version, Logger: logger, Listen: cfg.Listen})
	if err != nil {
		logger.Error("初始化失败", "error", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := srv.ListenAndServe(ctx, cfg.Socket); err != nil {
		logger.Error("服务退出", "error", err.Error())
		return 1
	}
	return 0
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dir := fs.String("config-dir", modeld.DefaultConfigDir, "配置目录")
	username := fs.String("user", "", "守护进程运行用户")
	socket := fs.String("socket", modeld.DefaultSocket, "管理面监听的 Unix socket 路径")
	listen := fs.String("listen", modeld.DefaultListen, "对外 API 监听地址（- 表示不开）")
	stateDir := fs.String("state-dir", modeld.DefaultStateDir, "状态与模型缓存目录")
	bin := fs.String("bin", modeld.DefaultBinPath, "二进制路径（写进 unit）")
	noSystemd := fs.Bool("no-systemd", false, "只落文件，不调用 systemctl")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *username == "" {
		*username = os.Getenv("SUDO_USER")
	}
	if *username == "" {
		if u, err := user.Current(); err == nil {
			*username = u.Username
		}
	}
	res, err := modeld.Install(modeld.InstallOptions{ConfigDir: *dir, BinPath: *bin, User: *username, Socket: *socket, Listen: *listen, StateDir: *stateDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate-modeld install: %v\n", err)
		return 1
	}
	if !*noSystemd {
		for _, c := range [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "enable", "--now", modeld.UnitName},
			{"systemctl", "restart", modeld.UnitName},
		} {
			if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "llmgate-modeld install: %s: %v\n%s", c, err, out)
				return 1
			}
		}
	}
	fmt.Printf("socket=%s\nunit=%s\nlisten=%s\nstate_dir=%s\n", res.Socket, res.UnitPath, res.Listen, res.StateDir)
	return 0
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	dir := fs.String("config-dir", modeld.DefaultConfigDir, "配置目录")
	noSystemd := fs.Bool("no-systemd", false, "只删文件，不调用 systemctl")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*noSystemd {
		exec.Command("systemctl", "disable", "--now", modeld.UnitName).Run()
	}
	if err := modeld.Uninstall(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "llmgate-modeld uninstall: %v\n", err)
		return 1
	}
	if !*noSystemd {
		exec.Command("systemctl", "daemon-reload").Run()
	}
	return 0
}
