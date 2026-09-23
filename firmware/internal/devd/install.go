package devd

// 安装布局（`llmgate-devd install` 落下的东西，`uninstall` 原样收走）：
//
//	/usr/local/bin/llmgate-devd              二进制（由调用方先放好）
//	/etc/llmgate-devd/config.json            {"socket": "/run/llmgate-devd/devd.sock", "user": "<用户名>"}
//	/etc/systemd/system/llmgate-devd.service 以 User=<用户名> 运行，RuntimeDirectory 给出 socket 目录
//
// 没有任何证书或密钥：鉴权由设备那条 SSH 连接承担（见 devd.go 文件头）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// 缺省路径。
const (
	DefaultConfigDir = "/etc/llmgate-devd"
	DefaultBinPath   = "/usr/local/bin/llmgate-devd"
	// RuntimeDir 是 systemd RuntimeDirectory= 建出来的 socket 目录（属运行用户、0700）。
	RuntimeDir    = "/run/llmgate-devd"
	DefaultSocket = RuntimeDir + "/devd.sock"
	UnitName      = "llmgate-devd.service"
	UnitPath      = "/etc/systemd/system/" + UnitName
)

// Config 是 config.json。
type Config struct {
	Socket string `json:"socket"`
	User   string `json:"user"`
}

// Layout 是配置目录里各文件的路径。
type Layout struct {
	Dir    string
	Config string
}

// LayoutIn 给出目录下的路径集合。
func LayoutIn(dir string) Layout {
	return Layout{Dir: dir, Config: filepath.Join(dir, "config.json")}
}

// ReadConfig 读 config.json（不存在给缺省）。
func ReadConfig(path string) (Config, error) {
	cfg := Config{Socket: DefaultSocket}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	return cfg, nil
}

// InstallOptions 是 Install 的入参。
type InstallOptions struct {
	ConfigDir string
	BinPath   string
	// User 是守护进程的运行用户（必填，须已存在）。
	User string
	// Socket 是监听的 Unix socket 路径（空 = DefaultSocket）。
	Socket string
}

// InstallResult 是安装落下的事实。
type InstallResult struct {
	UnitPath string
	Socket   string
}

// Install 落配置目录与 systemd unit 文件；不调用 systemctl（由调用方做，便于测试）。
// 幂等：重复执行只更新配置与 unit。
func Install(o InstallOptions) (*InstallResult, error) {
	if o.ConfigDir == "" {
		o.ConfigDir = DefaultConfigDir
	}
	if o.BinPath == "" {
		o.BinPath = DefaultBinPath
	}
	if o.Socket == "" {
		o.Socket = DefaultSocket
	}
	if o.User == "" {
		return nil, errors.New("须指定运行用户（--user）")
	}
	u, err := user.Lookup(o.User)
	if err != nil {
		return nil, fmt.Errorf("用户 %s 不存在", o.User)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	lay := LayoutIn(o.ConfigDir)
	if err := os.MkdirAll(lay.Dir, 0o700); err != nil {
		return nil, err
	}
	res := &InstallResult{UnitPath: unitPathFor(o.ConfigDir), Socket: o.Socket}
	cfg, _ := json.MarshalIndent(Config{Socket: o.Socket, User: o.User}, "", "  ")
	if err := writeFileAtomic(lay.Config, append(cfg, '\n'), 0o600); err != nil {
		return nil, err
	}
	for _, p := range []string{lay.Dir, lay.Config} {
		if err := os.Chown(p, uid, gid); err != nil && !errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("chown %s: %w", p, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(res.UnitPath), 0o755); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(res.UnitPath, []byte(UnitFile(o.BinPath, o.ConfigDir, o.User)), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

// unitPathFor 在缺省配置目录下用系统 unit 路径；别的目录（测试）把 unit 放进目录里。
func unitPathFor(configDir string) string {
	if configDir == DefaultConfigDir {
		return UnitPath
	}
	return filepath.Join(configDir, UnitName)
}

// UnitFile 是 systemd unit 文本。Restart=always：守护进程被杀也拉回来；ProtectSystem
// 不能开——工作空间就是要改这个用户的文件。RuntimeDirectory 给出 socket 目录
// （/run/llmgate-devd，属运行用户、0700），不开网络端口。
func UnitFile(binPath, configDir, username string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=llmgate-devd — LLM Gate dev console daemon",
		"",
		"[Service]",
		"Type=simple",
		"User=" + username,
		"RuntimeDirectory=" + filepath.Base(RuntimeDir),
		"RuntimeDirectoryMode=0700",
		"ExecStart=" + binPath + " serve --config-dir " + configDir,
		"Restart=always",
		"RestartSec=2",
		"KillMode=process",
		"NoNewPrivileges=no",
		"LimitNOFILE=65536",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

// Uninstall 删 unit 与配置目录（不调用 systemctl）。
func Uninstall(configDir string) error {
	if configDir == "" {
		configDir = DefaultConfigDir
	}
	if err := os.Remove(unitPathFor(configDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(configDir); err != nil {
		return err
	}
	return nil
}
