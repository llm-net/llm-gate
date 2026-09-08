// 页内确认弹层。承接管理台 dom.ts 的 `confirmDialog`——它是 Promise 化的，调用点
// 写成 `if (!(await confirm({...}))) return;`，迁过来的页面照这个形状用。
//
// **禁止 window.confirm**：浏览器原生框长在页面视觉体系之外，还带着 localhost 前缀。
//
// 两条语义必须保住（都来自 firmware/AGENTS.md 与 dom.ts 的注释）：
// - **取消键在前**，承接初始焦点：回车缺省是取消，危险动作不会被顺手一个回车误确认。
//   Radix 的 AlertDialog 本身就把初始焦点给 Cancel，DOM 顺序也要跟着放在前面。
// - **不可逆动作（删除、改名）用 danger 红键，可逆的停用/禁用类用缺省键。**

import { createContext, useCallback, useContext, useRef, useState } from "react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";

export interface ConfirmOptions {
  title: string;
  /** 正文。多段就传数组，渲染成多个段落。 */
  body?: React.ReactNode;
  confirmText?: string;
  cancelText?: string;
  /** 不可逆动作把确认键染红；缺省是普通主键。 */
  danger?: boolean;
}

type Confirm = (opts: ConfirmOptions) => Promise<boolean>;

const Ctx = createContext<Confirm | null>(null);

export function useConfirm(): Confirm {
  const fn = useContext(Ctx);
  if (fn === null) throw new Error("useConfirm 必须在 ConfirmProvider 内使用"); // i18n-ignore
  return fn;
}

export function ConfirmProvider({ children }: { children: React.ReactNode }): React.ReactElement {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  // resolve 存在 ref 里：关闭路径有三条（确认、取消、Esc/点遮罩），都要落到同一个
  // resolve 上，缺省 false——对齐 dom.ts 里「resolve(false) 由 close 事件兜底」。
  const resolveRef = useRef<((ok: boolean) => void) | null>(null);

  const confirm = useCallback<Confirm>((o) => {
    setOpts(o);
    return new Promise<boolean>((resolve) => {
      resolveRef.current = resolve;
    });
  }, []);

  function settle(ok: boolean): void {
    resolveRef.current?.(ok);
    resolveRef.current = null;
    setOpts(null);
  }

  return (
    <Ctx.Provider value={confirm}>
      {children}
      <AlertDialog
        open={opts !== null}
        onOpenChange={(open) => {
          if (!open) settle(false); // Esc 与点遮罩走这条
        }}
      >
        {opts === null ? null : (
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{opts.title}</AlertDialogTitle>
              {opts.body === undefined ? null : (
                <AlertDialogDescription asChild>
                  <div className="space-y-2">{opts.body}</div>
                </AlertDialogDescription>
              )}
            </AlertDialogHeader>
            <AlertDialogFooter>
              {/* 取消在前：DOM 顺序即焦点顺序。 */}
              <AlertDialogCancel onClick={() => settle(false)}>
                {opts.cancelText ?? t("取消")}
              </AlertDialogCancel>
              <AlertDialogAction
                className={cn(
                  opts.danger === true &&
                    "bg-destructive text-white hover:bg-destructive/90 focus-visible:ring-destructive/20",
                )}
                onClick={() => settle(true)}
              >
                {opts.confirmText ?? t("确定")}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        )}
      </AlertDialog>
    </Ctx.Provider>
  );
}
