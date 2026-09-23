package mediagen

import (
	"context"
	"errors"
	"net/http"
)

// HTTPError 把内核的错误翻成两组任务端点（管理面与凭 Key 自证）共用的响应：HTTP 状态、错误码
// 与给人看的中文原因。ok 为假表示调用方断开（不必写响应）；认不出的错误是 500
// media_save_failed，由调用方记日志。
func HTTPError(err error) (status int, code, msg string, retryAfterSec int, ok bool) {
	var (
		invalid     *InvalidError
		unavailable *UnavailableError
		rejected    *RejectedError
		busy        *BusyError
	)
	switch {
	case errors.As(err, &busy):
		return http.StatusTooManyRequests, "media_busy", BusyMessage, 0, true
	case errors.As(err, &invalid):
		return http.StatusBadRequest, "media_invalid", invalid.Msg, 0, true
	case errors.As(err, &rejected):
		return http.StatusTooManyRequests, rejected.Code, rejected.Msg, rejected.RetryAfterSec, true
	case errors.As(err, &unavailable):
		status := http.StatusConflict
		switch unavailable.Code {
		case ReasonSubscriptionNotAllowed:
			status = http.StatusForbidden
		case ReasonModelNotFound, "not_found":
			status = http.StatusNotFound
		case ReasonBackendUnavailable:
			status = http.StatusServiceUnavailable
		}
		return status, unavailable.Code, unavailable.Msg, 0, true
	case errors.Is(err, ErrModelNotFound):
		return http.StatusNotFound, ReasonModelNotFound, ErrModelNotFound.Error(), 0, true
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, "media_unavailable", "生成服务不可用", 0, true
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "任务不存在", 0, true
	case errors.Is(err, ErrMediaMissing), errors.Is(err, ErrNotReady):
		return http.StatusNotFound, "media_not_found", "媒体不存在或已过期", 0, true
	case errors.Is(err, ErrMediaExpired):
		return http.StatusGone, "media_expired", "平台媒体已过期，请重新生成", 0, true
	case errors.Is(err, ErrRefreshFailed):
		return http.StatusBadGateway, "media_refresh_failed", "查询平台任务失败", 0, true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 0, "", "", 0, false
	}
	return http.StatusInternalServerError, "media_save_failed", "保存生成任务失败", 0, true
}

// BusyMessage 是并发名额不够时的原因（429 media_busy）。
const BusyMessage = "同时生成的任务过多，请等待现有任务完成"

// BusyError 是 Submit 的并发闸拒绝：这把 Key 已有 Running 个未到终态的任务，再加本次提交的
// 候选数会越过 RunningPerKey。整批不建。
type BusyError struct {
	Running int64
}

func (e *BusyError) Error() string { return BusyMessage }
