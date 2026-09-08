// 复制到剪贴板。**两条路都要留着**：设备常以 http:// 直连内网地址访问，那不是
// secure context，`navigator.clipboard` 直接不可用——只留现代 API 等于在真实部署
// 形态下永远复制不了。降级路径是离屏 textarea + execCommand，照搬自管理台 dom.ts。

export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && "clipboard" in navigator) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 降级到选中复制
    }
  }
  const ta = document.createElement("textarea");
  ta.readOnly = true;
  // 离屏但仍可被选中：display:none / visibility:hidden 的元素选不中。
  ta.style.cssText = "position:fixed;top:-9999px;left:-9999px;opacity:0";
  ta.value = text;
  document.body.append(ta);
  ta.select();
  try {
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    ta.remove();
  }
}
