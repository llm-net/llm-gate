-- 0003_user_profile: 用户昵称与管理员备注（iteration-6 用户基础功能）。
-- 两列都 NOT NULL DEFAULT ''，历史行升级后即为空串（昵称空串表示"显示用户名"）。

-- 昵称：展示名，本人与管理员都可改；用户名（登录标识）建后不可改。
ALTER TABLE users ADD COLUMN nickname TEXT NOT NULL DEFAULT '';

-- 备注：管理员对该用户的标注，只有管理端点可写（本人接口既不回显也不可改）。
ALTER TABLE users ADD COLUMN note TEXT NOT NULL DEFAULT '';
