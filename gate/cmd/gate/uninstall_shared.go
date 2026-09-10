package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func (a *app) planShared(p *uninstallPlan) error {
	st, ok := a.cfg.Shared["codex"]
	if !ok {
		return nil
	}
	if !filepath.IsAbs(st.Home) || filepath.Clean(st.Catalog) != filepath.Clean(a.sharedCatalogPath()) {
		return errors.New("Codex 共享接入记录路径不匹配，拒绝清理")
	}
	path := filepath.Join(st.Home, "config.toml")
	b, err := readSmallFile(path)
	if err != nil {
		return err
	}
	if b != nil {
		text := string(b)
		i, j := tomlSectionBounds(text, sharedProviderHeader)
		ours := i < 0
		if i >= 0 {
			section := strings.TrimSpace(text[i:j])
			want := st.SectionHash
			if want == "" {
				want = digest([]byte(strings.TrimSpace(sharedProviderHeader + "\n" + a.sharedSectionBody())))
			}
			ours = digest([]byte(section)) == want
			if ours {
				text = tomlRemoveSection(text, sharedProviderHeader)
			} else {
				if a.cfg.APIKey != "" && strings.Contains(section, a.cfg.APIKey) {
					return errors.New("Codex 共享 provider 已改写且仍含 gate 凭据；请先修复该段后重试")
				}
				p.keep = append(p.keep, "保留用户改写的 Codex provider："+path)
			}
		}
		if v, ok := tomlTopLevel(text, "model_catalog_json"); ok && v == st.Catalog {
			if st.PrevCatalogSet {
				text = tomlSetTopLevel(text, "model_catalog_json", tomlQuote(st.PrevCatalog))
			} else {
				text = tomlRemoveTopLevel(text, "model_catalog_json")
			}
		}
		if v, ok := tomlTopLevel(text, "model_provider"); ok && v == "llmgate" && ours {
			if st.PrevProviderSet {
				text = tomlSetTopLevel(text, "model_provider", tomlQuote(st.PrevProvider))
			} else {
				text = tomlRemoveTopLevel(text, "model_provider")
			}
		}
		if v, ok := tomlTopLevel(text, "model"); ok && st.ModelWritten != "" && v == st.ModelWritten {
			text = tomlRemoveTopLevel(text, "model")
		}
		if (a.cfg.APIKey != "" && strings.Contains(text, a.cfg.APIKey)) || strings.Contains(text, st.Catalog) || strings.Contains(text, tomlQuote(st.Catalog)) {
			return errors.New("Codex 共享配置仍引用 gate 凭据或待删除的目录，卸载已停止")
		}
		if err := p.rewrite(path, b, []byte(text), "恢复 Codex 共享配置"); err != nil {
			return err
		}
	}
	if st.AuthWritten {
		path := filepath.Join(st.Home, "auth.json")
		b, err := readSmallFile(path)
		if err != nil {
			return err
		}
		if b != nil {
			var auth map[string]any
			if json.Unmarshal(b, &auth) != nil || auth == nil {
				return errors.New("Codex auth.json 无法解析，不能确认凭据已撤销")
			}
			if key, _ := auth["OPENAI_API_KEY"].(string); key == a.cfg.APIKey && key != "" {
				delete(auth, "OPENAI_API_KEY")
				if auth["auth_mode"] == "apikey" {
					delete(auth, "auth_mode")
				}
				var after []byte
				if len(auth) > 0 {
					after, err = json.MarshalIndent(auth, "", "  ")
					if err != nil {
						return err
					}
					after = append(after, '\n')
				}
				if bytes.Contains(after, []byte(a.cfg.APIKey)) {
					return errors.New("Codex auth.json 含额外 gate 凭据字段，请先修复")
				}
				if err := p.rewrite(path, b, after, "撤销 Codex 共享凭据"); err != nil {
					return err
				}
			} else if a.cfg.APIKey != "" && bytes.Contains(b, []byte(a.cfg.APIKey)) {
				return errors.New("Codex 登录文件含已改写的 gate 凭据，卸载已停止")
			}
		}
	}
	if err := p.remove(st.Catalog, nil, "删除 Codex 共享模型目录"); err != nil {
		return err
	}
	return nil
}

func (a *app) sharedSectionBody() string {
	return fmt.Sprintf("name = \"LLM Gate\"\nbase_url = %s\nwire_api = \"responses\"\nhttp_headers = { \"Authorization\" = %s, %s = %s }\n",
		tomlQuote(a.cfg.BaseURL+"/agents/codex/v1"), tomlQuote("Bearer "+a.cfg.APIKey), tomlQuote(codexActorAuthorizationHeader), tomlQuote(codexActorAuthorizationValue))
}
