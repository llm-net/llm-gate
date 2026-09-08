package gateway

// taskprober.go 让 *Server 成为 usage.TaskProber：懒对账（internal/usage 的
// settle.go）要现查厂商任务时，走的就是这里。
//
// 为什么住在 gateway 而不是 usage：厂商适配器、钉死上游的凭据取用、状态词汇
// 与 URL 合并策略全在本包，usage 直接 import 会成环（gateway 已经 import usage
// 挂 Record）。所以由 usage 定接口、本包实现、装配层（cmd/llmgate）把两头接起来——
// **不接就是弃轮询的视频任务永不入账**，唯一的痕迹是启动日志 lazy_settle=false。
//
// 与客户端查询路径（handleVideoGet → refreshVideoTask）共用同一段观测合并与
// 写库代码，只把「怎么回错」换成返回 error：后台协程没有响应可写，也不该把
// 厂商的错误体带出本函数（§15.1：厂商响应内容不进日志、不进账本）。

import (
	"context"
	"fmt"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// TaskTerminal 报告任务状态是否终结（usage.TaskProber）。状态词汇只有本包这
// 一份，懒对账绝不自己复制一套——两份迟早分叉，而账就错在没人看的地方。
func (s *Server) TaskTerminal(status string) bool { return aigcTerminal(status) }

// ProbeTask 现查厂商并把观测写回任务行，返回更新后的行（usage.TaskProber）。
//
// 三条与查询路径不同的处置：
//
//   - **厂商查不到记录（404）归一为 expired 返回，不算错误**：那是「最终用量
//     无从得知」这个**结论**，对账据此记 0 元 + 估算标；当成错误会让这一行
//     每轮重试到 14 天保留期结束。
//   - **其余非 2xx 一律成错**（错误体读尽即弃，不外带）：本轮跳过、留待下轮。
//     连败到阈值由调用方那边进冷却，见 usage/settle.go。
//   - **写库失败不成错**：观测已经在内存行里，金额照样算得出来，而清算本身
//     （SettleAIGCTaskWithUsage）是另一个事务，库真挂了那一步自会失败重来。
//
// ctx 由调用方给（每任务 30s，可取消）：本函数把它一路传到 HTTP 往返与两次
// 库操作上，所以整轮对账不会被一个挂死的厂商拖住。
func (s *Server) ProbeTask(ctx context.Context, task store.AIGCTask) (store.AIGCTask, error) {
	ru, err := s.store.GetRouteUpstreamByID(ctx, task.UpstreamID)
	if err != nil {
		// 上游行已删 / 凭证解不开（设备密钥换过）：现查无从谈起。**不在这里
		// 归一成 expired**——那是「厂商说没有」，而这是「设备打不出去」，
		// 混为一谈会把一笔可能真实发生的消费按 0 元永久钉死。
		return task, fmt.Errorf("任务上游不可用: %w", err)
	}
	ad, ok := videoAdapters[ru.Type]
	if !ok { // 行由适配器写入，类型必有适配器；防御分支
		return task, fmt.Errorf("上游类型 %s 无任务适配器", ru.Type)
	}
	acct := upstream.Account{Name: ru.Name, Type: ru.Type, APIKey: ru.APIKey, BaseURL: ru.BaseURL, EgressMode: ru.EgressMode}

	res, err := s.queryVendorTask(ctx, acct, ad, task.VendorTaskID)
	if err != nil {
		return task, fmt.Errorf("回查厂商任务失败: %w", err)
	}
	obs := res.obs
	switch {
	case res.gone:
		obs = expiredObservation()
	case res.relay != nil:
		// 非 2xx：读尽丢弃（连接可复用）后按失败处理。**错误体一个字节都不
		// 带出去**——它是厂商响应内容，进不了日志也进不了账本（§15.1）。
		status := res.relay.StatusCode
		drainBody(res.relay)
		return task, fmt.Errorf("厂商任务回查返回 %d", status)
	}

	mergeTaskObservation(&task, obs)
	if err := s.persistTaskObservation(ctx, &task); err != nil && ctx.Err() == nil {
		// 写库失败只记日志：清算用的是内存里这份观测，而清算自己那个事务
		// 失败时整笔会留待下一轮。
		s.log.Warn("懒对账写回任务观测失败", "task_id", task.ID, "err", err.Error())
	}
	return task, nil
}
