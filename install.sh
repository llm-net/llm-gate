#!/bin/sh
# LLM Gate 一键安装脚本。内嵌部署文件与 firmware/deploy/ 原字节一致。
# 公开仓库 llm-net/llm-gate 的 install.sh 与官网 https://llm.net/install.sh 逐字相同。
#
# 在设备上以 root 执行，安装或升级 /usr/local/bin/llmgate 并完成板上落位：
#
#   curl -fsSL https://llm.net/install.sh | sudo sh
#   curl -fsSL https://llm.net/install.sh | sudo sh -s -- --no-report
#   sudo sh install.sh --artifact ./llmgate-linux-arm64 --sha256 <hex>    # 离线：用已下载的固件
#                                  （x86-64 主机用 llmgate-linux-amd64）
#
# 做什么：核对系统（64 位 Linux：ARM64 或 x86-64，systemd）→ 按本机架构从官网固件索引取最新版本、
# 下载并核对 SHA-256 →
# 建服务用户 → 装二进制、systemd unit、polkit 授权与最小配置 → 启动或重启两个应用进程 → 本地健康
# 检查。只放置或替换 llmgate 应用程序，不重启、不刷写操作系统；已有配置与数据一律不动。
#
# 安装成功后向官网发送一次匿名计数（只含 install / upgrade 与固件版本，不含设备标识、地址或账号）；
# --no-report 或环境变量 LLMGATE_NO_REPORT=1 关闭。
set -eu

SITE=${LLMGATE_SITE:-https://llm.net}
SITE=${SITE%/}
REPO_URL=https://github.com/llm-net/llm-gate
BIN=/usr/local/bin/llmgate
CONF_DIR=/etc/llmgate
CONF=$CONF_DIR/gatewayd.yaml
UNIT_DIR=/etc/systemd/system
# 固件制品按架构分两份（ASSET 在核对系统时按 uname -m 定）：
#   aarch64 → llmgate-linux-arm64    x86_64 → llmgate-linux-amd64
ASSET=""
PLATFORM=""

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() { printf '\nLLM Gate 安装失败：%s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
用法：curl -fsSL https://llm.net/install.sh | sudo sh [-s -- 选项]
选项：
  --no-report          不向官网发送匿名安装计数（也可设环境变量 LLMGATE_NO_REPORT=1）
  --artifact FILE      用本机已有的固件文件安装，不从网络下载
  --sha256 HEX         与 --artifact 配合：固件的 SHA-256；省略则找同目录 SHA256SUMS，都没有只告警
  -h, --help           本说明
USAGE
}

# 板上落位文件（生成时从 firmware/deploy/ 原字节嵌入）。
write_deploy_files() {
  d=$1
  cat > "$d/llmgate-gatewayd.service" <<'LLMGATE_DEPLOY_FILE_EOF'
[Unit]
Description=llmgate gatewayd — AI API 网关
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=llmgate
Group=llmgate
ExecStart=/usr/local/bin/llmgate gatewayd --config /etc/llmgate/gatewayd.yaml
Restart=on-failure
RestartSec=2
# 非 root 绑定 80 端口
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
# 数据目录 /var/lib/llmgate
StateDirectory=llmgate
StateDirectoryMode=0750
# 运行期目录 /run/llmgate：Cloudflare Tunnel 启用时在这里建 origin socket
# （cloudflare-origin.sock，0660 llmgate:llmgate-tunnel）。gatewayd 要能把 socket
# 的属组改成 llmgate-tunnel，部署时须把 llmgate 用户加进该组（见 deploy/README.md）。
RuntimeDirectory=llmgate
RuntimeDirectoryMode=0755
# 基础加固
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
LimitNOFILE=65536
# 须大于 gatewayd 代码内 15s 优雅停机上限
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/llmgate-updated.service" <<'LLMGATE_DEPLOY_FILE_EOF'
[Unit]
Description=llmgate updated — 固件升级引擎
# 引擎不依赖网络（下载在 gatewayd 侧完成），只依赖本地文件系统与 systemd。
After=local-fs.target

[Service]
Type=simple
# root：它的本职就是替换 /usr/local/bin/llmgate 并 stop/start llmgate-gatewayd，
# 权限边界靠进程凭证（multi-call 架构决策）；调用面只有 UDS
# /run/llmgate-updated/updated.sock（0660 root:llmgate，gatewayd 是唯一调用方）。
User=root
Group=llmgate
ExecStart=/usr/local/bin/llmgate updated --config /etc/llmgate/gatewayd.yaml
Restart=on-failure
RestartSec=2
# /run/llmgate-updated（socket）与 /var/lib/llmgate-updated（prev 槽、DB 备份、任务状态）
RuntimeDirectory=llmgate-updated
RuntimeDirectoryMode=0750
StateDirectory=llmgate-updated
StateDirectoryMode=0700
# 加固：不能上 ProtectSystem=strict——写 /usr/local/bin 是它的本职。
ProtectHome=yes
PrivateTmp=yes
# 升级成功后引擎 self-restart（systemctl restart --no-block llmgate-updated）换上
# 新二进制里的自己；停机窗口只需覆盖状态落盘，无在途请求可言。
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/50-llmgate-network.rules" <<'LLMGATE_DEPLOY_FILE_EOF'
// polkit 本地授权（polkit 0.106+ 的 rules.d/JS 机制）：允许服务用户 llmgate
// （gatewayd，NoNewPrivileges 下无法 sudo）经 NetworkManager D-Bus 修改系统
// 连接与控制网络——管理台「设备设置」页查看/修改设备 IP 的授权基础。只放行
// 这两个动作；读取设备/连接状态本就无需授权。
//
// 与同目录 50-llmgate-network.pkla 是**同一条授权的两种机制，按板卡系统二选一**：
//   - Debian 11 / polkit 0.105（cubie-a7a）：只认 localauthority/*.pkla，
//     不认 rules.d/*.rules（JS 引擎是 0.106 才引入的）→ 装 .pkla
//   - Debian 12 / polkit 122（rk3576-evb1）：pkla 支持已被移除且该镜像没有
//     polkit-pkla-compat，/etc/polkit-1/ 下只有 rules.d → 装本文件
// 装错那一份不会报错，只会静默不生效：读得到网卡、一改就是「Insufficient
// privileges」。两块板都装上也无妨（各自只有一份被自己的 polkitd 认得）。
//
// 本文件是 JavaScript（不是 pkla 的 keyfile），注释用 //；polkitd 按需启动
// 并自动加载，无需 reload。
polkit.addRule(function (action, subject) {
    if (subject.user !== "llmgate") {
        return undefined;
    }
    if (action.id === "org.freedesktop.NetworkManager.settings.modify.system" ||
        action.id === "org.freedesktop.NetworkManager.network-control") {
        return polkit.Result.YES;
    }
    return undefined;
});
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/50-llmgate-network.pkla" <<'LLMGATE_DEPLOY_FILE_EOF'
# polkit 本地授权（Debian 11 polkit 0.105 的 localauthority/.pkla 机制，
# GLib keyfile 格式——注释只认行首 #）：允许服务用户 llmgate（gatewayd，
# NoNewPrivileges 下无法 sudo）经 NetworkManager D-Bus 修改系统连接与
# 控制网络——管理台「设备设置」页查看/修改设备 IP 的授权基础。只放行
# 这两个动作；读取设备/连接状态本就无需授权。
[llmgate-network]
Identity=unix-user:llmgate
Action=org.freedesktop.NetworkManager.settings.modify.system;org.freedesktop.NetworkManager.network-control
ResultAny=yes
ResultInactive=yes
ResultActive=yes
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/llmgate-cloudflared.service" <<'LLMGATE_DEPLOY_FILE_EOF'
[Unit]
Description=llmgate cloudflared — Cloudflare Tunnel connector
Documentation=https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/
After=network-online.target llmgate-gatewayd.service
Wants=network-online.target
# 不写 Requires/PartOf/BindsTo：Tunnel 不是网关的前置条件，也不随网关一起停。
# 本 unit **不 enable**：由 llmgate-updated（升级引擎）按管理台的启用/停用经
# systemctl start/stop 管理；每次启动前引擎先把运行期 token 写到
# /run/llmgate-cloudflared/token（0600 llmgate-tunnel）。

[Service]
Type=simple
User=llmgate-tunnel
Group=llmgate-tunnel
# 启动参数固定（docs-dev/firmware-cloudflare-tunnel.md §7.2）：不让上游程序自更新；
# metrics 只绑定 loopback 固定端口供就绪探针；token 只从文件读，不进 argv/环境变量。
ExecStart=/opt/llmgate/components/cloudflared/current/cloudflared tunnel --no-autoupdate --metrics 127.0.0.1:20241 --loglevel error --protocol auto run --token-file /run/llmgate-cloudflared/token
Restart=on-failure
RestartSec=5
# 不把 cloudflared 原始 stdout/stderr 写 journal（§8）：那里面可能带 hostname 与请求 URL。
StandardOutput=null
StandardError=null
Environment=NO_AUTOUPDATE=true
Environment=TUNNEL_METRICS=127.0.0.1:20241

# ---- 权限与沙箱（§8）----
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources @mount
UMask=0077
# 只暴露 §4.1 的文件：设备数据库、device-key、TLS 私钥、网关配置与升级引擎 socket
# 对 connector 不可见；resolv.conf 换成固定公共解析器（cloudflared-resolv.conf）。
InaccessiblePaths=-/var/lib/llmgate -/var/lib/llmgate-updated -/run/llmgate-updated -/etc/llmgate/gatewayd.yaml -/etc/llmgate/tls
BindReadOnlyPaths=/etc/llmgate/cloudflared-resolv.conf:/etc/resolv.conf
# 出站边界（§8）：只允许 Cloudflare Tunnel edge（TCP/UDP 7844）与固定 DNS 解析器；
# loopback 只为 metrics 探针放行，到设备 80/443 的回连由 llmgate-cloudflared.nft
# 按 uid 与端口拦下（systemd 的 IP 过滤不认端口）。
IPAddressDeny=any
IPAddressAllow=127.0.0.1 198.41.192.0/24 198.41.200.0/24 2606:4700:a0::/48 2606:4700:a8::/48 1.1.1.1 1.0.0.1 2606:4700:4700::1111 2606:4700:4700::1001
# 资源上限（H618：4×A53 / 2 GB）
MemoryMax=256M
TasksMax=64
LimitNOFILE=16384
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/llmgate-cloudflared.nft" <<'LLMGATE_DEPLOY_FILE_EOF'
#!/usr/sbin/nft -f
# llmgate-cloudflared.nft — connector 的 origin SSRF 隔离（docs-dev/firmware-cloudflare-tunnel.md §8）。
#
# remotely-managed tunnel 的 origin route 由 Cloudflare 侧配置：账号或 token 被攻破时，
# 攻击者可能把 route 改向 http://127.0.0.1:80 之类的本机/局域网服务。unit 里的
# IPAddressDeny/Allow 挡住了局域网与任意公网，但它不认端口，而 metrics 探针又需要
# loopback；这里按 **发起进程的 uid** 与端口把 loopback 收窄到只剩 metrics 的应答：
#
#   - llmgate-tunnel 发往 loopback 的新连接一律丢弃（它只该被动应答 20241）；
#   - 已建立连接的回包放行（gatewayd → 20241 的探针由 gatewayd 发起）。
#
# 落位：/etc/nftables.d/llmgate-cloudflared.nft（或 include 进 /etc/nftables.conf），
# `nft -f` 加载后 `systemctl enable nftables`。规则只针对该 uid，不影响 NetworkManager、
# gatewayd 或其他进程。
table inet llmgate_cloudflared {
	chain output {
		type filter hook output priority filter; policy accept;
		ct state established,related accept
		meta skuid "llmgate-tunnel" oifname "lo" ct state new counter drop
		meta skuid "llmgate-tunnel" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10, 224.0.0.0/4 } counter drop
		meta skuid "llmgate-tunnel" ip6 daddr { fc00::/7, fe80::/10, ff00::/8 } counter drop
	}
}
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/cloudflared-resolv.conf" <<'LLMGATE_DEPLOY_FILE_EOF'
# /etc/llmgate/cloudflared-resolv.conf — cloudflared 的固定解析器。
# unit 用 BindReadOnlyPaths 把它盖在 connector 命名空间里的 /etc/resolv.conf 上：
# 局域网 DHCP 分配的解析器不在 IPAddressAllow 内，connector 只走这里列出的公共解析器
# （与 unit 的 IPAddressAllow 保持一致）。
# use-vc：DNS 走 TCP/53。unit 的 IPAddressDeny=any + 具体 IPAddressAllow 在 H618 内核上
# 会把 UDP 应答丢掉（TCP 不受影响，真机钉死），connector 因此解析不到
# region1.v2.argotunnel.com 而起不来；改走 TCP 后正常，每次启动只多几次握手。
nameserver 1.1.1.1
nameserver 1.0.0.1
options timeout:3 attempts:2 use-vc
LLMGATE_DEPLOY_FILE_EOF
  cat > "$d/llmgate-mihomo.service" <<'LLMGATE_DEPLOY_FILE_EOF'
[Unit]
Description=llmgate mihomo — 板上代理内核（Clash 订阅）
Documentation=https://github.com/MetaCubeX/mihomo
After=network-online.target
Wants=network-online.target
# 不写 Requires/PartOf/BindsTo：内核不是网关的前置条件，也不随网关一起停。
# 本 unit **不 enable**：由 llmgate-updated（升级引擎）按管理台的启用/停用经
# systemctl start/stop 管理；每次启动前引擎先把固件生成的受限配置写到
# /run/llmgate-mihomo/config.yaml（0600 llmgate-proxy）并让内核 -t 自检。

[Service]
Type=simple
User=llmgate-proxy
Group=llmgate-proxy
# 启动参数固定：工作目录与配置都在 tmpfs 运行期目录；配置只绑 127.0.0.1 的 SOCKS 端口，
# 无 LAN 入站、TUN、redir/tproxy、DNS 劫持、external controller（由固件生成，不接受任意 YAML）。
ExecStart=/opt/llmgate/components/mihomo/current/mihomo -d /run/llmgate-mihomo -f /run/llmgate-mihomo/config.yaml
Restart=on-failure
RestartSec=5
# 不把内核原始 stdout/stderr 写 journal：那里面会带节点名与目标地址。
StandardOutput=null
StandardError=null
Environment=HOME=/run/llmgate-mihomo
# 内核不自更新 Geo 数据（受限配置里没有 GEO 规则，也不会去下载）。
Environment=SKIP_SAFE_PATH_CHECK=1

# ---- 权限与沙箱 ----
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources @mount
UMask=0077
ReadWritePaths=/run/llmgate-mihomo
# 设备数据库、device-key、TLS 私钥、网关配置与升级引擎 socket 对内核不可见。
InaccessiblePaths=-/var/lib/llmgate -/var/lib/llmgate-updated -/run/llmgate-updated -/etc/llmgate/gatewayd.yaml -/etc/llmgate/tls
# 资源上限（H618：4×A53 / 2 GB）
MemoryMax=384M
TasksMax=128
LimitNOFILE=65536
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
LLMGATE_DEPLOY_FILE_EOF
}

sha256_file() { sha256sum "$1" | awk '{print $1}'; }

verify_sha256() {
  got=$(sha256_file "$1")
  [ "$got" = "$2" ] || die "固件 SHA-256 不匹配（期望 $2，实际 $got）"
}

ensure_system_user() {
  getent group "$1" >/dev/null || groupadd --system "$1"
  id "$1" >/dev/null 2>&1 || useradd --system --gid "$1" --no-create-home \
    --home-dir "$2" --shell /usr/sbin/nologin --comment "$3" "$1"
}

# polkit 0.105（Debian 11）只认 localauthority/*.pkla；0.106+ 只认 rules.d/*.rules。装错那份不报错、
# 只会在管理台改 IP 时得到 Insufficient privileges，所以按版本挑，挑不出再看目录。
polkit_variant() {
  v=$(pkaction --version 2>/dev/null | awk '{print $NF}')
  case $v in
    0.10[0-5]) echo pkla ;;
    0.[0-9]*|[1-9]*) echo rules ;;
    *)
      if [ -d /etc/polkit-1/rules.d ]; then echo rules
      elif [ -d /etc/polkit-1/localauthority ]; then echo pkla
      else echo none; fi ;;
  esac
}

json_field() {
  # json_field <字段名> <正则>：从一个发布项（或整份索引）里取首个匹配字段。索引由 jq 逐行输出，
  # 单行 JSON 也先按逗号拆行再匹配。
  tr ',' '\n' | sed -n "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*\"\\($2\\)\".*/\\1/p" | head -n 1
}

index_release() {
  # index_release <平台>：从官网索引里挑出本机平台的首个（最新）发布项，输出那一个对象。
  # 发布项是扁平对象（字段值里没有花括号），按 } 切块、再把块首的索引头与 [{ 剥掉即可；
  # 没写 platform 的条目按 linux-arm64 理解。单行与 jq 逐行两种形态都认。
  awk -v want="$1" '
    BEGIN { RS = "}" }
    /"artifactUrl"/ {
      sub(/^.*\{/, "", $0)
      p = "linux-arm64"
      if (match($0, /"platform"[[:space:]]*:[[:space:]]*"[^"]*"/)) {
        p = substr($0, RSTART, RLENGTH); sub(/^"platform"[[:space:]]*:[[:space:]]*"/, "", p); sub(/"$/, "", p)
      }
      if (p == want) { print "{\n" $0 "\n}"; exit }
    }'
}

main() {
  report=1
  artifact=""
  sum=""
  while [ $# -gt 0 ]; do
    case $1 in
      --no-report) report=0 ;;
      --artifact) shift; artifact=${1:-}; [ -n "$artifact" ] || die '--artifact 缺少文件路径' ;;
      --sha256) shift; sum=${1:-}; [ -n "$sum" ] || die '--sha256 缺少值' ;;
      -h|--help) usage; exit 0 ;;
      *) usage >&2; die "不认识的参数：$1" ;;
    esac
    shift
  done
  [ -z "${LLMGATE_NO_REPORT:-}" ] || report=0

  step "核对系统"
  [ "$(id -u)" = 0 ] || die '请以 root 执行：curl -fsSL https://llm.net/install.sh | sudo sh'
  [ "$(uname -s)" = Linux ] || die "只支持 Linux（当前 $(uname -s)）"
  case $(uname -m) in
    aarch64|arm64) ASSET=llmgate-linux-arm64; PLATFORM=linux-arm64 ;;
    x86_64|amd64) ASSET=llmgate-linux-amd64; PLATFORM=linux-amd64 ;;
    *) die "固件只提供 ARM64（aarch64）与 x86-64 版本，当前架构 $(uname -m)" ;;
  esac
  for c in systemctl curl sha256sum install useradd groupadd getent awk sed; do
    command -v "$c" >/dev/null 2>&1 || die "缺少命令 $c"
  done
  [ -d /run/systemd/system ] || die '设备没有在 systemd 下运行'
  tmp=$(mktemp -d /tmp/llmgate-install.XXXXXX) || die '无法创建临时目录'
  trap 'rm -rf "$tmp"' EXIT HUP INT TERM
  say "系统 $(uname -m) · $(. /etc/os-release 2>/dev/null && printf '%s' "${PRETTY_NAME:-Linux}") · 固件 $ASSET"

  step "获取固件"
  if [ -n "$artifact" ]; then
    [ -f "$artifact" ] || die "找不到文件 $artifact"
    cp "$artifact" "$tmp/$ASSET"
    if [ -z "$sum" ] && [ -f "$(dirname "$artifact")/SHA256SUMS" ]; then
      sum=$(awk -v n="$(basename "$artifact")" '$2 == n {print $1}' "$(dirname "$artifact")/SHA256SUMS")
    fi
    if [ -n "$sum" ]; then verify_sha256 "$tmp/$ASSET" "$sum"; say "SHA-256 核对通过"
    else say "警告：没有 SHA-256 可核对，按原样使用 $artifact"; fi
  else
    index=$(curl -fsSL -m 20 "$SITE/updates/firmware/stable.json" 2>/dev/null) || index=""
    release=$(printf '%s' "$index" | index_release "$PLATFORM")
    url=$(printf '%s' "$release" | json_field artifactUrl '[^"]*')
    sum=$(printf '%s' "$release" | json_field artifactSha256 '[0-9a-f]\{64\}')
    if [ -n "$url" ] && [ -n "$sum" ]; then
      case $url in /*) url=$SITE$url ;; esac
      say "官网索引：$(printf '%s' "$release" | json_field version '[^"]*')（$PLATFORM）"
    else
      url=$REPO_URL/releases/latest/download/$ASSET
      sum=""
      say "官网索引不可用，改用 GitHub 最新 Release"
    fi
    say "下载 $url"
    progress=--progress-bar
    [ -t 2 ] || progress=-sS   # 非终端（ssh 管道、日志）里进度条只会刷屏
    curl -fL --retry 3 --retry-delay 2 -m 1800 $progress -o "$tmp/$ASSET" "$url" \
      || die "下载固件失败（网络不通或 GitHub 不可达）。可先在别处下载 Release 里的 $ASSET 与 SHA256SUMS，再用 --artifact 安装"
    if [ -z "$sum" ]; then
      curl -fsSL --retry 3 -m 60 -o "$tmp/SHA256SUMS" "$REPO_URL/releases/latest/download/SHA256SUMS" \
        || die '下载 SHA256SUMS 失败'
      sum=$(awk -v n="$ASSET" '$2 == n {print $1}' "$tmp/SHA256SUMS")
      [ -n "$sum" ] || die 'SHA256SUMS 里没有固件条目'
    fi
    verify_sha256 "$tmp/$ASSET" "$sum"
    say "SHA-256 核对通过"
  fi
  head -c 4 "$tmp/$ASSET" | grep -q ELF || die '下载的文件不是 ELF 可执行文件'
  chmod 0755 "$tmp/$ASSET"
  version=$("$tmp/$ASSET" --version 2>/dev/null | awk '{print $2}') || true
  [ -n "$version" ] || die '固件无法在本机运行（架构不符或文件损坏）'
  case $version in *[!0-9A-Za-z.-]*) die "固件自述版本异常：$version" ;; esac
  mode=install
  old=""
  if [ -x "$BIN" ]; then
    mode=upgrade
    old=$("$BIN" --version 2>/dev/null | awk '{print $2}') || true
  fi
  if [ "$mode" = upgrade ]; then say "固件 $version（当前已装 ${old:-未知}，将替换）"; else say "固件 $version"; fi

  step "落位"
  ensure_system_user llmgate /var/lib/llmgate "llmgate gatewayd service"
  install -m 0755 -o root -g root "$tmp/$ASSET" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  install -d -m 0750 -o root -g llmgate "$CONF_DIR"
  if [ -f "$CONF" ]; then
    say "保留已有配置 $CONF"
  else
    umask 027
    cat > "$CONF" <<'CONF_EOF'
# LLM Gate 网关配置（由 install.sh 生成的最小形态）。完整字段见公开仓库 firmware/deploy/gatewayd.yaml.example。
# 上游账号、模型与 API 密钥都在管理台维护；官网整段可省略，缺省 https://llm.net。
listen: "0.0.0.0:80"
data_dir: "/var/lib/llmgate"
log_level: "info"
CONF_EOF
    chown root:llmgate "$CONF"
    chmod 0640 "$CONF"
    umask 022
    say "写入最小配置 $CONF（监听 :80）"
  fi
  mkdir -p "$tmp/deploy"
  write_deploy_files "$tmp/deploy"
  install -m 0644 -o root -g root "$tmp/deploy/llmgate-gatewayd.service" "$UNIT_DIR/"
  install -m 0644 -o root -g root "$tmp/deploy/llmgate-updated.service" "$UNIT_DIR/"
  case $(polkit_variant) in
    rules)
      install -d -m 0755 /etc/polkit-1/rules.d
      install -m 0644 -o root -g root "$tmp/deploy/50-llmgate-network.rules" /etc/polkit-1/rules.d/
      say "polkit 授权：rules.d（管理台可修改设备 IP）" ;;
    pkla)
      install -d -m 0755 /etc/polkit-1/localauthority/50-local.d
      install -m 0644 -o root -g root "$tmp/deploy/50-llmgate-network.pkla" /etc/polkit-1/localauthority/50-local.d/
      say "polkit 授权：localauthority（管理台可修改设备 IP）" ;;
    *) say "没有 polkit：管理台修改设备 IP 不可用，其余功能不受影响" ;;
  esac
  # 可选组件的落位（Cloudflare Tunnel connector、Clash 订阅的板上 Mihomo 内核）：只放 unit 与专用用户，
  # 不 enable；可执行文件由固件在管理台启用时按官方 release 校验安装。
  ensure_system_user llmgate-tunnel /nonexistent "llmgate cloudflared connector"
  ensure_system_user llmgate-proxy /nonexistent "llmgate mihomo proxy core"
  usermod -aG llmgate-tunnel llmgate
  install -d -m 0755 -o root -g root /opt/llmgate/components
  install -m 0644 -o root -g root "$tmp/deploy/llmgate-cloudflared.service" "$UNIT_DIR/"
  install -m 0644 -o root -g root "$tmp/deploy/llmgate-mihomo.service" "$UNIT_DIR/"
  install -m 0644 -o root -g root "$tmp/deploy/cloudflared-resolv.conf" "$CONF_DIR/cloudflared-resolv.conf"
  install -d -m 0755 /etc/nftables.d
  install -m 0644 -o root -g root "$tmp/deploy/llmgate-cloudflared.nft" /etc/nftables.d/
  systemctl daemon-reload

  step "启动"
  if [ "$mode" = install ]; then
    systemctl enable --now -q llmgate-gatewayd llmgate-updated
  else
    systemctl enable -q llmgate-gatewayd llmgate-updated
    systemctl restart llmgate-gatewayd
    systemctl try-restart llmgate-updated
  fi
  port=$(sed -n 's/^listen:[[:space:]]*"\{0,1\}[^"]*:\([0-9][0-9]*\)"\{0,1\}.*$/\1/p' "$CONF" | head -n 1)
  [ -n "$port" ] || port=80
  i=0
  until curl -fsS -m 2 -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -ge 40 ]; then
      journalctl -u llmgate-gatewayd -n 30 --no-pager >&2 || true
      die "网关未在 20 秒内就绪（http://127.0.0.1:$port/healthz）"
    fi
    sleep 0.5
  done
  installed=$("$BIN" --version 2>/dev/null | awk '{print $2}') || true
  [ "$installed" = "$version" ] || die "安装后版本不符：$installed"

  if [ "$report" = 1 ]; then
    if curl -fsS -m 5 -o /dev/null -X POST -H 'Content-Type: application/json' \
         --data "{\"event\":\"$mode\",\"version\":\"$version\"}" "$SITE/api/install/report" 2>/dev/null; then
      say "已向官网发送一次匿名安装计数（只含 $mode 与版本号；--no-report 可关闭）"
    fi
  fi

  step "完成：LLM Gate $version（$mode）"
  suffix=""
  [ "$port" = 80 ] || suffix=":$port"
  for ip in $(hostname -I 2>/dev/null | tr ' ' '\n' | grep -v '^$' | head -n 3); do
    say "管理台：http://$ip$suffix/"
  done
  if [ "$mode" = install ]; then
    say "默认管理口令：llm-gate（无需用户名），首次登录后立即修改口令。"
  fi
  say "升级与回退：管理台「设备设置 → 固件升级」；说明：$REPO_URL/blob/main/docs/install.md"
}

main "$@"
