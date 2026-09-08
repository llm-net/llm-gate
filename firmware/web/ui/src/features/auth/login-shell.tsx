import { KeyRound, ShieldCheck } from "lucide-react";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { t } from "@/lib/i18n";
import { navigate } from "@/lib/router";

import { AuthShell } from "./auth-shell";

export type LoginMethod = "password" | "api-key";

// 外壳与标签保持挂载；两份表单叠在同一网格里占位，卡片按较高者自然撑开。
// 非活动面板不可见且 inert，不能聚焦或提交。每次切换重建表单以清空凭据。
export function LoginShell({
  method,
  passwordEntry,
  keyEntry,
}: {
  method: LoginMethod;
  passwordEntry: React.ReactNode;
  keyEntry: React.ReactNode;
}): React.ReactElement {
  return (
    <AuthShell heading={t("登录")}>
      <Tabs
        value={method}
        activationMode="manual"
        onValueChange={(value) => navigate(value === "password" ? "/login" : "/connect")}
        className="auth-tabs gap-6"
      >
        <TabsList aria-label={t("登录方式")} className="auth-methods w-full" data-method={method}>
          <span aria-hidden className="auth-method-indicator" />
          <TabsTrigger value="password"><ShieldCheck aria-hidden />{t("管理员密码")}</TabsTrigger>
          <TabsTrigger value="api-key"><KeyRound aria-hidden />{t("API密钥登录")}</TabsTrigger>
        </TabsList>
        <div className="auth-form-stage">
          <TabsContent value="password" forceMount inert={method !== "password"} className="auth-form-panel">
            <div key={method} className="auth-form-body">{passwordEntry}</div>
          </TabsContent>
          <TabsContent value="api-key" forceMount inert={method !== "api-key"} className="auth-form-panel">
            <div key={method} className="auth-form-body">{keyEntry}</div>
          </TabsContent>
        </div>
      </Tabs>
    </AuthShell>
  );
}
