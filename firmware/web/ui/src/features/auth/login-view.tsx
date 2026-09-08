// 登录页。/ui/ 里两张无会话页面之一（另一张是「接入方法」/connect）；新设备出厂
// 即带默认登录口令（固件侧 auth.Service.EnsureDefaultPassword），开机第一屏就是这里。
//
// **只有一个密码框**：设备只有一个管理员，没有用户名可填。服务端一律按「密码
// 错误」拒绝、不区分原因——界面照抄服务端文案即可，别自己编更具体的原因。
//
// 「管理员密码」与「API密钥登录」通过 TAB 切换；API 密钥入口在 /connect，
// 只凭 Key 查看接入信息，不创建管理员会话。

import { ArrowRight, LoaderCircle, LockKeyhole } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ConnectView, KeyEntry, type KeyHolder } from "@/features/access/connect-view";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { useSession } from "@/lib/session";

import { LoginShell, type LoginMethod } from "./login-shell";

// API 接入结果只存在当前入口的组件内存里；离开入口同步清空，不等 effect 下一帧。
export function LoginView({ method }: { method: LoginMethod }): React.ReactElement {
  const [access, setAccess] = useState<{ holder: KeyHolder | null; error: string | null } | null>(null);
  if (method === "password" && access !== null) setAccess(null);

  if (method === "api-key" && access?.holder) {
    return <ConnectView initialHolder={access.holder} onReset={(error) => setAccess({ holder: null, error })} />;
  }
  return (
    <LoginShell
      method={method}
      passwordEntry={<PasswordEntry />}
      keyEntry={
        <KeyEntry
          initialError={method === "api-key" ? access?.error ?? null : null}
          onAccepted={(key, data) => setAccess({ holder: { key, data }, error: null })}
        />
      }
    />
  );
}

function PasswordEntry(): React.ReactElement {
  const { onAuthed } = useSession();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const alive = useRef(false);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (busy) return;
    setError(null);
    setBusy(true);
    api.login(password).then(
      () => { if (alive.current) onAuthed(); },
      (err: unknown) => {
        if (!alive.current) return;
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <form className="auth-form" onSubmit={submit} aria-busy={busy}>
      <p className="auth-form-intro">{t("管理这台设备的模型接入、API 密钥与运行设置。")}</p>
      <label className="auth-field">
        {t("管理员密码")}
        <span className="auth-input-wrap">
          <LockKeyhole aria-hidden />
          <Input
            required
            type="password"
            value={password}
            maxLength={api.PasswordMaxLen}
            autoComplete="current-password"
            placeholder={t("输入管理员密码")}
            aria-invalid={error !== null}
            aria-describedby={error === null ? undefined : "password-error"}
            onChange={(e) => setPassword(e.target.value)}
            className="auth-input"
          />
        </span>
      </label>
      <p className="auth-form-hint">{t("使用设备管理员设置的密码登录。")}</p>
      <div className="auth-form-message">
        {error === null ? null : <p id="password-error" role="alert" className="text-destructive text-sm">{error}</p>}
      </div>
      <Button type="submit" disabled={busy} className="auth-submit">
        {busy ? <LoaderCircle aria-hidden className="animate-spin motion-reduce:animate-none" /> : null}
        {busy ? t("正在登录…") : t("登录")}
        {busy ? null : <ArrowRight aria-hidden className="auth-submit-arrow" />}
      </Button>
    </form>
  );
}
