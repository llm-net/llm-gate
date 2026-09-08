// 设备更新页：把设备上的东西**换新**——官网数据升级（官方目录价、平台模型信息）
// 与固件升级/回退。设备怎么被连上（网卡 IPv4、内网域名）在「网络/域名/代理」页。
//
// 两件事都以官网为可选只读来源：官网不可达时，数据沿用固件内嵌基线、固件仍可手动
// 上传包升级，网关照常工作。
//
// 安装/回退后的重启提示是**纯客户端计时器**，在管理台原实现里靠 `node.isConnected`
// 自检和整页 reload 清理。**React 里必须改成 effect cleanup**：StrictMode 双挂载会跑
// 两遍 effect，不清理就是双倒计时 + 内存泄漏。
//
// firmwareDownload 服务端同步陪等最多 60 秒再返回快照，**不能给它加请求超时**，否则
// 正常下载会被误报成失败。

import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { PricingItems } from "@/features/upstreams/models-column";
import * as api from "@/lib/api";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate, SectionHead } from "./page-shell";

// ---- 数据升级 ----

function CatalogPanel({ s, reload }: { s: api.DataStatus; reload: () => void }) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<api.CatalogSyncResult | null>(null);

  const official = s.official_pricing;
  const platform = s.platform_models;

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={s.automatic.enabled ? "outline" : "secondary"} className={s.automatic.enabled ? "text-signal-ok" : ""}>
          {s.automatic.enabled ? t("自动检查已启用") : t("未接入官网客户端")}
        </Badge>
        <Button
          className="ml-auto"
          size="sm"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            api.syncCatalog().then(
              (r) => {
                setBusy(false);
                setResult(r);
                reload();
              },
              (err: unknown) => {
                setBusy(false);
                toast.error(api.errorMessage(err));
              },
            );
          }}
        >
          {busy ? t("更新中…") : t("立即更新")}
        </Button>
      </div>
      <dl className="grid grid-cols-[7rem_1fr] gap-y-1 text-xs">
        <dt className="text-muted-foreground">{t("官方目录价")}</dt>
        <dd>
          {official.supported
            ? `${official.synced_at === undefined ? t("固件内嵌") : t("官网")} v${official.version ?? 0}` +
              (official.entries === undefined ? "" : ` · ${t("{n} 条", { n: official.entries })}`) +
              (official.synced_at === undefined
                ? ""
                : ` · ${t("同步于 {time}", { time: fmtTime(official.synced_at) })}`)
            : t("不可用")}
        </dd>
        <dt className="text-muted-foreground">{t("平台模型信息")}</dt>
        <dd>
          {platform.supported
            ? `${platform.origin === "synced" ? t("官网") : t("固件内嵌")} v${platform.version} · ` +
              t("{p} 个平台 / {m} 个型号", { p: platform.platforms, m: platform.models })
            : t("不可用")}
        </dd>
      </dl>
      <p className="text-muted-foreground text-xs">
        {t("设备启动后与此后每小时匿名检查一次；请求不携带设备身份、序列号或任何凭据。官网不可达时沿用固件内嵌基线，网关照常工作。")}
      </p>

      <Dialog
        open={result !== null}
        onOpenChange={(o) => {
          if (!o) setResult(null);
        }}
      >
        {result === null ? null : (
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t("数据升级结果")}</DialogTitle>
            </DialogHeader>
            <p className="text-sm">
              {result.skipped.length === 0
                ? t("目录价写入 {applied} 条；平台模型信息 v{version}（{p} 个平台 / {m} 个型号）。", {
                    applied: result.applied.length,
                    version: result.platform_models.version,
                    p: result.platform_models.platforms,
                    m: result.platform_models.models,
                  })
                : t("目录价写入 {applied} 条，跳过 {skipped} 条；平台模型信息 v{version}（{p} 个平台 / {m} 个型号）。", {
                    applied: result.applied.length,
                    skipped: result.skipped.length,
                    version: result.platform_models.version,
                    p: result.platform_models.platforms,
                    m: result.platform_models.models,
                  })}
            </p>
            {result.platform_error === undefined || result.platform_error === "" ? null : (
              <p className="text-signal-alert text-xs">
                {t("平台模型信息未更新：{error}", { error: result.platform_error })}
              </p>
            )}
            {result.applied.length === 0 && result.skipped.length === 0 ? null : (
              <div className="flex flex-col gap-2">
                {result.applied.map((m) => (
                  <div key={`a-${m.model}`} className="flex flex-wrap items-baseline gap-2 text-xs">
                    <code className="font-mono">{m.model}</code>
                    <PricingItems kind={m.kind} pricing={m.pricing} />
                  </div>
                ))}
                {result.skipped.map((m) => (
                  <div key={`s-${m.model}`} className="flex flex-wrap items-baseline gap-2 text-xs">
                    <code className="font-mono">{m.model}</code>
                    <span className="text-muted-foreground">{api.pricingSkipLabel(m.reason)}</span>
                  </div>
                ))}
              </div>
            )}
            <DialogFooter>
              <Button variant="outline" onClick={() => setResult(null)}>
                {t("完成")}
              </Button>
            </DialogFooter>
          </DialogContent>
        )}
      </Dialog>
    </Card>
  );
}

// ---- 固件升级 ----

function fwBytes(n: number): string {
  return n <= 0 ? "—" : `${(n / 1048576).toFixed(1)} MiB`;
}

// 安装/回退受理后的整页遮罩倒计时：纯客户端计时器，到点整页刷新——设备网关此刻已被
// 升级引擎停下重启，刷新回来就是新版本（或回退后的旧版本）。计时器随组件卸载清理。
function RestartNotice({ title }: { title: string | null }) {
  const [left, setLeft] = useState(30);
  useEffect(() => {
    if (title === null) return;
    setLeft(30);
    const timer = window.setInterval(() => {
      setLeft((n) => {
        if (n <= 1) {
          window.clearInterval(timer);
          window.location.reload();
          return 0;
        }
        return n - 1;
      });
    }, 1000);
    return () => window.clearInterval(timer);
  }, [title]);

  return (
    <Dialog open={title !== null} onOpenChange={() => undefined}>
      {title === null ? null : (
        <DialogContent showCloseButton={false}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
          </DialogHeader>
          <p className="text-sm">{t("设备网关正在重启，页面将在 {left} 秒后自动刷新。", { left })}</p>
          <p className="text-muted-foreground text-xs">
            {t("升级引擎会在健康窗口内看护新版本：启动失败将自动回退到升级前的版本，结果在本分区可见。")}
          </p>
        </DialogContent>
      )}
    </Dialog>
  );
}

function FirmwarePanel({ fw, reload }: { fw: api.FirmwareStatus | null; reload: () => void }) {
  const confirm = useConfirm();
  const fileRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [restart, setRestart] = useState<string | null>(null);

  if (fw === null) {
    return (
      <Card className="gap-3 p-5">
        <p className="text-muted-foreground text-sm">{t("升级读数暂时不可用，请刷新重试。")}</p>
      </Card>
    );
  }

  const adv = fw.advisory;
  const staged = fw.staged ?? null;

  function run(name: string, fn: () => Promise<unknown>, after: () => void): void {
    setBusy(name);
    fn().then(
      () => {
        setBusy(null);
        after();
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(api.errorMessage(err));
      },
    );
  }

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        {/* LED 语义：安装中琥珀（需留意）、引擎不可达灰（未点亮，不是故障）、其余绿。 */}
        {fw.engine.busy === true ? (
          <Badge variant="outline" className="text-signal-alert">
            {t("正在安装")}
          </Badge>
        ) : !fw.engine.available ? (
          <Badge variant="secondary">{t("引擎不可达")}</Badge>
        ) : (
          <Badge variant="outline" className="text-signal-ok">
            {t("就绪")}
          </Badge>
        )}
      </div>
      <dl className="grid grid-cols-[6rem_1fr] gap-y-1 text-xs">
        <dt className="text-muted-foreground">{t("当前版本")}</dt>
        <dd>
          <code className="font-mono">{fw.current_version}</code>
        </dd>
        {fw.engine.prev_version === undefined || fw.engine.prev_version === "" ? null : (
          <>
            <dt className="text-muted-foreground">{t("上一版本")}</dt>
            <dd>
              <code className="font-mono">{fw.engine.prev_version}</code>
            </dd>
          </>
        )}
        {adv === undefined || adv === null || adv.latest === undefined || adv.latest === null ? null : (
          <>
            <dt className="text-muted-foreground">{t("官网最新")}</dt>
            <dd>
              <code className="font-mono">{adv.latest.version}</code>
              {adv.upgrade_available === true ? (
                <Badge variant="outline" className="text-signal-alert ml-2">
                  {t("可升级")}
                </Badge>
              ) : null}
            </dd>
          </>
        )}
        {staged === null ? null : (
          <>
            <dt className="text-muted-foreground">{t("已就绪包")}</dt>
            <dd>
              <code className="font-mono">{staged.version}</code>
              <span className="text-muted-foreground ml-2">{fwBytes(staged.size_bytes)}</span>
            </dd>
          </>
        )}
      </dl>

      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          variant="outline"
          disabled={busy !== null}
          onClick={() => run("check", () => api.firmwareCheck(), reload)}
        >
          {busy === "check" ? t("检查中…") : t("检查更新")}
        </Button>
        {adv?.upgrade_available === true && staged === null && !fw.downloading ? (
          <Button
            size="sm"
            disabled={busy !== null}
            onClick={() => {
              toast(t("开始下载固件包…"));
              // 服务端同步陪等最多 60 秒，**不设超时**。
              run("download", () => api.firmwareDownload(), reload);
            }}
          >
            {busy === "download" ? t("下载中…") : t("下载新版本")}
          </Button>
        ) : null}
        {/* 手动上传（离线升级路径，恒可用）：隐藏 file input，选中即上传。 */}
        <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => fileRef.current?.click()}>
          {busy === "upload" ? t("正在上传…") : t("上传固件包")}
        </Button>
        <input
          ref={fileRef}
          type="file"
          className="hidden"
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file === undefined) return;
            setBusy("upload");
            api.firmwareUpload(file).then(
              () => {
                setBusy(null);
                toast(t("固件包已就绪"));
                reload();
              },
              (err: unknown) => {
                setBusy(null);
                // **失败要手动复位**，否则同一个文件再选不触发 change。
                if (fileRef.current !== null) fileRef.current.value = "";
                toast.error(api.errorMessage(err));
              },
            );
          }}
        />
        {staged === null ? null : (
          <Button
            size="sm"
            disabled={busy !== null}
            onClick={() => {
              void confirm({
                title: t("安装 {version}", { version: staged.version }),
                body: (
                  <>
                    <p>{t("将把固件升级到 {version} 并重启设备网关。", { version: staged.version })}</p>
                    <p className="text-destructive">
                      {t("升级期间 API 转发与本管理台会短暂中断（约半分钟）。启动失败将自动回退到当前版本。")}
                    </p>
                  </>
                ),
                confirmText: t("安装并重启"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("install", () => api.firmwareInstall(), () => setRestart(t("正在安装固件")));
              });
            }}
          >
            {t("安装")}
          </Button>
        )}
        {fw.engine.available && fw.engine.prev_version !== undefined && fw.engine.prev_version !== "" ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => {
              const prev = fw.engine.prev_version ?? "";
              void confirm({
                title: t("回退到 {version}", { version: prev }),
                body: (
                  <>
                    <p>{t("将把固件回退到上一版本 {version} 并重启设备网关。", { version: prev })}</p>
                    <p className="text-destructive">
                      {t("回退只更换程序，不回滚业务数据；回退期间 API 转发与本管理台会短暂中断（约半分钟）。回退后仍可再升回。")}
                    </p>
                  </>
                ),
                confirmText: t("回退并重启"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("rollback", () => api.firmwareRollback(), () => setRestart(t("正在回退固件")));
              });
            }}
          >
            {t("回退到上一版本")}
          </Button>
        ) : null}
        {staged === null ? null : (
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => run("discard", () => api.firmwareDiscardStaged(), reload)}
          >
            {t("丢弃已就绪包")}
          </Button>
        )}
      </div>

      <p className="text-muted-foreground text-xs">
        {t("在线检查与下载匿名读取 LLM Gate官网；官网不可达时仍可手动上传固件包。")}
      </p>
      {fw.engine.available ? null : (
        <p className="text-signal-alert text-xs">
          {t("升级引擎（llmgate-updated 服务）不可达：无法执行安装与回退。可检查该服务是否运行，或按部署文档手工替换二进制。")}
        </p>
      )}
      {fw.engine.last_result === undefined ||
      fw.engine.last_result === null ||
      fw.engine.last_result.outcome === "success" ? null : (
        <p className="text-signal-alert text-xs">
          {fw.engine.last_result.kind === "rollback"
            ? t("上次回退未成功（{from} → {to}）：{reason}", {
                from: fw.engine.last_result.from_version,
                to: fw.engine.last_result.to_version,
                reason: fw.engine.last_result.reason ?? fw.engine.last_result.outcome,
              })
            : t("上次升级未成功（{from} → {to}）：{reason}", {
                from: fw.engine.last_result.from_version,
                to: fw.engine.last_result.to_version,
                reason: fw.engine.last_result.reason ?? fw.engine.last_result.outcome,
              })}
        </p>
      )}

      <RestartNotice title={restart} />
    </Card>
  );
}

// ---- 页面 ----

export function UpdatesPage(): React.ReactElement {
  const res = useResource(
    () =>
      Promise.all([
        api.getDataStatus(),
        // 升级读数失败不拖垮整页（比如引擎聚合超时）：分区自己说明不可用。
        api.getFirmwareStatus().catch((): api.FirmwareStatus | null => null),
      ]).then(([d, f]) => ({ data: d, fw: f })),
    [],
  );

  return (
    <PageContainer>
      <PageHeader
        title={t("设备更新")}
        note={t("更新官方数据与设备固件，并管理固件回退。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(d) => (
          <>
            <SectionHead
              title={t("数据升级")}
              desc={t("设备每小时匿名检查 LLM Gate官网，更新官方目录价与平台模型信息。请求不携带设备身份或凭据，也可随时手动立即更新。")}
            />
            <CatalogPanel s={d.data} reload={res.reload} />

            <SectionHead
              title={t("固件升级")}
              desc={t("查看并升级这台设备的固件。安装前自动保留上一版本与数据备份，升级失败自动回退；必要时也可手动回退到上一版本。")}
            />
            <FirmwarePanel fw={d.fw} reload={res.reload} />
          </>
        )}
      </ResourceGate>
    </PageContainer>
  );
}
