// llmgate-devd 是「智能体 → 主机/SoC」在被纳管主机上运行的守护进程 devd：一个只在
// 本机 Unix socket 上监听的 HTTP 服务，给设备界面的工作空间提供文件目录、Git 与 tmux
// 终端；设备凭访问证书经 SSH 连到这个 socket。它是独立的纯标准库二进制（不是 llmgate
// multi-call 的子命令）：固件为 linux/amd64 与 linux/arm64 各内嵌一份，安装时按目标
// 架构下发。
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
	"path/filepath"
	"syscall"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
)

const usageText = `llmgate-devd — LLM Gate 守护进程 devd

用法:
  llmgate-devd serve     [--config-dir /etc/llmgate-devd]
  llmgate-devd install   --user <用户名> [--socket /run/llmgate-devd/devd.sock] [--config-dir DIR] [--no-systemd]
  llmgate-devd uninstall [--config-dir DIR] [--no-systemd]
  llmgate-devd --version
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
		fmt.Println("llmgate-devd " + buildinfo.Version)
	case "--help", "-h", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "llmgate-devd: 未知子命令 %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("config-dir", devd.DefaultConfigDir, "配置目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	lay := devd.LayoutIn(*dir)
	cfg, err := devd.ReadConfig(lay.Config)
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
		if _, err := os.Stat("/bin/bash"); err == nil {
			shell = "/bin/bash"
		}
	}
	srv := devd.New(devd.Options{Home: home, User: username, Shell: shell, Version: buildinfo.Version, Logger: logger, StateDir: filepath.Dir(cfg.Socket)})
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
	dir := fs.String("config-dir", devd.DefaultConfigDir, "配置目录")
	username := fs.String("user", "", "守护进程运行用户")
	socket := fs.String("socket", devd.DefaultSocket, "监听的 Unix socket 路径")
	bin := fs.String("bin", devd.DefaultBinPath, "二进制路径（写进 unit）")
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
	res, err := devd.Install(devd.InstallOptions{ConfigDir: *dir, BinPath: *bin, User: *username, Socket: *socket})
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgate-devd install: %v\n", err)
		return 1
	}
	if !*noSystemd {
		for _, c := range [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "enable", "--now", devd.UnitName},
			{"systemctl", "restart", devd.UnitName},
		} {
			if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "llmgate-devd install: %s: %v\n%s", c, err, out)
				return 1
			}
		}
	}
	fmt.Printf("socket=%s\nunit=%s\n", res.Socket, res.UnitPath)
	return 0
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	dir := fs.String("config-dir", devd.DefaultConfigDir, "配置目录")
	noSystemd := fs.Bool("no-systemd", false, "只删文件，不调用 systemctl")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*noSystemd {
		exec.Command("systemctl", "disable", "--now", devd.UnitName).Run()
	}
	if err := devd.Uninstall(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "llmgate-devd uninstall: %v\n", err)
		return 1
	}
	if !*noSystemd {
		exec.Command("systemctl", "daemon-reload").Run()
	}
	return 0
}
