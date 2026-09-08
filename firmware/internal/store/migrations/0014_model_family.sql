-- 0014_model_family: models 表加接口族声明列（2026-08-12 产品决定：AIGC 模型
-- 的接口族在建模时显式声明，不再由第一条来源钉定——H3 这类开源模型今后可能
-- 由多家平台以不同 API 格式承载，「模型名 → 厂商」不成立，族是模型对客户端
-- 的接口承诺，只能声明、不能推断。契约见 docs/firmware-aigc.md）。
--
-- 值是接口族协议名（minimax_video | ark_video | ark_image），text 模型恒空串。
-- 普通 ALTER TABLE ADD COLUMN，不带 CHECK（0013 同款取舍：合法性由管理层
-- 守卫，库层 CHECK 换一次表重建不值得）。建后不可改由「store 层只有带
-- family = '' 守卫的写方法（SetModelFamilyIfUnset）」保证，照 kind 无写方法
-- 的先例。
ALTER TABLE models ADD COLUMN family TEXT NOT NULL DEFAULT '';

-- 存量回填。image 只有一族，无条件声明；video 按既有来源的上游类型回填
-- （写入守卫自 2026-08-09 起保证全模型同族，任取一条判定即可；mock 不服务
-- 任何 AIGC 协议，不参与判定）。无来源的 video 行留空 = 未声明，由第一条
-- 来源懒钉，行为与旧「首源钉族」一致。
UPDATE models SET family = 'ark_image' WHERE kind = 'image';
UPDATE models SET family = 'minimax_video'
 WHERE kind = 'video' AND EXISTS (
       SELECT 1 FROM model_sources s JOIN upstreams u ON u.id = s.upstream_id
        WHERE s.model_id = models.id AND u.type = 'minimax');
UPDATE models SET family = 'ark_video'
 WHERE kind = 'video' AND family = '' AND EXISTS (
       SELECT 1 FROM model_sources s JOIN upstreams u ON u.id = s.upstream_id
        WHERE s.model_id = models.id AND u.type IN ('ark', 'ark_plan'));
