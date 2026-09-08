#!/bin/sh
set -eu

die() { printf 'gate 安装失败：%s\n' "$*" >&2; exit 1; }

base=${1:-}
key=${2:-}
[ -n "$base" ] || die '缺少设备地址'
base=${base%/}
case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "不支持的系统：$(uname -s)" ;;
esac
case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "不支持的架构：$(uname -m)" ;;
esac

name=gate-${os}-${arch}.tar.gz
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/gate-install.XXXXXX") || die '无法创建临时目录'
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM
curl -fsSL --max-time 900 -o "$tmpdir/$name" "$base/gate-helper/releases/$name" || die '下载 gate 失败'
curl -fsSL --max-time 30 -o "$tmpdir/SHA256SUMS" "$base/gate-helper/releases/SHA256SUMS" || die '下载校验清单失败'
want=$(awk -v n="$name" '$2 == n {print $1}' "$tmpdir/SHA256SUMS")
[ -n "$want" ] || die '校验清单没有当前平台'
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmpdir/$name" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmpdir/$name" | awk '{print $1}')
else
  die '系统缺少 SHA-256 校验工具'
fi
[ "$got" = "$want" ] || die 'gate 压缩包 SHA-256 不匹配'
tar -xzf "$tmpdir/$name" -C "$tmpdir" || die '解压 gate 失败'
[ -x "$tmpdir/gate" ] || die '压缩包缺少 gate'

# The downloaded gate holds the operation and usage locks throughout commit.
GATE_INSTALL_STAGE="$tmpdir" GATE_INSTALL_BASE="$base" GATE_INSTALL_KEY="$key" GATE_INSTALL_OS="$os" \
  "$tmpdir/gate" __installer-exec sh -s <<'GATE_INSTALL_COMMIT'
set -eu
die() { printf 'gate 安装失败：%s\n' "$*" >&2; exit 1; }
tmpdir=$GATE_INSTALL_STAGE
base=$GATE_INSTALL_BASE
key=$GATE_INSTALL_KEY
os=$GATE_INSTALL_OS
unset GATE_INSTALL_KEY

bindir=${GATE_INSTALL_DIR:-"$HOME/.local/bin"}
GATE_INSTALL_DIR_CREATED=0
[ -d "$bindir" ] || GATE_INSTALL_DIR_CREATED=1
export GATE_INSTALL_DIR_CREATED
mkdir -p "$bindir" || die "无法创建 $bindir"
bindir=$(cd "$bindir" && pwd -P) || die '无法确定安装目录'
target=$bindir/gate
case ${GATE_CONFIG_DIR:-} in
  ?*) config_file=${GATE_CONFIG_DIR%/}/config.json ;;
  *)
    case $os in
      darwin) config_file=$HOME/Library/Application\ Support/gate/config.json ;;
      *) config_file=${XDG_CONFIG_HOME:-"$HOME/.config"}/gate/config.json ;;
    esac
    ;;
esac

had_program=0
if [ -f "$target" ]; then
  cp -p "$target" "$tmpdir/gate.previous" || die '备份现有 gate 失败'
  had_program=1
fi
had_config=0
if [ -f "$config_file" ]; then
  cp -p "$config_file" "$tmpdir/config.previous" || die '备份现有配置失败'
  had_config=1
fi

locator_file=$target.install.json
had_locator=0
if [ -f "$locator_file" ]; then
  cp -p "$locator_file" "$tmpdir/locator.previous" || die '备份安装定位记录失败'
  had_locator=1
fi
state_file=$(dirname "$config_file")/install-state.json
had_state=0
if [ -f "$state_file" ]; then
  cp -p "$state_file" "$tmpdir/state.previous" || die '备份安装归属记录失败'
  had_state=1
fi

rollback() {
  failed=0
  if [ "$had_program" = 1 ]; then
    if ! cp -p "$tmpdir/gate.previous" "$target.rollback" ||
       ! mv -f "$target.rollback" "$target"; then
      failed=1
    fi
  elif ! rm -f "$target"; then
    failed=1
  fi
  if [ "$had_config" = 1 ]; then
    if ! mkdir -p "$(dirname "$config_file")" ||
       ! cp -p "$tmpdir/config.previous" "$config_file.rollback" ||
       ! mv -f "$config_file.rollback" "$config_file"; then
      failed=1
    fi
  elif ! rm -f "$config_file"; then
    failed=1
  fi
  if [ "$had_state" = 1 ]; then
    if ! cp -p "$tmpdir/state.previous" "$state_file.rollback" ||
       ! mv -f "$state_file.rollback" "$state_file"; then failed=1; fi
  elif ! rm -f "$state_file"; then failed=1; fi
  if [ "$had_locator" = 1 ]; then
    if ! cp -p "$tmpdir/locator.previous" "$locator_file.rollback" ||
       ! mv -f "$locator_file.rollback" "$locator_file"; then failed=1; fi
  elif ! rm -f "$locator_file"; then failed=1; fi
  if [ "$failed" = 1 ]; then
    printf 'gate 安装失败，且自动回退没有完整成功；请检查 %s 与 %s\n' "$target" "$config_file" >&2
    return 1
  fi
  return 0
}

chmod 755 "$tmpdir/gate"
mv "$tmpdir/gate" "$target.new" || die '暂存新程序失败'
mv -f "$target.new" "$target" || die '替换 gate 失败'
if ! chmod 755 "$target"; then
  rollback || true
  die '设置 gate 执行权限失败，已尝试恢复原状态'
fi

if [ -n "$key" ]; then
  if ! "$target" bootstrap "$base" "$key"; then
    rollback || true
    die 'gate 初始化失败：常见原因是设备地址或 API Key 验证失败，确切原因见上方 gate 输出；已尝试恢复原状态'
  fi
elif ! "$target" bootstrap "$base"; then
  rollback || true
  die 'gate 初始化失败：常见原因是设备地址或 API Key 验证失败，确切原因见上方 gate 输出；已尝试恢复原状态'
fi

if ! "$target" __record-install; then
  rollback || true
  die '安装归属记录写入失败，已尝试恢复原状态'
fi

printf 'gate 已安装到 %s\n'  "$target"

# 安装目录不在 PATH 时，把它写进登录 shell 的启动文件。只在程序与配置都已提交后进行；
# 写入失败不影响安装结果，只退回手工提示。$HOME 下的目录在启动文件里写成 $HOME 形式。
append_path_line() {
  # $1 启动文件，$2 要追加的行；文件里已有同一行则不重复追加。
  if [ -f "$1" ] && grep -Fqx -e "$2" -- "$1" 2>/dev/null; then
    return 0
  fi
  mkdir -p "$(dirname "$1")" 2>/dev/null || return 1
  printf '\n# gate: LLM Gate 开发工具引导器\n%s\n' "$2" >> "$1" 2>/dev/null || return 1
  "$target" __record-install --path-block "$1" "$bindir" "$2" || return 1
  written=${written:+$written、}$1
}

case :${PATH:-}: in
  *:"$bindir":*) ;;
  *)
    escape_path() { printf '%s' "$1" | sed 's/[\\"`$]/\\&/g'; }
    case $bindir in
      "$HOME"/*) display_dir='$HOME'$(escape_path "${bindir#"$HOME"}") ;;
      *) display_dir=$(escape_path "$bindir") ;;
    esac
    posix_line="export PATH=\"$display_dir:\$PATH\""
    fish_line="if not contains -- \"$display_dir\" \$PATH; set -gx PATH \"$display_dir\" \$PATH; end"
    now_line=$posix_line
    written=
    path_failed=
    shell_name=${SHELL:-}
    shell_name=${shell_name##*/}
    if [ -z "$shell_name" ]; then
      case $os in
        darwin) shell_name=zsh ;;
        *) shell_name=bash ;;
      esac
    fi
    case $shell_name in
      zsh)
        append_path_line "${ZDOTDIR:-$HOME}/.zshrc" "$posix_line" || path_failed=1
        ;;
      bash)
        append_path_line "$HOME/.bashrc" "$posix_line" || path_failed=1
        if [ "$os" = darwin ]; then
          append_path_line "$HOME/.bash_profile" "$posix_line" || path_failed=1
        fi
        ;;
      fish)
        now_line="set -gx PATH \"$display_dir\" \$PATH"
        append_path_line "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/gate.fish" "$fish_line" || path_failed=1
        ;;
      sh|dash|ash|ksh|mksh)
        append_path_line "$HOME/.profile" "$posix_line" || path_failed=1
        ;;
    esac
    if [ -n "$written" ]; then
      printf '已把 %s 写入 %s；请重新打开终端，或在当前终端先执行：\n  %s\n' "$bindir" "$written" "$now_line"
    fi
    if [ -n "$path_failed" ] || [ -z "$written" ]; then
      printf '请把 %s 加入 PATH，例如在当前终端执行：\n  %s\n' "$bindir" "$now_line"
    fi
    ;;
esac

GATE_INSTALL_COMMIT
