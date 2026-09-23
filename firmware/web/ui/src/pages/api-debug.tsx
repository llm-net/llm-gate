import { FlaskConical, LoaderCircle, Paperclip, Square, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import * as api from "@/lib/api";
import { debugClient, type DebugClient, type DebugInput, type DebugModel, type DebugResult } from "@/lib/api-debug";
import { keyMask } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";
import { PageContainer, PageHeader } from "./page-shell";

export function APIDebugPage({ holderKey }: { holderKey?: string }): React.ReactElement {
  const client = useMemo(() => debugClient(holderKey), [holderKey]);
  const keys = useResource(() => holderKey === undefined ? api.listKeys() : Promise.resolve({ keys: [] }), [holderKey]);
  const [keyID, setKeyID] = useState(0);
  const available = (keys.data?.keys ?? []).filter((key) => !key.disabled && !key.archived);
  const selectedID = available.some((key) => key.id === keyID) ? keyID : 0;
  return <PageContainer wide>
    <PageHeader title={t("API调测")} note={t("每次请求独立发送，不携带历史对话；用量计入所选 API 密钥。")}
      {...(holderKey === undefined ? { refreshing: keys.loading, onRefresh: keys.reload } : {})} />
    <section className="bg-card rounded-lg border p-4">
      <label className="flex flex-col gap-2 text-sm font-medium">
        {t("API 密钥")}
        {holderKey === undefined ? <Select value={selectedID ? String(selectedID) : ""} onValueChange={(value) => setKeyID(Number(value))} disabled={keys.loading}>
          <SelectTrigger className="w-full sm:w-96"><SelectValue placeholder={t("请选择 API 密钥")} /></SelectTrigger>
          <SelectContent>{available.map((key) => <SelectItem key={key.id} value={String(key.id)}>{key.label ? `${key.label} · ` : ""}{key.display_prefix}…{key.display_last4}</SelectItem>)}</SelectContent>
        </Select> : <Input readOnly disabled value={keyMask(holderKey)} className="sm:w-96" />}
      </label>
      {holderKey !== undefined ? <p className="text-muted-foreground mt-2 text-xs">{t("已锁定当前登录的 API 密钥。")}</p> : null}
      {keys.error ? <p role="alert" className="text-destructive mt-2 text-sm">{keys.error}</p> : null}
      {holderKey === undefined && !keys.loading && !keys.error && available.length === 0 ? <p className="text-muted-foreground mt-2 text-sm">{t("暂无启用的 API 密钥，请先在 API密钥页面创建或启用。")}</p> : null}
    </section>
    {holderKey !== undefined || selectedID > 0 ? <KeyDebug key={`${selectedID}:${holderKey === undefined ? "admin" : "holder"}`} client={client} keyID={selectedID} /> : null}
  </PageContainer>;
}

function modelID(model: DebugModel): string { return JSON.stringify([model.provider ?? "", model.name]); }

function KeyDebug({ client, keyID }: { client: DebugClient; keyID: number }): React.ReactElement {
  const models = useResource(() => client.models(keyID), [client, keyID]);
  const [name, setName] = useState("");
  const [protocol, setProtocol] = useState<api.Protocol>("openai_chat");
  const model = models.data?.models.find((item) => modelID(item) === name) ?? models.data?.models[0];
  const surface = api.TextProtocolSurfaces.find((item) => item.protocol === protocol && model?.protocols.includes(item.protocol))
    ?? api.TextProtocolSurfaces.find((item) => model?.protocols.includes(item.protocol));
  return <>
    <section className="bg-card grid gap-4 rounded-lg border p-4 sm:grid-cols-2">
      <label className="flex flex-col gap-2 text-sm font-medium">{t("文本模型")}
        <Select value={model ? modelID(model) : ""} onValueChange={setName} disabled={models.loading || !!models.error}>
          <SelectTrigger className="w-full"><SelectValue placeholder={t("请选择模型")} /></SelectTrigger>
          <SelectContent>{(models.data?.models ?? []).map((item) => <SelectItem key={modelID(item)} value={modelID(item)}>{item.name}{item.provider === "codex" ? ` · ${t("Codex 订阅")}` : item.provider === "grok" ? ` · ${t("Grok 订阅")}` : ""}</SelectItem>)}</SelectContent>
        </Select>
      </label>
      <label className="flex flex-col gap-2 text-sm font-medium">{t("协议面")}
        <Select value={surface?.protocol ?? ""} onValueChange={(value) => setProtocol(value as api.Protocol)} disabled={!model || models.loading || !!models.error}>
          <SelectTrigger className="w-full"><SelectValue placeholder={t("请选择协议面")} /></SelectTrigger>
          <SelectContent>{api.TextProtocolSurfaces.filter((item) => model?.protocols.includes(item.protocol)).map((item) => <SelectItem key={item.protocol} value={item.protocol}>{api.protocolLabel(item.protocol)}</SelectItem>)}</SelectContent>
        </Select>
      </label>
      {models.error ? <p role="alert" className="text-destructive text-sm">{models.error}</p> : null}
      {!models.loading && !models.error && !model ? <p className="text-muted-foreground text-sm">{t("这把密钥暂无可用的文本模型。")}</p> : null}
      <Button className="justify-self-start" variant="outline" size="sm" onClick={models.reload} disabled={models.loading}>{t("刷新模型")}</Button>
    </section>
    {model && surface && !models.loading && !models.error ? <DebugForm key={`${modelID(model)}:${surface.protocol}`} client={client} keyID={keyID} model={model} protocol={surface.protocol} path={model.provider ? `/agents/${model.provider}${surface.path}` : surface.path} /> : null}
  </>;
}

interface Attachment { file: File; type: string }
const MAX_BYTES = 4 * 1024 * 1024;
const EXT_TYPES: Record<string, string> = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", webp: "image/webp", gif: "image/gif", pdf: "application/pdf", wav: "audio/wav", mp3: "audio/mpeg", mp4: "video/mp4" };
function fileType(file: File): string { return EXT_TYPES[file.name.toLowerCase().split(".").pop() ?? ""] ?? file.type; }
function encode(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result).split(",")[1] ?? "");
    reader.onerror = reject;
    reader.readAsDataURL(file);
  });
}

function DebugForm({ client, keyID, model, protocol, path }: { client: DebugClient; keyID: number; model: DebugModel; protocol: api.Protocol; path: string }): React.ReactElement {
  const [prompt, setPrompt] = useState("");
  const [maxTokens, setMaxTokens] = useState(protocol === "anthropic_messages" ? 2048 : 0);
  const [files, setFiles] = useState<Attachment[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<DebugResult | null>(null);
  const controller = useRef<AbortController | null>(null);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => () => { controller.current?.abort(); controller.current = null; }, []);
  const accepted = model.file_types[protocol] ?? [];

  function addFiles(list: FileList | null): void {
    if (!list) return;
    const next = [...files, ...Array.from(list).map((file) => ({ file, type: fileType(file) }))];
    if (next.length > 8) { setError(t("最多上传 8 个文件")); return; }
    if (next.some((item) => !accepted.includes(item.type) || item.file.size === 0)) { setError(t("模型或协议面不支持该文件类型，或文件为空。")); return; }
    if (next.reduce((sum, item) => sum + item.file.size, 0) > MAX_BYTES) { setError(t("附件总大小不能超过 4 MiB")); return; }
    setFiles(next); setError(null);
  }

  async function submit(event: React.FormEvent): Promise<void> {
    event.preventDefault();
    if (controller.current || !prompt.trim()) return;
    const abort = new AbortController();
    controller.current = abort;
    setBusy(true); setError(null); setResult(null);
    try {
      const attachments: DebugInput["files"] = await Promise.all(files.map(async (item) => ({ name: item.file.name, type: item.type, data: await encode(item.file) })));
      if (abort.signal.aborted) return;
      const response = await client.submit(keyID, { model: model.name, provider: model.provider, protocol, prompt, max_tokens: maxTokens, files: attachments }, abort.signal);
      if (controller.current === abort) setResult(response);
    } catch (err) {
      if (controller.current === abort) setError(abort.signal.aborted ? t("调测已停止。") : api.errorMessage(err));
    } finally {
      if (controller.current === abort) { controller.current = null; setBusy(false); }
    }
  }

  return <div className="grid items-start gap-4 xl:grid-cols-2">
    <form onSubmit={(event) => { void submit(event); }} className="bg-card flex flex-col gap-4 rounded-lg border p-4" aria-busy={busy}>
      <h2 className="font-semibold">{t("请求")}</h2>
      <code className="text-muted-foreground text-xs">POST {path}</code>
      <label className="flex flex-col gap-2 text-sm font-medium">{t("提示词")}
        <Textarea className="min-h-40" required value={prompt} onChange={(event) => setPrompt(event.target.value)} disabled={busy} rows={8} placeholder={t("输入本次要发送给模型的内容")} />
      </label>
      {model.provider !== "codex" ? <label className="flex flex-col gap-2 text-sm font-medium">{t("输出 Token 上限")}
        <Input type="number" min={1} max={32768} step={1} required={protocol === "anthropic_messages"} value={maxTokens || ""} placeholder={t("留空使用平台默认值")} disabled={busy} onChange={(event) => setMaxTokens(Number(event.target.value))} />
      </label> : <p className="text-muted-foreground text-xs">{t("Codex 订阅使用平台默认输出 Token 上限")}</p>}
      {accepted.length ? <>
        <input ref={input} type="file" multiple accept={accepted.join(",")} className="hidden" onChange={(event) => { addFiles(event.target.files); event.target.value = ""; }} />
        <Button type="button" variant="outline" disabled={busy} onClick={() => input.current?.click()}><Paperclip />{t("上传多模态文件")}</Button>
        <p className="text-muted-foreground break-words text-xs">{t("最多 8 个文件，合计不超过 4 MiB。支持：{types}", { types: accepted.join("、") })}</p>
        {files.map((item, index) => <div key={index} className="flex items-center gap-2 rounded border p-2 text-sm"><span className="min-w-0 flex-1 break-all">{item.file.name}</span><span className="text-muted-foreground">{Math.ceil(item.file.size / 1024)} KiB</span><Button type="button" size="icon-sm" variant="ghost" disabled={busy} aria-label={t("移除附件")} onClick={() => setFiles(files.filter((_, i) => i !== index))}><X /></Button></div>)}
      </> : <p className="text-muted-foreground text-xs">{t("当前模型与协议面未声明可用的文件输入，仅支持文本调测。")}</p>}
      {error ? <p role="alert" className="text-destructive text-sm">{error}</p> : null}
      <div className="flex gap-2">
        <Button type="submit" disabled={busy || !prompt.trim()}>{busy ? <LoaderCircle className="animate-spin" /> : <FlaskConical />}{busy ? t("正在调测…") : t("发送请求")}</Button>
        {busy ? <Button type="button" variant="outline" onClick={() => controller.current?.abort()}><Square />{t("停止")}</Button> : null}
      </div>
    </form>
    <section className="bg-card min-w-0 space-y-4 rounded-lg border p-4" aria-live="polite">
      <h2 className="font-semibold">{t("响应")}</h2>
      {result ? <>
        <p className={result.status >= 400 ? "text-destructive text-sm" : "text-muted-foreground text-sm"}>HTTP {result.status} · {result.elapsed} ms</p>
        {result.requestID ? <p className="text-muted-foreground break-all text-xs">Request ID: {result.requestID}</p> : null}
        {result.streamStatus && result.streamStatus !== "completed" ? <p role="alert" className="text-destructive text-sm">{t("模型未正常完成响应，请查看完整响应。")}</p> : null}
        {result.text ? <pre className="whitespace-pre-wrap break-words font-sans text-sm">{result.text}</pre> : null}
        <details open={!result.text || !!result.streamStatus && result.streamStatus !== "completed"}><summary className="cursor-pointer text-sm">{t("完整响应")}</summary><pre className="mt-3 whitespace-pre-wrap break-all text-xs">{result.body}</pre></details>
      </> : <p className="text-muted-foreground text-sm">{busy ? t("等待模型响应…") : t("发送请求后在此查看结果、HTTP 状态与耗时。")}</p>}
    </section>
  </div>;
}
