// 「媒体生成」：用一把 API 密钥可用的图像 / 视频生成模型（订阅流量或按量计费）提交一次图像 / 视频生成。
//
// 同一张页面两种身份（MediaView 的 client 入参）：
// - 管理员在 /media 走 /admin/v1/media/*：提交前必须在底部选一把 API 密钥
//   ——生成以它的名义调用平台（能选的模型就是那把密钥可用的那些，用量也记在它
//   头上），但看得到全部任务（含各把 Key 发起的，列表与详情标出发起密钥，也能删除）；
// - Key 持有人在「接入方法」页（/ui/connect）凭 Key 走 /gate-helper/v1/media/*：只能
//   选这把 Key 可用的模型，只看自己的任务；一个可用模型都没有时提交框换成一句说明。
//
// 提交框由设备的能力表（GET …/media/models）驱动：模型、操作、媒体输入位（角色、个数与
// 字节上限、格式）、生成参数（类型、取值、范围）与提示词可否省略全部来自它，页面不写死
// 任何模型名、枚举或取值范围；服务端下发的 label / description / note / reason 已本地化，
// 直接显示。
//
// 这一页**刻意**不走 PageContainer 的整页滚动：顶栏、页头与底部输入区常驻，只有
// 「任务信息」「任务列表」两张卡片各自内部滚动——提交框像即梦 / 可灵那样钉在底部，
// 生成结果在列表里滚，页头与提交框永远不会被滚出视口。页面根用 h-full 撑满外壳
// 给的滚动区，外壳本身因此没有可滚的余量。
import { ArrowUp, Clapperboard, Download, Film, Image as ImageIcon, ImagePlus, KeyRound, LoaderCircle, RefreshCw, Trash2, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from "@/components/ui/select";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, fmtTimeSeconds } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageHeader } from "./page-shell";

type Kind = api.MediaModel["kind"];
type Role = api.MediaInputRole;

// 提交框里挂着的输入媒体：浏览器读成 data URI，只活在页面内存里，随提交进请求体的
// inputs（按角色）；设备把角色折成各平台的形状，任务行只记输入形态。
interface Attachment { name: string; dataUrl: string }
type Inputs = Partial<Record<Role, Attachment[]>>;
const EMPTY_INPUTS: Inputs = {};

// 生成参数：键名即能力表里的参数名；键不在 = 缺省（不带字段、由平台取缺省值）。
type ParamValue = string | number | boolean | string[];
type ParamValues = Record<string, ParamValue>;
const EMPTY_PARAMS: ParamValues = {};

// 角色的展示顺序；能力表里查不到 label 时（模型已不在表里的旧任务）用这里的名字。
const ROLE_ORDER: Role[] = ["source_video", "first_frame", "last_frame", "reference_images"];
function roleFallbackLabel(role: string): string {
  switch (role) {
    case "first_frame": return t("首帧");
    case "last_frame": return t("尾帧");
    case "reference_images": return t("参考图");
    case "source_video": return t("源视频");
    default: return role;
  }
}

// 后端名 → 平台名（品牌名不翻）；不认识的后端原样显示。
function backendLabel(backend: string): string {
  switch (backend) {
    case "grok": return "Grok";
    case "codex": return "Codex";
    case "ark_image":
    case "ark_video": return t("火山方舟");
    case "minimax_video": return "MiniMax";
    default: return backend;
  }
}

function billingLabel(billing: string): string {
  return billing === "metered" ? t("按量计费") : t("订阅流量");
}

function kindLabel(kind: string): string {
  return kind === "video" ? t("视频") : t("图像");
}

// 操作名的兜底叫法：能力表里有这条任务的模型时用表里的 label。
function opFallbackLabel(name: string): string {
  switch (name) {
    case "edit": return t("编辑");
    case "extend": return t("延长");
    case "generate": return t("生成");
    default: return name;
  }
}

function readDataUrl(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
    reader.onerror = () => reject(new Error("read"));
    reader.readAsDataURL(file);
  });
}

// ---- 能力表的读法 ----

// 能力表的格式词 → MIME；表里出现这里没有的格式时按角色猜 image/<格式> 或 video/<格式>。
const FORMAT_MIME: Record<string, string> = { png: "image/png", jpeg: "image/jpeg", jpg: "image/jpeg", webp: "image/webp", gif: "image/gif", mp4: "video/mp4" };
function acceptMimes(spec: api.MediaInputSpec): string[] {
  const family = spec.role === "source_video" ? "video" : "image";
  return [...new Set((spec.formats ?? []).map((f) => FORMAT_MIME[f.toLowerCase()] ?? `${family}/${f.toLowerCase()}`))];
}

function formatsText(spec: api.MediaInputSpec): string {
  return (spec.formats ?? []).map((f) => f.toUpperCase()).join(" / ");
}

// fileAccepted：按 MIME 或扩展名对上该输入位的格式表；表里没给格式时只按角色分图像 / 视频。
function fileAccepted(file: File, spec: api.MediaInputSpec): boolean {
  const mimes = acceptMimes(spec);
  if (mimes.length === 0) return file.type.startsWith(spec.role === "source_video" ? "video/" : "image/");
  if (mimes.includes(file.type)) return true;
  const ext = file.name.toLowerCase().split(".").pop() ?? "";
  return (spec.formats ?? []).some((f) => f.toLowerCase() === ext || (f.toLowerCase() === "jpeg" && ext === "jpg"));
}

// normalizeDataUrl：浏览器按扩展名给不出 MIME、或设备按扩展名给的 content-type 不在格式表里时，
// 把 data URI 头改写成该输入位接受的第一种类型。
function normalizeDataUrl(dataUrl: string, spec: api.MediaInputSpec): string {
  const mimes = acceptMimes(spec);
  const comma = dataUrl.indexOf(",");
  if (mimes.length === 0 || comma < 0) return dataUrl;
  const mime = /^data:([^;,]+)/.exec(dataUrl.slice(0, comma))?.[1] ?? "";
  return mimes.includes(mime) ? dataUrl : `data:${mimes[0]};base64${dataUrl.slice(comma)}`;
}

// tooLarge：单值上限按 data URI 的长度算（与设备受理时同口径）；表里没给上限就不拦。
function tooLarge(dataUrl: string, spec: api.MediaInputSpec): boolean {
  return Boolean(spec.max_bytes) && dataUrl.length > (spec.max_bytes ?? 0);
}

function inputSpecs(op: api.MediaOperation | null): api.MediaInputSpec[] {
  const specs = op?.inputs ?? [];
  return [...specs].sort((a, b) => ROLE_ORDER.indexOf(a.role) - ROLE_ORDER.indexOf(b.role));
}

function attached(inputs: Inputs, role: Role): Attachment[] {
  return inputs[role] ?? [];
}

function hasInputs(op: api.MediaOperation | null, inputs: Inputs): boolean {
  return inputSpecs(op).some((spec) => attached(inputs, spec.role).length > 0);
}

// promptOptional：带了该操作 prompt_optional_with 里任一角色的输入时提示词可省略。
function promptOptional(op: api.MediaOperation, inputs: Inputs): boolean {
  return (op.prompt_optional_with ?? []).some((role) => attached(inputs, role).length > 0);
}

// paramSet：这个参数有没有给值（没给 = 缺省）。
function paramSet(value: ParamValue | undefined): boolean {
  if (value === undefined) return false;
  if (typeof value === "string") return value !== "";
  if (Array.isArray(value)) return value.length > 0;
  return true;
}

// paramValid：给了值的参数在页面就按能力表拦一道（整数区间）；其余搭配由设备裁决。
function paramValid(spec: api.MediaParamSpec, value: ParamValue | undefined): boolean {
  if (!paramSet(value)) return !spec.required;
  if (spec.type !== "integer") return true;
  if (typeof value !== "number" || !Number.isInteger(value)) return false;
  return (spec.min === undefined || value >= spec.min) && (spec.max === undefined || value <= spec.max);
}

// canSubmit：提示词（或可省略）、必填的输入与参数齐了才能发。
function canSubmit(op: api.MediaOperation, prompt: string, inputs: Inputs, params: ParamValues): boolean {
  if (prompt.trim().length === 0 && !promptOptional(op, inputs)) return false;
  if (inputSpecs(op).some((spec) => spec.required && attached(inputs, spec.role).length === 0)) return false;
  return (op.params ?? []).every((spec) => paramValid(spec, params[spec.name]));
}

// buildSubmission 把提交框状态翻成请求体：只带所选操作认的角色与参数，缺省的不带。
function buildSubmission(model: api.MediaModel, op: api.MediaOperation, prompt: string, inputs: Inputs, params: ParamValues, count: number): api.MediaSubmission {
  const sub: api.MediaSubmission = { model: model.id, operation: op.name, prompt };
  const media: api.MediaSubmissionInputs = {};
  for (const spec of inputSpecs(op)) {
    const items = attached(inputs, spec.role).slice(0, Math.max(1, spec.max));
    if (items.length === 0) continue;
    if (spec.role === "reference_images") media.reference_images = items.map((item) => item.dataUrl);
    else media[spec.role] = items[0]?.dataUrl ?? "";
  }
  if (Object.keys(media).length > 0) sub.inputs = media;
  const values: Record<string, unknown> = {};
  for (const spec of op.params ?? []) {
    const value = params[spec.name];
    if (paramSet(value)) values[spec.name] = value;
  }
  if (Object.keys(values).length > 0) sub.params = values;
  if (count > 1) sub.count = count;
  return sub;
}

// ---- 任务行的读法 ----

function jobOperation(models: api.MediaModel[], job: api.MediaJob): api.MediaOperation | undefined {
  return models.find((item) => item.id === job.model)?.operations.find((item) => item.name === (job.operation || "generate"));
}

function opLabel(models: api.MediaModel[], job: api.MediaJob): string {
  return jobOperation(models, job)?.label ?? opFallbackLabel(job.operation || "generate");
}

// paramSpecOf：先在这条任务的模型 / 操作里找参数定义，找不到（模型已不在表里）再按同名参数找。
function paramSpecOf(models: api.MediaModel[], job: api.MediaJob, name: string): api.MediaParamSpec | undefined {
  const own = jobOperation(models, job)?.params?.find((item) => item.name === name);
  if (own) return own;
  for (const model of models) for (const op of model.operations) {
    const hit = op.params?.find((item) => item.name === name);
    if (hit) return hit;
  }
  return undefined;
}

function roleLabelOf(models: api.MediaModel[], job: api.MediaJob | null, role: string): string {
  const own = job ? jobOperation(models, job)?.inputs?.find((item) => item.role === role) : undefined;
  if (own) return own.label;
  for (const model of models) for (const op of model.operations) {
    const hit = op.inputs?.find((item) => item.role === role);
    if (hit) return hit.label;
  }
  return roleFallbackLabel(role);
}

function paramValueText(spec: api.MediaParamSpec | undefined, value: unknown): string {
  if (typeof value === "boolean") return value ? t("开") : t("关");
  if (Array.isArray(value)) return value.map((item) => String(item)).join(", ");
  if (typeof value === "number") return spec?.unit ? `${value} ${spec.unit}` : String(value);
  if (typeof value === "string") return value;
  return JSON.stringify(value);
}

// jobParams 遍历任务行的 params 对象：label 从能力表里查，查不到用键名。
function jobParams(models: api.MediaModel[], job: api.MediaJob): { name: string; label: string; text: string; flag: boolean }[] {
  return Object.entries(job.params ?? {})
    .filter(([, value]) => value !== null && value !== undefined && value !== "")
    .map(([name, value]) => {
      const spec = paramSpecOf(models, job, name);
      return { name, label: spec?.label ?? name, text: paramValueText(spec, value), flag: typeof value === "boolean" };
    });
}

// inputShapeLabel 把任务行的输入形态拼成一句：首帧 · 尾帧 · 参考图 ×2 · 源视频。
function inputShapeLabel(models: api.MediaModel[], job: api.MediaJob): string {
  const shape = (job.inputs ?? {}) as Record<string, number | undefined>;
  return Object.keys(shape)
    .sort((a, b) => ROLE_ORDER.indexOf(a as Role) - ROLE_ORDER.indexOf(b as Role))
    .flatMap((role) => {
      const n = Number(shape[role] ?? 0);
      if (!(n > 0)) return [];
      const label = roleLabelOf(models, job, role);
      return [n > 1 ? t("{name} ×{n}", { name: label, n }) : label];
    })
    .join(" · ");
}

// paramSummary 给任务列表的一行紧凑参数：操作 · 各参数值 · 输入形态。
function paramSummary(models: api.MediaModel[], job: api.MediaJob): string {
  const parts: string[] = [];
  if (job.operation && job.operation !== "generate") parts.push(opLabel(models, job));
  for (const item of jobParams(models, job)) parts.push(item.flag ? `${item.label} ${item.text}` : item.text);
  const shape = inputShapeLabel(models, job);
  if (shape) parts.push(shape);
  return parts.join(" · ");
}

// ThumbShortEdge 与设备缩略图的短边同值：浏览器抓的封面帧也按短边 480 缩小
// 再回传，设备仍会重新解码缩放。
const ThumbShortEdge = 480;

// durationSeconds 是提交到完成的整秒数（不足 1 秒记 1）。
function durationSeconds(from: string, to: string): number {
  const ms = new Date(to).getTime() - new Date(from).getTime();
  return Number.isFinite(ms) ? Math.max(1, Math.round(ms / 1000)) : 0;
}

// isActive：两种尚未到终态的状态，陪等按它裁决。
function isActive(status: string): boolean {
  return status === "running" || status === "queued";
}

// statusLabel 把任务状态翻成人话；running 是设备后台还在生成，queued 是平台侧排队
// （设备后台按间隔向平台查询，页面照常陪等）。
function statusLabel(status: string): string {
  switch (status) {
    case "running": return t("生成中…");
    case "queued": return t("平台排队中");
    case "succeeded": return t("已完成");
    case "failed": return t("失败");
    case "expired": return t("已过期");
    default: return status;
  }
}

function StatusBadge({ status }: { status: string }): React.ReactElement {
  const variant = status === "succeeded" ? "default" : status === "failed" ? "destructive" : status === "running" || status === "queued" ? "secondary" : "outline";
  return <Badge variant={variant} className="h-5 px-1.5 text-[11px]">{(status === "running" || status === "queued") && <LoaderCircle className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}{statusLabel(status)}</Badge>;
}

// useJobWatch 给每个未到终态（running：设备后台生成中；queued：平台排队、设备后台
// 查询中）的任务开一条陪等：服务端一帧一帧回，直到任务到终态。
// 页面刷新后列表里仍在生成的任务也会自动接上；卸载即停，不定时重取。
// 任务在陪等期间被删掉时服务端答 404，那不是错误，静默收场。
function useJobWatch(client: api.MediaClient, tasks: api.MediaJob[], replace: (task: api.MediaJob) => void, removed: React.RefObject<Set<string>>): void {
  const watching = useRef(new Set<string>());
  const cancelled = useRef(false);
  useEffect(() => {
    cancelled.current = false;
    return () => { cancelled.current = true; watching.current.clear(); };
  }, []);
  useEffect(() => {
    for (const task of tasks) {
      if (!isActive(task.status) || watching.current.has(task.id)) continue;
      watching.current.add(task.id);
      void (async () => {
        for (;;) {
          let next: api.MediaJob;
          try {
            next = await client.wait(task.id);
          } catch (err: unknown) {
            if (!cancelled.current && !removed.current.has(task.id)) toast.error(api.errorMessage(err));
            break;
          }
          if (cancelled.current) return;
          if (removed.current.has(task.id)) break;
          replace(next);
          if (!isActive(next.status)) break;
        }
        watching.current.delete(task.id);
      })();
    }
  }, [client, tasks, replace, removed]);
}

// useThumbSources 给每条已有缩略图的任务解析一次 <img> 能用的地址：管理员拿设备的
// …/thumb 端点，Key 持有人凭 Key 取回 Blob 得到对象 URL；卸载时 revoke 全部对象 URL。
// 列表与任务信息只画缩略图（短边 480 的 JPEG），原图 / 原视频只在放大查看时才取。
function useThumbSources(client: api.MediaClient, tasks: api.MediaJob[]): Map<string, string> {
  const [sources, setSources] = useState<Map<string, string>>(() => new Map());
  const pending = useRef(new Set<string>());
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      setSources((current) => {
        for (const url of current.values()) if (url.startsWith("blob:")) URL.revokeObjectURL(url);
        return new Map();
      });
    };
  }, []);
  useEffect(() => {
    for (const task of tasks) {
      if (!task.thumb_file || sources.has(task.id) || pending.current.has(task.id)) continue;
      pending.current.add(task.id);
      client.thumbSource(task).then(
        (url) => {
          pending.current.delete(task.id);
          if (!alive.current || url === "") { if (url.startsWith("blob:")) URL.revokeObjectURL(url); return; }
          setSources((current) => new Map(current).set(task.id, url));
        },
        (err: unknown) => { pending.current.delete(task.id); if (alive.current) toast.error(api.errorMessage(err)); },
      );
    }
  }, [client, tasks, sources]);
  return sources;
}

// captureFrame 在浏览器里给一份结果抓一帧封面：视频取 0.1 秒处的画面，图像整张画上；
// 按短边 480 缩小后编成 JPEG。设备是没有视频解码器的静态二进制，视频封面只能在这里抓。
async function captureFrame(src: string, kind: Kind): Promise<Blob> {
  const draw = (source: CanvasImageSource, width: number, height: number): Promise<Blob> => {
    const scale = Math.min(1, ThumbShortEdge / Math.max(1, Math.min(width, height)));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(width * scale));
    canvas.height = Math.max(1, Math.round(height * scale));
    const ctx = canvas.getContext("2d");
    if (!ctx) return Promise.reject(new Error("canvas"));
    ctx.drawImage(source, 0, 0, canvas.width, canvas.height);
    return new Promise((resolve, reject) => canvas.toBlob((blob) => blob ? resolve(blob) : reject(new Error("toBlob")), "image/jpeg", 0.9));
  };
  if (kind === "image") {
    const img = new Image();
    await new Promise<void>((resolve, reject) => { img.onload = () => resolve(); img.onerror = () => reject(new Error("image")); img.src = src; });
    return draw(img, img.naturalWidth, img.naturalHeight);
  }
  const video = document.createElement("video");
  video.muted = true;
  video.playsInline = true;
  video.preload = "auto";
  try {
    await new Promise<void>((resolve, reject) => {
      const timer = window.setTimeout(() => reject(new Error("timeout")), 60_000);
      video.onerror = () => { window.clearTimeout(timer); reject(new Error("video")); };
      video.onloadedmetadata = () => { video.currentTime = Math.min(0.1, Math.max(0, (video.duration || 1) / 2)); };
      video.onseeked = () => { window.clearTimeout(timer); resolve(); };
      video.src = src;
      video.load();
    });
    return await draw(video, video.videoWidth, video.videoHeight);
  } finally {
    video.removeAttribute("src");
    video.load();
  }
}

// usePosterCapture 给「有结果、但设备还没有缩略图」的任务补封面：一次一条，取回原件
// 抓一帧回传设备，设备缩放保存后回更新后的任务行（其他页面已回传过时也回当前行）。
// 每条任务本次挂载只试一次，失败静默——列表照样能用，只是那一条没有缩略图。
function usePosterCapture(client: api.MediaClient, tasks: api.MediaJob[], replace: (task: api.MediaJob) => void): Set<string> {
  const attempted = useRef(new Set<string>());
  const [capturing, setCapturing] = useState<Set<string>>(() => new Set());
  const queue = useRef<Promise<void>>(Promise.resolve());
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);
  useEffect(() => {
    for (const task of tasks) {
      if (task.thumb_file || !api.mediaJobHasResult(task) || attempted.current.has(task.id)) continue;
      attempted.current.add(task.id);
      setCapturing((current) => new Set(current).add(task.id));
      queue.current = queue.current.then(async () => {
        if (!alive.current) return;
        let src = "";
        try {
          src = await client.mediaSource(task);
          const frame = await captureFrame(src, task.kind);
          if (!alive.current) return;
          replace(await client.uploadThumb(task.id, frame));
        } catch {
          // 抓不到封面（浏览器解不开、任务已删、网络故障）：这一条不画缩略图。
        } finally {
          if (src.startsWith("blob:")) URL.revokeObjectURL(src);
          if (alive.current) setCapturing((current) => { const next = new Set(current); next.delete(task.id); return next; });
        }
      });
    }
  }, [client, tasks, replace]);
  return capturing;
}

// useLightboxSource 只在放大查看时取回原图 / 原视频：管理员拿设备的 …/media 端点，
// Key 持有人凭 Key 取回 Blob；关掉弹框即 revoke 对象 URL。
function useLightboxSource(client: api.MediaClient, task: api.MediaJob | null): { src: string | undefined; loading: boolean } {
  const [state, setState] = useState<{ id: string; src: string } | null>(null);
  useEffect(() => {
    if (!task) return;
    let cancelled = false;
    let url = "";
    client.mediaSource(task).then(
      (resolved) => { url = resolved; if (cancelled) { if (url.startsWith("blob:")) URL.revokeObjectURL(url); return; } setState({ id: task.id, src: url }); },
      (err: unknown) => { if (!cancelled) toast.error(api.errorMessage(err)); },
    );
    return () => { cancelled = true; if (url.startsWith("blob:")) URL.revokeObjectURL(url); setState(null); };
  }, [client, task]);
  if (!task) return { src: undefined, loading: false };
  return state?.id === task.id ? { src: state.src, loading: false } : { src: undefined, loading: true };
}


export function MediaPage(): React.ReactElement {
  return <MediaView client={api.adminMedia} audience="admin" />;
}

// 「结果作为下一次提交的输入」的一个可选动作：把结果挂到 model / op 的 role 输入位上。
interface ReuseAction { role: Role; label: string; model: api.MediaModel; op: api.MediaOperation }

function acceptsRole(op: api.MediaOperation, role: Role): boolean {
  return (op.inputs ?? []).some((item) => item.role === role);
}

// reuseTargets 给一个角色排出可去的（模型，操作）：当前所选的模型 / 操作优先，其次当前模型的
// 其他操作，最后才是别的可用模型——尽量不把人从正在用的形态上带走。
function reuseTargets(models: api.MediaModel[], model: api.MediaModel | null, op: api.MediaOperation | null, role: Role): { model: api.MediaModel; op: api.MediaOperation }[] {
  const out: { model: api.MediaModel; op: api.MediaOperation }[] = [];
  const ordered = [...(model ? [model] : []), ...models.filter((item) => item.available && item.id !== model?.id)];
  for (const item of ordered) {
    const ops = item.id === model?.id && op ? [op, ...item.operations.filter((o) => o.name !== op.name)] : item.operations;
    for (const candidate of ops) if (acceptsRole(candidate, role)) out.push({ model: item, op: candidate });
  }
  return out;
}

// reuseActions：图像结果可作参考图 / 首帧 / 尾帧（每个角色一个去处），视频结果可作源视频
// （去处那个模型里每个收源视频的操作各一个，如编辑、延长）。
function reuseActions(job: api.MediaJob, models: api.MediaModel[], model: api.MediaModel | null, op: api.MediaOperation | null): ReuseAction[] {
  const name = (target: { model: api.MediaModel; op: api.MediaOperation }, role: Role): string => {
    const label = target.op.inputs?.find((item) => item.role === role)?.label ?? roleFallbackLabel(role);
    if (target.model.id !== model?.id) return t("作为{name}（{model} · {op}）", { name: label, model: target.model.id, op: target.op.label });
    if (target.op.name !== op?.name) return t("作为{name}（{op}）", { name: label, op: target.op.label });
    return t("作为{name}", { name: label });
  };
  if (job.kind === "video") {
    const targets = reuseTargets(models, model, op, "source_video");
    const first = targets[0];
    if (!first) return [];
    return targets.filter((item) => item.model.id === first.model.id).map((item) => ({ role: "source_video", label: name(item, "source_video"), ...item }));
  }
  const out: ReuseAction[] = [];
  for (const role of ["reference_images", "first_frame", "last_frame"] as const) {
    const target = reuseTargets(models, model, op, role)[0];
    if (target) out.push({ role, label: name(target, role), ...target });
  }
  return out;
}

export function MediaView({ client, audience }: {
  client: api.MediaClient;
  audience: "admin" | "holder";
}): React.ReactElement {
  const confirm = useConfirm();
  const [tasks, setTasks] = useState<api.MediaJob[]>([]);
  const [modelID, setModelID] = useState("");
  const [opName, setOpName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [inputs, setInputs] = useState<Inputs>(EMPTY_INPUTS);
  const [params, setParams] = useState<ParamValues>(EMPTY_PARAMS);
  const [count, setCount] = useState(1);
  const [sourcing, setSourcing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<api.MediaJob | null>(null);
  const removed = useRef(new Set<string>());
  // 管理员必须选一把 API 密钥：提交以它的名义调用平台（可用模型按它的授权裁决，用量与
  // 并发上限也算它的）。密钥只取 id 与展示串，明文从不经过这一页。
  const [keys, setKeys] = useState<api.ApiKey[]>([]);
  const [keyID, setKeyID] = useState<number | null>(null);
  useEffect(() => {
    if (audience !== "admin") return;
    // 停用的密钥调不动模型，不进选项。
    void api.listKeys()
      .then((v) => setKeys((v.keys ?? []).filter((item) => !item.disabled)))
      .catch((err: unknown) => toast.error(api.errorMessage(err)));
  }, [audience]);
  // 能力表 + 可用性：进页取一次，换密钥即重取（管理面带 key_id；没选密钥时只有能力表，
  // 每项都不可用）。读数记着它属于哪把密钥：新密钥的那份回来之前这一页没有可选模型，
  // 提交按钮也就按不动。
  // Go 的空切片序列化成 null：这里把 models / operations 收成数组，其余可空字段读的时候兜底。
  const catalogRes = useResource(async () => {
    const table = await client.models(keyID);
    return { keyID, running_per_key: table.running_per_key, models: (table.models ?? []).map((item) => ({ ...item, operations: item.operations ?? [] })) };
  }, [client, keyID]);
  const catalog = catalogRes.data !== null && catalogRes.data.keyID === keyID ? catalogRes.data : null;
  const models = useMemo(() => catalog?.models ?? [], [catalog]);
  const runningPerKey = Math.max(1, catalog?.running_per_key ?? 1);
  const model = models.find((item) => item.id === modelID && item.available) ?? null;
  const op = model ? model.operations.find((item) => item.name === opName) ?? model.operations[0] ?? null : null;
  const usable = models.some((item) => item.available);
  // 所选模型不在这把密钥的可用模型里（刚进页、刚换过密钥）：切到第一个能用的，参数与附件一并重置。
  useEffect(() => {
    if (catalog === null || model !== null) return;
    const next = models.find((item) => item.available);
    if (!next) return;
    setModelID(next.id);
    setOpName(next.operations[0]?.name ?? "");
    setInputs(EMPTY_INPUTS);
    setParams(EMPTY_PARAMS);
  }, [catalog, models, model]);
  useEffect(() => { void client.list().then((v) => setTasks(v.jobs ?? [])).catch((err: unknown) => toast.error(api.errorMessage(err))); }, [client]);
  // 换模型 / 换操作：各认自己的输入与参数，切换即清空，避免带着编辑的源视频去生成。
  const changeModel = (id: string) => {
    const next = models.find((item) => item.id === id);
    if (!next || id === model?.id) return;
    setModelID(id); setOpName(next.operations[0]?.name ?? ""); setInputs(EMPTY_INPUTS); setParams(EMPTY_PARAMS);
  };
  const changeOp = (name: string) => { if (name === op?.name) return; setOpName(name); setInputs(EMPTY_INPUTS); setParams(EMPTY_PARAMS); };
  const replace = useMemo(() => (task: api.MediaJob) => { setTasks((current) => current.map((item) => item.id === task.id ? task : item)); }, []);
  useJobWatch(client, tasks, replace, removed);
  const sources = useThumbSources(client, tasks);
  const capturing = usePosterCapture(client, tasks, replace);
  const lightboxSource = useLightboxSource(client, lightbox);
  const selected = tasks.find((item) => item.id === selectedId) ?? tasks[0] ?? null;
  // 管理员的提交框恒在（密钥选择器就在里面）；Key 持有人一个可用模型都没有时换成说明。
  const permitted = audience === "admin" || catalog === null || usable;
  const tries = Math.min(count, runningPerKey);

  // 提交只是把任务交给设备后台（立刻返回 running），生成本身由陪等接力显示；
  // 候选数大于 1 时回来同批的多条任务，全部进列表、各自陪等。
  const submit = async () => {
    const text = prompt.trim();
    if (busy || model === null || op === null || !canSubmit(op, text, inputs, params)) return;
    if (audience === "admin" && keyID === null) return;
    setBusy(true);
    try {
      const body = buildSubmission(model, op, text, inputs, params, tries);
      if (keyID !== null) body.key_id = keyID;
      const accepted = (await client.create(body)).jobs ?? [];
      setTasks((current) => [...accepted, ...current]);
      if (accepted[0]) setSelectedId(accepted[0].id);
      setPrompt("");
      setInputs(EMPTY_INPUTS);
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
      // 可用性在两次读数之间变了（订阅被收回、模型被停用…）：重取一次能力表。
      if (err instanceof api.ApiError && ["subscription_not_allowed", "agent_not_configured", "model_not_found", "key_disabled", "key_archived"].includes(err.code)) catalogRes.reload();
    } finally {
      setBusy(false);
    }
  };
  const refresh = async (id: string) => { try { replace(await client.refresh(id)); } catch (err: unknown) { toast.error(api.errorMessage(err)); } };
  // useAsSource 把一条已完成的结果取回浏览器，作为下一次提交的输入：去处（模型、操作、角色）
  // 由 reuseActions 按能力表给出，不是当前所选的模型 / 操作时先切过去（参数与附件重置）。
  // 取回的是设备上的文件（管理员经内联端点、Key 持有人凭 Key 取 Blob），读成 data URI 后与
  // 手选文件一样只活在页面内存里。
  const useAsSource = async (task: api.MediaJob, action: ReuseAction) => {
    const spec = action.op.inputs?.find((item) => item.role === action.role);
    if (sourcing || !spec) return;
    const same = model?.id === action.model.id && op?.name === action.op.name;
    const base = same ? inputs : EMPTY_INPUTS;
    const existing = attached(base, action.role);
    if (spec.max > 1 && existing.length >= spec.max) { toast.error(t("{name}最多 {n} 个", { name: spec.label, n: spec.max })); return; }
    setSourcing(true);
    let src = "";
    try {
      src = await client.mediaSource(task);
      const blob = await (await fetch(src)).blob();
      const dataUrl = normalizeDataUrl(await readDataUrl(blob), spec);
      if (!dataUrl || tooLarge(dataUrl, spec)) { toast.error(t("结果文件过大，无法作为输入")); return; }
      const item: Attachment = { name: `${task.id.slice(-6)}.${action.role === "source_video" ? "mp4" : "png"}`, dataUrl };
      if (!same) { setModelID(action.model.id); setOpName(action.op.name); setParams(EMPTY_PARAMS); }
      setInputs({ ...base, [action.role]: spec.max > 1 ? [...existing, item] : [item] });
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      if (src.startsWith("blob:")) URL.revokeObjectURL(src);
      setSourcing(false);
    }
  };
  const download = (task: api.MediaJob) => { client.download(task).catch((err: unknown) => toast.error(api.errorMessage(err))); };
  const remove = async (task: api.MediaJob) => {
    if (!(await confirm({ title: t("删除这条预览记录？"), body: <p>{t("将删除设备上的任务记录与已保存的生成结果；仍在生成中的任务其结果将被丢弃。")}</p>, confirmText: t("删除"), danger: true }))) return;
    try {
      await client.remove(task.id);
      removed.current.add(task.id);
      setTasks((current) => current.filter((item) => item.id !== task.id));
      if (selectedId === task.id) setSelectedId(null);
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    }
  };
  const clearAll = async () => {
    if (!(await confirm({ title: t("清空全部预览记录？"), body: <p>{t("将删除设备上全部 {n} 条任务记录及其已保存的生成结果。", { n: tasks.length })}</p>, confirmText: t("清空"), danger: true }))) return;
    try {
      await client.clear();
      for (const task of tasks) removed.current.add(task.id);
      setTasks([]);
      setSelectedId(null);
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    }
  };

  const note = audience === "holder"
    ? t("用你的 API 密钥生成图像或视频。生成结果保存在这台设备上，删除任务记录时一并删除。")
    : t("以选中的 API 密钥名义生成图像或视频。生成结果保存在这台设备上，删除任务记录时一并删除。");
  const actions = selected && api.mediaJobHasResult(selected) ? reuseActions(selected, models, model, op) : [];
  return <div className="flex h-full min-h-0 w-full flex-col gap-3 p-3 sm:p-4">
    <div className="shrink-0">
      <PageHeader compact title={t("媒体生成")} note={note} actions={
        <Button type="button" size="sm" variant="outline" disabled={tasks.length === 0} onClick={() => void clearAll()}><Trash2 aria-hidden="true" />{t("清空记录")}</Button>
      } />
    </div>
    <div className="grid min-h-0 flex-1 grid-rows-[minmax(0,2fr)_minmax(0,3fr)] gap-3 lg:grid-cols-[minmax(17rem,2fr)_minmax(0,3fr)] lg:grid-rows-1">
      <TaskDetail task={selected} src={selected ? sources.get(selected.id) : undefined} capturing={selected ? capturing.has(selected.id) : false} models={models} model={permitted ? model : null} op={op} audience={audience} sourcing={sourcing} actions={actions} onOpen={setLightbox} onDownload={download} onDelete={remove} onRefresh={refresh} onUseAsSource={(task, action) => void useAsSource(task, action)} />
      <TaskList tasks={tasks} models={models} sources={sources} capturing={capturing} selectedId={selected?.id ?? null} audience={audience} onSelect={setSelectedId} onOpen={setLightbox} onDownload={download} onDelete={remove} onRefresh={refresh} />
    </div>
    {permitted
      ? <Composer models={models} model={model} op={op} loading={catalog === null && catalogRes.loading} error={catalogRes.error} onRetry={catalogRes.reload} prompt={prompt} inputs={inputs} params={params} count={tries} maxCount={runningPerKey} busy={busy || sourcing} audience={audience} keys={keys} keyID={keyID} onKey={setKeyID} onModel={changeModel} onOp={changeOp} onPrompt={setPrompt} onInputs={setInputs} onParams={setParams} onCount={setCount} onSubmit={() => void submit()} />
      : <div className="shrink-0 space-y-1 rounded-xl border border-dashed px-3 py-3 text-center text-xs text-muted-foreground">
        <p>{t("这把 API 密钥当前没有可用的图像或视频生成模型，暂不能发起生成任务；请联系设备管理员为这把密钥开通订阅或可用模型。")}</p>
        <UnavailableReasons models={models} />
      </div>}
    <Lightbox task={lightbox} src={lightboxSource.src} loading={lightboxSource.loading} poster={lightbox ? sources.get(lightbox.id) : undefined} onClose={() => setLightbox(null)} onDownload={download} />
  </div>;
}

// UnavailableReasons 逐个列出不可用模型的原因（设备给的 reason，已本地化）。
function UnavailableReasons({ models }: { models: api.MediaModel[] }): React.ReactElement | null {
  const rows = models.filter((item) => !item.available && item.reason);
  if (rows.length === 0) return null;
  return <ul className="space-y-0.5">
    {rows.map((item) => <li key={item.id}><span className="font-mono">{item.id}</span> · {item.reason}</li>)}
  </ul>;
}

// ---- 任务信息 ----

function Field({ label, children, mono = false }: { label: string; children: React.ReactNode; mono?: boolean }): React.ReactElement {
  return <div className="grid grid-cols-[4.5rem_minmax(0,1fr)] gap-x-2 text-xs leading-5"><span className="text-muted-foreground">{label}</span><span className={cn("min-w-0 break-all", mono && "font-mono")}>{children}</span></div>;
}

function TaskDetail({ task, src, capturing, models, model, op, audience, sourcing, actions, onOpen, onDownload, onDelete, onRefresh, onUseAsSource }: {
  task: api.MediaJob | null; src: string | undefined; capturing: boolean; models: api.MediaModel[]; model: api.MediaModel | null; op: api.MediaOperation | null; audience: "admin" | "holder";
  sourcing: boolean; actions: ReuseAction[];
  onOpen: (task: api.MediaJob) => void; onDownload: (task: api.MediaJob) => void; onDelete: (task: api.MediaJob) => void; onRefresh: (id: string) => void;
  onUseAsSource: (task: api.MediaJob, action: ReuseAction) => void;
}): React.ReactElement {
  const shape = task ? inputShapeLabel(models, task) : "";
  return <section className="flex min-h-0 min-w-0 flex-col rounded-xl border bg-card text-card-foreground shadow-sm">
    <header className="flex shrink-0 items-center justify-between gap-2 border-b px-3 py-2"><h2 className="text-sm font-semibold">{t("任务信息")}</h2>{task && <StatusBadge status={task.status} />}</header>
    <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-3 py-3">
      {task === null ? <div className="space-y-2 text-xs text-muted-foreground">
        <p>{t("选定模型后，在页面底部输入提示词。")}</p>
        {model !== null && <>
          <Field label={t("平台")}>{backendLabel(model.backend)}</Field>
          <Field label={t("类型")}>{kindLabel(model.kind)}</Field>
          <Field label={t("模型")} mono>{model.id}</Field>
          {op !== null && <Field label={t("操作")}>{op.label}</Field>}
          <Field label={t("计费")}>{billingLabel(model.billing)}</Field>
        </>}
      </div> : <>
        <TaskMedia task={task} src={src} capturing={capturing} size="large" onOpen={onOpen} />
        {task.error && <p className="rounded-md bg-destructive/10 px-2 py-1.5 text-xs text-destructive">{task.error}</p>}
        {task.status === "expired" && <p className="text-xs text-muted-foreground">{t("平台媒体已过期，请重新生成")}</p>}
        <div className="space-y-1">
          <Field label={t("任务 ID")} mono>{task.id}</Field>
          <Field label={t("平台")}>{backendLabel(task.backend || task.provider || "")}</Field>
          <Field label={t("类型")}>{kindLabel(task.kind)}</Field>
          <Field label={t("模型")} mono>{task.model}</Field>
          <Field label={t("操作")}>{opLabel(models, task)}</Field>
          {shape && <Field label={t("输入")}>{shape}</Field>}
          {jobParams(models, task).map((item) => <Field key={item.name} label={item.label} mono={!item.flag}>{item.text}</Field>)}
          {audience === "admin" && task.key_display && <Field label={t("发起密钥")} mono>{task.key_display}</Field>}
          <Field label={t("提交时间")}>{fmtTimeSeconds(task.created_at)}</Field>
          {task.finished_at && <Field label={t("完成时间")}>{fmtTimeSeconds(task.finished_at)}</Field>}
          {task.finished_at && <Field label={t("耗时")}>{t("{n} 秒", { n: durationSeconds(task.created_at, task.finished_at), count: durationSeconds(task.created_at, task.finished_at) })}</Field>}
        </div>
        {task.prompt
          ? <div className="space-y-1"><p className="text-xs text-muted-foreground">{t("提示词")}</p><p className="rounded-md bg-muted/60 px-2 py-1.5 text-xs leading-5 whitespace-pre-wrap break-words">{task.prompt}</p></div>
          : <p className="text-xs text-muted-foreground">{t("未填提示词，由输入媒体驱动生成")}</p>}
        <div className="flex flex-wrap gap-2">
          {api.mediaJobHasResult(task) && <Button size="sm" variant="outline" onClick={() => onDownload(task)}><Download aria-hidden="true" />{t("下载保存")}</Button>}
          {canRefresh(task) && <Button size="sm" variant="outline" onClick={() => onRefresh(task.id)}><RefreshCw aria-hidden="true" />{t("查询平台任务")}</Button>}
          <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => onDelete(task)}><Trash2 aria-hidden="true" />{t("删除记录")}</Button>
        </div>
        {actions.length > 0 && <div className="space-y-1">
          <p className="text-xs text-muted-foreground">{t("以此结果为下一次提交的输入")}</p>
          <div className="flex flex-wrap gap-2">
            {actions.map((action) => <Button key={`${action.model.id}/${action.op.name}/${action.role}`} size="sm" variant="outline" disabled={sourcing} onClick={() => onUseAsSource(task, action)}>
              {action.role === "source_video" ? <Clapperboard aria-hidden="true" /> : action.role === "reference_images" ? <ImagePlus aria-hidden="true" /> : <Film aria-hidden="true" />}{action.label}
            </Button>)}
            {sourcing && <span className="inline-flex items-center gap-1 text-xs text-muted-foreground"><LoaderCircle className="size-3.5 animate-spin motion-reduce:animate-none" aria-hidden="true" />{t("正在取回结果作为输入…")}</span>}
          </div>
        </div>}
      </>}
    </div>
  </section>;
}

// canRefresh：Grok 视频在平台侧排队时才有「查询平台任务」可按——设备后台已在按间隔
// 查询，这只是不想等下一轮的人工插队。
function canRefresh(task: api.MediaJob): boolean {
  return task.kind === "video" && task.status !== "succeeded" && task.status !== "expired" && task.status !== "running" && task.status !== "failed";
}

// TaskMedia 渲染任务的结果缩略图（图像与视频都是短边 480 的 JPEG，视频角上带胶片
// 标记），点开才在弹框里取原图 / 原视频；缩略图还在解析或浏览器还在抓封面时转圈，
// 没有结果时按状态给占位。
function TaskMedia({ task, src, capturing, size, onOpen }: { task: api.MediaJob; src: string | undefined; capturing: boolean; size: "thumb" | "large"; onOpen: (task: api.MediaJob) => void }): React.ReactElement {
  const thumb = size === "thumb";
  const box = thumb ? "size-16 shrink-0 rounded-md" : "w-full rounded-lg";
  const hasMedia = api.mediaJobHasResult(task);
  if (hasMedia && src) {
    return <button type="button" title={t("点击放大查看")} onClick={() => onOpen(task)} className={cn(box, "group relative cursor-zoom-in overflow-hidden bg-muted/40 outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50")}>
      <img src={src} alt="" className={cn("block", thumb ? "size-full object-cover" : "mx-auto max-h-72 w-auto object-contain transition-transform group-hover:scale-[1.01]")} />
      {task.kind === "video" && <Film className={cn("absolute text-white drop-shadow", thumb ? "right-1 bottom-1 size-3.5" : "right-2 bottom-2 size-5")} aria-hidden="true" />}
    </button>;
  }
  const loading = task.status === "running" || task.status === "queued" || (hasMedia && (capturing || Boolean(task.thumb_file)));
  const icon = loading
    ? <LoaderCircle className={cn("animate-spin motion-reduce:animate-none", thumb ? "size-4" : "size-6")} aria-hidden="true" />
    : task.kind === "video" ? <Film className={thumb ? "size-4" : "size-6"} aria-hidden="true" /> : <ImageIcon className={thumb ? "size-4" : "size-6"} aria-hidden="true" />;
  const placeholder = cn(box, "flex items-center justify-center bg-muted/40 text-muted-foreground", !thumb && "h-24");
  // 有结果却没有缩略图（浏览器也抓不到封面）：仍可点开看原件。
  if (hasMedia) {
    return <button type="button" title={t("点击放大查看")} onClick={() => onOpen(task)} className={cn(placeholder, "cursor-zoom-in outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50")}>{icon}</button>;
  }
  return <div className={placeholder}>{icon}</div>;
}

// ---- 任务列表 ----

function TaskList({ tasks, models, sources, capturing, selectedId, audience, onSelect, onOpen, onDownload, onDelete, onRefresh }: {
  tasks: api.MediaJob[]; models: api.MediaModel[]; sources: Map<string, string>; capturing: Set<string>; selectedId: string | null; audience: "admin" | "holder"; onSelect: (id: string) => void;
  onOpen: (task: api.MediaJob) => void; onDownload: (task: api.MediaJob) => void; onDelete: (task: api.MediaJob) => void; onRefresh: (id: string) => void;
}): React.ReactElement {
  return <section className="flex min-h-0 min-w-0 flex-col rounded-xl border bg-card text-card-foreground shadow-sm">
    <header className="flex shrink-0 items-center justify-between gap-2 border-b px-3 py-2"><h2 className="text-sm font-semibold">{t("任务列表")}</h2><span className="text-xs text-muted-foreground">{t("共 {n} 条", { n: tasks.length })}</span></header>
    <div className="min-h-0 flex-1 overflow-y-auto">
      {tasks.length === 0 ? <p className="px-3 py-6 text-center text-xs text-muted-foreground">{t("还没有预览任务")}</p> : <ul className="divide-y">
        {tasks.map((task) => {
          const summary = paramSummary(models, task);
          return <li key={task.id} className={cn("group flex items-start gap-2.5 px-3 py-2 transition-colors", task.id === selectedId ? "bg-accent/60" : "hover:bg-accent/30")}>
            <TaskMedia task={task} src={sources.get(task.id)} capturing={capturing.has(task.id)} size="thumb" onOpen={onOpen} />
            <button type="button" onClick={() => onSelect(task.id)} className="min-w-0 flex-1 cursor-pointer text-left outline-none">
              <div className="flex items-center gap-2"><span className="min-w-0 truncate font-mono text-xs">{task.model}</span><StatusBadge status={task.status} /></div>
              <p className="line-clamp-1 text-xs text-muted-foreground">{task.prompt || t("（无提示词）")}</p>
              <p className="text-[11px] text-muted-foreground/80">{backendLabel(task.backend || task.provider || "")} · {kindLabel(task.kind)} · {fmtTime(task.created_at)}{summary && <span className="ml-1 text-muted-foreground/70">· {summary}</span>}{audience === "admin" && task.key_display && <span className="ml-1 inline-flex items-center gap-0.5 font-mono" title={t("发起密钥")}><KeyRound className="size-3" aria-hidden="true" />{task.key_display}</span>}</p>
              {task.error && <p className="line-clamp-1 text-xs text-destructive">{task.error}</p>}
            </button>
            <div className="flex shrink-0 items-center gap-0.5 opacity-70 group-hover:opacity-100">
              {api.mediaJobHasResult(task) && <Button size="icon-xs" variant="ghost" title={t("下载保存")} onClick={() => onDownload(task)}><Download aria-hidden="true" /></Button>}
              {canRefresh(task) && <Button size="icon-xs" variant="ghost" title={t("查询平台任务")} onClick={() => onRefresh(task.id)}><RefreshCw aria-hidden="true" /></Button>}
              <Button size="icon-xs" variant="ghost" className="text-destructive hover:text-destructive" title={t("删除记录")} onClick={() => onDelete(task)}><Trash2 aria-hidden="true" /></Button>
            </div>
          </li>;
        })}
      </ul>}
    </div>
  </section>;
}

// ---- 底部输入区 ----

// Composer 是钉在页面底部的提交框：上面一行紧凑的多行输入（Enter 提交、Shift+Enter
// 换行），中间是附件行（所选模型有多个操作时先选操作，再按该操作的输入位挂媒体）与
// 参数行（该操作的参数，逐个按类型出控件），下面一行是模型选择器、候选数、密钥选择器与
// 圆形发送键——即梦、可灵那种「一块输入板」的形状，不再是整张卡片。
function Composer({ models, model, op, loading, error, onRetry, prompt, inputs, params, count, maxCount, busy, audience, keys, keyID, onKey, onModel, onOp, onPrompt, onInputs, onParams, onCount, onSubmit }: {
  models: api.MediaModel[]; model: api.MediaModel | null; op: api.MediaOperation | null; loading: boolean; error: string | null; onRetry: () => void;
  prompt: string; inputs: Inputs; params: ParamValues; count: number; maxCount: number; busy: boolean;
  audience: "admin" | "holder"; keys: api.ApiKey[]; keyID: number | null; onKey: (id: number) => void;
  onModel: (id: string) => void; onOp: (name: string) => void; onPrompt: (v: string) => void; onInputs: (v: Inputs) => void; onParams: (v: ParamValues) => void; onCount: (n: number) => void; onSubmit: () => void;
}): React.ReactElement {
  // 管理员选定密钥前不给提交：这一页没有「不属于任何密钥」的调用。
  const keyPicked = audience !== "admin" || keyID !== null;
  const ready = !busy && keyPicked && model !== null && op !== null && canSubmit(op, prompt, inputs, params);
  const kinds: { key: Kind; label: string }[] = [{ key: "image", label: t("图像") }, { key: "video", label: t("视频") }];
  const acceptsVideo = op !== null && (op.inputs ?? []).some((item) => item.role === "source_video");
  const placeholder = op !== null && promptOptional(op, inputs) ? t("可选：描述帧之间的运动与镜头，Enter 提交，Shift+Enter 换行")
    : op?.name === "edit" ? acceptsVideo ? t("描述要对源视频做的修改，Enter 提交，Shift+Enter 换行") : t("描述要对图像做的修改，Enter 提交，Shift+Enter 换行")
      : op?.name === "extend" ? t("描述视频接下来发生什么，Enter 提交，Shift+Enter 换行")
        : model?.kind === "video" ? t("描述你想生成的视频，Enter 提交，Shift+Enter 换行")
          : t("描述你想生成的图像，Enter 提交，Shift+Enter 换行");
  return <div className="shrink-0 rounded-xl border bg-card shadow-sm transition-[border-color,box-shadow] focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/30">
    <textarea
      value={prompt}
      onChange={(event) => onPrompt(event.target.value)}
      onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); if (ready) onSubmit(); } }}
      placeholder={placeholder}
      rows={2}
      className="field-sizing-content max-h-32 min-h-10 w-full resize-none bg-transparent px-3 pt-2 pb-1 text-sm leading-5 outline-none placeholder:text-muted-foreground"
    />
    {model !== null && op !== null && <AttachmentBar model={model} op={op} inputs={inputs} disabled={busy} onChange={onInputs} onOp={onOp} />}
    {op !== null && <ParamsBar op={op} params={params} disabled={busy} onChange={onParams} />}
    <div className="flex flex-wrap items-center gap-1.5 px-2 pb-2">
      <Select value={model?.id ?? ""} disabled={models.length === 0} onValueChange={onModel}>
        <SelectTrigger size="sm" className="h-7 min-w-0 max-w-[22rem] gap-1 px-2 text-xs" aria-label={t("模型")}><SelectValue placeholder={loading ? t("正在读取可用模型…") : t("选择模型")} /></SelectTrigger>
        <SelectContent position="popper" align="start" className="max-w-[calc(100vw-2rem)]">
          {kinds.map((group) => {
            const rows = models.filter((item) => item.kind === group.key);
            if (rows.length === 0) return null;
            // 不可用的模型灰显并带上设备给的原因；可用的标出计费类别。
            return <SelectGroup key={group.key}>
              <SelectLabel>{group.label}</SelectLabel>
              {rows.map((item) => <SelectItem key={item.id} value={item.id} disabled={!item.available} className="text-xs">
                <span className="font-mono">{item.id}</span>
                <span className="text-muted-foreground">{item.available ? billingLabel(item.billing) : item.reason || billingLabel(item.billing)}</span>
              </SelectItem>)}
            </SelectGroup>;
          })}
        </SelectContent>
      </Select>
      <label className="inline-flex items-center gap-1 text-[11px] text-muted-foreground" title={t("一次提交生成几个候选，各自成为一条任务；每把密钥同时最多 {n} 个未完成的任务。", { n: maxCount })}>
        <span>{t("候选数")}</span>
        <Select value={String(count)} disabled={busy || maxCount <= 1} onValueChange={(v) => onCount(Number(v))}>
          <SelectTrigger size="sm" className="h-7 gap-1 px-1.5 text-xs" aria-label={t("候选数")}><SelectValue /></SelectTrigger>
          <SelectContent>{Array.from({ length: maxCount }, (_, i) => i + 1).map((n) => <SelectItem key={n} value={String(n)} className="text-xs">{n}</SelectItem>)}</SelectContent>
        </Select>
      </label>
      {model?.note && <span className="text-[11px] text-muted-foreground">{model.note}</span>}
      {error !== null && <span className="inline-flex items-center gap-1 text-[11px] text-destructive">{error}<Button type="button" size="xs" variant="ghost" className="h-5 px-1 text-[11px]" onClick={onRetry}>{t("重试")}</Button></span>}
      {audience === "admin" && !keyPicked && <span className="text-[11px] text-muted-foreground">{t("选一把 API 密钥后提交，用量记在它头上")}</span>}
      {audience === "admin" && keyPicked && !loading && error === null && !models.some((item) => item.available) && <span className="text-[11px] text-destructive">{t("这把 API 密钥当前没有可用的图像或视频生成模型")}</span>}
      {audience === "admin" && <Select value={keyID === null ? "" : String(keyID)} onValueChange={(v) => onKey(Number(v))}>
        <SelectTrigger size="sm" className="ml-auto h-7 min-w-0 max-w-[18rem] gap-1 px-2 text-xs" aria-label={t("API 密钥")}>
          <SelectValue placeholder={t("选择 API 密钥")} />
        </SelectTrigger>
        {/* 一行是「标签 + sk_ 掩码」：菜单给足宽度，掩码不换行，标签长了才截断。
            选择器钉在提交框右端，菜单按右对齐弹出并让 Radix 避让边界，窄屏也不出屏。 */}
        <SelectContent position="popper" align="end" className="min-w-[20rem] max-w-[calc(100vw-2rem)]">
          {keys.length === 0
            ? <div className="px-2 py-1.5 text-xs text-muted-foreground">{t("还没有可用的 API 密钥")}</div>
            : keys.map((item) => <SelectItem key={item.id} value={String(item.id)} className="text-xs">
              <span className="min-w-0 flex-1 truncate">{item.label || t("未命名密钥")}</span>
              <span className="shrink-0 font-mono whitespace-nowrap text-muted-foreground">{item.display_prefix}…{item.display_last4}</span>
            </SelectItem>)}
        </SelectContent>
      </Select>}
      <span className={cn("text-[11px] tabular-nums text-muted-foreground", audience !== "admin" && "ml-auto")}>{prompt.length}</span>
      <Button type="button" size="icon-sm" className="rounded-full" disabled={!ready} aria-label={busy ? t("提交中…") : t("提交预览")} title={busy ? t("提交中…") : t("提交预览")} onClick={onSubmit}>
        {busy ? <LoaderCircle className="animate-spin motion-reduce:animate-none" aria-hidden="true" /> : <ArrowUp aria-hidden="true" />}
      </Button>
    </div>
  </div>;
}

// AttachmentBar 是提交框里的媒体输入行：所选模型有多个操作时先选操作，再按该操作在能力表里
// 列出的输入位（首帧 / 尾帧 / 参考图 / 源视频）各给一个添加按钮——上限 1 个的是添加 / 更换，
// 多个的加到上限为止。文件在浏览器读成 data URI，超过字节上限或不在格式表里的在页面就拦下；
// 搭配限制由设备与平台裁决。
function AttachmentBar({ model, op, inputs, disabled, onChange, onOp }: {
  model: api.MediaModel; op: api.MediaOperation; inputs: Inputs; disabled: boolean; onChange: (v: Inputs) => void; onOp: (name: string) => void;
}): React.ReactElement | null {
  const fileRef = useRef<HTMLInputElement>(null);
  const slotRef = useRef<api.MediaInputSpec | null>(null);
  const specs = inputSpecs(op);
  if (specs.length === 0 && model.operations.length <= 1) return null;
  const pick = (spec: api.MediaInputSpec) => {
    const el = fileRef.current;
    if (!el) return;
    slotRef.current = spec;
    el.multiple = spec.max > 1;
    el.accept = acceptMimes(spec).join(",") || (spec.role === "source_video" ? "video/*" : "image/*");
    el.value = "";
    el.click();
  };
  const onFiles = async (list: FileList | null) => {
    const spec = slotRef.current;
    if (!spec || !list || list.length === 0) return;
    const picked: Attachment[] = [];
    for (const file of Array.from(list)) {
      if (!fileAccepted(file, spec)) { toast.error(formatsText(spec) ? t("{name}只接受 {formats} 格式", { name: spec.label, formats: formatsText(spec) }) : t("不支持的文件类型")); continue; }
      let dataUrl: string;
      try { dataUrl = normalizeDataUrl(await readDataUrl(file), spec); } catch { toast.error(t("读取文件失败")); continue; }
      if (!dataUrl) { toast.error(t("读取文件失败")); continue; }
      if (tooLarge(dataUrl, spec)) { toast.error(t("{name}单个不能超过 {n} MiB", { name: spec.label, n: Math.floor((spec.max_bytes ?? 0) / (1 << 20)) })); continue; }
      picked.push({ name: file.name, dataUrl });
    }
    if (picked.length === 0) return;
    if (spec.max <= 1) { onChange({ ...inputs, [spec.role]: picked.slice(0, 1) }); return; }
    const existing = attached(inputs, spec.role);
    if (existing.length + picked.length > spec.max) toast.error(t("{name}最多 {n} 个", { name: spec.label, n: spec.max }));
    onChange({ ...inputs, [spec.role]: [...existing, ...picked].slice(0, spec.max) });
  };
  const chips = specs.flatMap((spec) => attached(inputs, spec.role).map((item, index, all) => ({
    key: `${spec.role}-${index}`,
    label: all.length > 1 || spec.max > 1 ? t("{name} {n}", { name: spec.label, n: index + 1 }) : spec.label,
    item,
    video: spec.role === "source_video",
    remove: () => onChange({ ...inputs, [spec.role]: attached(inputs, spec.role).filter((_, i) => i !== index) }),
  })));
  const required = specs.filter((spec) => spec.required).map((spec) => spec.label);
  const hint = specs.length === 0 ? ""
    : required.length > 0 ? t("此操作必须添加：{names}。", { names: required.join(" / ") })
      : t("媒体输入均为可选。");
  // 每个输入位的限制（个数、单值大小、格式）放在按钮的悬停提示里。
  const limits = (spec: api.MediaInputSpec): string => {
    const parts = [t("最多 {n} 个", { n: spec.max })];
    if (spec.max_bytes) parts.push(t("单个不超过 {n} MiB", { n: Math.floor(spec.max_bytes / (1 << 20)) }));
    if (formatsText(spec)) parts.push(formatsText(spec));
    return parts.join(" · ");
  };
  return <div className="flex flex-col gap-1.5 px-2 pb-1.5">
    <input ref={fileRef} type="file" className="hidden" onChange={(event) => void onFiles(event.target.files)} />
    {chips.length > 0 && <ul className="flex flex-wrap gap-1.5">
      {chips.map((chip) => <li key={chip.key} className="inline-flex items-center gap-1.5 rounded-md border bg-muted/50 py-0.5 pr-1 pl-0.5 text-xs">
        {chip.video
          ? <span className="inline-flex size-7 items-center justify-center rounded-sm bg-muted text-muted-foreground"><Clapperboard className="size-4" aria-hidden="true" /></span>
          : <img src={chip.item.dataUrl} alt="" className="size-7 rounded-sm object-cover" />}
        <span className="text-muted-foreground">{chip.label}</span>
        <span className="max-w-[8rem] truncate">{chip.item.name}</span>
        <button type="button" disabled={disabled} aria-label={t("移除{name}", { name: chip.label })} title={t("移除{name}", { name: chip.label })} onClick={chip.remove} className="inline-flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:bg-muted hover:text-foreground [&_svg]:size-3"><X aria-hidden="true" /></button>
      </li>)}
    </ul>}
    <div className="flex flex-wrap items-center gap-1.5">
      {model.operations.length > 1 && <div className="inline-flex h-6 items-center gap-0.5 rounded-md bg-muted p-0.5" role="group" aria-label={t("操作")}>
        {model.operations.map((item) => <button key={item.name} type="button" disabled={disabled} aria-pressed={op.name === item.name} onClick={() => onOp(item.name)} className={cn("inline-flex h-5 items-center rounded-sm px-2 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-40", op.name === item.name ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground")}>{item.label}</button>)}
      </div>}
      {specs.map((spec) => {
        const n = attached(inputs, spec.role).length;
        const single = spec.max <= 1;
        return <button key={spec.role} type="button" title={limits(spec)} disabled={disabled || (!single && n >= spec.max)} onClick={() => pick(spec)} className="inline-flex h-6 items-center gap-1 rounded-md border border-dashed px-2 text-xs text-muted-foreground transition-colors hover:border-ring hover:text-foreground disabled:cursor-not-allowed disabled:opacity-40 [&_svg]:size-3.5">
          {spec.role === "source_video" ? <Clapperboard aria-hidden="true" /> : <ImagePlus aria-hidden="true" />}
          {single && n > 0 ? t("更换{name}", { name: spec.label }) : t("添加{name}", { name: spec.label })}
          {spec.required && <span className="text-destructive" aria-hidden="true">*</span>}
          {!single && <span className="tabular-nums">{n}/{spec.max}</span>}
        </button>;
      })}
      {hint && !hasInputs(op, inputs) && <span className="text-[11px] text-muted-foreground">{hint}</span>}
    </div>
  </div>;
}

// ParamSelect 是参数行里的小选择器：首项「缺省」——不带字段、由平台取缺省值（必填的参数没有
// 这一项）。Radix Select 不接受空串做项值，用哨兵替代。
const DEFAULT_SENTINEL = "__default__";
function ParamSelect({ value, options, disabled, required, mono = true, label, onChange }: {
  value: string; options: { value: string; label: string }[]; disabled: boolean; required: boolean; mono?: boolean; label: string; onChange: (v: string) => void;
}): React.ReactElement {
  return <Select value={value === "" ? (required ? "" : DEFAULT_SENTINEL) : value} disabled={disabled} onValueChange={(v) => onChange(v === DEFAULT_SENTINEL ? "" : v)}>
    <SelectTrigger size="sm" className={cn("h-6 min-w-0 gap-1 px-1.5 text-xs", mono && value !== "" && "font-mono")} aria-label={label}><SelectValue placeholder={t("请选择")} /></SelectTrigger>
    <SelectContent>
      {!required && <SelectItem value={DEFAULT_SENTINEL} className="text-xs">{t("缺省")}</SelectItem>}
      {options.map((item) => <SelectItem key={item.value} value={item.value} className={cn("text-xs", mono && "font-mono")}>{item.label}</SelectItem>)}
    </SelectContent>
  </Select>;
}

const PARAM_INPUT = "h-6 rounded-md border bg-transparent px-1.5 text-xs outline-none placeholder:text-muted-foreground focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-50";

// StringsInput 是多值参数的输入：逗号或回车分隔，每个值 [a-z0-9_-]{1,64}，至多 max_items 个。
function StringsInput({ spec, values, disabled, onChange }: { spec: api.MediaParamSpec; values: string[]; disabled: boolean; onChange: (v: string[]) => void }): React.ReactElement {
  const [draft, setDraft] = useState("");
  const limit = spec.max_items ?? 0;
  const full = limit > 0 && values.length >= limit;
  const add = (raw: string) => {
    let next = values;
    for (const part of raw.split(/[,，\s]+/)) {
      const id = part.trim().toLowerCase();
      if (!id || next.includes(id)) continue;
      if (!/^[a-z0-9_-]{1,64}$/.test(id)) { toast.error(t("{name}的每个值只能是小写字母、数字、连字符与下划线", { name: spec.label })); return; }
      if (limit > 0 && next.length >= limit) { toast.error(t("{name}最多 {n} 个", { name: spec.label, n: limit })); break; }
      next = [...next, id];
    }
    if (next !== values) onChange(next);
    setDraft("");
  };
  return <span className="inline-flex flex-wrap items-center gap-1">
    {values.map((id) => <span key={id} className="inline-flex items-center gap-0.5 rounded-md border bg-muted/50 px-1.5 py-0.5 font-mono text-xs text-foreground">
      {id}
      <button type="button" disabled={disabled} aria-label={t("移除{name}", { name: id })} title={t("移除{name}", { name: id })} onClick={() => onChange(values.filter((v) => v !== id))} className="inline-flex size-4 items-center justify-center rounded-sm text-muted-foreground hover:bg-muted hover:text-foreground [&_svg]:size-3"><X aria-hidden="true" /></button>
    </span>)}
    {!full && <input
      value={draft}
      disabled={disabled}
      onChange={(event) => setDraft(event.target.value)}
      onKeyDown={(event) => { if ((event.key === "Enter" || event.key === "," || event.key === "，") && !event.nativeEvent.isComposing) { event.preventDefault(); add(draft); } }}
      onBlur={() => add(draft)}
      placeholder={values.length === 0 ? t("缺省；逗号或回车分隔") : t("再加一个")}
      aria-label={spec.label}
      className={cn(PARAM_INPUT, "w-36 font-mono placeholder:font-sans")}
    />}
  </span>;
}

// ParamsBar 是提交框里的参数行，逐个按能力表里的类型出控件：enum → 下拉（首项缺省）；
// integer → 数字输入（min / max，留空缺省，失焦时收进区间）；boolean → 缺省 / 开 / 关三态；
// strings → 多值输入；string → 文本输入。参数的说明放在悬停提示里，必填的带星号。
function ParamsBar({ op, params, disabled, onChange }: {
  op: api.MediaOperation; params: ParamValues; disabled: boolean; onChange: (v: ParamValues) => void;
}): React.ReactElement | null {
  const specs = op.params ?? [];
  if (specs.length === 0) return null;
  const set = (name: string, value: ParamValue | undefined) => {
    const next = { ...params };
    if (paramSet(value)) next[name] = value as ParamValue; else delete next[name];
    onChange(next);
  };
  const control = (spec: api.MediaParamSpec): React.ReactNode => {
    const value = params[spec.name];
    const required = spec.required === true;
    switch (spec.type) {
      case "enum":
        return <ParamSelect label={spec.label} value={typeof value === "string" ? value : ""} disabled={disabled} required={required} options={(spec.values ?? []).map((v) => ({ value: v, label: v }))} onChange={(v) => set(spec.name, v)} />;
      case "boolean":
        return <ParamSelect label={spec.label} value={typeof value === "boolean" ? String(value) : ""} disabled={disabled} required={required} mono={false} options={[{ value: "true", label: t("开") }, { value: "false", label: t("关") }]} onChange={(v) => set(spec.name, v === "" ? undefined : v === "true")} />;
      case "integer": {
        const range = spec.min !== undefined && spec.max !== undefined ? `${spec.min}–${spec.max}` : spec.min !== undefined ? `≥ ${spec.min}` : spec.max !== undefined ? `≤ ${spec.max}` : "";
        const clamp = (n: number) => Math.min(spec.max ?? n, Math.max(spec.min ?? n, n));
        return <>
          <input
            type="number" inputMode="numeric" step={1} min={spec.min} max={spec.max}
            value={typeof value === "number" ? value : ""}
            disabled={disabled}
            aria-invalid={!paramValid(spec, value)}
            onChange={(event) => { const n = Number.parseInt(event.target.value, 10); set(spec.name, Number.isFinite(n) ? n : undefined); }}
            onBlur={() => { if (typeof value === "number" && clamp(value) !== value) set(spec.name, clamp(value)); }}
            placeholder={required ? range : t("缺省")}
            aria-label={spec.label}
            className={cn(PARAM_INPUT, "w-16 tabular-nums aria-invalid:border-destructive")}
          />
          {(range || spec.unit) && <span className="tabular-nums text-muted-foreground/80">{[range, spec.unit].filter(Boolean).join(" ")}</span>}
        </>;
      }
      case "strings":
        return <StringsInput spec={spec} values={Array.isArray(value) ? value : []} disabled={disabled} onChange={(v) => set(spec.name, v)} />;
      default:
        return <input
          value={typeof value === "string" ? value : ""}
          disabled={disabled}
          maxLength={spec.max_length || undefined}
          onChange={(event) => set(spec.name, event.target.value)}
          placeholder={required ? "" : t("缺省")}
          aria-label={spec.label}
          className={cn(PARAM_INPUT, "w-28 font-mono placeholder:font-sans")}
        />;
    }
  };
  return <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-2 pb-1.5">
    {specs.map((spec) => <div key={spec.name} className="inline-flex flex-wrap items-center gap-1 text-[11px] text-muted-foreground" title={spec.description || undefined}>
      <span>{spec.label}{spec.required && <span className="text-destructive" aria-hidden="true"> *</span>}</span>
      {control(spec)}
    </div>)}
  </div>;
}

// ---- 放大查看 ----

// Lightbox 放大看一张图或一段视频：打开时才取回原图 / 原视频（列表与任务信息只有
// 缩略图），取回前先铺缩略图占位并转圈。遵守管理台对话框规则：卡片不设 max-height、
// 不内滚，图片按宽度铺满、超高时整张卡片随遮罩滚动。
function Lightbox({ task, src, loading, poster, onClose, onDownload }: { task: api.MediaJob | null; src: string | undefined; loading: boolean; poster: string | undefined; onClose: () => void; onDownload: (task: api.MediaJob) => void }): React.ReactElement {
  return <Dialog open={task !== null} onOpenChange={(open) => { if (!open) onClose(); }}>
    {task && <DialogContent className="gap-2 bg-background/95 p-2 sm:max-w-[min(94vw,80rem)]">
      <DialogTitle className="sr-only">{t("放大查看")}</DialogTitle>
      <DialogDescription className="sr-only">{task.prompt}</DialogDescription>
      {src
        ? task.kind === "image"
          ? <img src={src} alt="" className="block h-auto w-full rounded-md object-contain" />
          : <video src={src} poster={poster} controls autoPlay className="block h-auto w-full rounded-md bg-black" />
        : <div className="relative flex min-h-48 w-full items-center justify-center overflow-hidden rounded-md bg-muted/40">
          {poster && <img src={poster} alt="" className="block h-auto w-full object-contain opacity-60" />}
          <div className={cn("flex items-center gap-2 text-xs text-muted-foreground", poster && "absolute inset-0 justify-center bg-background/40")}>
            {loading ? <><LoaderCircle className="size-5 animate-spin motion-reduce:animate-none" aria-hidden="true" />{t("正在取回原始文件…")}</> : task.kind === "video" ? <Film className="size-6" aria-hidden="true" /> : <ImageIcon className="size-6" aria-hidden="true" />}
          </div>
        </div>}
      <div className="flex items-center justify-between gap-3 px-1 pb-1">
        <p className="min-w-0 truncate font-mono text-xs text-muted-foreground">{task.id} · {task.model}</p>
        <Button size="sm" variant="outline" onClick={() => onDownload(task)}><Download aria-hidden="true" />{t("下载保存")}</Button>
      </div>
    </DialogContent>}
  </Dialog>;
}
