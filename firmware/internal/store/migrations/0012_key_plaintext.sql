-- 0012_key_plaintext: api_keys 封存明文列（2026-08-12 产品决定：已有 Key 属主
-- 可随时复制使用，不再「仅签发时展示一次」）。普通 ALTER TABLE ADD COLUMN，
-- 不带重建标记。
--
-- 列存设备密钥 AES-256-GCM 密文（AAD = "apikey:<key_digest>"，密文钉死在本行
-- 摘要上，挪到别的行解不开）；空串 = 明文未留存。本迁移之前签发的行恒为空串
-- ——库里只有摘要，明文物理上不可恢复，自助复制端点对这些行答 409
-- plaintext_unavailable，出路是重新签发一把。
ALTER TABLE api_keys ADD COLUMN plaintext_sealed TEXT NOT NULL DEFAULT '';
