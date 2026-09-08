// 第三方组件卡（「第三方组件」页，每个组件一张）。
//
// 组件不随固件发布：签名清单（官网在线检查或离线粘贴导入）→ 从上游官方 release 拉取精确制品
// （或把同一文件上传）→ 升级引擎核对摘要、架构与自述版本后装进 A/B 槽位 → 可回退到上一槽位。
// 两个组件（cloudflared、Mihomo 内核）走的是同一条链，差异全部收在 `ComponentSpec` 里：
// 名字、上游与许可证、清单路径、各自的 API 调用、被哪项功能使用。
//
// 卸载只管组件本身，**从不替管理员停用功能**：使用它的功能启用中时按钮置灰并指回功能页，
// 服务端同样以 409 component_in_use 拒绝。
//
// 零轮询：读数只在进页/刷新/动作后重取一次。

import { useRef, useState } from "react";
import { toast } from "sonner";

import { HelpTip } from "@/components/help-tip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import * as api from "@/lib/api";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { Link } from "@/lib/router";
import type { BusinessRoute } from "@/lib/routes";

export type Run = (name: string, fn: () => Promise<unknown>, after?: () => void) => void;

type ComponentReply = { component: api.ComponentStatus };

/** 一个第三方组件的全部差异点；页面与先决条件卡都只认它。 */
export interface ComponentSpec {
  id: "cloudflared" | "mihomo";
  /** 组件名（界面标题）。 */
  name: string;
  /** 上游名字：清单、拉取提示与许可证说明都用它。 */
  upstream: string;
  /** 这个组件在设备上做什么（一句话）。 */
  purpose: string;
  /** 组件来源说明（问号提示）。 */
  sourceNote: string;
  /** 安装确认框里的许可证与边界说明。 */
  licenseNote: string;
  /** 官网签名清单路径（离线导入对话框展示）。 */
  manifestPath: string;
  /** 版本号展示：Mihomo 清单里是裸三段号，界面加 v。 */
  versionLabel: (version: string) => string;
  /** 安装 / 回退时受影响的进程名（「connector 运行中会切到新版本」那句）。 */
  process: string;
  /** 使用该组件的功能：名字与页面。 */
  usedBy: { label: string; route: BusinessRoute };
  /** 卸载时保留的东西（确认框一句话）。 */
  keepsOnRemove: string;
  api: {
    check: () => Promise<ComponentReply>;
    importManifest: (index: string, signature: string) => Promise<ComponentReply>;
    download: () => Promise<ComponentReply>;
    upload: (file: File) => Promise<ComponentReply>;
    install: () => Promise<ComponentReply>;
    rollback: () => Promise<ComponentReply>;
    discard: () => Promise<ComponentReply>;
    remove: () => Promise<ComponentReply>;
  };
}

export function bytesLabel(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  return `${Math.round(n / 1024)} KiB`;
}

export type Tone = "ok" | "alert" | "off";

/** 组件状态徽标：state 的七个取值各一条。 */
export function componentBadge(c: api.ComponentStatus): { label: string; tone: Tone } {
  switch (c.state) {
    case "engine_unavailable":
      return { label: t("引擎不可达"), tone: "alert" };
    case "downloading":
      return { label: t("拉取中"), tone: "off" };
    case "blocked":
      return { label: t("已阻断"), tone: "alert" };
    case "staged":
      return { label: t("制品就绪"), tone: "off" };
    case "not_installed":
      return { label: t("未安装"), tone: "off" };
    case "update_available":
      return { label: t("可更新"), tone: "ok" };
    default:
      return { label: t("已安装"), tone: "ok" };
  }
}

export function toneClass(tone: Tone): string {
  return tone === "ok" ? "text-signal-ok" : tone === "alert" ? "text-signal-alert" : "";
}

/** useRun 把「置忙 → 调用 → 提示错误 → 重取读数」收成一处；页面与面板各自持有一份。 */
export function useRun(reload: () => void): { busy: string | null; run: Run } {
  const [busy, setBusy] = useState<string | null>(null);
  const run: Run = (name, fn, after) => {
    setBusy(name);
    fn().then(
      () => {
        setBusy(null);
        after?.();
        reload();
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(api.errorMessage(err));
        reload();
      },
    );
  };
  return { busy, run };
}

export function ComponentCard({
  spec,
  comp,
  inUse,
  busy,
  run,
}: {
  spec: ComponentSpec;
  comp: api.ComponentStatus;
  /** 使用它的功能是否启用中（启用中不能卸载）。 */
  inUse: boolean;
  busy: string | null;
  run: Run;
}): React.ReactElement {
  const fileRef = useRef<HTMLInputElement>(null);
  const confirm = useConfirm();
  const [importOpen, setImportOpen] = useState(false);
  const [indexText, setIndexText] = useState("");
  const [sigText, setSigText] = useState("");
  const adv = comp.advisory ?? null;
  const staged = comp.staged ?? null;
  const badge = componentBadge(comp);
  const v = spec.versionLabel;

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{spec.name}</span>
        <Badge variant={badge.tone === "off" ? "secondary" : "outline"} className={toneClass(badge.tone)}>
          {badge.label}
        </Badge>
        {inUse ? (
          <Badge variant="outline" className="text-signal-ok">
            {t("使用中")}
          </Badge>
        ) : null}
        <HelpTip label={t("组件来源说明")}>{spec.sourceNote}</HelpTip>
        <span className="text-muted-foreground ml-auto text-xs">
          {t("用于：")}
          <Link to={spec.usedBy.route} className="underline">
            {spec.usedBy.label}
          </Link>
        </span>
      </div>
      <p className="text-muted-foreground text-xs">{spec.purpose}</p>
      <dl className="grid grid-cols-[6rem_1fr] gap-y-1 text-xs">
        <dt className="text-muted-foreground">{t("已安装")}</dt>
        <dd>
          {comp.installed ? (
            <>
              <code className="font-mono">{v(comp.version ?? "")}</code>
              <span className="text-muted-foreground ml-2">slot {comp.slot}</span>
            </>
          ) : (
            "—"
          )}
        </dd>
        {comp.previous_version === undefined || comp.previous_version === "" ? null : (
          <>
            <dt className="text-muted-foreground">{t("上一版本")}</dt>
            <dd>
              <code className="font-mono">{v(comp.previous_version)}</code>
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">{t("签名清单")}</dt>
        <dd>
          {adv === null
            ? t("尚未取得")
            : t("revision {revision}（{source}{checked}）", {
                revision: adv.revision,
                source: adv.source === "website" ? t("官网") : adv.source === "upload" ? t("离线导入") : t("已存"),
                checked: adv.checked_at === undefined ? "" : ` · ${fmtTime(adv.checked_at)}`,
              })}
          {adv?.latest !== undefined && adv?.latest !== null ? (
            <Badge variant="outline" className="text-signal-alert ml-2">
              {comp.installed
                ? t("可更新到 {version}", { version: v(adv.latest.version) })
                : t("可安装 {version}", { version: v(adv.latest.version) })}
            </Badge>
          ) : null}
          {adv?.needs_firmware === true ? <span className="text-signal-alert ml-2">{t("更新版本要求先升级固件")}</span> : null}
        </dd>
        {adv?.newest !== undefined && adv?.newest !== null ? (
          <>
            <dt className="text-muted-foreground">{t("官方制品")}</dt>
            <dd className="min-w-0">
              <span className="break-all">
                <code className="font-mono">{v(adv.newest.version)}</code> · {bytesLabel(adv.newest.sizeBytes)}
                {adv.newest.unpackedSizeBytes !== undefined ? t("（解压后 {size}）", { size: bytesLabel(adv.newest.unpackedSizeBytes) }) : ""} ·{" "}
                {adv.newest.license} ·{" "}
                <a className="underline" href={adv.newest.artifactUrl} target="_blank" rel="noreferrer">
                  {t("官方 release")}
                </a>
                {adv.newest.sourceUrl === undefined ? null : (
                  <>
                    {" · "}
                    <a className="underline" href={adv.newest.sourceUrl} target="_blank" rel="noreferrer">
                      {t("源码")}
                    </a>
                  </>
                )}
                {adv.newest.licenseUrl === undefined ? null : (
                  <>
                    {" · "}
                    <a className="underline" href={adv.newest.licenseUrl} target="_blank" rel="noreferrer">
                      {t("许可证")}
                    </a>
                  </>
                )}
              </span>
            </dd>
          </>
        ) : null}
        {staged === null ? null : (
          <>
            <dt className="text-muted-foreground">{t("已就绪制品")}</dt>
            <dd>
              <code className="font-mono">{v(staged.version)}</code>
              <span className="text-muted-foreground ml-2">
                {bytesLabel(staged.size_bytes)} · {staged.source === "upload" ? t("手动上传") : t("设备拉取")}
              </span>
            </dd>
          </>
        )}
      </dl>
      {comp.download_error === undefined || comp.download_error === "" ? null : (
        <p className="text-signal-alert text-xs">{t("上次拉取失败：{err}", { err: comp.download_error })}</p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" disabled={busy !== null || !comp.website_enabled} onClick={() => run("check", () => spec.api.check())}>
          {busy === "check" ? t("检查中…") : t("检查签名清单")}
        </Button>
        {adv?.latest !== undefined && adv?.latest !== null && staged === null && !comp.downloading ? (
          <Button
            size="sm"
            disabled={busy !== null || !comp.website_enabled}
            onClick={() => {
              toast(t("开始从 {upstream} 官方 release 拉取…", { upstream: spec.upstream }));
              run("download", () => spec.api.download());
            }}
          >
            {busy === "download" ? t("拉取中…") : t("拉取 {version}", { version: v(adv.latest.version) })}
          </Button>
        ) : null}
        <Button size="sm" variant="outline" disabled={busy !== null || adv === null} title={adv === null ? t("先取得签名清单") : undefined} onClick={() => fileRef.current?.click()}>
          {busy === "upload" ? t("正在上传…") : t("上传官方制品")}
        </Button>
        <input
          ref={fileRef}
          type="file"
          className="hidden"
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file === undefined) return;
            run("upload", () =>
              spec.api
                .upload(file)
                .then((r) => {
                  if (fileRef.current !== null) fileRef.current.value = "";
                  toast(t("制品已就绪"));
                  return r;
                })
                .catch((err: unknown) => {
                  if (fileRef.current !== null) fileRef.current.value = "";
                  throw err;
                }),
            );
          }}
        />
        <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => setImportOpen(true)}>
          {t("导入签名清单")}
        </Button>
        {staged === null ? null : (
          <Button
            size="sm"
            disabled={busy !== null || !comp.engine_available}
            onClick={() => {
              void confirm({
                title: t("安装 {name} {version}", { name: spec.name, version: v(staged.version) }),
                body: (
                  <>
                    <p>
                      {t("升级引擎核对摘要、架构与自述版本后装进非活动 slot；{process}运行中会切到新版本，未就绪自动切回旧版本。", {
                        process: spec.process,
                      })}
                    </p>
                    <p className="text-muted-foreground text-xs">{spec.licenseNote}</p>
                  </>
                ),
                confirmText: t("安装"),
              }).then((ok) => {
                if (!ok) return;
                run("install", () =>
                  spec.api.install().then((r) => {
                    toast(t("{name} {version} 已安装", { name: spec.name, version: v(staged.version) }));
                    return r;
                  }),
                );
              });
            }}
          >
            {busy === "install" ? t("安装中…") : t("安装")}
          </Button>
        )}
        {comp.previous_version !== undefined && comp.previous_version !== "" ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => {
              const prev = comp.previous_version ?? "";
              void confirm({
                title: t("回退到 {version}", { version: v(prev) }),
                body: t("把 current 切回上一个 slot；{process}运行中会重启。", { process: spec.process }),
                confirmText: t("回退"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("rollback", () => spec.api.rollback());
              });
            }}
          >
            {t("回退到上一版本")}
          </Button>
        ) : null}
        {staged === null ? null : (
          <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => run("discard", () => spec.api.discard())}>
            {t("丢弃已就绪制品")}
          </Button>
        )}
        {comp.installed ? (
          <Button
            size="sm"
            variant="outline"
            className="text-destructive ml-auto"
            disabled={busy !== null || inUse || !comp.engine_available}
            title={inUse ? t("「{feature}」启用中，先到该页停用再卸载", { feature: spec.usedBy.label }) : undefined}
            onClick={() => {
              void confirm({
                title: t("卸载 {name}", { name: spec.name }),
                body: (
                  <>
                    <p>{t("删除设备上该组件的全部 slot（含上一版本）与已就绪制品；{keeps}保留，需要时可重新安装。", { keeps: spec.keepsOnRemove })}</p>
                    <p className="text-muted-foreground text-xs">{t("使用它的功能须已停用；本页不会替你停用功能。")}</p>
                  </>
                ),
                confirmText: t("卸载"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("remove", () =>
                  spec.api.remove().then((r) => {
                    toast(t("{name} 已卸载", { name: spec.name }));
                    return r;
                  }),
                );
              });
            }}
          >
            {busy === "remove" ? t("卸载中…") : t("卸载")}
          </Button>
        ) : null}
      </div>
      {inUse ? (
        <p className="text-muted-foreground text-xs">
          {t("「{feature}」启用中：可以升级或回退（进程会随之重启），卸载前先到该页停用。", { feature: spec.usedBy.label })}
        </p>
      ) : null}

      <Dialog
        open={importOpen}
        onOpenChange={(o) => {
          if (!o) setImportOpen(false);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("导入签名清单")}</DialogTitle>
            <DialogDescription>
              {t("粘贴官网 {path} 与 stable.json.sig 的原文；验签通过且 revision 不回退才接受。", { path: spec.manifestPath })}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor={`${spec.id}-index`}>stable.json</Label>
            <Textarea id={`${spec.id}-index`} rows={8} className="font-mono text-xs" value={indexText} spellCheck={false} onChange={(e) => setIndexText(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor={`${spec.id}-sig`}>stable.json.sig</Label>
            <Textarea id={`${spec.id}-sig`} rows={5} className="font-mono text-xs" value={sigText} spellCheck={false} onChange={(e) => setSigText(e.target.value)} />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setImportOpen(false)}>
              {t("取消")}
            </Button>
            <Button
              type="button"
              disabled={busy !== null || indexText.trim() === "" || sigText.trim() === ""}
              onClick={() =>
                run("manifest", () =>
                  spec.api.importManifest(indexText, sigText).then((r) => {
                    setImportOpen(false);
                    setIndexText("");
                    setSigText("");
                    toast(t("签名清单已接受"));
                    return r;
                  }),
                )
              }
            >
              {busy === "manifest" ? t("导入中…") : t("导入")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
