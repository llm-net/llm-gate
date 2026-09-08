package admin

// 界面入口：根路径与 /admin/ 旧书签。
//
// 界面本体在 internal/ui（`/ui/` 子树，web/ui 的构建产物）。本文件只剩两个
// 重定向：
//
//   - `/`      精确匹配，302 到 /ui/。只匹配根本身，吞不掉任何 API 路径。
//   - `/admin/` 旧管理台的书签。**必须是客户端跳转而不是 302**：老书签形如
//     /admin/#/users，fragment 到不了服务端，302 换不出 hash 里那一段——只有把
//     一张小页面送到浏览器里，由它读 location.hash 再换成 /ui/ 下的新路径。
//     旧 hash 的归一化表与前端 lib/routes.ts 的 LEGACY_HASH 同源；这里多一层
//     兜底，前端那份负责 /ui/#/... 的情形。
//
// /admin/v1* 是 API 命名空间，静态层一律统一 JSON 404、绝不回退 HTML——保住 API
// 的统一错误体语义（TestAuthedUnknownPathIs404）。

import (
	"net/http"
	"strings"
)

// adminRedirectHTML 是 /admin/ 的跳转页。没有外部资源、不需要构建步骤；把 hash
// 里的旧路由换成 /ui/ 下的新路径后 replace 掉历史，浏览器后退键不会弹回本页。
const adminRedirectHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LLM Gate</title></head>
<body>
<p>管理台已并入设备界面，正在跳转…若未自动跳转请打开 <a href="/ui/">/ui/</a>。</p>
<script>
(function () {
  var legacy = {
    "#/assistant": "/model-routing/api",
    "#/agents": "/model-routing/dev-tools",
    "#/models": "/agent-accounts",
    "#/login": "/login"
  };
  var h = window.location.hash;
  var to = legacy[h];
  if (to === undefined && h.indexOf("#/") === 0) to = h.slice(1);
  window.location.replace("/ui" + (to === undefined ? "/" : to));
})();
</script>
</body></html>
`

// handleAdminLegacy 送出旧书签跳转页。/admin/v1 的未知路径仍走统一 JSON 404，
// 不能落到 HTML 上。
func (s *Server) handleAdminLegacy(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/admin/v1") {
		writeError(w, http.StatusNotFound, "not_found", "路径不存在")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminRedirectHTML))
}

// handleRootRedirect 把根路径引到界面。
func (s *Server) handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusFound)
}
