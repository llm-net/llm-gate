package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func testCatalog() *catalog {
	return newCatalog(map[string]string{
		"固件包为空":           "Firmware package is empty",
		"固件包超过 %d 字节上限":   "Firmware package exceeds the %d-byte limit",
		"读取板卡型号: %w":      "read board model: %w",
		"名称不合法：%s":        "Invalid name: %s",
		"模型 %q 在 %s 上不可用": "Model %q is unavailable on %s",
		"%s 已改为 %s":       "%[2]s is now used by %[1]s",
		"服务内部错误":          "Internal server error",
		"还没翻":             "",
		"下载失败":            "Download failed",
	})
}

func TestTranslateExactAndFallback(t *testing.T) {
	c := testCatalog()
	if got := c.translate("固件包为空", 0); got != "Firmware package is empty" {
		t.Fatalf("exact: %q", got)
	}
	if got := c.translate("还没翻", 0); got != "还没翻" {
		t.Fatalf("empty entry must fall back: %q", got)
	}
	if got := c.translate("完全没有这条", 0); got != "完全没有这条" {
		t.Fatalf("unknown must fall back: %q", got)
	}
}

func TestTranslatePatterns(t *testing.T) {
	c := testCatalog()
	cases := map[string]string{
		fmt.Sprintf("固件包超过 %d 字节上限", 1024):         "Firmware package exceeds the 1024-byte limit",
		"名称不合法：" + "太长了":                           "Invalid name: 太长了",
		fmt.Sprintf("模型 %q 在 %s 上不可用", "gpt", "x"): `Model "gpt" is unavailable on x`,
		fmt.Sprintf("%s 已改为 %s", "A", "B"):         "B is now used by A",
		// %w 链：内层也是目录里的键，递归翻。
		fmt.Errorf("读取板卡型号: %w", errors.New("下载失败")).Error(): "read board model: Download failed",
		// 内层不认识就原样保留。
		fmt.Errorf("读取板卡型号: %w", errors.New("open /x: eacces")).Error(): "read board model: open /x: eacces",
	}
	for in, want := range cases {
		if got := c.translate(in, 0); got != want {
			t.Errorf("translate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslateSegments(t *testing.T) {
	c := testCatalog()
	// 非目录键的前缀 + 目录键的后缀：按 ": " 切段后逐段翻。
	if got := c.translate("apply: 固件包为空", 0); got != "apply: Firmware package is empty" {
		t.Fatalf("segment: %q", got)
	}
	if got := c.translate("服务内部错误：固件包为空", 0); got != "Internal server error：Firmware package is empty" {
		t.Fatalf("cjk colon segment: %q", got)
	}
	if got := c.translate("a: b: c", 0); got != "a: b: c" {
		t.Fatalf("untranslatable segments must stay: %q", got)
	}
}

func TestVerbsAndSignature(t *testing.T) {
	verbs := Verbs("a %d b %[3]s c %s %% %5.2f")
	var got []int
	for _, v := range verbs {
		got = append(got, v.Index)
	}
	if want := []int{1, 3, 4, 0, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verb indexes = %v, want %v", got, want)
	}
	if s := Signature("%s 已改为 %s"); s != "%s%s" {
		t.Fatalf("signature = %q", s)
	}
	if s := Signature("%[2]s is now used by %[1]s"); s != "%s%s" {
		t.Fatalf("reordered signature = %q", s)
	}
	if Signature("%d 个") == Signature("%s 个") {
		t.Fatal("different verbs must differ")
	}
	if s := normalizeTemplate("100%% of %d and %[1]q"); s != "100%% of %[1]s and %[1]s" {
		t.Fatalf("normalizeTemplate = %q", s)
	}
}

func TestNegotiate(t *testing.T) {
	cases := map[string]Lang{
		"":                           Source,
		"zh-CN":                      Source,
		"en":                         English,
		"en-US,en;q=0.9":             English,
		"EN-GB":                      English,
		"ja":                         Japanese,
		"ja-JP,en;q=0.9":             Japanese,
		"JA-jp":                      Japanese,
		"en;q=0.5,ja;q=0.9":          Japanese,
		"ja;q=0,en;q=0.8":            English,
		"ja;q=0":                     Source,
		"fr, en;q=0.5, zh;q=0.8":     Source,
		"fr, en;q=0.8, zh;q=0.5":     English,
		"de":                         Source,
		"*":                          Source,
		"en;q=0":                     Source,
		"zh-TW":                      Source,
		"  en ; q=0.7 , zh-CN;q=0.3": English,
	}
	for in, want := range cases {
		if got := Negotiate(in); got != want {
			t.Errorf("Negotiate(%q) = %q, want %q", in, got, want)
		}
	}
}

type inner struct {
	Note  string `json:"note" i18n:"text"`
	Model string `json:"model"`
}

type outer struct {
	Message string           `json:"message" i18n:"text"`
	Items   []inner          `json:"items"`
	Ptr     *inner           `json:"ptr"`
	ByName  map[string]inner `json:"by_name"`
	Any     any              `json:"any"`
	Tags    []string         `json:"tags" i18n:"text"`
	hidden  string           //nolint:unused // 非导出字段不碰
	Plain   string           `json:"plain"`
}

func TestLocalizeWalker(t *testing.T) {
	c := testCatalog()
	installForTest(t, map[Lang]*catalog{English: c})

	in := &outer{
		Message: "固件包为空",
		Items:   []inner{{Note: "下载失败", Model: "固件包为空"}, {Note: "x"}},
		Ptr:     &inner{Note: "服务内部错误"},
		ByName:  map[string]inner{"a": {Note: "下载失败"}},
		Any:     inner{Note: "固件包为空"},
		Tags:    []string{"下载失败", "keep"},
		hidden:  "固件包为空",
		Plain:   "固件包为空",
	}
	got, ok := Localize(English, in).(*outer)
	if !ok {
		t.Fatal("Localize must keep the pointer shape")
	}
	if got == in {
		t.Fatal("Localize must copy, not mutate")
	}
	if got.Message != "Firmware package is empty" || got.Plain != "固件包为空" || got.hidden != "固件包为空" {
		t.Fatalf("top-level: %+v", got)
	}
	if got.Items[0].Note != "Download failed" || got.Items[0].Model != "固件包为空" || got.Items[1].Note != "x" {
		t.Fatalf("slice: %+v", got.Items)
	}
	if got.Ptr.Note != "Internal server error" || in.Ptr.Note != "服务内部错误" {
		t.Fatalf("pointer: %+v / original %+v", got.Ptr, in.Ptr)
	}
	if got.ByName["a"].Note != "Download failed" || in.ByName["a"].Note != "下载失败" {
		t.Fatalf("map: %+v", got.ByName)
	}
	if got.Any.(inner).Note != "Firmware package is empty" {
		t.Fatalf("interface: %+v", got.Any)
	}
	if !reflect.DeepEqual(got.Tags, []string{"Download failed", "keep"}) || in.Tags[0] != "下载失败" {
		t.Fatalf("[]string: %v", got.Tags)
	}
	// 没有任何可翻字段时原样返回，不复制。
	same := &outer{Plain: "固件包为空", Items: []inner{{Model: "m"}}}
	if Localize(English, same) != any(same) {
		t.Fatal("unchanged value must be returned as-is")
	}
	if Localize(Source, in) != any(in) {
		t.Fatal("source language must be a no-op")
	}
	// 走一遍 JSON 编码，确认副本能正常序列化。
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

// TestEmbeddedCatalogs 校验入库目录：每条译文的 fmt 动词指纹必须与中文一致，
// 否则运行时回填参数会错位。
func TestEmbeddedCatalogs(t *testing.T) {
	installForTest(t, nil)
	load()
	if got, want := Supported(), []Lang{Source, English, Japanese}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supported languages = %v, want %v", got, want)
	}
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := localeFS.ReadFile("locales/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for k, v := range m {
			if v == "" {
				continue
			}
			if Signature(k) != Signature(v) {
				t.Errorf("%s: %q → %q: fmt verbs differ (%s vs %s)", e.Name(), k, v, Signature(k), Signature(v))
			}
		}
	}
}

// installForTest 用给定目录替换嵌入目录（nil = 重新从嵌入文件加载），测试结束复原。
func installForTest(t *testing.T, cats map[Lang]*catalog) {
	t.Helper()
	loadOnce = sync.Once{}
	catalogs = nil
	if cats != nil {
		loadOnce.Do(func() { catalogs = cats })
	}
	t.Cleanup(func() {
		loadOnce = sync.Once{}
		catalogs = nil
	})
}
