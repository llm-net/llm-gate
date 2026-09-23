-- 创作工作空间的媒体生成能力与对话归档。
--   studio_media_models  一个工作空间里启用的生成模型（显式白名单：没有行 = 这个空间不能生成）
--                        与各自的使用场景说明（写进创作智能体的开发者指令，可空）。
--   studio_chats.archived_at  归档时刻；归档后的对话只能查看，不再接受指令。
-- 生成模型说明不再存进对话行的开发者指令（引擎会话启动时按当前配置渲染），把已有对话里
-- 那一节裁掉：它位于「## 可用的生成模型…」或「## 生成模型」到「## 工作方式」之间。
CREATE TABLE studio_media_models (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    model        TEXT NOT NULL,
    usage        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, model)
);
ALTER TABLE studio_chats ADD COLUMN archived_at TEXT;
UPDATE studio_chats
   SET instructions = substr(instructions, 1, instr(instructions, '## 可用的生成模型') - 1)
                   || substr(instructions, instr(instructions, '## 工作方式'))
 WHERE instr(instructions, '## 可用的生成模型') > 0
   AND instr(instructions, '## 工作方式') > instr(instructions, '## 可用的生成模型');
UPDATE studio_chats
   SET instructions = substr(instructions, 1, instr(instructions, '## 生成模型') - 1)
                   || substr(instructions, instr(instructions, '## 工作方式'))
 WHERE instr(instructions, '## 生成模型') > 0
   AND instr(instructions, '## 工作方式') > instr(instructions, '## 生成模型');
