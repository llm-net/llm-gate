package cloudflared

import (
	"crypto/ed25519"
	"encoding/base64"
)

// 组件清单的信任根：固件内嵌的 Ed25519 公钥表（键是清单签名文件里的 keyId）。
// 对应私钥只存在于维护者本机项目根 gitignored 的 .component-signing-key，用
// firmware/tools/componentsign 签发。轮换时追加新键、保留旧键一到两个固件版本，
// 让还没升级的设备仍能验旧签名；紧急吊销则删键并发固件。
//
// 只有 HTTPS 和 GitHub asset digest 不足以防官网/CDN 发布面被篡改；清单签名是
// 「设备允许装哪个 cloudflared」这一决策的唯一依据（docs-dev/firmware-cloudflare-tunnel.md §7.1）。
var trustedKeys = map[string]string{
	"llmgate-components-2026-08": "Dv1eXOLBuF6nB31Ddk9HdPAc9TdWIpo/yZjsw0HYfWs=",
}

// TrustedKeys 返回内嵌公钥表的解码副本。解不开的条目是打包错误，直接 panic
// ——那只会发生在改坏本文件的构建里，不会出现在设备上。
func TrustedKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(trustedKeys))
	for id, b64 := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("cloudflared: 内嵌公钥 " + id + " 不是合法的 Ed25519 公钥")
		}
		out[id] = ed25519.PublicKey(raw)
	}
	return out
}
