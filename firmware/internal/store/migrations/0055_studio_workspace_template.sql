-- 0055_studio_workspace_template: 创作工作空间的创作类型（template）。
--   建空间时选定一次、之后不改：决定写进开发者指令的工作规程与 agent/PROJECT.md 的初始骨架
--   （固件内嵌 internal/studio/templates/<template>/）。开发工作空间为空串；存量创作工作空间
--   按「通用」（general）处理。
ALTER TABLE workspaces ADD COLUMN template TEXT NOT NULL DEFAULT '';
UPDATE workspaces SET template = 'general' WHERE kind = 'studio';
