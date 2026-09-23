package agenthost

// 远端只跑这几段 POSIX sh 脚本：装公钥、探状态、摘公钥、写/删免密 sudo 条目。
//
// 三条纪律：
//   - **变量只从 stdin 来**。公钥、口令都经会话 stdin 交给对端，不拼进命令行——
//     命令行会出现在主机的进程表与 shell 历史里（口令更是 §15.1 明确禁止入 argv）。
//     唯一拼进命令的是用户名，且已收窄到 POSIX 可移植字符集（usernameRE）。
//   - **幂等**。装公钥先查再追加，写 sudoers 先写临时文件、visudo 校验通过才顶上去；
//     同一步重复执行不会写出第二行、也不会留下半个文件。
//   - **只碰自己那几行**。authorized_keys 用 grep -vxF 逐行精确匹配，删的只有设备
//     自己的公钥；sudoers 只写 /etc/sudoers.d/90-llmgate-<用户名> 一个文件，不动
//     主 sudoers，也不改主机上别的任何配置。
//
// 退出码约定（脚本自己用的，与 sh 的常规码区分开）：3 = stdin 少了该有的行，
// 4 = HOME 没设，5 = sudoers 语法校验没过。

import (
	"context"
	"fmt"
	"strings"
)

// installKeyScript 把新公钥写进 authorized_keys，并顺手摘掉上一把（stdin 第二行，
// 可为空行）。缺目录/文件就建，权限统一收成 700 / 600。
const installKeyScript = `set -e
umask 077
[ -n "$HOME" ] || exit 4
IFS= read -r newkey || exit 3
IFS= read -r oldkey || oldkey=
[ -n "$newkey" ] || exit 3
d=$HOME/.ssh
f=$d/authorized_keys
mkdir -p "$d"
chmod 700 "$d"
[ -f "$f" ] || : > "$f"
chmod 600 "$f"
t=$f.llmgate.$$
if [ -n "$oldkey" ]; then
grep -vxF "$oldkey" "$f" > "$t" || :
else
cat "$f" > "$t"
fi
grep -qxF "$newkey" "$t" || printf '%s\n' "$newkey" >> "$t"
cat "$t" > "$f"
rm -f "$t"`

// probeScript 只读不写：报告 stdin 那把公钥在不在 authorized_keys 里，再吐一行
// 系统自述（uname -srm）。
const probeScript = `[ -n "$HOME" ] || exit 4
IFS= read -r key || exit 3
if grep -qxF "$key" "$HOME/.ssh/authorized_keys" 2>/dev/null; then
echo llmgate-key:installed
else
echo llmgate-key:missing
fi
uname -srm 2>/dev/null || :`

// removeKeyScript 把 stdin 每一行（设备的当前与上一把公钥）从 authorized_keys 里摘掉。
const removeKeyScript = `set -e
umask 077
[ -n "$HOME" ] || exit 4
f=$HOME/.ssh/authorized_keys
[ -f "$f" ] || exit 0
t=$f.llmgate.$$
cat "$f" > "$t"
while IFS= read -r key; do
[ -n "$key" ] || continue
grep -vxF "$key" "$t" > "$t.n" || :
mv "$t.n" "$t"
done
cat "$t" > "$f"
rm -f "$t" "$t.n"`

// probeInstalledMarker 是 probeScript 报告公钥在位的那一行。
const probeInstalledMarker = "llmgate-key:installed"

// installKey 在这条连接上装好 newLine 并摘掉 oldLine（可为空）。
func installKey(ctx context.Context, c *conn, newLine, oldLine string) error {
	_, stderr, code, err := c.run(ctx, installKeyScript, newLine+"\n"+oldLine+"\n")
	if err != nil {
		return err
	}
	if code != 0 {
		return &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("在主机上安装公钥失败（退出码 %d）%s", code, tail(stderr))}
	}
	return nil
}

// parseProbe 解 probeScript 的输出：公钥在不在、系统自述是什么。
func parseProbe(out string) (installed bool, system string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == probeInstalledMarker:
			installed = true
		case strings.HasPrefix(line, "llmgate-key:"), line == "":
		default:
			if system == "" {
				system = line
			}
		}
	}
	return installed, system
}

// sudoersPath 是本包在主机上写的唯一一个文件。文件名不含点号——sudo 会忽略
// /etc/sudoers.d/ 下带点号的文件。
func sudoersPath(username string) string { return "/etc/sudoers.d/90-llmgate-" + username }

// sudoersScript 是交给 sudo 执行的内层脚本：先写临时文件、visudo 校验通过再顶上去，
// 一条写坏的规则不会把主机的 sudo 弄瘫。用户名已过 usernameRE，拼进去是安全的。
func sudoersScript(username string) string {
	return `set -e
u=` + username + `
f=` + sudoersPath(username) + `
t=$f.tmp
umask 022
printf "%s ALL=(ALL) NOPASSWD:ALL\n" "$u" > $t
if command -v visudo >/dev/null 2>&1; then
visudo -cf $t >/dev/null 2>&1 || { rm -f $t; exit 5; }
fi
chmod 0440 $t
mv $t $f`
}

// sudoersRemoveCommand 删掉本包写的那个文件（解除纳管时尽力而为，要求当前已免密）。
func sudoersRemoveCommand(username string) string {
	return "sudo -n /bin/sh -c 'rm -f " + sudoersPath(username) + "'"
}

// configureSudo 让该用户免密 sudo：口令经 stdin 交给 `sudo -S`（-p ” 去掉提示符），
// 绝不进命令行。
func configureSudo(ctx context.Context, c *conn, username, password string) error {
	cmd := "sudo -S -p '' /bin/sh -c '" + sudoersScript(username) + "'"
	_, stderr, code, err := c.run(ctx, cmd, password+"\n")
	if err != nil {
		return err
	}
	switch code {
	case 0:
		return nil
	case 5:
		return &Error{Code: CodeSudoFailed, Msg: "主机拒绝了免密 sudo 规则（visudo 语法校验未通过）"}
	case 127:
		return &Error{Code: CodeSudoFailed, Msg: "主机上没有 sudo：请先安装 sudo，或在添加时不要勾选「配置免密 sudo」。"}
	default:
		return &Error{Code: CodeSudoFailed, Msg: fmt.Sprintf(
			"配置免密 sudo 失败（退出码 %d）：口令不对，或该用户不在 sudoers 里。%s", code, tail(stderr))}
	}
}

// sudoReady 报告该用户现在能不能免密 sudo（`sudo -n` 不会索要口令）。
func sudoReady(ctx context.Context, c *conn) bool {
	_, _, code, err := c.run(ctx, "sudo -n true", "")
	return err == nil && code == 0
}

// tail 截一小段远端 stderr 附在错误后面（远端信息里不含口令：口令只走 stdin）。
func tail(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	const max = 200
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return "：" + strings.ReplaceAll(s, "\n", " ")
}
