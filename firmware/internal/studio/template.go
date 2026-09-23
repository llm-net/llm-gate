package studio

// 创作类型（Template）：建空间时选定一次、之后不改的工作空间属性，决定两样东西——写进开发者
// 指令的一节工作规程（GUIDE.md：流程、交付物格式、命名与取舍），与 agent/PROJECT.md 的初始
// 骨架（PROJECT.md：栏目固定，之后由智能体维护）。两份文本都是给模型看的（不是界面文案，
// 不进 i18n 目录），随固件内嵌在 templates/<ID>/ 下；名称与简介是界面文案，作为 Go 字面量
// 由 i18n 抽取器收进目录。没有可编辑的模板表：管理员定制走上传覆盖 agent/PROJECT.md。

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed templates/*/GUIDE.md templates/*/PROJECT.md
var templateFS embed.FS

// Template 是一种创作类型。
type Template struct {
	// ID 是稳定标识（workspaces.template 存它），形状见 store.NormalizeWorkspaceTemplate。
	ID string `json:"id"`
	// Name / Description 是界面上的名称与一句话简介（中文源文本，管理面按 Accept-Language 翻）。
	Name        string `json:"name" i18n:"text"`
	Description string `json:"description" i18n:"text"`
	// Guide 是写进开发者指令的工作规程（Markdown，二级标题以下）；Brief 是 agent/PROJECT.md 的初始内容。
	Guide string `json:"-"`
	Brief string `json:"-"`
}

// DefaultTemplate 是创建时没指定创作类型时取的 ID，也是存量创作工作空间的类型。
const DefaultTemplate = "general"

// templateTable 按界面上的展示顺序列出全部创作类型；正文在 templates/<ID>/。
var templateTable = []Template{
	{ID: DefaultTemplate, Name: "通用", Description: "不预设题材：按指令规划目标、风格与交付物，适合海报、插画、单条视频等零散创作。"},
	{ID: "short-drama", Name: "1 分钟短剧", Description: "约 60 秒竖屏短剧：立意、人物定妆、分镜表、逐镜首帧与视频、字幕与交付清单。"},
	{ID: "music-video", Name: "1 分钟 MV", Description: "以一到两位人物的形象照为准，围绕上传的歌曲做约 60 秒情景 MV：定妆、情境脚本、按歌曲段落分镜、逐镜首帧与视频、铺歌粗剪。"},
	{ID: "product-visual", Name: "产品视觉", Description: "围绕上传的产品照片产出主图、场景图、细节图、短视频与文案，产品外形与 Logo 不改。"},
	{ID: "storyboard", Name: "分镜脚本", Description: "把故事或脚本拆成场次、分镜表与关键帧画板，只出图不出视频。"},
}

var templates = loadTemplates()

func loadTemplates() []Template {
	out := make([]Template, 0, len(templateTable))
	seen := map[string]bool{}
	for _, t := range templateTable {
		if seen[t.ID] {
			panic(fmt.Sprintf("studio: 创作类型 %q 重复", t.ID))
		}
		seen[t.ID] = true
		guide, err := templateFS.ReadFile("templates/" + t.ID + "/GUIDE.md")
		if err != nil {
			panic(fmt.Sprintf("studio: 创作类型 %q 缺 GUIDE.md: %v", t.ID, err))
		}
		brief, err := templateFS.ReadFile("templates/" + t.ID + "/PROJECT.md")
		if err != nil {
			panic(fmt.Sprintf("studio: 创作类型 %q 缺 PROJECT.md: %v", t.ID, err))
		}
		t.Guide = strings.TrimSpace(string(guide))
		t.Brief = strings.TrimRight(string(brief), "\n") + "\n"
		if t.Guide == "" || strings.TrimSpace(t.Brief) == "" {
			panic(fmt.Sprintf("studio: 创作类型 %q 的正文为空", t.ID))
		}
		out = append(out, t)
	}
	return out
}

// Templates 按展示顺序返回全部创作类型（副本）。
func Templates() []Template {
	out := make([]Template, len(templates))
	copy(out, templates)
	return out
}

// TemplateByID 按 ID 取创作类型；空串取 DefaultTemplate。不存在时 ok 为假。
func TemplateByID(id string) (Template, bool) {
	if id == "" {
		id = DefaultTemplate
	}
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// templateFor 是工作空间的创作类型；行里的名字不认识（不该发生）时退到 DefaultTemplate，
// 让对话仍能开。
func templateFor(id string) Template {
	if t, ok := TemplateByID(id); ok {
		return t
	}
	t, _ := TemplateByID(DefaultTemplate)
	return t
}
