// Key 分享：把「接入方法」页 /ui/connect 的地址与这把 Key 的明文拼成一段可以直接
// 发给使用者的文本，一次放进剪贴板。
//
// 明文纪律与 key-plaintext.tsx 同款：分享文本只活在这一次点击的调用栈里，剪贴板
// 写不进去时才落到降级窗的组件内存——不进任何持久化状态、不写 URL、不进查询缓存、
// 不落日志。再分享一次就再向服务端解封一次（服务端记一条 key.reveal 审计）。
//
// 地址取浏览器地址栏的 origin：管理员正用它打开本页，那它就是一条确证走得通的
// 地址。设备无从知道局域网里还有没有别的域名指向自己（同 features/access 的
// 「内网域名」判据），所以这里不问服务端、也不编造第二条。

import { useEffect, useRef } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { protocolLabel, TextProtocolSurfaces } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { t } from "@/lib/i18n";
import { toHref } from "@/lib/router";

/** connectURL 是「接入方法」页的完整地址（`<当前 origin>/ui/connect`）。 */
export function connectURL(): string {
  return `${window.location.origin}${toHref("/connect")}`;
}

/**
 * buildShareText 拼出发给使用者的那段文本：接入方法页地址、这把 Key 的明文、
 * 三个协议面的调用地址，外加一句怎么用。空行是给 IM 里粘贴留的段落。
 */
export function buildShareText(plaintext: string): string {
  const origin = window.location.origin;
  return [
    t("LLM Gate 接入信息"),
    "",
    t("接入方法：{url}", { url: connectURL() }),
    t("API 密钥：{key}", { key: plaintext }),
    ...TextProtocolSurfaces.map((surface) =>
      t("{surface}：{url}", { surface: protocolLabel(surface.protocol), url: `POST ${origin}${surface.path}` }),
    ),
    "",
    t(
      "用浏览器打开上面的「接入方法」地址，贴入这把 API 密钥，即可看到全部调用地址、可用模型和开发工具接入命令。",
    ),
  ].join("\n");
}

/**
 * KeyShareDialog 是剪贴板写不进去时的降级窗：把同一段文本摆出来让人手动选中
 * 复制。打开即全选；「复制」按钮再试一次——这一下在用户手势里，多数浏览器能过。
 */
export function KeyShareDialog({
  value,
  onClose,
}: {
  value: string | null;
  onClose: () => void;
}): React.ReactElement {
  const areaRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (value !== null) areaRef.current?.select();
  }, [value]);

  return (
    <Dialog
      open={value !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      {value === null ? null : (
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("分享接入信息")}</DialogTitle>
            <DialogDescription>
              {t("浏览器没能直接写入剪贴板。请选中下面这段手动复制。")}
            </DialogDescription>
          </DialogHeader>
          <Textarea
            ref={areaRef}
            readOnly
            spellCheck={false}
            rows={9}
            value={value}
            className="font-mono text-xs"
            onFocus={(e) => e.currentTarget.select()}
          />
          <p className="text-muted-foreground text-xs">
            {t("这段文本含 API 密钥明文，只发给该用它的人。")}
          </p>
          <DialogFooter>
            <Button
              onClick={() => {
                void copyText(value).then((ok) => {
                  if (ok) {
                    toast(t("接入信息已复制到剪贴板"));
                    onClose();
                  } else {
                    toast.error(t("复制失败，请手动选中复制"));
                  }
                });
              }}
            >
              {t("复制")}
            </Button>
            <Button variant="outline" onClick={onClose}>
              {t("关闭")}
            </Button>
          </DialogFooter>
        </DialogContent>
      )}
    </Dialog>
  );
}
