package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/i18n"
)

// 管理面的错误体与带 i18n:"text" 标记的响应字段按 Accept-Language 本地化；
// 没有头、或要的是源语言，就是中文原文。译文本身是什么由 internal/i18n 的目录
// 决定，这里只钉「走了那条路」：结果必须等于 i18n.T 的答案。
func TestResponsesFollowAcceptLanguage(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const msg = "请求体不是合法的 JSON 或含未知字段"
	errHandler := s.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
	}))
	jsonHandler := s.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, balanceReport{Message: msg, Name: msg})
	}))

	for _, tc := range []struct {
		header string
		want   string
	}{
		{"", msg},
		{"zh-CN", msg},
		{"en-US,en;q=0.9", i18n.T(i18n.English, msg)},
		{"ja-JP,en;q=0.9", "リクエストボディが有効なJSONではないか、不明なフィールドが含まれています"},
		{"de", msg},
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin/v1/x", nil)
		if tc.header != "" {
			req.Header.Set("Accept-Language", tc.header)
		}
		rr := httptest.NewRecorder()
		errHandler.ServeHTTP(rr, req)
		var eb errorBody
		if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
			t.Fatalf("[%q] decode: %v", tc.header, err)
		}
		if eb.Error.Message != tc.want {
			t.Errorf("[%q] writeError message = %q, want %q", tc.header, eb.Error.Message, tc.want)
		}

		rr = httptest.NewRecorder()
		jsonHandler.ServeHTTP(rr, req)
		var br balanceReport
		if err := json.Unmarshal(rr.Body.Bytes(), &br); err != nil {
			t.Fatalf("[%q] decode: %v", tc.header, err)
		}
		if br.Message != tc.want {
			t.Errorf("[%q] tagged field = %q, want %q", tc.header, br.Message, tc.want)
		}
		if br.Name != msg {
			t.Errorf("[%q] untagged field must stay verbatim, got %q", tc.header, br.Name)
		}
	}

	// 没经过中间件链的 writer（没有 responseRecorder）恒为源语言。
	rr := httptest.NewRecorder()
	writeError(rr, http.StatusBadRequest, "bad_request", msg)
	var eb errorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Message != msg {
		t.Errorf("bare writer: %q", eb.Error.Message)
	}
}
