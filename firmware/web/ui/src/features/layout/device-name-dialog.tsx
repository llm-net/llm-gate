import { Shuffle } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

// 与 store.DeviceName 的自动名称同域。getRandomValues 在纯 IP HTTP 上也可用。
function randomDeviceName(previous: string): string {
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
  let result: string;
  do {
    result = "";
    while (result.length < 6) {
      const bytes = crypto.getRandomValues(new Uint8Array(6));
      for (const byte of bytes) {
        if (byte < 252 && result.length < 6) result += alphabet[byte % alphabet.length];
      }
    }
  } while (result === previous);
  return result;
}

// 每次打开重新挂载，初值取顶栏最新名称；生成只改草稿，保存成功才更新顶栏。
export function DeviceNameDialog({
  name, onClose, onSaved,
}: {
  name: string;
  onClose: () => void;
  onSaved: (name: string) => void;
}) {
  const [draft, setDraft] = useState(name);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    const normalized = draft.trim();
    if (!normalized) {
      setError(t("设备名不能为空"));
      return;
    }
    if (Array.from(normalized).length > 32 || /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(normalized)) {
      setError(t("设备名须为 1–32 个字符，不能包含控制字符或不可见格式字符"));
      return;
    }
    setBusy(true);
    setError(null);
    api.setDeviceName(normalized).then(
      (saved) => {
        onSaved(saved.name);
        toast(t("设备名已修改"));
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader><DialogTitle>{t("修改设备名")}</DialogTitle></DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="device-name">{t("设备名")}</Label>
            <Input id="device-name" value={draft} required disabled={busy}
              aria-invalid={error !== null} aria-describedby={error ? "device-name-help device-name-error" : "device-name-help"}
              onChange={(event) => { setDraft(event.target.value); setError(null); }} />
            <p id="device-name-help" className="text-muted-foreground text-xs">{t("1–32 个字符，可使用中文。随机生成会填入 6 位字母和数字。")}</p>
          </div>
          <Button type="button" variant="outline" className="self-start" disabled={busy}
            onClick={() => { setDraft(randomDeviceName(draft)); setError(null); }}>
            <Shuffle />{t("随机生成")}
          </Button>
          {error === null ? null : <p id="device-name-error" role="alert" className="text-destructive text-sm">{error}</p>}
          <DialogFooter>
            <Button type="submit" disabled={busy}>{t("保存")}</Button>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
