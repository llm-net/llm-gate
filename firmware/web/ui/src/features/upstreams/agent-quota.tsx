import { useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import * as api from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";

function periodLabel(period: string): string {
  switch (period) {
    case "session": return t("Session 额度");
    case "week": return t("周额度");
    case "month": return t("月额度");
    default: return t("其他周期额度");
  }
}

function issue(code?: string): string {
  switch (code) {
    case "authorization_expired": return t("额度授权已失效，请重新进行浏览器授权。setup-token 模型调用不受影响。");
    case "permission_required": return t("当前凭据无权查询完整额度。");
    case "unauthorized": return t("额度接口未接受当前凭据，请检查订阅登录状态。");
    case "rate_limited": return t("额度查询被限流，设备将延后重试。");
    case "unsupported": return t("上游未开放此额度查询接口。");
    case "invalid_response": return t("上游未返回可识别的额度，设备将稍后重试。");
    default: return t("额度同步暂时失败，设备将稍后重试。");
  }
}

export function AgentQuotaPanel({ account, reload }: { account: api.AgentAccount; reload: () => void }): React.ReactElement {
  const [busy, setBusy] = useState(false);
  const q = account.quota;
  const syncedAt = q?.last_success_at;
  const syncTime = syncedAt ? fmtTime(syncedAt) : "";
  const today = fmtTime(new Date().toISOString()).slice(0, 10);
  const compactSyncTime = syncTime.startsWith(today) ? syncTime.slice(11) : syncTime;
  const windows = q?.windows ?? [];
  const additional = account.provider === "codex" ? windows.filter((w) => w.id.startsWith("additional_")) : [];
  const main = account.provider === "codex" ? windows.filter((w) => !w.id.startsWith("additional_")) : windows;
  const enabled = account.status === "active" || account.provider === "claude" && account.status === "auth_expired";
  async function sync(): Promise<void> {
    setBusy(true);
    try { await api.syncAgentQuota(account.id); reload(); }
    catch (err) { toast.error(api.errorMessage(err)); }
    finally { setBusy(false); }
  }
  return (
    <div className="space-y-2 border-t px-4 py-3 text-xs">
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="shrink-0 font-medium">{t("上游订阅额度")}</span>
          {syncedAt && <time dateTime={syncedAt} title={t("同步于 {time}", { time: syncTime })} className="text-muted-foreground truncate whitespace-nowrap tabular-nums">
            {t("同步于 {time}", { time: compactSyncTime })}
          </time>}
        </div>
        <Button className="shrink-0" size="xs" variant="outline" disabled={busy || !enabled} onClick={() => void sync()}>
          {busy ? t("同步中…") : t("同步额度")}
        </Button>
      </div>
      {!enabled ? <p className="text-muted-foreground">{t("订阅不可用，额度同步已暂停。")}</p> : (
        <>
          {(!q || q.status === "pending") && <p className="text-muted-foreground">{t("等待设备同步额度")}</p>}
          {q?.error_code && <p className="text-signal-alert" role="status">{issue(q.error_code)}</p>}
          {q?.stale && q.last_success_at && <p className="text-signal-alert">{t("以下为旧读数，不能据此判断当前剩余额度。")}</p>}
          {main.map((w) => <QuotaWindow key={w.id} window={w} />)}
          {additional.length > 0 && <details className="space-y-2">
            <summary className="text-muted-foreground cursor-pointer py-1">{t("其他模型额度（{count} 项）", { count: additional.length })}</summary>
            <p className="text-muted-foreground">{t("以下为其他模型的独立额度，按各自窗口统计。")}</p>
            {additional.map((w) => <QuotaWindow key={w.id} window={w} />)}
          </details>}
        </>
      )}
    </div>
  );
}

function QuotaWindow({ window: w }: { window: api.AgentQuota["windows"][number] }): React.ReactElement {
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap justify-between gap-x-3 gap-y-1">
        <span>{periodLabel(w.period)} · {w.label}</span>
        <span className="tabular-nums">{w.used_bps === null ? t("未知") : t("已用 {percent}%", { percent: (w.used_bps / 100).toFixed(2) })}</span>
      </div>
      {w.used_bps !== null && <div role="progressbar" aria-label={`${periodLabel(w.period)} ${w.label}`} aria-valuenow={Math.min(100, w.used_bps / 100)} aria-valuemin={0} aria-valuemax={100} className="bg-muted h-1.5 overflow-hidden rounded-full"><div className="bg-primary h-full" style={{ width: `${Math.min(100, w.used_bps / 100)}%` }} /></div>}
      <p className="text-muted-foreground">{w.resets_at ? t("重置时间：{time}", { time: fmtTime(w.resets_at) }) : t("重置时间未知")}</p>
    </div>
  );
}
