# LLM Gate

[English](README.md) | **简体中文**

在自己的设备上集中管理模型 API 与开发工具订阅，供多个客户端使用。**免费下载使用，固件源码 MIT 开源。** 可自行安装到符合要求的 Linux 服务器、个人电脑或低功耗设备，也可使用预装整机。

[官网](https://llm.net) · [安装指引](docs/install.zh-CN.md) · [下载](https://github.com/llm-net/llm-gate/releases/latest) · [English installation guide](docs/install.md)

## 安装

要求 **64 位 Linux ARM64 或 x86-64**、正在运行的 **systemd** 与 root 权限。脚本自动选择对应架构的固件并核对 SHA-256。

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

从内网访问 `http://<设备IP>/ui/`。默认管理口令为 **`llm-gate`**，不需要用户名；首次登录后立即修改。纯 HTTP 没有 TLS 保护，请在可信内网使用或配置 HTTPS。

离线安装、已有设备升级、系统要求与故障排查见[安装指引](docs/install.zh-CN.md)。

## 功能

- 在本地管理台集中维护 API 按量账号、API 订阅套餐和开发工具订阅。
- 签发客户端 API Key，配置可用模型、工具权限、请求速率与日／周／月预算。
- 在设备上记录用量与名义金额；订阅的用量金额不等于厂商现金账单。
- 通过可下载的 `gate` 引导器接入 Codex、Grok Build、Claude Code、Cursor 和 OpenCode，开发工具在使用者电脑上运行。
- 按模型启用的能力提供文本、图像与视频协议面。
- 在管理台检查固件升级、手动上传固件或回退管理台安装的升级。

| 协议面 | 客户端入口 |
| --- | --- |
| OpenAI Chat | `POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Anthropic Messages | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| 火山方舟 视频 | `/ark/api/v3/contents/generations/tasks` |
| 火山方舟 图像 | `POST /ark/api/v3/images/generations` |
| MiniMax 视频 | `/minimax/v2/video_generation`、`/minimax/v2/h3_context_ir`、`/minimax/v2/query/video_generation/{id}` |

共享模型目录的 OpenAI Responses 是经 Chat 上游承载的无状态文本转换面。开发工具订阅使用独立的 `/agents/<tool>/` 路径。图像与视频协议面保留厂商路径和任务语义。

## 本地运行

固件是单一静态 Go 应用程序 `llmgate`，由 systemd 分别启动网关与升级进程。它不是操作系统镜像，不在设备上托管模型或运行 Agent。

配置、模型凭据和用量数据保留在设备上。官网或 DNS 不可用时，本地核心服务、手动固件上传与回退仍然可用；调用远程模型仍需连接对应厂商。可选的官网账号关联仅用于内网域名与证书，官网账号不能读取或控制设备。

提示词、模型响应和上传媒体不写入固件日志。升级与目录下载保持匿名。安装成功后仅发送一次包含安装／升级事件与固件版本的匿名计数，可用 `--no-report` 关闭。

## 仓库与下载

| 路径或下载文件 | 内容 |
| --- | --- |
| [`firmware/`](firmware/) | MIT 固件源码、测试、部署文件、内嵌管理台与预编译 `gate` 制品 |
| [`catalog/`](catalog/) | 官方目录价与平台模型信息的维护源 |
| [`install.sh`](install.sh) | 与 `https://llm.net/install.sh` 逐字相同的安装脚本 |
| [`docs/install.md`](docs/install.md) / [`docs/install.zh-CN.md`](docs/install.zh-CN.md) | 英文／简体中文安装指引 |
| `llmgate-linux-arm64`、`llmgate-linux-amd64` | Linux 静态固件 |
| `gate-{linux,darwin,windows}-{amd64,arm64}` 压缩包 | 六平台预编译引导器；Unix 为 `.tar.gz`，Windows 为 `.zip` |
| `SHA256SUMS` | Release 下载文件的校验清单 |

每个 Release tag 指向对应的固件源码快照，固件和 `gate` 下载使用同一版本。官网与 `gate` 引导器源码未包含在本仓，`gate` 以预编译文件提供。第三方依赖与单独安装的组件遵循各自许可证。

## 构建固件

准备 [`firmware/go.mod`](firmware/go.mod) 要求的 Go 版本（Go 1.26 或更高）、Git 与 Make：

```sh
git clone https://github.com/llm-net/llm-gate.git
cd llm-gate/firmware
make build-amd64    # x86-64 Linux
# ARM64 Linux：make build-arm64
```

公开检出使用已包含的管理台和 `gate` 制品，Go 构建不需要 Node.js 或 `gate` 源码。`make web` 重建管理台时需要 Node.js/npm。详见[源码构建与验证](docs/install.zh-CN.md#从源码构建)。

## 许可证与反馈

固件源码采用 [MIT 许可证](LICENSE)，可按许可证使用、修改和再分发。本仓**不接受外部贡献或 Pull Request**，PR 会自动关闭。反馈方式见 [CONTRIBUTING.md](CONTRIBUTING.md)，私密漏洞报告见 [SECURITY.md](SECURITY.md)。
