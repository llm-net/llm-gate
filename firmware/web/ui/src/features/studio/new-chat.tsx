// 创作工作空间的新建对话对话框：先选引擎——工作空间所在的工作节点上的 Codex、Claude Code 或 Grok（读数现探，
// 节点上没有的标明原因并链到那台节点的工具配置页）——再选这把 API 密钥在该引擎的开发工具接入面上
// 能用的模型与推理档位。选中的密钥下方列出这个空间启用的生成模型里它能用的那些。

import { LoaderCircle } from "lucide-react";
import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from "@/components/ui/select";
import { effortLabel } from "@/features/agentchat/shared";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { Link, navigate } from "@/lib/router";

const DEFAULT_MODEL = "__default__";
const DEFAULT_EFFORT = "__default__";

/** 一把密钥能不能驱动这种引擎。 */
function keyUsable(k: api.StudioKeyOption, engineId: string): boolean {
  return !k.disabled && k.plaintext_available && k.engines[engineId]?.enabled === true;
}

/** 密钥选项的后缀：为什么选不了 / 订阅现状。 */
function keySuffix(k: api.StudioKeyOption, engine: api.StudioEngineOption | undefined): string {
  if (k.disabled) return t("已停用");
  if (!k.plaintext_available) return t("无封存明文");
  const access = engine === undefined ? undefined : k.engines[engine.id];
  if (engine === undefined || access === undefined) return "";
  if (!access.enabled) return engine.tool === "grok" ? t("未授权 Grok 订阅") : t("未授权 {engine}，也无开发工具可见的模型", { engine: engine.label });
  if (access.configured && !access.available) return t("订阅暂不可用");
  if (!access.configured) return t("仅目录模型");
  return "";
}

function keyLabel(k: api.StudioKeyOption, engine: api.StudioEngineOption | undefined): string {
  const suffix = keySuffix(k, engine);
  return (k.label === "" ? k.display : `${k.label} · ${k.display}`) + (suffix === "" ? "" : ` · ${suffix}`);
}

/** 选中密钥下方的生成模型说明。 */
function MediaNote({ k }: { k: api.StudioKeyOption }): React.ReactElement {
  if ((k.media_models ?? []).length === 0) {
    return <p className="text-muted-foreground text-xs">{t("这个工作空间还没有启用生成模型：对话里只能查看、整理素材与写文案。可在顶栏「配置媒体生成能力」里启用。")}</p>;
  }
  const usable = (k.media_models ?? []).filter((m) => m.available);
  const line = (label: string, kind: api.MediaModel["kind"]) => {
    const models = usable.filter((m) => m.kind === kind);
    return (
      <li className="flex flex-wrap items-center gap-1">
        <span className="shrink-0">{label}</span>
        {models.length > 0 ? <span className="font-mono">{models.map((m) => m.id).join(" · ")}</span> : <span className="text-muted-foreground">{t("不可用")}</span>}
      </li>
    );
  };
  return (
    <ul className="text-muted-foreground flex flex-col gap-0.5 text-xs">
      {line(t("图像："), "image")}
      {line(t("视频："), "video")}
    </ul>
  );
}

export function StudioNewChatDialog({ ws, onClose, onCreated }: {
  ws: api.Workspace;
  onClose: () => void;
  onCreated: (chat: api.StudioChat) => void;
}): React.ReactElement {
  const [options, setOptions] = useState<api.StudioChatOptions | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [engineId, setEngineId] = useState("");
  const [keyId, setKeyId] = useState("");
  const [model, setModel] = useState(DEFAULT_MODEL);
  const [effort, setEffort] = useState(DEFAULT_EFFORT);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    api.getStudioOptions(ws.id).then(
      (o) => {
        if (!alive) return;
        setOptions(o);
        // 缺省选第一个节点上有的引擎；它只有一把能用的密钥就直接选上。
        const engine = o.engines.find((e) => e.ready) ?? o.engines[0];
        if (engine === undefined) return;
        setEngineId(engine.id);
        const usable = o.keys.filter((k) => keyUsable(k, engine.id));
        if (usable.length === 1) setKeyId(String(usable[0]?.id ?? ""));
      },
      (err: unknown) => {
        if (alive) setLoadError(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
  }, [ws.id]);

  const engine = options?.engines.find((e) => e.id === engineId);
  const selectedKey = options?.keys.find((k) => String(k.id) === keyId);
  const access = selectedKey === undefined || engine === undefined ? undefined : selectedKey.engines[engine.id];
  const subscriptionModels = access?.models.filter((m) => m.source === "subscription") ?? [];
  const catalogModels = access?.models.filter((m) => m.source === "catalog") ?? [];

  function chooseEngine(id: string): void {
    setEngineId(id);
    setModel(DEFAULT_MODEL);
    const next = options?.engines.find((e) => e.id === id);
    if (next !== undefined && !next.efforts.includes(effort)) setEffort(DEFAULT_EFFORT);
    const usable = options?.keys.filter((k) => keyUsable(k, id)) ?? [];
    if (selectedKey === undefined || !keyUsable(selectedKey, id)) setKeyId(usable.length === 1 ? String(usable[0]?.id ?? "") : "");
  }

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    if (engine === undefined || !engine.ready) {
      setError(t("请选择工作节点上可用的引擎"));
      return;
    }
    if (keyId === "") {
      setError(t("请选择 API 密钥"));
      return;
    }
    setBusy(true);
    setError(null);
    api
      .createStudioChat(ws.id, { title: title.trim(), engine: engine.id, key_id: Number(keyId), model: model === DEFAULT_MODEL ? "" : model, effort: effort === DEFAULT_EFFORT ? "" : effort })
      .then(
        (r) => {
          onCreated(r.chat);
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
          <DialogHeader>
            <DialogTitle>{t("新建对话")}</DialogTitle>
          </DialogHeader>
          <p className="text-muted-foreground text-xs leading-relaxed">
            {t("智能体在工作节点 {host} 上运行，当前目录就是这个工作空间：它用节点上的 shell 与文件工具直接处理素材（以登录用户身份，每条命令与每次改文件都记进时间线），生成图像 / 视频经设备完成。对话新建时会注入素材库清单与项目说明；下面选的引擎、API 密钥、模型与推理档位在对话里不能再改，模型调用与生成都按这把密钥结算。", { host: ws.host_name === "" ? ws.host_address : ws.host_name })}
          </p>
          {loadError !== null ? <p className="text-signal-alert text-sm">{loadError}</p> : null}
          {options === null ? (
            loadError === null ? <p className="text-muted-foreground text-sm">{t("加载中…")}</p> : null
          ) : (
            <>
              <div className="flex flex-col gap-1.5">
                <Label>{t("引擎")}</Label>
                <Select value={engineId} disabled={busy} onValueChange={chooseEngine}>
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder={t("未选择")} />
                  </SelectTrigger>
                  <SelectContent>
                    {options.engines.map((e) => (
                      <SelectItem key={e.id} value={e.id} disabled={!e.ready}>
                        {e.label}
                        {e.ready ? (e.version !== undefined && e.version !== "" ? ` · ${e.version}` : "") : ` · ${t("节点上没有")}`}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {engine !== undefined && !engine.ready ? (
                  <div className="flex flex-col gap-1">
                    <p className="text-signal-alert text-xs">{engine.reason ?? t("工作节点上没有这个引擎")}</p>
                    {ws.host_id !== 0 ? (
                      <button type="button" className="self-start text-xs underline" onClick={() => navigate("/host-tools", { search: `?host=${ws.host_id}` })}>
                        {t("去这台节点的工具配置页安装")}
                      </button>
                    ) : null}
                  </div>
                ) : engine?.path !== undefined ? (
                  <p className="text-muted-foreground font-mono text-[11px] break-all">{engine.path}</p>
                ) : null}
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="studio-chat-title">{t("标题（可选）")}</Label>
                <Input id="studio-chat-title" value={title} maxLength={80} disabled={busy} placeholder={t("留空则用第一条指令的首行")} onChange={(e) => setTitle(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label>{t("API 密钥")}</Label>
                <Select
                  value={keyId}
                  disabled={busy || engine === undefined}
                  onValueChange={(v) => {
                    setKeyId(v);
                    setModel(DEFAULT_MODEL);
                  }}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder={t("未选择")} />
                  </SelectTrigger>
                  <SelectContent>
                    {options.keys.map((k) => (
                      <SelectItem key={k.id} value={String(k.id)} disabled={!keyUsable(k, engineId)}>
                        {keyLabel(k, engine)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {options.keys.length === 0 ? (
                  <Link to="/keys" className="text-xs underline">{t("还没有 API 密钥，去创建")}</Link>
                ) : engine !== undefined && !options.keys.some((k) => keyUsable(k, engine.id)) ? (
                  <Link to="/keys" className="text-xs underline">{engine.tool === "grok" ? t("去为密钥授权 Grok 订阅") : t("去为密钥授权 {engine} 订阅或勾选开发工具可见的模型", { engine: engine.label })}</Link>
                ) : null}
                {selectedKey !== undefined ? <MediaNote k={selectedKey} /> : null}
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-1.5">
                  <Label>{t("模型")}</Label>
                  <Select value={model} disabled={busy || keyId === ""} onValueChange={setModel}>
                    <SelectTrigger className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={DEFAULT_MODEL}>
                        {access === undefined || access.default_model === "" ? t("缺省模型") : t("缺省（{model}）", { model: access.default_model })}
                      </SelectItem>
                      {subscriptionModels.length > 0 ? (
                        <SelectGroup>
                          <SelectLabel>{t("{engine} 订阅自带", { engine: engine?.label ?? "" })}</SelectLabel>
                          {subscriptionModels.map((m) => (
                            <SelectItem key={m.name} value={m.name}>{m.name}</SelectItem>
                          ))}
                        </SelectGroup>
                      ) : null}
                      {catalogModels.length > 0 ? (
                        <SelectGroup>
                          <SelectLabel>{t("目录模型")}</SelectLabel>
                          {catalogModels.map((m) => (
                            <SelectItem key={m.name} value={m.name}>{m.name}</SelectItem>
                          ))}
                        </SelectGroup>
                      ) : null}
                    </SelectContent>
                  </Select>
                  {access !== undefined && access.models.length === 0 ? (
                    <p className="text-muted-foreground text-xs">
                      {access.configured && !access.available
                        ? t("钉死的订阅账号暂不可用，订阅自带的模型此刻不可选。")
                        : t("这把密钥在 {engine} 面还没有可见模型。", { engine: engine?.label ?? "" })}
                    </p>
                  ) : null}
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label>{t("推理档位")}</Label>
                  <Select value={effort} disabled={busy || engine === undefined} onValueChange={setEffort}>
                    <SelectTrigger className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={DEFAULT_EFFORT}>{effortLabel("")}</SelectItem>
                      {(engine?.efforts ?? []).map((e) => (
                        <SelectItem key={e} value={e}>{effortLabel(e)} · {e}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>
            </>
          )}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy || options === null || engine === undefined || !engine.ready || keyId === ""}>
              {busy ? <LoaderCircle className="animate-spin" /> : null}
              {t("新建")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
