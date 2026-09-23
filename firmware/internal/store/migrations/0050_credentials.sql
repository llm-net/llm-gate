-- 0050_credentials: 「智能体 → 凭证管理」。
-- 一行是一份交给智能体使用的第三方凭证：kind 是凭证类型（目前只有 git），host 是它属于
-- 哪个站点（git：github.com / gitee.com），username 是那边的账号，secret_sealed 是令牌 /
-- 口令经设备密钥封存的密文（AAD 钉本行 id，密文挪行解不开），secret_hint 只留末 4 位供
-- 界面辨认。同一类型、同一站点、同一账号只留一份。
CREATE TABLE credentials (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('git')),
    name          TEXT NOT NULL DEFAULT '',
    host          TEXT NOT NULL,
    username      TEXT NOT NULL,
    secret_sealed TEXT NOT NULL,
    secret_hint   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (kind, host, username)
);
