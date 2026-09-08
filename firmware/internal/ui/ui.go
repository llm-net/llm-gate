// Package ui 承载设备界面的静态资源：web/ui 的 vite 构建产物入库在 uidist/，
// 经 go:embed 打进二进制——与 internal/admin 同规矩，go build 绝不依赖 Node，
// 重建产物用 `make web`（一次编两份：管理台 + 产品面）。
//
// 路由约定：/ui/* 服务单页应用，未知子路径回退 index.html（直达/刷新不
// 404）；一律 Cache-Control: no-store，固件升级后旧界面不得从缓存复活
// （同管理页的口径）。
//
// 本包只有静态层，没有 API 命名空间：界面的数据全部打管理面
// （/admin/v1/*）与数据面，本包一个接口都不提供。
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed uidist
var uiFiles embed.FS

// UIHandler 服务 /ui/ 子树的嵌入静态资源。静态资源不要求会话：这一层只是
// 页面本体，与管理台登录页同性质；登录门在前端，数据面各自鉴权。
func UIHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "uidist")
	if err != nil {
		panic("uidist 嵌入缺失（先 make web 生成并入库）: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/ui/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(sub, name); err != nil {
			// 单页应用：未知子路径（含刷新/直达与非法文件名）回退 index.html。
			name = "index.html"
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, sub, name)
	})
}
