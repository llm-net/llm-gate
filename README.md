# LLM Gate

**English** | [简体中文](README.zh-CN.md)

A self-hosted gateway for managing model APIs and developer-tool subscriptions across your devices. **Free to download and use; firmware source code is open source under the MIT license.** Install it on a supported Linux server, computer, or low-power device, or use a preinstalled appliance.

[Website](https://llm.net) · [Installation guide](docs/install.md) · [Downloads](https://github.com/llm-net/llm-gate/releases/latest) · [中文安装指引](docs/install.zh-CN.md)

## Install

Requires 64-bit **Linux ARM64 or x86-64**, a running **systemd**, and root access. The installer selects the correct firmware and verifies its SHA-256 checksum.

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

Open `http://<device-ip>/ui/` from your local network. The default administrator password is **`llm-gate`**, with no username. Change it immediately after signing in. Plain HTTP has no TLS protection; use a trusted local network or configure HTTPS.

For offline installation, an existing installation, system requirements, and troubleshooting, see the [installation guide](docs/install.md).

## What it does

- Manage pay-as-you-go API accounts, API subscription plans, and developer-tool subscriptions from a local web console.
- Issue client API keys with model access, tool permissions, rate limits, and daily, weekly, or monthly budgets.
- Track usage and nominal costs locally; subscription usage figures are not the provider's cash bill.
- Connect Codex, Grok Build, Claude Code, Cursor, and OpenCode using the downloadable `gate` launcher. Developer tools run on the user's computer.
- Serve text, image, and video protocol surfaces according to each model's enabled capabilities.
- Update firmware from the console, upload a firmware file manually, or roll back a console-managed update locally.

| Protocol surface | Client entry point |
| --- | --- |
| OpenAI Chat | `POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Anthropic Messages | `POST /v1/messages`, `POST /v1/messages/count_tokens` |
| Volcengine Ark Video | `/ark/api/v3/contents/generations/tasks` |
| Volcengine Ark Image | `POST /ark/api/v3/images/generations` |
| MiniMax Video | `/minimax/v2/video_generation`, `/minimax/v2/h3_context_ir`, `/minimax/v2/query/video_generation/{id}` |

The shared model catalog's OpenAI Responses surface performs stateless text conversion over a Chat upstream. Tool subscriptions use separate `/agents/<tool>/` paths. Image and video surfaces retain vendor-specific paths and task semantics.

## Local operation

The firmware is one static Go application, `llmgate`, started as separate gateway and updater processes by systemd. It is an application binary, not an operating-system image, and it does not host models or run agents on the device.

Configuration, model credentials, and usage data stay on the device. Core local services, manual firmware uploads, and rollback remain available if the website or DNS is unavailable. Calling a remote model still requires connectivity to that provider. Optional account linking is used only for local domain names and certificates; a website account cannot read or control the device.

Prompts, model responses, and uploaded media are not written to firmware logs. Update and catalog downloads are anonymous. A successful installation sends one anonymous event containing only the installation/upgrade event and firmware version; pass `--no-report` to disable it.

## Repository and releases

| Path or download | Contents |
| --- | --- |
| [`firmware/`](firmware/) | MIT firmware source, tests, deployment assets, embedded console, and prebuilt `gate` assets |
| [`catalog/`](catalog/) | Maintained official pricing and platform/model catalogs |
| [`install.sh`](install.sh) | The same installer served at `https://llm.net/install.sh` |
| [`docs/install.md`](docs/install.md) / [`docs/install.zh-CN.md`](docs/install.zh-CN.md) | English / Simplified Chinese installation guides |
| `llmgate-linux-arm64`, `llmgate-linux-amd64` | Static Linux firmware downloads |
| `gate-{linux,darwin,windows}-{amd64,arm64}` archives | Prebuilt launcher for six desktop platforms; Unix archives use `.tar.gz`, Windows archives use `.zip` |
| `SHA256SUMS` | Release download checksums |

Each release tag points to the corresponding firmware source snapshot. Firmware and `gate` downloads share the release version. The website and `gate` launcher source code are not included; `gate` is distributed as a prebuilt download. Third-party dependencies and separately installed components retain their own licenses.

## Build the firmware

Use the Go version required by [`firmware/go.mod`](firmware/go.mod) (Go 1.26 or newer), Git, and Make:

```sh
git clone https://github.com/llm-net/llm-gate.git
cd llm-gate/firmware
make build-amd64    # x86-64 Linux
# Or: make build-arm64
```

The public checkout builds with its included console and `gate` assets; Node.js and the `gate` source are not needed for a Go build. `make web` rebuilds the console and requires Node.js/npm. See the [build and verification instructions](docs/install.md#build-from-source) for details.

## License and feedback

The firmware source is available under the [MIT license](LICENSE). You may use, modify, and redistribute it under that license. This repository does **not accept external contributions or pull requests**; pull requests are automatically closed. See [CONTRIBUTING.md](CONTRIBUTING.md) for feedback and [SECURITY.md](SECURITY.md) for private vulnerability reporting.
