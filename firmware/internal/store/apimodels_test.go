package store

// api_key_api_model_configs / api_key_api_models（单把 API 密钥经 API 调用可用
// 的模型范围）的 store 层可执行验收。admin 层的整份替换契约与审计在
// internal/admin/apimodels_test.go，数据面的闸门在 internal/gateway，
// 这里只钉仓储行为：
//
//   - 缺配置行 = 不限制的正常空策略，不是错误；Key 不存在才 ErrNotFound。
//   - restricted 与模型集合整份替换；等值保存不写盘、revision 不虚增。
//   - 数据面读数（LookupKeyAPIModelScope）：不限制时恒放行，限制时只放行
//     选中的模型；restricted=1 且选择为空 = 一个都不放行。
//   - 模型删除随外键清掉选择（策略里不留悬空 id）。

import (
	"context"
	"errors"
	"testing"
)

func TestKeyAPIModelConfigRoundTrip(t *testing.T) {
	s, _ := mustOpen(t)
	ctx := context.Background()
	key := mustKey(t, s, "digest-apimodels-1")
	up, err := s.CreateUpstream(ctx, "deepseek", "deepseek", "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateModel(ctx, "model-a", ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateModel(ctx, "model-b", ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateModelSource(ctx, first.ID, up.ID, "", 10); err != nil {
		t.Fatal(err)
	}

	// Key 存在、无配置行：不限制，revision=0，选择为空。
	cfg, err := s.GetKeyAPIModelConfig(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetKeyAPIModelConfig: %v", err)
	}
	if cfg.Restricted || cfg.Revision != 0 || len(cfg.ModelIDs) != 0 {
		t.Errorf("空策略应不限制: %+v", cfg)
	}
	scope, err := s.LookupKeyAPIModelScope(ctx, key.ID)
	if err != nil {
		t.Fatalf("LookupKeyAPIModelScope: %v", err)
	}
	if scope.Restricted || !scope.Allows(first.ID) || !scope.Allows(second.ID) {
		t.Errorf("缺省应放行全部模型: %+v", scope)
	}

	// Key 不存在才是 ErrNotFound（读写两侧同一口径）。
	if _, err := s.GetKeyAPIModelConfig(ctx, key.ID+999); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 Key 应 ErrNotFound, 得到 %v", err)
	}
	if _, _, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{KeyID: key.ID + 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 Key 应 ErrNotFound, 得到 %v", err)
	}

	// 首份配置（INSERT 路）：revision 从 1 开始，重复 id 折成一条。
	saved, changed, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{
		KeyID: key.ID, Restricted: true, ModelIDs: []int64{first.ID, first.ID},
	})
	if err != nil {
		t.Fatalf("ReplaceKeyAPIModelConfig: %v", err)
	}
	if !changed || saved.Revision != 1 || !saved.Restricted || len(saved.ModelIDs) != 1 || saved.ModelIDs[0] != first.ID {
		t.Fatalf("首份配置未按变更落库: changed=%v %+v", changed, saved)
	}

	// 数据面读数按选择收窄。
	scope, err = s.LookupKeyAPIModelScope(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Restricted || !scope.Allows(first.ID) || scope.Allows(second.ID) {
		t.Errorf("限制态放行口径不符: %+v", scope)
	}

	// 等值保存：不算变更、revision 不虚增。
	again, changed, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{
		KeyID: key.ID, Restricted: true, ModelIDs: []int64{first.ID},
	})
	if err != nil {
		t.Fatalf("等值保存: %v", err)
	}
	if changed || again.Revision != 1 {
		t.Errorf("等值保存不该动 revision: changed=%v revision=%d", changed, again.Revision)
	}

	// 只翻开关也是一次变更；关掉限制后模型集合不再参与放行判定。
	next, changed, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{
		KeyID: key.ID, Restricted: false, ModelIDs: []int64{first.ID},
	})
	if err != nil {
		t.Fatalf("翻开关: %v", err)
	}
	if !changed || next.Revision != 2 || next.Restricted {
		t.Fatalf("翻开关未生效: changed=%v %+v", changed, next)
	}
	scope, err = s.LookupKeyAPIModelScope(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Restricted || !scope.Allows(second.ID) {
		t.Errorf("关掉限制后应恒放行: %+v", scope)
	}

	// restricted=1 且选择为空：一个都不放行（合法表达，不是缺省态）。
	if _, _, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{KeyID: key.ID, Restricted: true}); err != nil {
		t.Fatal(err)
	}
	scope, err = s.LookupKeyAPIModelScope(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Restricted || scope.Allows(first.ID) {
		t.Errorf("空选择应一个都不放行: %+v", scope)
	}

	// 模型删除随外键清掉选择。
	if _, _, err := s.ReplaceKeyAPIModelConfig(ctx, KeyAPIModelConfig{
		KeyID: key.ID, Restricted: true, ModelIDs: []int64{first.ID, second.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetKeyAPIModelConfig(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ModelIDs) != 1 || got.ModelIDs[0] != first.ID {
		t.Errorf("删模型后选择应同步清理: %+v", got.ModelIDs)
	}
}
