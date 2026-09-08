// /admin/v1 传输层。契约由服务端钉死，逐条照搬自管理台 web/admin/src/api.ts：
// - 变更请求必须带 X-LlmGate-CSRF: 1 且 Content-Type: application/json（CSRF
//   三重简版；对无 body 的 logout、DELETE 同样成立）；
// - 错误体统一 {"error":{"code","message"}}，message 已按本次请求的 Accept-Language
//   本地化（每个请求都带当前界面语言，服务端 internal/i18n 翻），可直接展示；
// - 会话在 HttpOnly Cookie 里，JS 永不接触令牌；401 且 code=unauthorized 表示
//   会话失效，经统一回调跳登录（登录错误的 401 code 不同，不触发跳转）；
//   403 code=forbidden 是当前角色够不着的管理端点，当普通错误展示。
//
// 请求超时：**一概不设**。firmwareDownload 那条服务端会同步陪等最多 60 秒再
// 返回快照，任何默认超时都会把正常下载误报成失败。

import { currentLang, t } from "@/lib/i18n";

interface ErrorBody {
  error?: { code?: string; message?: string };
}

// ApiError 承载统一错误体；message 已本地化，界面可直接展示。
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

// errorMessage 把任意抛出物转成可展示文案（非 ApiError 属代码缺陷，给通用文案）。
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return t("发生未知错误，请重试");
}

let sessionExpired: (() => void) | null = null;

// setSessionExpiredHandler 注册会话失效回调（api 层不认识路由，由主程序
// 注册「跳登录」动作）。
export function setSessionExpiredHandler(fn: () => void): void {
  sessionExpired = fn;
}

export async function request<T>(
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE",
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { "Accept-Language": currentLang() };
  const init: RequestInit = { method, credentials: "same-origin", headers };
  if (method !== "GET") {
    headers["X-LlmGate-CSRF"] = "1";
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body ?? {});
  }
  let resp: Response;
  try {
    resp = await fetch(path, init);
  } catch {
    throw new ApiError(0, "network", t("无法连接服务器，请检查设备与网络后重试"));
  }
  if (resp.status === 204) {
    return undefined as unknown as T;
  }
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // 非 JSON 响应（网关层错误等），落入下方通用文案。
  }
  if (!resp.ok) {
    const eb = (data ?? {}) as ErrorBody;
    const code = eb.error?.code ?? "unknown";
    const message = eb.error?.message ?? t("请求失败（HTTP {status}）", { status: resp.status });
    if (resp.status === 401 && code === "unauthorized") sessionExpired?.();
    throw new ApiError(resp.status, code, message);
  }
  return data as T;
}

// 客户端 API 密钥要进 HTTP 头（Authorization: Bearer），所以贴进来的东西必须先
// 收窄成头值放得下的形状：整串可见 ASCII（0x21–0x7E）、长度不超过 keyMaxLen。
//
// 两个理由，缺一不可：
//  ① 不把未经检查的用户输入拼进请求头。浏览器自己会拒掉 CR/LF，但「靠运行时替我
//     兜底」不是纪律——头值的形状该由发起方保证。
//  ② 错误要说人话。把一整段文字粘进来时，fetch 会因为值里有非 Latin-1 码位或控制符
//     直接抛 TypeError，被 catch 成「无法连接服务器」——那是把「这不是一把 Key」
//     误报成设备或网络故障，会让人去查根本没坏的东西。
//
// 上限取 512：现行签发格式是 `sk_` + 43 位 base62（46 字符），启动导入表允许管理员
// 自带任意非空串，512 给它留足余量，同时挡住把整份文档粘进来撑爆请求头的输入。
const keyMaxLen = 512;

function wellFormedKey(key: string): boolean {
  if (key === "" || key.length > keyMaxLen) return false;
  for (let i = 0; i < key.length; i += 1) {
    const c = key.charCodeAt(i);
    if (c < 0x21 || c > 0x7e) return false;
  }
  return true;
}

// keyRequest 是凭 API 密钥自证的只读请求，只打数据面 /gate-helper/v1/* 那两条端点
// （「接入方法」页 /ui/connect 用）。与 request 的三处不同：
// - 凭据是调用方逐次传入的 Key，走 Authorization: Bearer；恒不带 Cookie
//   （credentials: "omit"），与管理员会话完全无关；
// - 错误体是数据面的 OpenAI 形 {"error":{"message","type","code"}}，message 是给
//   程序看的英文；ApiError 照带，界面按 status/code 自己给中文；
// - 401 在这里是「Key 无效或已停用」，**不是**会话失效——绝不触发 sessionExpired
//   回调（那会把贴 Key 的人送去管理员登录页）。
// Key 先过上面的 wellFormedKey 再进头：形状不对当场拒绝，不发请求。
// Key 不进 URL、不进日志；本函数不缓存任何东西。
export async function keyRequest<T>(path: string, key: string): Promise<T> {
  if (!wellFormedKey(key)) {
    // 形状就不对，不发请求。status 0 沿用「没走到服务端」那一档，message 已是中文
    // 可直接展示；code 与网络故障分开，免得把「粘错了东西」说成设备或网络坏了。
    throw new ApiError(
      0,
      "invalid_key_format",
      t("这不像一把 API 密钥：请只粘贴密钥本身，不要带空格、换行或其他内容。"),
    );
  }
  let resp: Response;
  try {
    resp = await fetch(path, {
      method: "GET",
      credentials: "omit",
      cache: "no-store",
      headers: { Authorization: `Bearer ${key}`, "Accept-Language": currentLang() },
    });
  } catch {
    throw new ApiError(0, "network", t("无法连接服务器，请检查设备与网络后重试"));
  }
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // 非 JSON 响应落入下方通用文案。
  }
  if (!resp.ok) {
    const eb = (data ?? {}) as ErrorBody;
    const code = eb.error?.code ?? "unknown";
    const message = eb.error?.message ?? t("请求失败（HTTP {status}）", { status: resp.status });
    throw new ApiError(resp.status, code, message);
  }
  return data as T;
}

// requestBinary 是唯一的非 JSON 变更请求通道：体是裸二进制
// （application/octet-stream），CSRF 头、错误解码与 401 回调与 request 同纪律。
// 固件包上传走它——request 把体写死成 JSON，套不进去。
export async function requestBinary<T>(path: string, body: Blob): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        "X-LlmGate-CSRF": "1",
        "Content-Type": "application/octet-stream",
        "Accept-Language": currentLang(),
      },
      body,
    });
  } catch {
    throw new ApiError(0, "network", t("无法连接服务器，请检查设备与网络后重试"));
  }
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // 非 JSON 响应落入下方通用文案。
  }
  if (!resp.ok) {
    const eb = (data ?? {}) as ErrorBody;
    const code = eb.error?.code ?? "unknown";
    const message = eb.error?.message ?? t("请求失败（HTTP {status}）", { status: resp.status });
    if (resp.status === 401 && code === "unauthorized") sessionExpired?.();
    throw new ApiError(resp.status, code, message);
  }
  return data as T;
}
