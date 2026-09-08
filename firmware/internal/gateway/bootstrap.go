package gateway

// bootstrap.go 是 YAML 上游/模型两段的启动导入器（iteration-5 决策 6，
// 决策 6b 改为 seed 语义）：[ImportConfigCatalog] 是唯一的接线入口——
//
//   - **seed 语义**：只有 upstreams 与 models 两表**皆空**时才导入，
//     否则整体跳过并记一条 INFO（reason=bootstrap_skipped）。空库首启（含板上
//     升级后的第一次启动）完成一次性迁移，此后上游与模型全部经管理界面维护：
//     管理台删掉/改名的行不会被下次重启的 YAML 复活。
//   - 代价（已接受）：seed 之后往 YAML 新增上游/模型不再生效，只能经管理台
//     录入；要重新 seed 只能清空这两张表（或重置库）。
//   - 客户端 Key 段不受此约束：仍按摘要幂等导入（iteration-4 决策 7，
//     见 keyauth.go 的 ImportConfigKeys），两者在 cmd/llmgate 里定序执行。
//
// seed 时的转换口径：
//
//   - upstreams 按 name 建行（api_key 明文经设备密钥封存入库）。
//   - logical_models 自动转换：模型名取 upstream_model_id（键名即历史逻辑名，
//     丢弃），同名跨上游合并为同一模型的多条来源，(模型, 上游) 重复即跳过，
//     protocol 字段忽略（可服务入口由来源上游的 type 在运行时决定，决策 4）。
//
// [ImportConfigUpstreams] 与 [ImportConfigModels] 是不带 seed 闸门的底层导入
// 原语（各自仍按名字幂等，可安全重跑），供 ImportConfigCatalog 与测试调用；
// 接线一律走 ImportConfigCatalog，别绕过闸门。
//
// §15.1 纪律：本文件的日志只记条数与名称（模型名、上游账户名），绝不记
// api_key 物料；上游凭证明文只作为 store.CreateUpstream 的入参出现一次，
// 入库即为设备密钥封存的密文。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// ImportConfigCatalog 是 YAML 上游/模型两段的 seed 导入闸门（决策 6b）：
// 两表皆空才导入（上游先于模型——来源行要引用上游行），否则整体跳过并记一条
// INFO。两段都为空时无事可做，也不记日志。
func ImportConfigCatalog(ctx context.Context, st *store.Store, ups []config.Upstream,
	bindings map[string]config.Binding, logger *slog.Logger) error {

	if len(ups) == 0 && len(bindings) == 0 {
		return nil
	}
	existingUps, err := st.ListUpstreams(ctx)
	if err != nil {
		return fmt.Errorf("导入 YAML 上游/模型: %w", err)
	}
	existingModels, err := st.ListModelsWithSources(ctx)
	if err != nil {
		return fmt.Errorf("导入 YAML 上游/模型: %w", err)
	}
	if len(existingUps) > 0 || len(existingModels) > 0 {
		// 库里已有上游或模型：YAML 这张一次性 seed 表不再参与，避免管理台的
		// 删除/改名被下次重启复活（决策 6b）。
		logger.Info("YAML 上游/模型段已跳过（库内已有数据，seed 只在空库执行）",
			"reason", "bootstrap_skipped",
			"db_upstreams", len(existingUps),
			"db_models", len(existingModels),
			"yaml_upstreams", len(ups),
			"yaml_logical_models", len(bindings),
		)
		return nil
	}
	if _, _, err := ImportConfigUpstreams(ctx, st, ups, logger); err != nil {
		return err
	}
	_, _, _, err = ImportConfigModels(ctx, st, bindings, logger)
	return err
}

// priorityStep 是导入时相邻来源的优先级步长：全新模型的第 n 条来源取 n*100，
// 留出手工插队的空隙（管理台可改任意整数，小者优先）。
const priorityStep = 100

// ImportConfigUpstreams 把 YAML upstreams 幂等导入 SQLite（seed 闸门在
// [ImportConfigCatalog]，本函数不判空库）。逐条按 name 查重：同名已存在
// （含已停用——停用状态不被导入复活）整条跳过；缺失则建行，api_key 明文经设备
// 密钥封存入库。返回新建与跳过条数；日志只记条数。
func ImportConfigUpstreams(ctx context.Context, st *store.Store, ups []config.Upstream, logger *slog.Logger) (imported, skipped int, err error) {
	if len(ups) == 0 {
		return 0, 0, nil
	}
	existing, err := st.ListUpstreams(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("导入 upstreams: %w", err)
	}
	known := make(map[string]struct{}, len(existing))
	for _, u := range existing {
		known[u.Name] = struct{}{}
	}

	for i, u := range ups {
		if _, dup := known[u.Name]; dup {
			skipped++
			continue
		}
		if _, err := st.CreateUpstream(ctx, u.Name, u.Type, u.APIKey, u.BaseURL); err != nil {
			if errors.Is(err, store.ErrConflict) {
				skipped++ // 同名行已被并发落库（防御分支）：视同已存在
				continue
			}
			// store 的错误只含约束名与列名，不回显参数值（凭证不会出现在里面）。
			return imported, skipped, fmt.Errorf("导入 upstreams[%d]（%s）: %w", i, u.Name, err)
		}
		known[u.Name] = struct{}{}
		imported++
	}
	logger.Info("YAML upstreams 已导入 SQLite",
		"imported", imported, "skipped", skipped, "total", len(ups))
	return imported, skipped, nil
}

// ImportConfigModels 把 YAML logical_models 转换并幂等导入 SQLite（seed 闸门
// 在 [ImportConfigCatalog]，本函数不判空库）：模型名取 upstream_model_id
// （客户端可见的原始名），同名跨上游合并为多来源，(模型, 上游) 已存在即跳过。
// 来源的 upstream_model_id 留空——空即"与模型名相同"，而模型名正是转换的来源。
//
// 优先级从该模型现有来源的最高值往后排：全新模型得 100/200/300…，往已导入的
// 模型追加新来源时接在其后（如现有最大 100 → 新来源 200）。绝不与既有优先级
// 相同，也绝不把新来源插到管理台已排好的顺序前面——YAML 这张一次性导入表
// 无权重排故障切换链。
//
// 段内顺序取逻辑模型名的字典序：YAML 映射解码进 map 后原始行序已不可得，
// 字典序是唯一可复现的确定序（同一份 YAML 反复启动得到同样的优先级）。
//
// 必然成立的不变式：sourcesCreated + skipped == len(bindings)。
func ImportConfigModels(ctx context.Context, st *store.Store, bindings map[string]config.Binding, logger *slog.Logger) (modelsCreated, sourcesCreated, skipped int, err error) {
	if len(bindings) == 0 {
		return 0, 0, 0, nil
	}
	upstreamIDs, err := upstreamIDsByName(ctx, st)
	if err != nil {
		return 0, 0, 0, err
	}
	index, err := modelIndex(ctx, st)
	if err != nil {
		return 0, 0, 0, err
	}

	// 第一遍：按确定序展开成「模型名 → 有序去重的上游名列表」，同时把无法
	// 解析的条目就地记为跳过，保证第二遍只做建行。
	logicalNames := make([]string, 0, len(bindings))
	for name := range bindings {
		logicalNames = append(logicalNames, name)
	}
	slices.Sort(logicalNames)

	var modelOrder []string // 模型名首次出现序，决定建行顺序（进而决定 id 序）
	grouped := make(map[string][]string, len(bindings))
	for _, logical := range logicalNames {
		b := bindings[logical]
		model, upName := b.UpstreamModelID, b.Upstream
		if _, ok := upstreamIDs[upName]; !ok {
			// 防御分支：config 校验保证 binding 的上游在同一份 YAML 里有定义，
			// 而 ImportConfigUpstreams 先于本函数执行，所以经 config.Load 来的
			// 配置走不到这里。真出现引用不上的上游也只跳过该条、不让 gatewayd
			// 起不来——库里的现状优先于 YAML 这张一次性导入表。
			logger.Warn("跳过 logical_models 条目：上游不在库中",
				"logical_model", logical, "upstream", upName)
			skipped++
			continue
		}
		if _, seen := grouped[model]; !seen {
			modelOrder = append(modelOrder, model)
		} else if slices.Contains(grouped[model], upName) {
			skipped++ // 同一 (模型, 上游) 的另一条 binding（多为同模型双协议）：合并
			continue
		}
		grouped[model] = append(grouped[model], upName)
	}

	// 第二遍：建模型行与来源行。
	for _, model := range modelOrder {
		state, ok := index[model]
		if !ok {
			// YAML seed 只描述文本对话面（upstreams/logical_models 是
			// chat/messages 的历史入口），导入的模型恒为 text。目录价恒为
			// 空（未定价）：价格只住在 DB、由管理台录入，给只跑一次的 seed
			// 路径加价格字段等于给它加一份长期契约（迭代 9 决策）。
			m, err := st.CreateModel(ctx, model, store.ModelKindText, "")
			if err != nil {
				return modelsCreated, sourcesCreated, skipped, fmt.Errorf("导入模型 %s: %w", model, err)
			}
			state = &modelState{id: m.ID, sources: map[int64]bool{}}
			index[model] = state
			modelsCreated++
		}
		for _, upName := range grouped[model] {
			upstreamID := upstreamIDs[upName]
			if state.sources[upstreamID] {
				skipped++
				continue
			}
			// 接在该模型现有来源之后，逐条推进梯级（见函数注释）。
			state.maxPriority += priorityStep
			if _, err := st.CreateModelSource(ctx, state.id, upstreamID, "", state.maxPriority); err != nil {
				if errors.Is(err, store.ErrConflict) {
					// 唯一性或外键冲突：这条来源已存在，或它的模型/上游行刚被删。
					// 两者都记为跳过、继续导入其余条目——重启即修复，
					// 不因一条来源让 gatewayd 起不来。
					skipped++
					continue
				}
				return modelsCreated, sourcesCreated, skipped,
					fmt.Errorf("导入模型 %s 的来源（上游 %s）: %w", model, upName, err)
			}
			state.sources[upstreamID] = true
			sourcesCreated++
		}
	}
	logger.Info("YAML logical_models 已转换导入 SQLite",
		"models_created", modelsCreated, "sources_created", sourcesCreated,
		"skipped", skipped, "total", len(bindings))
	return modelsCreated, sourcesCreated, skipped, nil
}

// modelState 是导入过程中一个模型的库内现状：行 id、已挂来源的上游 id 集合
// （对应 model_sources 的 UNIQUE(model_id, upstream_id)），以及现有来源的最大
// 优先级——新来源从它往后排，故导入过程中随建行推进。
type modelState struct {
	id          int64
	maxPriority int64
	sources     map[int64]bool
}

// upstreamIDsByName 取库内上游的 name → id 映射（含已停用的：停用只影响选路，
// 不影响"这个名字已被占用/可被引用"）。
func upstreamIDsByName(ctx context.Context, st *store.Store) (map[string]int64, error) {
	ups, err := st.ListUpstreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("导入 logical_models: %w", err)
	}
	ids := make(map[string]int64, len(ups))
	for _, u := range ups {
		ids[u.Name] = u.ID
	}
	return ids, nil
}

// modelIndex 取库内全部模型的现状，按模型名索引（见 modelState）。
func modelIndex(ctx context.Context, st *store.Store) (map[string]*modelState, error) {
	models, err := st.ListModelsWithSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("导入 logical_models: %w", err)
	}
	index := make(map[string]*modelState, len(models))
	for _, m := range models {
		state := &modelState{id: m.ID, sources: make(map[int64]bool, len(m.Sources))}
		for _, s := range m.Sources {
			state.sources[s.UpstreamID] = true
			state.maxPriority = max(state.maxPriority, s.Priority)
		}
		index[m.Name] = state
	}
	return index, nil
}
