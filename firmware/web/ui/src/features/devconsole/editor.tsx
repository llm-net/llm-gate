// 工作空间文件编辑器：一个等宽 textarea，Ctrl/⌘+S 保存。二进制与被截断的文件只读——
// 半截内容存回去会把文件写坏。读数与保存都只在动作时发生。

import { Loader2Icon } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type * as api from "@/lib/api";
import { errorMessage } from "@/lib/api";
import { t } from "@/lib/i18n";

export function Editor({
  console: con,
  file,
  loading,
  onSaved,
}: {
  console: api.DevConsole;
  file: api.DevFile | null;
  loading: boolean;
  onSaved: (file: api.DevFile) => void;
}): React.ReactElement {
  const [text, setText] = useState("");
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    setText(file?.content ?? "");
    setDirty(false);
  }, [file]);

  const readOnly = file === null || file.binary || file.truncated;

  function save(): void {
    if (file === null || readOnly || saving) return;
    setSaving(true);
    con.write(file.path, text).then(
      (saved) => {
        setSaving(false);
        setDirty(false);
        toast(t("已保存"));
        onSaved(saved);
      },
      (err: unknown) => {
        setSaving(false);
        toast.error(errorMessage(err));
      },
    );
  }

  if (file === null) {
    return (
      <div className="text-muted-foreground flex h-full items-center justify-center p-6 text-center text-sm">
        {loading ? <Loader2Icon className="animate-spin" /> : t("在左侧选择一个文件查看或编辑")}
      </div>
    );
  }
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-1.5 text-xs">
        <code className="min-w-0 flex-1 truncate font-mono" title={file.path}>{file.path}</code>
        {file.binary ? <Badge variant="secondary">{t("二进制")}</Badge> : null}
        {file.truncated ? <Badge variant="secondary">{t("已截断")}</Badge> : null}
        {dirty ? <Badge variant="outline">{t("未保存")}</Badge> : null}
        <Button size="sm" disabled={readOnly || !dirty || saving} onClick={save}>
          {saving ? <Loader2Icon className="animate-spin" /> : null}
          {t("保存")}
        </Button>
      </div>
      {file.binary ? (
        <p className="text-muted-foreground p-4 text-sm">{t("这是二进制文件（{size}），不显示内容。", { size: `${file.size} B` })}</p>
      ) : (
        <textarea
          className="bg-background min-h-0 flex-1 resize-none p-3 font-mono text-xs leading-5 outline-none"
          spellCheck={false}
          readOnly={readOnly}
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            setDirty(true);
          }}
          onKeyDown={(e) => {
            if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
              e.preventDefault();
              save();
            }
            if (e.key === "Tab" && !readOnly) {
              e.preventDefault();
              const el = e.currentTarget;
              const { selectionStart, selectionEnd } = el;
              const next = `${text.slice(0, selectionStart)}\t${text.slice(selectionEnd)}`;
              setText(next);
              setDirty(true);
              window.requestAnimationFrame(() => {
                el.selectionStart = selectionStart + 1;
                el.selectionEnd = selectionStart + 1;
              });
            }
          }}
        />
      )}
      {file.truncated ? (
        <p className="text-muted-foreground border-t px-3 py-1 text-xs">{t("文件超过 2 MB，只显示前面一段，不能在这里保存。")}</p>
      ) : null}
    </div>
  );
}
