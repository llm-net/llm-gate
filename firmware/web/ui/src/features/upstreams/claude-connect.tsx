import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

export function ClaudeConnect({ label, model, secure, account, onDone }: { account: api.AgentAccount | undefined; label: string; model: string; secure: boolean; onDone: () => void }): React.ReactElement {
  const [mode, setMode] = useState<"browser" | "setup">(account?.setup_token_configured && account.status !== "auth_expired" ? "browser" : "setup");
  const [login, setLogin] = useState<api.ClaudeLoginStart | null>(null);
  const [credential, setCredential] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const operation = useRef(0);
  useEffect(() => () => { operation.current++; }, []);

  function choose(next: "browser" | "setup"): void {
    operation.current++;
    setMode(next);
    setCredential("");
    setLogin(null);
    setError(null);
  }
  async function start(): Promise<void> {
    const sequence = ++operation.current;
    setBusy(true);
    setError(null);
    setCredential("");
    setLogin(null);
    try { const result = await api.startClaudeLogin(); if (sequence === operation.current) setLogin(result); }
    catch (err) { if (sequence === operation.current) setError(api.errorMessage(err)); }
    finally { if (sequence === operation.current) setBusy(false); }
  }
  async function submit(): Promise<void> {
    if (!credential.trim()) { setError(t("请先提供登录凭据。")); return; }
    const sequence = ++operation.current;
    setBusy(true);
    setError(null);
    try {
      if (mode === "browser") await api.completeClaudeLogin(credential.trim(), label.trim(), model.trim());
      else await api.connectClaudeSetupToken(credential.trim(), label.trim(), model.trim());
      if (sequence !== operation.current) return;
      setCredential("");
      setLogin(null);
      onDone();
    } catch (err) {
      if (sequence !== operation.current) return;
      setError(api.errorMessage(err));
      if (mode === "browser" && err instanceof api.ApiError && err.code !== "invalid_claude_code") {
        setLogin(null);
        setCredential("");
      }
    }
    finally { if (sequence === operation.current) setBusy(false); }
  }

  return <div className="flex flex-col gap-3">
    <p className="text-muted-foreground text-xs">{t("setup-token 用于 gate 模型调用，OAuth 授权用于查询额度。请使用同一个 Claude 账号，分别保存；更新其中一项会保留另一项。")}</p>
    <p className="text-xs">{t("模型调用凭据：{status}", { status: account?.setup_token_configured ? account.status === "auth_expired" ? t("已失效") : t("已配置") : t("未配置") })} · {t("额度授权：{status}", { status: account?.quota_oauth_configured ? account.quota_oauth_expired ? t("已失效") : t("已授权") : t("未授权") })}</p>
    <div className="flex flex-wrap gap-2">
      <Button variant={mode === "browser" ? "default" : "outline"} aria-pressed={mode === "browser"} disabled={busy} onClick={() => choose("browser")}>{t("额度查询（OAuth）")}</Button>
      <Button variant={mode === "setup" ? "default" : "outline"} aria-pressed={mode === "setup"} disabled={busy} onClick={() => choose("setup")}>{t("模型调用（setup-token）")}</Button>
    </div>
    {mode === "browser" ? <>
      <p className="text-muted-foreground text-xs">{t("OAuth 仅用于额度查询。设备为额度查询自动续期；授权过期或续期失败不会影响 setup-token 模型调用。")}</p>
      <p className="text-muted-foreground text-xs">{t("打开 Claude 授权页面，登录你的订阅账号并允许授权，再把页面显示的完整授权码粘贴回来。")}</p>
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" disabled={busy} onClick={() => void start()}>{busy ? t("处理中…") : login ? t("重新生成授权链接") : t("生成 Claude 授权链接")}</Button>
        {login && <a href={login.authorize_url} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer" className="text-primary text-sm underline underline-offset-4">{t("打开 Claude 授权页面 ↗")}</a>}
      </div>
      {login && <>
        <p className="text-muted-foreground text-xs">{t("链接有效期为 {minutes} 分钟。请保留本页；重新生成后，旧链接和旧授权码将作废。", { minutes: Math.floor(login.expires_in / 60) })}</p>
        <div className="space-y-1.5">
          <Label htmlFor="claude-authorization-code">{t("Claude 授权码")}</Label>
          <Input id="claude-authorization-code" type="password" value={credential} autoComplete="off" spellCheck={false} disabled={busy} onChange={(event) => setCredential(event.target.value)} />
          <p className="text-muted-foreground text-xs">{t("请完整复制授权码，包括 # 后的内容。")}</p>
        </div>
      </>}
    </> : <>
      <p className="text-muted-foreground text-xs">{t("在可信电脑上运行官方 claude setup-token，完成授权后粘贴输出。gate 模型调用只使用这份令牌；到期后在这里更新，不会自动刷新。")}</p>
      <div className="space-y-1.5">
        <Label htmlFor="claude-token">setup-token</Label>
        <Input id="claude-token" type="password" value={credential} autoComplete="off" spellCheck={false} disabled={busy} onChange={(event) => setCredential(event.target.value)} />
      </div>
    </>}
    {!secure && <p className="text-signal-alert text-xs">{t("当前管理页使用明文 HTTP，登录凭据没有 TLS 保护。请通过可信内网或系统信任的 HTTPS 地址连接设备。")}</p>}
    <p className="text-muted-foreground text-xs">{t("凭据仅在本页内存和设备封存区中处理，不在响应、日志或成员配置中回显。")}</p>
    {error && <p role="alert" className="text-destructive text-sm">{error}</p>}
    {(mode !== "browser" || login) && <div><Button disabled={busy || !credential} onClick={() => void submit()}>{busy ? t("处理中…") : mode === "browser" ? t("保存额度授权") : t("保存 setup-token")}</Button></div>}
  </div>;
}
