// 工作空间终端帧（与固件 internal/devd/wire 同一格式）：一条 WebSocket 二进制消息 = 一帧，
// 1 字节类型 + 4 字节大端长度 + 载荷。Data 双向是终端字节；Resize 浏览器 → 主机，
// 载荷 cols、rows 各 uint16；Exit 主机 → 浏览器，载荷是一句可展示的原因。

export const FRAME_DATA = 0;
export const FRAME_RESIZE = 1;
export const FRAME_EXIT = 2;

export function encodeFrame(type: number, payload: Uint8Array): ArrayBuffer {
  const buf = new ArrayBuffer(5 + payload.byteLength);
  const view = new DataView(buf);
  view.setUint8(0, type);
  view.setUint32(1, payload.byteLength);
  new Uint8Array(buf, 5).set(payload);
  return buf;
}

export function encodeResize(cols: number, rows: number): ArrayBuffer {
  const p = new Uint8Array(4);
  new DataView(p.buffer).setUint16(0, cols);
  new DataView(p.buffer).setUint16(2, rows);
  return encodeFrame(FRAME_RESIZE, p);
}

export function decodeFrame(buf: ArrayBuffer): { type: number; payload: Uint8Array } | null {
  if (buf.byteLength < 5) return null;
  const view = new DataView(buf);
  const n = view.getUint32(1);
  if (n !== buf.byteLength - 5) return null;
  return { type: view.getUint8(0), payload: new Uint8Array(buf, 5) };
}
