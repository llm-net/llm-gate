// 与固件 mediagen.ThumbShortEdge / thumbJPEGQuality 同源：短边至多 480，等比缩小、
// 不裁切、不放大小图，透明部分合成白底。浏览器只负责设备未能解码的图像与视频。
const SHORT_EDGE = 480;
const CAPTURE_TIMEOUT_MS = 20_000;
const SEEK_TIMEOUT_MS = 2_000;

function drawPreview(source: CanvasImageSource, width: number, height: number): Promise<Blob | null> {
  if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) return Promise.resolve(null);
  const shrinking = Math.min(width, height) > SHORT_EDGE;
  const targetWidth = shrinking ? (width <= height ? SHORT_EDGE : Math.floor(width * SHORT_EDGE / height)) : width;
  const targetHeight = shrinking ? (height <= width ? SHORT_EDGE : Math.floor(height * SHORT_EDGE / width)) : height;
  // 极端长宽比超过浏览器画布能力时留占位图，避免分配巨幅画布。
  if (Math.max(targetWidth, targetHeight) > 32_768 || targetWidth * targetHeight > 16_777_216) return Promise.resolve(null);
  const canvas = document.createElement("canvas");
  canvas.width = targetWidth;
  canvas.height = targetHeight;
  const release = () => { canvas.width = 0; canvas.height = 0; };
  try {
    const ctx = canvas.getContext("2d");
    if (ctx === null) { release(); return Promise.resolve(null); }
    ctx.fillStyle = "#fff";
    ctx.fillRect(0, 0, targetWidth, targetHeight);
    ctx.imageSmoothingEnabled = true;
    ctx.imageSmoothingQuality = "high";
    ctx.drawImage(source, 0, 0, targetWidth, targetHeight);
    return new Promise((resolve) => {
      try {
        canvas.toBlob((blob) => { release(); resolve(blob); }, "image/jpeg", 0.85);
      } catch {
        release();
        resolve(null);
      }
    });
  } catch {
    release();
    return Promise.resolve(null);
  }
}

/** 有限时、可取消的浏览器解码；卸载时停止读原件，也不回传迟到的编码结果。 */
export function captureImagePreview(src: string, signal: AbortSignal): Promise<Blob | null> {
  if (signal.aborted) return Promise.resolve(null);
  return new Promise((resolve) => {
    const img = new Image();
    let finished = false;
    const done = (blob: Blob | null) => {
      if (finished) return;
      finished = true;
      clearTimeout(timeout);
      signal.removeEventListener("abort", abort);
      img.onload = null;
      img.onerror = null;
      img.removeAttribute("src");
      resolve(blob);
    };
    const abort = () => done(null);
    const timeout = setTimeout(abort, CAPTURE_TIMEOUT_MS);
    signal.addEventListener("abort", abort, { once: true });
    img.onload = () => { void drawPreview(img, img.naturalWidth, img.naturalHeight).then(done); };
    img.onerror = abort;
    img.src = src;
  });
}

/** 优先取开头 0.1 秒，短片取时长一半；不能定位时保留已解码的首帧。 */
export function captureVideoPreview(src: string, signal: AbortSignal): Promise<Blob | null> {
  if (signal.aborted) return Promise.resolve(null);
  return new Promise((resolve) => {
    const video = document.createElement("video");
    video.muted = true;
    video.playsInline = true;
    video.preload = "auto";
    let finished = false;
    let firstFrame: Blob | null = null;
    let seekTimeout: ReturnType<typeof setTimeout> | undefined;
    const done = (blob: Blob | null) => {
      if (finished) return;
      finished = true;
      clearTimeout(timeout);
      clearTimeout(seekTimeout);
      signal.removeEventListener("abort", abort);
      video.onloadeddata = null;
      video.onseeked = null;
      video.onerror = null;
      video.pause();
      video.removeAttribute("src");
      video.load();
      resolve(blob);
    };
    const abort = () => done(null);
    const timeout = setTimeout(() => done(firstFrame), CAPTURE_TIMEOUT_MS);
    signal.addEventListener("abort", abort, { once: true });
    video.onerror = () => done(firstFrame);
    video.onloadeddata = () => {
      video.onloadeddata = null;
      void drawPreview(video, video.videoWidth, video.videoHeight).then((blob) => {
        if (finished) return;
        firstFrame = blob;
        const target = Number.isFinite(video.duration) && video.duration > 0 ? Math.min(0.1, video.duration / 2) : 0;
        if (target === 0) { done(firstFrame); return; }
        video.onseeked = () => {
          video.onseeked = null;
          void drawPreview(video, video.videoWidth, video.videoHeight).then((frame) => done(frame ?? firstFrame));
        };
        seekTimeout = setTimeout(() => done(firstFrame), SEEK_TIMEOUT_MS);
        try { video.currentTime = target; } catch { done(firstFrame); }
      });
    };
    video.src = src;
    video.load();
  });
}
