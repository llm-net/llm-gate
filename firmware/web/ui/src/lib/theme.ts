// 界面配色偏好。外观共四种，互斥：绿色是缺省方案（globals.css 的 :root token，
// 不挂类）；蓝 / 金是整组换 hue 的浅色覆写（.theme-blue / .theme-gold）；夜间模式
// 挂 .dark 启用预置的深色 token。类挂在 <html> 上，切换入口有两个：登录后在顶栏
// 配色圆点，登录前在登录页面板底部那条铭牌上——那时还没有顶栏，配色却已经在眼前。
//
// 选择只存本浏览器 localStorage：设备只有一个管理员，外观是纯本地显示偏好，
// 不进服务端设置、不发请求，因此无会话的登录页也改得动。index.html 里有一段同
// 键名、同映射的首帧内联脚本（防非绿主题闪一帧绿），改键名或类名时两处同步。

import { useCallback, useState } from "react";

import { t } from "@/lib/i18n";

export type ThemeId = "green" | "blue" | "gold" | "night";

// swatch 是菜单里那粒色点，取各主题的 --primary（夜间取深色 --card）。CSS 变量
// 到不了「展示别的主题长什么样」这件事，只能在这里镜像一份；改 globals.css 的
// 对应色时顺手同步。
export const THEMES: { id: ThemeId; label: string; swatch: string }[] = [
  { id: "green", label: t("绿色（默认）"), swatch: "oklch(0.53 0.15 155)" },
  { id: "blue", label: t("蓝色"), swatch: "oklch(0.52 0.16 250)" },
  { id: "gold", label: t("金色"), swatch: "oklch(0.56 0.12 80)" },
  { id: "night", label: t("夜间模式"), swatch: "oklch(0.205 0.025 165)" },
];

const STORAGE_KEY = "llmgate.ui.theme";

const THEME_CLASS: Record<ThemeId, string> = {
  green: "",
  blue: "theme-blue",
  gold: "theme-gold",
  night: "dark",
};

export function loadTheme(): ThemeId {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === "blue" || v === "gold" || v === "night") return v;
  } catch {
    // storage 不可用（隐私模式等）就当没存过。
  }
  return "green";
}

// 换类 + 持久化。绿色是缺省：落回缺省时清掉存储项，而不是存一个 "green"。
export function setTheme(theme: ThemeId): void {
  const cl = document.documentElement.classList;
  cl.remove("theme-blue", "theme-gold", "dark");
  if (THEME_CLASS[theme] !== "") cl.add(THEME_CLASS[theme]);
  try {
    if (theme === "green") localStorage.removeItem(STORAGE_KEY);
    else localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // 存不上就只在本次打开的页面里生效。
  }
}

// 两个切换入口共用这一份：state 只喂界面上的选中态，真正生效的恒是挂到 <html>
// 上的类。初值与 index.html 首帧内联脚本读同一处 localStorage，两边天然一致。
// 登录页与顶栏不会同时在场（app.tsx 二选一渲染），各自持有一份 state 不会看到
// 对方改完的旧值，所以不必再给它加一层共享 store。
export function useTheme(): [ThemeId, (next: ThemeId) => void] {
  const [theme, setThemeState] = useState<ThemeId>(loadTheme);
  const choose = useCallback((next: ThemeId): void => {
    setThemeState(next);
    setTheme(next);
  }, []);
  return [theme, choose];
}
