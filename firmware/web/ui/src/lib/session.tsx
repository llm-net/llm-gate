// 会话。三件事：boot 探一次「我登着吗」、暴露登录态、会话过期时送回登录页。
//
// 整个 /ui/ 要登录：静态层本身不要求会话（Go 侧 requiresSession 只管
// /admin/v1/*），门在这里——boot 打一次 GET /admin/v1/session，答不上来就送去
// 登录页。
//
// **登录态是一个布尔**：设备只有一个管理员、一个口令，没有「当前用户」可读
// （0019 起用户概念整个退场，服务端那条探针也刻意不回 body）。

import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { t } from "@/lib/i18n";

import * as api from "./api";
import { navigate } from "./router";
import { LANDING } from "./routes";

export interface Session {
  /** 已登录。boot 未回来时恒为 false，判定要先看 booting。 */
  authed: boolean;
  /** boot 还没回来：这时既不能判定已登录，也不能判定未登录。 */
  booting: boolean;
  /** 传输层都问不通（不是 401）时的文案；非 null 即渲染启动失败卡。 */
  fatal: string | null;
  retryBoot: () => void;
  onAuthed: () => void;
  logout: () => void;
}

const Ctx = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(Ctx);
  if (s === null) throw new Error("useSession 必须在 SessionProvider 内使用"); // i18n-ignore
  return s;
}

export function SessionProvider({ children }: { children: React.ReactNode }): React.ReactElement {
  const [authed, setAuthed] = useState(false);
  const [booting, setBooting] = useState(true);
  const [fatal, setFatal] = useState<string | null>(null);
  const [epoch, setEpoch] = useState(0);

  // 会话失效回调要读到「刚才是不是登着的」才能决定要不要弹提示，用 ref 取最新值。
  const wasAuthed = useRef(false);
  wasAuthed.current = authed;

  useEffect(() => {
    let alive = true;
    setBooting(true);
    setFatal(null);
    api.session().then(
      () => {
        if (!alive) return;
        setAuthed(true);
        setBooting(false);
      },
      (err: unknown) => {
        if (!alive) return;
        // **401 是「还没登录」的正常答复，不是故障。** 启动失败卡只给「连自己的
        // 管理面都问不通」准备。
        if (!(err instanceof api.ApiError)) {
          setFatal(api.errorMessage(err));
          setBooting(false);
          return;
        }
        setAuthed(false);
        setBooting(false);
      },
    );
    return () => {
      alive = false;
    };
  }, [epoch]);

  useEffect(() => {
    api.setSessionExpiredHandler(() => {
      // 未登录时的启动探针 401 不属于会话过期，保留 /connect 的 API 密钥入口。
      if (!wasAuthed.current) return;
      toast.error(t("会话已过期，请重新登录"));
      setAuthed(false);
      navigate("/login", { replace: true });
    });
  }, []);

  const onAuthed = useCallback(() => {
    setAuthed(true);
    navigate(LANDING);
  }, []);

  const logout = useCallback(() => {
    void (async () => {
      try {
        await api.logout();
      } catch {
        // 会话已失效等价于已登出，静默降级。
      }
      setAuthed(false);
      toast(t("已退出登录"));
      navigate("/login", { replace: true });
    })();
  }, []);

  const retryBoot = useCallback(() => setEpoch((n) => n + 1), []);

  return (
    <Ctx.Provider value={{ authed, booting, fatal, retryBoot, onAuthed, logout }}>
      {children}
    </Ctx.Provider>
  );
}
