package devhost

// 远程安装只跑这几条 POSIX sh：上传二进制到临时文件、以 root 安装并起服务、卸载。三条纪律与 internal/agenthost/scripts.go 相同：口令只经 sudo -S 的 stdin；
// 拼进命令行的只有经 agenthost 收窄过的用户名、端口与随机后缀；每一步幂等。
//
// `llmgate-devd install` 自己负责落配置目录、写 unit 与 systemctl；这里只把二进制
// 送到主机上再调用它。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// tmpSuffix 给临时文件名一段随机后缀：多次安装互不踩，也不用 $$（每条命令一个 shell）。
func tmpSuffix() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// uploadCommand 把 stdin 落到 path（0600）。
func uploadCommand(path string) string {
	return "umask 077 && cat > " + path
}

// archCommand 读架构自述。
const archCommand = "uname -m"

// goArch 把 uname -m 折成 GOARCH；不支持的架构回空串。
func goArch(uname string) string {
	switch strings.TrimSpace(uname) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

// installScript 是以 root 身份跑的内层脚本：把上传的二进制装到位、调用它的 install
// 子命令，无论成败都清掉临时文件。用户名与临时路径都已收窄，拼进去是安全的。
func installScript(spec daemonSpec, binTmp, username string) string {
	return "install -m 0755 " + binTmp + " " + spec.BinPath + " && " +
		spec.BinPath + " install --user " + username + "; " +
		"rc=$?; rm -f " + binTmp + "; exit $rc"
}

// uninstallScript 停服务、删 unit 与配置目录、删二进制（modeld 的状态目录与模型缓存留着）。
func uninstallScript(spec daemonSpec) string {
	return "if [ -x " + spec.BinPath + " ]; then " + spec.BinPath + " uninstall; fi; " +
		"rm -f " + spec.BinPath + "; " +
		"rm -f /etc/systemd/system/" + spec.UnitName + "; rm -rf " + spec.ConfigDir + "; " +
		"systemctl daemon-reload 2>/dev/null || true"
}

// privileged 把内层脚本包成一条命令：root 直接跑；其他用户经 sudo——给了口令就
// `sudo -S`（口令走 stdin，-p ” 去掉提示符），没给就 `sudo -n`（要求已免密）。脚本里
// 没有单引号，可以整段放进 sh -c '…'。viaSudo 报告这条命令经过 sudo（失败时按 sudo
// 的退出码解释）。
func privileged(username, script, password string) (command, stdin string, viaSudo bool) {
	inner := "/bin/sh -c '" + script + "'"
	switch {
	case isRoot(username):
		return inner, "", false
	case password != "":
		return "sudo -S -p '' " + inner, password + "\n", true
	default:
		return "sudo -n " + inner, "", true
	}
}

const rootUser = "root"

func isRoot(username string) bool { return username == rootUser }

// parseInstallOutput 解 `llmgate-devd install` / `llmgate-modeld install` 的输出：socket=… 那一行
// （没有就是没装成）。
func parseInstallOutput(out string) (socket string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "socket=") {
			return strings.TrimPrefix(line, "socket=")
		}
	}
	return ""
}

// tail 截一小段远端 stderr 附在错误后面（远端信息里不含口令：口令只走 stdin）。
func tail(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	const max = 240
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return "：" + strings.ReplaceAll(s, "\n", " ")
}

// sudoFailure 把经 sudo 跑的命令的退出码翻成人话；what 是动作（「在主机上安装守护进程失败」）。
func sudoFailure(code int, stderr, what string) error {
	switch code {
	case 127:
		return &Error{Code: CodeSudoFailed, Msg: "主机上没有 sudo，也不是 root 用户：请用 root 纳管，或先给该用户配好 sudo。"}
	case 1:
		// sudo 自己被拒时退出码 1、stderr 是 "Sorry, try again." / "a password is required"
		// 一类；内层脚本自己失败时 stderr 会有它的报错。两者都没说清的按 sudo 算。
		trimmed := strings.TrimSpace(stderr)
		if trimmed == "" || strings.Contains(trimmed, "try again") || strings.Contains(trimmed, "not in the sudoers") || strings.Contains(trimmed, "password") {
			return &Error{Code: CodeSudoFailed, Msg: "sudo 被拒绝：口令不对、该用户不在 sudoers 里，或没有免密 sudo" + tail(stderr)}
		}
	}
	return &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("%s（退出码 %d）%s", what, code, tail(stderr))}
}
