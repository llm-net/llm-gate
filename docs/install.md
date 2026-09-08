# Install LLM Gate

**English** | [简体中文](install.zh-CN.md) · [Home](../README.md)

LLM Gate firmware is a static Linux application. Installation adds the application, its service users, systemd units, and configuration; upgrades restart the application processes and preserve existing configuration and data. The installer does not reboot the operating system.

## Requirements

- 64-bit Linux: `aarch64` / `arm64`, or `x86_64` / `amd64`.
- A running systemd and root access (directly or through `sudo`).
- `curl`, CA certificates, SHA-256 tools, standard shell utilities, and user/group management tools. The installer checks required commands; it does not install OS packages.
- An available TCP port 80 for the default configuration. HTTPS is optional and normally uses port 443.
- For online installation, access to `https://llm.net` and GitHub release downloads. Offline installation is described below.

NetworkManager and polkit are needed only for changing host network settings from the console. Set the host's timezone to the local timezone used for daily, weekly, and monthly budgets. Windows and macOS downloads are for the `gate` client; the firmware runs on Linux.

## Online installation

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

To inspect the script before running it:

```sh
curl -fsSL https://llm.net/install.sh -o install.sh
less install.sh
sudo sh install.sh
```

The script selects the architecture, reads the stable firmware index, downloads the firmware, and verifies SHA-256 before installation. If the index is unavailable, it uses GitHub's latest release and its `SHA256SUMS`. It preserves `/etc/llmgate/gatewayd.yaml` if present and waits for the local health check after starting the services. The installer's terminal messages are in Chinese.

A successful run sends one anonymous `{event, version}` count to the website, without a device identifier, address, or account in the report. To disable that report:

```sh
curl -fsSL https://llm.net/install.sh | sudo sh -s -- --no-report
```

The website also counts installer downloads in daily aggregates. `--no-report` disables the completion report.

## First login

1. Open `http://<device-ip>/ui/` from the local network.
2. Sign in with the default administrator password **`llm-gate`**; there is no username.
3. Change the password using the device-name menu in the top bar.
4. Add a model API account or developer-tool subscription, then create a client API key with the required permissions.
5. Follow the console's usage guides to configure applications or install `gate` on your computer. A key holder can open `/ui/connect` for the connection guide.

Plain IP HTTP has no TLS protection. Use it on a trusted local network, or configure HTTPS with a certificate trusted by your clients. Optional website account linking is needed only for local domain names and certificates; it does not grant website users device control.

## Offline installation or a specific release

On a connected computer, open [Releases](https://github.com/llm-net/llm-gate/releases) and download **from the same release**:

- `install.sh`
- `SHA256SUMS`
- `llmgate-linux-arm64` for ARM64, or `llmgate-linux-amd64` for x86-64

Transfer the files into one directory on the target Linux host. For x86-64, run:

```sh
cd /path/to/downloads
awk '$2 == "install.sh" || $2 == "llmgate-linux-amd64"' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256
sudo sh install.sh --artifact ./llmgate-linux-amd64 --no-report
```

For ARM64, replace `llmgate-linux-amd64` with `llmgate-linux-arm64` in both commands. Confirm that **both** files appear in the checksum output with `OK`. The script also checks the firmware against the adjacent `SHA256SUMS`. Alternatively, supply its expected digest explicitly:

```sh
sudo sh install.sh --artifact ./llmgate-linux-arm64 --sha256 <expected-sha256> --no-report
```

Always provide `SHA256SUMS` or `--sha256`: local artifact mode only warns if neither is available. Core local services work without Internet access; remote model calls, online updates, and optional domain/certificate services require their corresponding upstream connections.

## Verify the installation

Run these commands on the installed Linux host:

```sh
llmgate --version
systemctl is-active llmgate-gatewayd llmgate-updated
curl -fsS http://127.0.0.1/healthz
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1/
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1/v1/models
```

Both services should be `active`. `/healthz` returns HTTP 200, `/` redirects with 302, and `/v1/models` without an API key returns 401. Adjust the port if you changed the listener.

## Configuration and files

| Path | Purpose |
| --- | --- |
| `/usr/local/bin/llmgate` | Static multi-call application |
| `/etc/llmgate/gatewayd.yaml` | Configuration, `root:llmgate`, mode `0640` |
| `/var/lib/llmgate/` | Local database, device encryption key, and gateway data |
| `/var/lib/llmgate-updated/` | Updater state and rollback data |
| `/run/llmgate-updated/updated.sock` | Local updater socket |
| `/etc/systemd/system/llmgate-*.service` | Gateway, updater, and optional component units |
| `/opt/llmgate/components/` | Separately installed optional components |

The configuration directory must be `root:llmgate` with mode `0750`. A minimal configuration is:

```yaml
listen: "0.0.0.0:80"
data_dir: "/var/lib/llmgate"
log_level: "info"
```

`official_site.base_url` defaults to `https://llm.net`. See [`gatewayd.yaml.example`](../firmware/deploy/gatewayd.yaml.example) for fields. Model accounts, models, and client keys are maintained in the console after initial import; editing a YAML seed entry does not overwrite existing database entries.

After changing the configuration:

```sh
sudo systemctl restart llmgate-gatewayd llmgate-updated
```

Back up the configuration and data together. In particular, keep `llmgate.db` and `device-key` together: the database's encrypted credentials depend on that key. Use a consistent backup or stop the application services while copying the data.

Optional cloudflared and Mihomo binaries are downloaded separately after explicit setup in the console. The installer places their units and service users but does not enable those components or bundle their executables.

## HTTPS

For a certificate you manage yourself, add all three fields:

```yaml
tls:
  listen: "0.0.0.0:443"
  cert_file: "/etc/llmgate/tls/cert.pem"
  key_file: "/etc/llmgate/tls/key.pem"
```

The certificate must cover the hostname clients use. Keep the private key readable only by root and the `llmgate` service group (for example, `0640`, with directory mode `0750`). Restart the services after updating this configuration. If you also configure the console's local-domain HTTPS listener, give the two listeners different ports.

## Upgrade and rollback

The normal path is **Device settings → Firmware update** in the console. The device selects its Linux architecture, validates the downloaded file, and installs only after the administrator's action. The updater checks health and automatically rolls back a failed installation. The console also supports local firmware upload and rollback when a previous slot is available, without the website.

Rerunning the installer upgrades in place and preserves configuration and data:

```sh
curl -fsSL https://llm.net/install.sh | sudo sh
```

Installer-based replacement does not create the console updater's rollback slot. Use the console for a managed upgrade with rollback, or keep a known-good release file for manual recovery. To pin a version or recover by SSH, use that release's script, firmware, and checksums with `--artifact` as above. This restarts LLM Gate processes only.

## Build from source

Use Go 1.26 or newer as required by [`go.mod`](../firmware/go.mod), Git, and Make. Choose a release tag if you need a particular source snapshot:

```sh
git clone https://github.com/llm-net/llm-gate.git
cd llm-gate
# Optional: git checkout <release-tag>
cd firmware
make build-amd64
# Or: make build-arm64
```

The resulting file is `firmware/bin/llmgate-linux-amd64` or `firmware/bin/llmgate-linux-arm64`. Go builds use `CGO_ENABLED=0`; the console and prebuilt `gate` archives are included in the checkout, so Node.js and `gate` source are unnecessary. To rebuild the console, install the Node.js version required by its dependencies and run `make web`.

Basic verification:

```sh
CGO_ENABLED=0 go build ./...
make check
make test
bash scripts/smoke.sh
```

The default tests use local fixtures; online tests are opt-in. Some tests for website files are skipped in this firmware-only checkout. PowerShell 7 (`pwsh` on `PATH`) is needed for the full Windows installer fixture checks.

To install a local build, generate its digest and pass it with the artifact:

```sh
sha256sum bin/llmgate-linux-amd64
sudo sh ../install.sh --artifact ./bin/llmgate-linux-amd64 --sha256 <printed-sha256> --no-report
```

## Troubleshooting

- **No systemd / unsupported architecture:** use 64-bit Linux with systemd running as the service manager; a plain container or 32-bit OS does not meet the requirements.
- **Port 80 is occupied:** choose a free port in `/etc/llmgate/gatewayd.yaml`, restart the services, and include that port in console and API URLs.
- **Configuration permission denied:** check the configuration file's owner and mode, plus execute permission for the `llmgate` group on `/etc/llmgate`.
- **GitHub download fails:** use the offline installation steps. Keep TLS certificate verification enabled.
- **Website or domain service unavailable:** use the device's local IP; model calls depend on the model provider's connectivity, not website availability.
- **Network settings unavailable:** host network changes require NetworkManager and the correct polkit support. Other gateway features remain usable.

Inspect local service status and metadata logs:

```sh
systemctl status llmgate-gatewayd llmgate-updated --no-pager
journalctl -u llmgate-gatewayd -u llmgate-updated -n 100 --no-pager
```

Before sharing diagnostics, remove credentials and personal data. For private security reports, see [SECURITY.md](../SECURITY.md).
