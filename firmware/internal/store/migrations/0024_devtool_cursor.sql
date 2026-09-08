-- 0024_devtool_cursor: api_key_devtool_configs 增第四个订阅开关
-- allow_cursor_subscription（Cursor 订阅）。既有行按 DEFAULT 0 视为未勾选，
-- 与缺少配置行时「订阅全关」的语义一致；列形状比照 0023 的三个既有开关。

ALTER TABLE api_key_devtool_configs
    ADD COLUMN allow_cursor_subscription INTEGER NOT NULL DEFAULT 0
        CHECK (allow_cursor_subscription IN (0, 1));
