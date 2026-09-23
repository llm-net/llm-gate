// 主机/SoC 与工具配置两页共用的守护进程部件：徽标判定、sudo 口令判定与安装 / 卸载 /
// 解除纳管对话框。守护进程按主机类型二选一（工作节点 devd、模型服务节点 modeld），读数都是
// 主机行的 AgentHost.devd、端点都是 /devd/*；文案按 api.daemonName(host.kind) 带名字。契约见
// AGENTS.md「守护进程 devd 与透传」。

import { Loader2Icon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

export type Tone = "ok" | "warn" | "alert" | "none";

export function toneClass(tone: Tone): string {
  return tone === "ok" ? "text-signal-ok" : tone === "alert" ? "text-signal-alert" : "";
}

/** 安装 / 卸载守护进程（以及装 git）要 root：root 用户与已免密 sudo 的用户不必再填口令。 */
export function needsSudoPassword(host: api.AgentHost): boolean {
  return host.username !== "root" && !host.sudo_nopasswd;
}

/** 守护进程那一列的徽标：没装 / 已安装 / 版本不一致 / 异常。 */
export function devdBadge(devd: api.AgentHostDevd | undefined, daemonVersion: string | undefined): { label: string; tone: Tone } {
  if (devd === undefined) return { label: t("未安装"), tone: "none" };
  if (devd.status !== "ready") return { label: t("异常"), tone: "alert" };
  if (staleDaemon(devd, daemonVersion)) return { label: t("版本不一致"), tone: "warn" };
  return { label: t("已安装"), tone: "ok" };
}

/**
 * 主机上装的守护进程与本固件内嵌的不是同一版。两边的协议是一起改的（socket 路径、
 * 请求形状都变过），版本对不上时工作空间可能整个连不上，而行里的状态是上一次安装留下的
 * 读数，看不出这回事——所以在这里直接说出来。
 */
export function staleDaemon(devd: api.AgentHostDevd | undefined, daemonVersion: string | undefined): boolean {
  if (devd === undefined || devd.version === undefined || devd.version === "") return false;
  return daemonVersion !== undefined && daemonVersion !== "" && devd.version !== daemonVersion;
}

export type DevdMode = "install" | "uninstall" | "remove";

export function DevdDialog({
  mode,
  host,
  onClose,
  onDone,
}: {
  mode: DevdMode;
  host: api.AgentHost;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const askPassword = needsSudoPassword(host);
  const reinstall = mode === "install" && host.devd !== undefined;
  const name = api.daemonName(host.kind) || "devd";

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    setBusy(true);
    setError(null);
    const call: Promise<unknown> =
      mode === "install"
        ? api.installDevd(host.id, { password }).then(() => toast(reinstall ? t("{name} 已重新安装", { name }) : t("{name} 已安装", { name })))
        : mode === "uninstall"
          ? api.uninstallDevd(host.id, password).then(() => toast(t("{name} 已卸载", { name })))
          : api.removeAgentHost(host.id, true, password).then((r) => {
              toast(
                r.key_removed && r.devd_removed
                  ? t("已解除纳管，公钥与 {name} 已从主机上移除", { name })
                  : r.key_removed
                    ? t("已解除纳管，公钥已移除；{name} 未能卸载，请在主机上执行 llmgate-{name} uninstall", { name })
                    : t("已解除纳管；主机不可达，公钥与 {name} 需在主机上手工清理", { name }),
              );
            });
    call.then(
      () => {
        onDone();
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  const title =
    mode === "install"
      ? reinstall
        ? t("重新安装 {name}", { name })
        : t("安装 {name}", { name })
      : mode === "uninstall"
        ? t("卸载 {name}？", { name })
        : t("解除纳管？");
  const target = `${host.username}@${host.address}:${host.port}`;
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
          </DialogHeader>
          <code className="text-muted-foreground font-mono text-xs">{target}</code>
          {askPassword ? (
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="devd-password">{t("sudo 口令")}</Label>
              <Input id="devd-password" type="password" value={password} required={mode !== "remove"} disabled={busy} autoComplete="new-password" onChange={(e) => setPassword(e.target.value)} />
            </div>
          ) : null}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" variant={mode === "install" ? "default" : "destructive"} disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {busy ? t("连接中…") : mode === "install" ? t("连接并安装") : mode === "uninstall" ? t("卸载 {name}", { name }) : t("解除纳管")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
