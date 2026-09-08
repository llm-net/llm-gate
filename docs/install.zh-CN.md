# 安装 LLM Gate

[English](install.md) | **简体中文** · [返回首页](../README.zh-CN.md)

LLM Gate 固件是 Linux 静态应用程序。安装会放置程序、服务用户、systemd unit 与配置；升级只重启应用进程，保留已有配置与数据。脚本不会重启操作系统。

## 系统要求

- 64 位 Linux：`aarch64` / `arm64` 或 `x86_64` / `amd64`。
- 正在运行的 systemd，以及 root 权限（直接使用或通过 `sudo`）。
- `curl`、CA 证书、SHA-256 工具、标准 shell 工具与用户／用户组管理命令。脚本会检查必要命令，不负责安装系统软件包。
- 默认配置需要空闲的 TCP 80 端口；可选 HTTPS 通常使用 443。
- 在线安装需要访问 `https://llm.net` 与 GitHub Release 下载地址；无外网时按下文离线安装。

只有通过管理台修改主机网络配置时才需要 NetworkManager 与 polkit。请将主机时区设为日／周／月预算使用的现场时区。Windows 与 macOS 下载文件是 `gate` 客户端，固件运行在 Linux 上。

## 在线安装

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

需要先查看脚本再执行时：

```sh
curl -fsSL https://llm.net/install.sh -o install.sh
less install.sh
sudo sh install.sh
```

脚本选择本机架构，从官网稳定索引取得固件并核对 SHA-256 后安装。索引不可用时使用 GitHub 最新 Release 及其 `SHA256SUMS`。已有 `/etc/llmgate/gatewayd.yaml` 会保留；启动服务后等待本地健康检查通过。脚本终端提示为中文。

安装成功后向官网发送一次匿名 `{event, version}` 计数，报告不含设备标识、地址或账号。关闭完成报告：

```sh
curl -fsSL https://llm.net/install.sh | sudo sh -s -- --no-report
```

官网还会按天聚合统计安装脚本获取次数，`--no-report` 关闭的是完成报告。

## 首次登录

1. 在内网打开 `http://<设备IP>/ui/`。
2. 使用默认管理口令 **`llm-gate`** 登录，不需要用户名。
3. 通过顶栏设备名菜单立即修改管理口令。
4. 添加模型 API 账号或开发工具订阅，再签发具有所需权限的客户端 API Key。
5. 按管理台「使用指南」连接应用，或在电脑上安装 `gate`。Key 持有人可打开 `/ui/connect` 查看接入指引。

纯 IP HTTP 没有 TLS 保护，请在可信内网使用，或配置客户端信任的 HTTPS 证书。可选的官网账号关联仅用于内网域名与证书，不授予官网使用者设备控制权限。

## 离线安装或安装指定版本

在有网络的电脑上打开 [Releases](https://github.com/llm-net/llm-gate/releases)，从**同一个版本**下载：

- `install.sh`
- `SHA256SUMS`
- ARM64 用 `llmgate-linux-arm64`，x86-64 用 `llmgate-linux-amd64`

将文件传到目标 Linux 主机的同一个目录。以 x86-64 为例：

```sh
cd /path/to/downloads
awk '$2 == "install.sh" || $2 == "llmgate-linux-amd64"' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256
sudo sh install.sh --artifact ./llmgate-linux-amd64 --no-report
```

ARM64 主机将两条命令中的 `llmgate-linux-amd64` 换成 `llmgate-linux-arm64`。确认校验输出中**两个文件均为 `OK`**。安装脚本还会从固件旁的 `SHA256SUMS` 核对固件。也可显式传入预期摘要：

```sh
sudo sh install.sh --artifact ./llmgate-linux-arm64 --sha256 <预期SHA256> --no-report
```

始终提供 `SHA256SUMS` 或 `--sha256`：本地文件模式在二者都缺失时只会告警。没有外网时，本地核心服务仍可使用；远程模型调用、在线升级与可选的域名／证书服务需要连接各自上游。

## 验证安装

在安装固件的 Linux 主机上执行：

```sh
llmgate --version
systemctl is-active llmgate-gatewayd llmgate-updated
curl -fsS http://127.0.0.1/healthz
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1/
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1/v1/models
```

两个服务都应为 `active`。`/healthz` 返回 HTTP 200，`/` 返回 302 跳转，未携带 API Key 的 `/v1/models` 返回 401。修改过监听端口时同步调整命令。

## 配置与文件

| 路径 | 用途 |
| --- | --- |
| `/usr/local/bin/llmgate` | 单一 multi-call 应用程序 |
| `/etc/llmgate/gatewayd.yaml` | 配置，`root:llmgate`，权限 `0640` |
| `/var/lib/llmgate/` | 本地数据库、设备加密密钥与网关数据 |
| `/var/lib/llmgate-updated/` | 升级状态与回退数据 |
| `/run/llmgate-updated/updated.sock` | 本地升级引擎 socket |
| `/etc/systemd/system/llmgate-*.service` | 网关、升级与可选组件 unit |
| `/opt/llmgate/components/` | 单独安装的可选组件 |

配置目录必须为 `root:llmgate`、权限 `0750`。最小配置：

```yaml
listen: "0.0.0.0:80"
data_dir: "/var/lib/llmgate"
log_level: "info"
```

`official_site.base_url` 缺省为 `https://llm.net`，完整字段见 [`gatewayd.yaml.example`](../firmware/deploy/gatewayd.yaml.example)。初始导入后，模型账号、模型与客户端 Key 在管理台维护；编辑 YAML seed 条目不会覆盖已有数据库记录。

修改配置后执行：

```sh
sudo systemctl restart llmgate-gatewayd llmgate-updated
```

配置和数据应成套备份，特别是 `llmgate.db` 与 `device-key` 必须一起保留，数据库内封存的凭据依赖该密钥。使用一致性备份，或停止应用服务后复制数据。

可选的 cloudflared 与 Mihomo 在管理台显式配置后单独下载。安装脚本只放置其 unit 与专用用户，不启用组件，也不携带其可执行文件。

## HTTPS

使用自管证书时同时配置以下三项：

```yaml
tls:
  listen: "0.0.0.0:443"
  cert_file: "/etc/llmgate/tls/cert.pem"
  key_file: "/etc/llmgate/tls/key.pem"
```

证书须覆盖客户端实际使用的域名。私钥仅允许 root 与 `llmgate` 服务组读取，例如文件权限 `0640`、父目录 `0750`。更新配置后重启服务。如同时启用管理台的内网域名 HTTPS 监听，两者应使用不同端口。

## 升级与回退

正常入口为管理台 **「设备设置 → 固件升级」**。设备选择对应 Linux 架构，核对下载文件，管理员显式操作后才安装。升级引擎会检查健康状态，失败时自动回退。有上一槽位时，也可在管理台手动回退；本地固件上传与回退不依赖官网。

再次运行安装脚本可原地升级，保留配置和数据：

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

脚本替换不会创建管理台升级引擎的回退槽位。需要受管理的升级与回退时使用管理台，或自行保留已知可用版本用于恢复。通过 SSH 安装指定版本或恢复时，使用同版本脚本、固件和校验清单，按上文 `--artifact` 方式执行；这只重启 LLM Gate 进程。

## 从源码构建

准备 [`go.mod`](../firmware/go.mod) 要求的 Go 1.26 或更高版本、Git 与 Make。需要指定源码快照时检出对应 Release tag：

```sh
git clone https://github.com/llm-net/llm-gate.git
cd llm-gate
# 可选：git checkout <release-tag>
cd firmware
make build-amd64
# ARM64：make build-arm64
```

制品为 `firmware/bin/llmgate-linux-amd64` 或 `firmware/bin/llmgate-linux-arm64`。构建使用 `CGO_ENABLED=0`；检出中已包含管理台与预编译 `gate` 压缩包，Go 构建不需要 Node.js 或 `gate` 源码。重建管理台时，安装其依赖所要求的 Node.js 版本后执行 `make web`。

基础验证：

```sh
CGO_ENABLED=0 go build ./...
make check
make test
bash scripts/smoke.sh
```

缺省测试使用本地夹具，出网测试需要显式启用。仅固件的公开检出会跳过部分官网文件检查。完整的 Windows 安装脚本夹具检查需要 PowerShell 7，确保 `pwsh` 在 `PATH` 中。

安装自行编译的固件时，先生成摘要，再将其传给安装脚本：

```sh
sha256sum bin/llmgate-linux-amd64
sudo sh ../install.sh --artifact ./bin/llmgate-linux-amd64 --sha256 <上一步摘要> --no-report
```

## 故障排查

- **没有 systemd／架构不支持：** 使用运行 systemd 的 64 位 Linux，普通容器或 32 位系统不满足要求。
- **80 端口被占用：** 在 `/etc/llmgate/gatewayd.yaml` 中改为空闲端口，重启服务，并在管理台和 API 地址中加入端口。
- **读取配置权限不足：** 检查文件属主与权限，以及 `/etc/llmgate` 对 `llmgate` 组的目录执行权限。
- **GitHub 下载失败：** 按离线步骤安装，保持 TLS 证书校验开启。
- **官网或域名服务不可用：** 用设备内网 IP 访问，模型调用取决于对应上游是否可达。
- **无法修改网络设置：** 主机需要 NetworkManager 与正确的 polkit 支持，其他网关功能仍可使用。

在本地查看服务状态与元数据日志：

```sh
systemctl status llmgate-gatewayd llmgate-updated --no-pager
journalctl -u llmgate-gatewayd -u llmgate-updated -n 100 --no-pager
```

分享诊断信息前移除凭据与个人数据。私密安全报告见 [SECURITY.md](../SECURITY.md)。
