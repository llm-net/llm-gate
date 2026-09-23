package catalogcheck

// 两条绊线 + 一组拒收用例：
//   1. 仓库里的官网副本必须通过发布前校验；
//   2. 官网副本与固件内嵌副本逐字节一致；
//   3. 发布侧比设备严的那些规则各自能抓到错。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

var websitePath = filepath.Join("..", "..", "..", "website", "public", "updates", "data", FileName)

func readWebsite(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(websitePath)
	if os.IsNotExist(err) {
		t.Skipf("官网副本不在场（%s）——仅允许出现在脱离仓库的独立构建里", websitePath)
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWebsiteCopyPasses(t *testing.T) {
	r := Check(readWebsite(t))
	for _, e := range r.Errors {
		t.Error(e)
	}
	for _, i := range r.Infos {
		t.Log(i)
	}
}

func TestCopiesIdentical(t *testing.T) {
	if !bytes.Equal(readWebsite(t), platformcatalog.BuiltinRaw()) {
		t.Error("固件内嵌的 model-catalog.json 与官网副本不一致——改动只经 make -C firmware catalog 生成")
	}
}

// mutate 把官网副本解成通用 JSON、让用例改一处、再按规范格式编码回去，
// 这样用例只测语义规则，不会被格式规则误伤。
func mutate(t *testing.T, raw []byte, f func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	f(doc)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	formatted, err := Format(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return formatted
}

// pricedTextModel 找第一条带 schedule 的文本模型（DeepSeek），用例改它。
func pricedTextModel(doc map[string]any) map[string]any {
	for _, p := range doc["platforms"].([]any) {
		for _, m := range p.(map[string]any)["models"].([]any) {
			mm := m.(map[string]any)
			if mm["kind"] == "text" && mm["schedule"] != nil {
				return mm
			}
		}
	}
	panic("官网副本里没有带分时段价的文本模型")
}

func firstPlatform(doc map[string]any) map[string]any {
	return doc["platforms"].([]any)[0].(map[string]any)
}

func TestCheckRejects(t *testing.T) {
	raw := readWebsite(t)
	cases := []struct {
		name string
		raw  []byte
		want string
	}{{
		name: "不是规范格式",
		raw:  append([]byte("  "), raw...),
		want: "不是规范格式",
	}, {
		name: "模型条目含未知字段",
		raw:  mutate(t, raw, func(d map[string]any) { pricedTextModel(d)["schedul"] = map[string]any{} }),
		want: "不认识的字段",
	}, {
		name: "平台缺出处",
		raw:  mutate(t, raw, func(d map[string]any) { firstPlatform(d)["source"] = "" }),
		want: "source 必须是",
	}, {
		name: "缺 source 段的 tag",
		raw:  mutate(t, raw, func(d map[string]any) { d["source"].(map[string]any)["tag"] = "" }),
		want: "source 必须写全",
	}, {
		name: "分时段价重叠",
		raw: mutate(t, raw, func(d map[string]any) {
			periods := pricedTextModel(d)["schedule"].(map[string]any)["periods"].([]any)
			periods[1].(map[string]any)["days"] = []any{"fri", "sat"}
		}),
		want: "schedule 不合法",
	}, {
		name: "分时段价字段集与标准价不同",
		raw: mutate(t, raw, func(d map[string]any) {
			periods := pricedTextModel(d)["schedule"].(map[string]any)["periods"].([]any)
			periods[0].(map[string]any)["pricing"] = map[string]any{"in": 1, "out": 2}
		}),
		want: "字段集",
	}, {
		name: "文本价目带视频字段",
		raw:  mutate(t, raw, func(d map[string]any) { pricedTextModel(d)["pricing"].(map[string]any)["ark_video_token"] = 1 }),
		want: "不认的字段",
	}, {
		name: "只配输入价",
		raw: mutate(t, raw, func(d map[string]any) {
			m := pricedTextModel(d)
			delete(m, "schedule")
			m["pricing"] = map[string]any{"in": 1}
		}),
		want: "成对录入",
	}, {
		name: "平台三类混排",
		raw: mutate(t, raw, func(d map[string]any) {
			ps := d["platforms"].([]any)
			last := ps[len(ps)-1] // 通用适配
			d["platforms"] = append([]any{last}, ps[:len(ps)-1]...)
		}),
		want: "排在了后一类平台之后",
	}, {
		name: "平台条目含未知字段",
		raw:  mutate(t, raw, func(d map[string]any) { firstPlatform(d)["base-url"] = "x" }),
		want: "不认识的字段",
	}, {
		name: "agents 段未知 provider",
		raw: mutate(t, raw, func(d map[string]any) {
			d["agents"] = append(d["agents"].([]any), map[string]any{"provider": "unknown", "vendor": "Unknown", "source": "https://example.invalid/docs", "checked_at": "2026-09-07", "models": []any{map[string]any{"name": "x", "kind": "text"}}})
		}),
		want: "provider 只认 codex | grok | claude | cursor",
	}, {
		name: "文本模型声明了 family",
		raw:  mutate(t, raw, func(d map[string]any) { pricedTextModel(d)["family"] = "ark_video" }),
		want: "不该声明 family",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(tc.raw)
			if r.OK() {
				t.Fatalf("应报错（期望含 %q）", tc.want)
			}
			if !strings.Contains(strings.Join(r.Errors, "\n"), tc.want) {
				t.Errorf("错误里没有 %q：\n%s", tc.want, strings.Join(r.Errors, "\n"))
			}
		})
	}
}

func TestAgentModelWithoutPricingIsInfoOnly(t *testing.T) {
	raw := mutate(t, readWebsite(t), func(d map[string]any) {
		a := d["agents"].([]any)[0].(map[string]any)
		a["models"] = append(a["models"].([]any), map[string]any{"name": "gpt-99-nova", "kind": "text"})
	})
	r := Check(raw)
	if !r.OK() {
		t.Fatalf("没有参考价的订阅型号不该阻塞发布：%v", r.Errors)
	}
	if !strings.Contains(strings.Join(r.Infos, "\n"), "gpt-99-nova") {
		t.Errorf("提示里应点名没价的型号：%v", r.Infos)
	}
}

func TestFormatIsIdempotent(t *testing.T) {
	raw := readWebsite(t)
	once, err := Format(raw)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Format(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) || !bytes.Equal(once, raw) {
		t.Error("规范格式应幂等，且官网副本已是规范格式")
	}
}

func TestInputModalitiesValidation(t *testing.T) {
	raw := readWebsite(t)
	for _, modalities := range [][]string{{"text", "unknown"}, {"image", "image"}} {
		modified := mutate(t, raw, func(doc map[string]any) {
			pricedTextModel(doc)["capabilities"] = map[string]any{"input_modalities": modalities}
		})
		if result := Check(modified); len(result.Errors) == 0 {
			t.Fatalf("accepted invalid modalities: %v", modalities)
		}
	}
}
