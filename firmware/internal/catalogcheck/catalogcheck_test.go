package catalogcheck

// 两条绊线 + 一组拒收用例：
//   1. 仓库里的两份官网副本必须通过发布前校验；
//   2. 维护源（公开发布仓库检出 distribute/llm-gate/catalog/，可能不在场）、官网副本、
//      固件内嵌副本三份逐字节一致；
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

var (
	websiteDir = filepath.Join("..", "..", "..", "website", "public", "updates", "data")
	sourceDir  = filepath.Join("..", "..", "..", "distribute", "llm-gate", "catalog")
)

func readWebsite(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(websiteDir, name))
	if os.IsNotExist(err) {
		t.Skipf("官网副本不在场（%s）——仅允许出现在脱离仓库的独立构建里", name)
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWebsiteCopiesPass(t *testing.T) {
	pricing := readWebsite(t, OfficialPricingFile)
	platform := readWebsite(t, PlatformModelsFile)
	r := Check(pricing, platform)
	for _, e := range r.Errors {
		t.Error(e)
	}
	for _, i := range r.Infos {
		t.Log(i)
	}
}

func TestThreeCopiesIdentical(t *testing.T) {
	platform := readWebsite(t, PlatformModelsFile)
	if !bytes.Equal(platform, platformcatalog.BuiltinRaw()) {
		t.Error("固件内嵌的 platform-models.json 与官网副本不一致")
	}
	for _, name := range []string{OfficialPricingFile, PlatformModelsFile} {
		src, err := os.ReadFile(filepath.Join(sourceDir, name))
		if os.IsNotExist(err) {
			t.Logf("维护源不在场（%s），跳过与它的比对", filepath.Join(sourceDir, name))
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, readWebsite(t, name)) {
			t.Errorf("%s：维护源与官网副本不一致——改动只在维护源做，然后 make -C firmware catalog", name)
		}
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
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := Format(out)
	if err != nil {
		t.Fatal(err)
	}
	return formatted
}

func firstModel(doc map[string]any) map[string]any {
	return doc["models"].([]any)[0].(map[string]any)
}

func TestCheckRejects(t *testing.T) {
	pricing := readWebsite(t, OfficialPricingFile)
	platform := readWebsite(t, PlatformModelsFile)
	cases := []struct {
		name     string
		pricing  []byte
		platform []byte
		want     string
	}{{
		name:     "价目文件不是规范格式",
		pricing:  append([]byte("  "), pricing...),
		platform: platform,
		want:     "不是规范格式",
	}, {
		name:     "价目条目含未知字段",
		pricing:  mutate(t, pricing, func(d map[string]any) { firstModel(d)["schedul"] = map[string]any{} }),
		platform: platform,
		want:     "不认识的字段",
	}, {
		name:     "价目条目缺出处",
		pricing:  mutate(t, pricing, func(d map[string]any) { firstModel(d)["source"] = "" }),
		platform: platform,
		want:     "source 必须是",
	}, {
		name: "分时段价重叠",
		pricing: mutate(t, pricing, func(d map[string]any) {
			m := firstModel(d)
			periods := m["schedule"].(map[string]any)["periods"].([]any)
			periods[1].(map[string]any)["days"] = []any{"fri", "sat"}
		}),
		platform: platform,
		want:     "schedule 不合法",
	}, {
		name: "分时段价字段集与标准价不同",
		pricing: mutate(t, pricing, func(d map[string]any) {
			m := firstModel(d)
			periods := m["schedule"].(map[string]any)["periods"].([]any)
			periods[0].(map[string]any)["pricing"] = map[string]any{"in": 1, "out": 2}
		}),
		platform: platform,
		want:     "字段集",
	}, {
		name:     "文本价目带视频字段",
		pricing:  mutate(t, pricing, func(d map[string]any) { firstModel(d)["pricing"].(map[string]any)["ark_video_token"] = 1 }),
		platform: platform,
		want:     "不认的字段",
	}, {
		name:     "只配输入价",
		pricing:  mutate(t, pricing, func(d map[string]any) { firstModel(d)["pricing"] = map[string]any{"in": 1} }),
		platform: platform,
		want:     "成对录入",
	}, {
		name:    "订阅型号没录价",
		pricing: pricing,
		platform: mutate(t, platform, func(d map[string]any) {
			a := d["agents"].([]any)[0].(map[string]any)
			a["models"] = append(a["models"].([]any), map[string]any{"name": "gpt-99-nova", "kind": "text"})
		}),
		want: "没有带 agent 标记的同名条目",
	}, {
		name:    "平台三类混排",
		pricing: pricing,
		platform: mutate(t, platform, func(d map[string]any) {
			ps := d["platforms"].([]any)
			last := ps[len(ps)-1] // 通用适配
			d["platforms"] = append([]any{last}, ps[:len(ps)-1]...)
		}),
		want: "排在了后一类平台之后",
	}, {
		name:     "平台条目含未知字段",
		pricing:  pricing,
		platform: mutate(t, platform, func(d map[string]any) { d["platforms"].([]any)[0].(map[string]any)["base-url"] = "x" }),
		want:     "不认识的字段",
	}, {
		name:    "agents 段未知 provider",
		pricing: pricing,
		platform: mutate(t, platform, func(d map[string]any) {
			d["agents"] = append(d["agents"].([]any), map[string]any{"provider": "unknown", "vendor": "Unknown", "source": "https://example.invalid/docs", "checked_at": "2026-09-07", "models": []any{}})
		}),
		want: "provider 只认 codex | grok | claude | cursor",
	}, {
		name:    "文本模型声明了 family",
		pricing: pricing,
		platform: mutate(t, platform, func(d map[string]any) {
			d["platforms"].([]any)[0].(map[string]any)["models"].([]any)[0].(map[string]any)["family"] = "ark_video"
		}),
		want: "不该声明 family",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(tc.pricing, tc.platform)
			if r.OK() {
				t.Fatalf("应报错（期望含 %q）", tc.want)
			}
			if !strings.Contains(strings.Join(r.Errors, "\n"), tc.want) {
				t.Errorf("错误里没有 %q：\n%s", tc.want, strings.Join(r.Errors, "\n"))
			}
		})
	}
}

func TestFormatIsIdempotent(t *testing.T) {
	raw := readWebsite(t, OfficialPricingFile)
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
