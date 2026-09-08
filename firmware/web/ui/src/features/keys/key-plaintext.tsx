// Key 明文读数窗（签发与行内复制共用）。
//
// 明文封存入库、随时可再复制，所以这里不是「仅此一次」的窗口——但**明文仍只
// 活在这个对话框里**：不进任何持久化状态、不写 URL、不进查询缓存，再看一眼
// 就再解封一次。调用方把它放进 useState 即可，关窗时清掉。

import { useEffect, useRef } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import type { ApiKey } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { t } from "@/lib/i18n";

export interface KeyPlaintext {
  key: ApiKey;
  plaintext: string;
  title?: string;
  /** null 表示不显示补充说明（行内复制的降级窗用）。 */
  note?: string | null;
}

export function KeyPlaintextDialog({
  value,
  onClose,
}: {
  value: KeyPlaintext | null;
  onClose: () => void;
}): React.ReactElement {
  const inputRef = useRef<HTMLInputElement>(null);

  // 打开即聚焦并全选：拿到这一窗的人多半正要复制它。
  useEffect(() => {
    if (value !== null) inputRef.current?.select();
  }, [value]);

  const note =
    value?.note === undefined ? t("这把密钥的明文随时可在本页列表里重新复制。") : value.note;

  return (
    <Dialog
      open={value !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      {value === null ? null : (
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{value.title ?? t("Key 已创建")}</DialogTitle>
            <DialogDescription>
              {value.key.label === ""
                ? t("密钥 {prefix}…{last4}", { prefix: value.key.display_prefix, last4: value.key.display_last4 })
                : t("标签：{label}", { label: value.key.label })}
            </DialogDescription>
          </DialogHeader>
          <Input
            ref={inputRef}
            readOnly
            spellCheck={false}
            value={value.plaintext}
            className="font-mono text-xs"
            onFocus={(e) => e.currentTarget.select()}
          />
          {note === null ? null : <p className="text-muted-foreground text-xs">{note}</p>}
          <DialogFooter>
            <Button
              onClick={() => {
                void copyText(value.plaintext).then((ok) => {
                  if (ok) toast(t("已复制到剪贴板"));
                  else toast.error(t("复制失败，请手动选中复制"));
                });
              }}
            >
              {t("复制")}
            </Button>
            <Button variant="outline" onClick={onClose}>
              {t("关闭")}
            </Button>
          </DialogFooter>
        </DialogContent>
      )}
    </Dialog>
  );
}
