// 「添加模型」：把这个账号平台上的模型加进模型目录。勾选清单 + 自定义一条。
//
// 模型不从模型列「新建」，而是从**承载它的账号**加——平台已知，协议面与优先级都不必
// 再问一遍。清单来自平台模型信息文件（固件内嵌基线，可从官网进行数据升级），**且恒可
// 自定义**：厂商上新而清单还没更新时照样录得进去。
//
// 打开时读一次可选清单（用户动作触发的一次性读取，不违零轮询）。清单读不到不该把这个
// 对话框废掉：自定义那条路不依赖它（服务端仍会按这个账号服务的协议面裁决），所以退化
// 成「只能自定义」，三个种类都列出来。

import { useEffect, useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

// 一句话说清这份选单是哪来的、有多新：内嵌基线是设备常态（官网不可达），同步来的更新
// 版本才标 v 号与厂商核对日期。
function catalogOrigin(c: api.UpstreamModelsCatalog): string {
  const v = c.version;
  const at = c.checked_at ?? "";
  if (c.origin === "synced") {
    return at === ""
      ? t("清单来自官网更新的 v{v}。", { v })
      : t("清单来自官网更新的 v{v}，官方型号表核对于 {at}。", { v, at });
  }
  return at === ""
    ? t("清单来自固件内嵌的 v{v}。", { v })
    : t("清单来自固件内嵌的 v{v}，官方型号表核对于 {at}。", { v, at });
}

export interface AddModelsState {
  target: api.Upstream | null;
  open: (u: api.Upstream) => void;
  close: () => void;
}

export function useAddModels(): AddModelsState {
  const [target, setTarget] = useState<api.Upstream | null>(null);
  return { target, open: setTarget, close: () => setTarget(null) };
}

export function AddModelsDialog({
  state,
  onDone,
}: {
  state: AddModelsState;
  onDone: () => void;
}): React.ReactElement {
  const u = state.target;
  const [res, setRes] = useState<api.UpstreamModels | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [customName, setCustomName] = useState("");
  const [customKind, setCustomKind] = useState<api.ModelKind>("text");
  const [customID, setCustomID] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (u === null) return;
    let alive = true;
    setRes(null);
    setLoadError(null);
    setPicked(new Set());
    setCustomName("");
    setCustomID("");
    setError(null);
    api.listUpstreamModels(u.id).then(
      (r) => {
        if (!alive) return;
        setRes(r);
        setCustomKind(r.kinds[0]?.kind ?? "text");
      },
      (err: unknown) => {
        if (!alive) return;
        setLoadError(api.errorMessage(err));
        setCustomKind("text");
      },
    );
    return () => {
      alive = false;
    };
  }, [u]);

  if (u === null) return <Dialog open={false} onOpenChange={() => undefined} />;

  const addable = (res?.models ?? []).filter((m) => m.added !== true && (m.blocked ?? "") === "");
  const canSubmit = picked.size > 0 || customName.trim() !== "";
  // 清单读不到时三个种类都列出来（服务端仍会裁决）。
  const kindOptions: api.UpstreamKind[] =
    res !== null ? res.kinds : (["text", "video", "image"] as const).map((k) => ({ kind: k }));

  function submit(): void {
    if (u === null) return;
    const models: api.AddUpstreamModel[] = [];
    for (const m of addable) {
      if (!picked.has(m.name)) continue;
      const one: api.AddUpstreamModel = { name: m.name, kind: m.kind };
      if (m.upstream_model_id !== undefined && m.upstream_model_id !== "") {
        one.upstream_model_id = m.upstream_model_id;
      }
      models.push(one);
    }
    const custom = customName.trim();
    if (custom !== "") {
      const one: api.AddUpstreamModel = { name: custom, kind: customKind };
      if (customID.trim() !== "") one.upstream_model_id = customID.trim();
      models.push(one);
    }
    if (models.length === 0) return;
    setError(null);
    setBusy(true);
    api.addUpstreamModels(u.id, models).then(
      (r) => {
        setBusy(false);
        // POST 回添加后的最新清单，弹框原地重绘，不必再发一次 GET。
        setRes(r);
        setPicked(new Set());
        setCustomName("");
        setCustomID("");
        toast(t("已添加 {created} 个模型、挂上 {added} 条上游", { created: r.created, added: r.added }));
        onDone();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) state.close();
      }}
    >
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("添加模型 — {name}", { name: u.name })}</DialogTitle>
        </DialogHeader>
        <p className="text-muted-foreground text-xs">
          {res === null
            ? loadError === null
              ? t("载入模型清单…")
              : t("清单载入失败，仍可用下面的「自定义」录入。")
            : t(
                "勾选要加进模型目录的模型：添加后即建好模型行并把本账号挂成它的上游（默认优先级 P{priority}，可在「模型」列表的行内「编辑」里改）。{origin}",
                {
                  priority: res.priority,
                  origin: res.catalog.listed
                    ? catalogOrigin(res.catalog)
                    : t("这家平台的官方型号表还没收录进清单，请用下面的「自定义」录入。"),
                },
              )}
        </p>

        {res === null ? null : res.models.length === 0 ? (
          // 未收录的平台不再补一句：上面那段说明已经说了「请用自定义录入」。
          res.catalog.listed ? (
            <p className="text-muted-foreground text-sm">
              {t("这个平台的清单里暂时没有可加的型号，请用「自定义」录入。")}
            </p>
          ) : null
        ) : (
          <div className="flex flex-col gap-1 rounded-md border p-2">
            {res.models.map((m) => {
              const already = m.added === true;
              const blocked = (m.blocked ?? "") !== "";
              return (
                <label
                  key={m.name}
                  className={`flex items-center gap-2 rounded px-1 py-1 text-sm ${already || blocked ? "opacity-60" : ""}`}
                  title={blocked ? api.addModelBlockedLabel(m.blocked ?? "") : m.note}
                >
                  <Checkbox
                    disabled={already || blocked}
                    checked={picked.has(m.name)}
                    onCheckedChange={(c) => {
                      const next = new Set(picked);
                      if (c === true) next.add(m.name);
                      else next.delete(m.name);
                      setPicked(next);
                    }}
                  />
                  <code className="font-mono text-xs">{m.name}</code>
                  <span className="text-muted-foreground text-xs">{api.kindLabel(m.kind)}</span>
                  {already ? <span className="text-muted-foreground ml-auto text-xs">{t("已添加")}</span> : null}
                  {blocked ? (
                    <span className="text-signal-alert ml-auto text-xs">
                      {api.addModelBlockedLabel(m.blocked ?? "")}
                    </span>
                  ) : null}
                </label>
              );
            })}
          </div>
        )}

        <div className="flex flex-col gap-2 border-t pt-3">
          <h3 className="text-sm font-medium">{t("自定义")}</h3>
          <p className="text-muted-foreground text-xs">
            {t("厂商上新而清单还没更新时用它：协议面由账号所属平台定，不必在这里选。")}
          </p>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="cm-name">{t("模型名")}</Label>
            <Input
              id="cm-name"
              value={customName}
              maxLength={api.CatalogNameMaxLen}
              placeholder={t("厂商的原始模型名，如 deepseek-v4-pro")}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setCustomName(e.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t("种类")}</Label>
            <Select value={customKind} onValueChange={(v) => setCustomKind(v as api.ModelKind)}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {kindOptions.map((k) => (
                  <SelectItem key={k.kind} value={k.kind}>
                    {/* 协议面名已含厂商与模态（「火山方舟 视频」），不再前缀种类。 */}
                    {k.family === undefined ? api.kindLabel(k.kind) : api.protocolLabel(k.family)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="cm-id">{t("上游侧模型 ID")}</Label>
            <Input
              id="cm-id"
              value={customID}
              maxLength={api.CatalogNameMaxLen}
              placeholder={t("与模型名相同时留空")}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setCustomID(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("上游那边的真实模型 ID；与模型名一致时留空即可。")}</p>
          </div>
        </div>

        {loadError === null ? null : <p className="text-muted-foreground text-xs">{loadError}</p>}
        {error === null ? null : (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={state.close}>
            {t("取消")}
          </Button>
          <Button disabled={!canSubmit || busy} onClick={submit}>
            {t("添加")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
