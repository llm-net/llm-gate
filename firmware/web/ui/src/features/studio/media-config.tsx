// 创作工作空间顶栏的「配置媒体生成能力」：显式指定哪些图像 / 视频生成模型能在这个空间里用
// （白名单，一个都没启用即不能生成），并可给每个模型写一段「什么情况下用它」的说明——说明写进
// 创作智能体的开发者指令。存下即生效：生成工具按它裁决，进行中的对话在下一条指令前收到新的
// 模型清单；已归档的对话不受影响。
//
// 按钮进页取一次读数（显示已启用的个数），对话框保存后用响应换读数；不轮询。模型名、参数都
// 来自设备的能力表，页面里不写死。

import { SlidersHorizontalIcon } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";
import { Link } from "@/lib/router";

const MAX_USAGE = 400;

interface Row {
  enabled: boolean;
  usage: string;
}

function ModelRow({ model, row, busy, onChange }: { model: api.StudioMediaModel; row: Row; busy: boolean; onChange: (next: Row) => void }): React.ReactElement {
  const switchId = `media-model-${model.id}`;
  return (
    <li className={cn("flex flex-col gap-2 rounded-md border px-3 py-2", row.enabled ? "border-primary/40 bg-primary/5" : "")}>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <Switch
          id={switchId}
          checked={row.enabled}
          disabled={busy}
          onCheckedChange={(on) => onChange({ enabled: on, usage: on && row.usage === "" ? model.default_usage : row.usage })}
        />
        <label htmlFor={switchId} className="min-w-0 cursor-pointer font-mono text-sm break-all">{model.id}</label>
        <Badge variant="outline" className="text-[10px]">{model.backend}</Badge>
        <Badge variant="outline" className={cn("text-[10px]", model.billing === "metered" ? "text-signal-warn" : "")}>
          {model.billing === "metered" ? t("按量计费") : t("订阅流量")}
        </Badge>
        {model.operations.length > 1 ? <span className="text-muted-foreground text-[11px]">{model.operations.map((op) => op.label).join(" / ")}</span> : null}
      </div>
      {model.note !== undefined && model.note !== "" ? <p className="text-muted-foreground text-[11px]">{model.note}</p> : null}
      {row.enabled ? (
        <div className="flex flex-col gap-1">
          <Textarea
            value={row.usage}
            rows={2}
            maxLength={MAX_USAGE}
            disabled={busy}
            aria-label={t("{model} 的使用场景说明", { model: model.id })}
            placeholder={t("什么情况下用这个模型（可选）。留空则由智能体自行判断。")}
            className="min-h-0 text-xs"
            onChange={(e) => onChange({ enabled: true, usage: e.target.value })}
          />
          {model.default_usage !== "" && row.usage !== model.default_usage ? (
            <button type="button" disabled={busy} className="text-muted-foreground hover:text-foreground self-start text-[11px] underline" onClick={() => onChange({ enabled: true, usage: model.default_usage })}>
              {t("填入缺省说明")}
            </button>
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

function ConfigDialog({ wsId, models, onClose, onSaved }: { wsId: string; models: api.StudioMediaModel[]; onClose: () => void; onSaved: (models: api.StudioMediaModel[]) => void }): React.ReactElement {
  const known = useMemo(() => models.filter((m) => m.missing !== true), [models]);
  const missing = useMemo(() => models.filter((m) => m.missing === true), [models]);
  const [rows, setRows] = useState<Record<string, Row>>(() => Object.fromEntries(known.map((m) => [m.id, { enabled: m.enabled, usage: m.usage }])));
  const [busy, setBusy] = useState(false);
  const enabledCount = known.filter((m) => rows[m.id]?.enabled === true).length;

  function save(e: React.FormEvent): void {
    e.preventDefault();
    setBusy(true);
    const payload = known.filter((m) => rows[m.id]?.enabled === true).map((m) => ({ model: m.id, usage: (rows[m.id]?.usage ?? "").trim() }));
    api.putStudioMedia(wsId, payload).then(
      (r) => {
        toast(t("媒体生成能力已保存，进行中的对话从下一条指令起生效"));
        onSaved(r.models);
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        toast.error(api.errorMessage(err));
      },
    );
  }

  const group = (kind: api.MediaModel["kind"], title: string) => {
    const list = known.filter((m) => m.kind === kind);
    return (
      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-medium">{title}</h3>
        {list.length === 0 ? (
          <p className="text-muted-foreground text-xs">{t("设备上还没有这类生成模型。")}</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {list.map((m) => (
              <ModelRow key={m.id} model={m} row={rows[m.id] ?? { enabled: false, usage: "" }} busy={busy} onChange={(next) => setRows((cur) => ({ ...cur, [m.id]: next }))} />
            ))}
          </ul>
        )}
      </section>
    );
  };

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent className="sm:max-w-2xl">
        <form className="flex flex-col gap-4" onSubmit={save}>
          <DialogHeader>
            <DialogTitle>{t("配置媒体生成能力")}</DialogTitle>
          </DialogHeader>
          <p className="text-muted-foreground text-xs leading-relaxed">
            {t("只有在这里启用的模型，这个工作空间的智能体才能用来生成图像 / 视频；一个都不启用即不能生成。每个模型可以写一段使用场景说明，智能体据此挑选模型。保存后，进行中的对话从下一条指令起按新的配置工作，已归档的对话不受影响。模型最终能否调用还取决于对话所选 API 密钥的授权。")}
          </p>
          {group("image", t("图像模型"))}
          {group("video", t("视频模型"))}
          {missing.length > 0 ? (
            <p className="text-muted-foreground text-xs">
              {t("以下模型启用过，但设备上已经没有了，保存时会移除：{models}", { models: missing.map((m) => m.id).join("、") })}
            </p>
          ) : null}
          <p className="text-muted-foreground text-xs">
            {t("订阅模型来自「开发工具订阅」，按量模型是「模型接入」里建的图像 / 视频模型。")}{" "}
            <Link to="/agent-accounts" className="underline">{t("开发工具订阅")}</Link>
          </p>
          <DialogFooter>
            <span className="text-muted-foreground mr-auto self-center text-xs">{t("已启用 {n} 个", { n: enabledCount })}</span>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy}>{busy ? t("保存中…") : t("保存")}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** 顶栏按钮 + 对话框。 */
export function MediaConfigButton({ wsId }: { wsId: string }): React.ReactElement {
  const [models, setModels] = useState<api.StudioMediaModel[] | null>(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    let alive = true;
    setModels(null);
    api.getStudioMedia(wsId).then(
      (r) => {
        if (alive) setModels(r.models);
      },
      (err: unknown) => {
        if (alive) toast.error(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
  }, [wsId]);

  const enabled = models === null ? null : models.filter((m) => m.enabled && m.missing !== true).length;
  return (
    <>
      <Button size="sm" variant="outline" disabled={models === null} title={enabled === 0 ? t("还没有启用任何生成模型：智能体现在不能生成图像 / 视频") : undefined} onClick={() => setOpen(true)}>
        <SlidersHorizontalIcon />
        {t("配置媒体生成能力")}
        {enabled === null ? null : (
          <Badge variant="outline" className={cn("ml-0.5 px-1.5 text-[10px]", enabled === 0 ? "text-signal-warn" : "")}>
            {enabled === 0 ? t("未配置") : t("{n} 个模型", { n: enabled })}
          </Badge>
        )}
      </Button>
      {open && models !== null ? <ConfigDialog wsId={wsId} models={models} onClose={() => setOpen(false)} onSaved={setModels} /> : null}
    </>
  );
}
