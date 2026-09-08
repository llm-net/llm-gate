package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

// writeCfg 把 YAML 内容写入临时文件并返回路径。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validYAML = `
listen: "127.0.0.1:8080"
upstreams:
  - name: mock-main
    type: mock
    base_url: "http://127.0.0.1:18080/v1"
logical_models:
  chat-fast:
    upstream: mock-main
    upstream_model_id: mock-gpt-4o-mini
    protocol: openai_chat
api_keys:
  - key: "sk_test_0123456789abcdef"
`

func TestLoadValid(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	// 未给出的字段取缺省值。
	if cfg.DataDir != config.DefaultDataDir {
		t.Errorf("data_dir 缺省 = %q，期望 %q", cfg.DataDir, config.DefaultDataDir)
	}
	if cfg.LogLevel != config.DefaultLogLevel {
		t.Errorf("log_level 缺省 = %q，期望 %q", cfg.LogLevel, config.DefaultLogLevel)
	}
	b, ok := cfg.LogicalModels["chat-fast"]
	if !ok || b.Upstream != "mock-main" || b.UpstreamModelID != "mock-gpt-4o-mini" {
		t.Errorf("logical_models 解析异常: %+v", cfg.LogicalModels)
	}
}

// TestProtocolDefault：protocol 省略时缺省为 openai_chat。
func TestProtocolDefault(t *testing.T) {
	yaml := strings.Replace(validYAML, "    protocol: openai_chat\n", "", 1)
	cfg, err := config.Load(writeCfg(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LogicalModels["chat-fast"].Protocol; got != config.ProtocolOpenAIChat {
		t.Errorf("protocol 缺省 = %q，期望 %q", got, config.ProtocolOpenAIChat)
	}
}

// TestArkPlanAndAnthropicProtocol：iteration-3 新增组合可加载——type ark_plan
// 上游、以及 deepseek/ark_plan 上游被 protocol anthropic_messages binding 引用。
func TestArkPlanAndAnthropicProtocol(t *testing.T) {
	const yaml = `
listen: "127.0.0.1:8080"
upstreams:
  - name: ark-plan
    type: ark_plan
    api_key: "test-key-not-real"
  - name: ds
    type: deepseek
    api_key: "test-key-not-real-2"
logical_models:
  cc-ark:
    upstream: ark-plan
    upstream_model_id: deepseek-v4-flash
    protocol: anthropic_messages
  cc-ds:
    upstream: ds
    upstream_model_id: deepseek-chat
    protocol: anthropic_messages
api_keys:
  - key: "sk_test_0123456789abcdef"
`
	cfg, err := config.Load(writeCfg(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"cc-ark", "cc-ds"} {
		if got := cfg.LogicalModels[name].Protocol; got != config.ProtocolAnthropicMessages {
			t.Errorf("logical_models.%s.protocol = %q，期望 %q", name, got, config.ProtocolAnthropicMessages)
		}
	}
}

// TestLoadExampleFile：仓库内的样例配置必须始终可加载。
func TestLoadExampleFile(t *testing.T) {
	cfg, err := config.Load("../../configs/dev.example.yaml")
	if err != nil {
		t.Fatalf("configs/dev.example.yaml 加载失败: %v", err)
	}
	if len(cfg.LogicalModels) == 0 || len(cfg.APIKeys) == 0 {
		t.Error("样例配置应包含逻辑模型与 API密钥")
	}
}

// TestAPIKeysOptional（iteration-4 Phase 4）：api_keys 段降级为启动时导入用，
// 整段缺省或显式空列表都能正常加载（正式 Key 经管理界面签发、存 SQLite）；
// 条目内 key 非空与去重校验保留（见 TestLoadInvalid 与 Redact 用例）。
func TestAPIKeysOptional(t *testing.T) {
	const section = "api_keys:\n  - key: \"sk_test_0123456789abcdef\"\n"
	for name, yaml := range map[string]string{
		"整段缺省":  strings.Replace(validYAML, section, "", 1),
		"显式空列表": strings.Replace(validYAML, section, "api_keys: []\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if yaml == validYAML {
				t.Fatal("用例未生效：api_keys 段未被替换")
			}
			cfg, err := config.Load(writeCfg(t, yaml))
			if err != nil {
				t.Fatalf("无 api_keys 的配置应可加载: %v", err)
			}
			if len(cfg.APIKeys) != 0 {
				t.Errorf("api_keys 应为空，got %d 条", len(cfg.APIKeys))
			}
		})
	}
}

// TestImportSectionsOptional（iteration-5 Phase 2）：upstreams 与 logical_models
// 与 api_keys 一样降级为启动导入表——整段缺省或显式为空都能加载（上游与模型
// 全部在管理界面维护，SQLite 是唯一事实源）。
func TestImportSectionsOptional(t *testing.T) {
	for name, yaml := range map[string]string{
		"整段缺省": "listen: \"127.0.0.1:8080\"\n",
		"显式空值": "listen: \"127.0.0.1:8080\"\nupstreams: []\nlogical_models: {}\napi_keys: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Load(writeCfg(t, yaml))
			if err != nil {
				t.Fatalf("无导入表的配置应可加载: %v", err)
			}
			if len(cfg.Upstreams) != 0 || len(cfg.LogicalModels) != 0 || len(cfg.APIKeys) != 0 {
				t.Errorf("三张导入表都应为空: %d/%d/%d", len(cfg.Upstreams), len(cfg.LogicalModels), len(cfg.APIKeys))
			}
		})
	}
}

// TestArkAnthropicAccepted（iteration-5 决策 4）：config 不再做
// (type ark, anthropic_messages) 交叉校验——可服务入口改由运行时按来源上游的
// type 过滤，binding 的 protocol 字段在导入时就被丢弃。
func TestArkAnthropicAccepted(t *testing.T) {
	yaml := strings.Replace(validYAML, "type: mock", "type: ark\n    api_key: \"test-key-not-real\"", 1)
	yaml = strings.Replace(yaml, "protocol: openai_chat", "protocol: anthropic_messages", 1)
	if _, err := config.Load(writeCfg(t, yaml)); err != nil {
		t.Fatalf("ark + anthropic_messages 组合应可加载（运行时过滤）: %v", err)
	}
}

// TestLoadMissing：配置缺失（未指定 / 文件不存在 / 内容为空）都得到可读错误。
func TestLoadMissing(t *testing.T) {
	if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Errorf("空路径应报缺少 --config，got: %v", err)
	}
	if _, err := config.Load(filepath.Join(t.TempDir(), "no-such.yaml")); err == nil {
		t.Error("不存在的文件应报错")
	}
	if _, err := config.Load(writeCfg(t, "")); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空文件应报错，got: %v", err)
	}
}

// TestLoadInvalid：非法配置逐项报可读错误。
func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			"未知字段（拼写错误）",
			func(y string) string { return y + "\nlisten_addr: \":9\"\n" },
			"listen_addr",
		},
		{
			"listen 非法",
			func(y string) string { return strings.Replace(y, `"127.0.0.1:8080"`, `"127.0.0.1"`, 1) },
			"host:port",
		},
		{
			"listen 端口非数字",
			func(y string) string { return strings.Replace(y, `"127.0.0.1:8080"`, `"127.0.0.1:notaport"`, 1) },
			"端口非法",
		},
		{
			"listen 端口越界",
			func(y string) string { return strings.Replace(y, `"127.0.0.1:8080"`, `"127.0.0.1:99999"`, 1) },
			"端口非法",
		},
		{
			// 单端口决策（2026-08-06）：admin_listen 已删除，旧配置须去掉这一行
			// ——KnownFields 把它当未知字段拒绝，启动即失败而非静默忽略。
			"admin_listen 已移除",
			func(y string) string { return y + "\nadmin_listen: \"127.0.0.1:8088\"\n" },
			"admin_listen",
		},
		{
			"多 YAML 文档",
			func(y string) string { return y + "\n---\napi_keys: []\n" },
			"多个 YAML 文档",
		},
		{
			"log_level 非法",
			func(y string) string { return y + "\nlog_level: verbose\n" },
			"log_level",
		},
		{
			"upstream type 非法",
			func(y string) string { return strings.Replace(y, "type: mock", "type: openai", 1) },
			"未知 type",
		},
		{
			"mock 缺 base_url",
			func(y string) string {
				return strings.Replace(y, "    base_url: \"http://127.0.0.1:18080/v1\"\n", "", 1)
			},
			"base_url",
		},
		{
			"deepseek 缺 api_key",
			func(y string) string { return strings.Replace(y, "type: mock", "type: deepseek", 1) },
			"api_key",
		},
		{
			"ark_plan 缺 api_key",
			func(y string) string { return strings.Replace(y, "type: mock", "type: ark_plan", 1) },
			"api_key",
		},
		{
			"qwen_plan 缺 api_key",
			func(y string) string { return strings.Replace(y, "type: mock", "type: qwen_plan", 1) },
			"api_key",
		},
		{
			"opencode_go 缺 api_key",
			func(y string) string { return strings.Replace(y, "type: mock", "type: opencode_go", 1) },
			"api_key",
		},
		{
			// openai_compat 两样都要：地址不内置（base_url 必填），凭证也照发。
			"openai_compat 缺 api_key",
			func(y string) string { return strings.Replace(y, "type: mock", "type: openai_compat", 1) },
			"api_key",
		},
		{
			"openai_compat 缺 base_url",
			func(y string) string {
				y = strings.Replace(y, "type: mock", "type: openai_compat\n    api_key: \"test-key-not-real\"", 1)
				return strings.Replace(y, "    base_url: \"http://127.0.0.1:18080/v1\"\n", "", 1)
			},
			"base_url",
		},
		{
			"base_url 非 http(s)",
			func(y string) string {
				return strings.Replace(y, "http://127.0.0.1:18080/v1", "127.0.0.1:18080", 1)
			},
			"http(s)",
		},
		{
			"逻辑模型引用未定义 upstream",
			func(y string) string { return strings.Replace(y, "upstream: mock-main", "upstream: nope", 1) },
			"未在 upstreams 中定义",
		},
		{
			"缺 upstream_model_id",
			func(y string) string {
				return strings.Replace(y, "    upstream_model_id: mock-gpt-4o-mini\n", "", 1)
			},
			"upstream_model_id",
		},
		{
			"protocol 不支持",
			func(y string) string {
				return strings.Replace(y, "protocol: openai_chat", "protocol: anthropic", 1)
			},
			"protocol",
		},
		{
			"upstream name 重复",
			func(y string) string {
				return strings.Replace(y, "upstreams:\n",
					"upstreams:\n  - name: mock-main\n    type: mock\n    base_url: \"http://127.0.0.1:18080/v1\"\n", 1)
			},
			"重复",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := c.mutate(validYAML)
			if mutated == validYAML {
				t.Fatal("用例未生效：mutate 没有改变配置内容")
			}
			_, err := config.Load(writeCfg(t, mutated))
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误信息 %q 应包含 %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestValidationErrorsRedactKeys：涉及 API密钥 的校验错误必须走 RedactKey，
// 错误文本不得出现 Key 明文（§15.1：密钥不落日志，启动错误同样会进日志/终端）。
func TestValidationErrorsRedactKeys(t *testing.T) {
	const secret = "sk_secret_fedcba9876543210"

	t.Run("重复 key", func(t *testing.T) {
		yaml := validYAML + "  - key: \"" + secret + "\"\n" +
			"  - key: \"" + secret + "\"\n"
		_, err := config.Load(writeCfg(t, yaml))
		if err == nil {
			t.Fatal("重复 key 应报错")
		}
		assertRedacted(t, err.Error(), secret)
		if !strings.Contains(err.Error(), "重复") {
			t.Errorf("错误应指明重复: %v", err)
		}
	})

	t.Run("残留的 user 字段", func(t *testing.T) {
		// 0019 起条目只有 key 一个字段。升级上来的配置若还留着 `user:`，
		// KnownFields 会拒绝解码——**宁可在启动时说清楚，也不静默忽略一个
		// 作者以为还在起作用的字段**；错误文本同样不得回显 Key 明文。
		yaml := validYAML + "  - key: \"" + secret + "\"\n    user: legacy\n"
		_, err := config.Load(writeCfg(t, yaml))
		if err == nil {
			t.Fatal("残留的 user 字段应报错")
		}
		assertRedacted(t, err.Error(), secret)
	})

	// yaml 类型错误会把文件里的标量值回显进错误文本（如把 api_keys 误写成
	// 字符串列表时），解码路径同样必须脱敏。
	t.Run("yaml 类型错误不回显值", func(t *testing.T) {
		for _, s := range []string{secret, "sk_short"} { // 长短各一：≤10 字符曾被全文回显
			yaml := strings.Replace(validYAML,
				"api_keys:\n  - key: \"sk_test_0123456789abcdef\"\n",
				"api_keys:\n  - \""+s+"\"\n", 1)
			_, err := config.Load(writeCfg(t, yaml))
			if err == nil {
				t.Fatal("类型不匹配应报错")
			}
			assertRedacted(t, err.Error(), s)
			if !strings.Contains(err.Error(), "解析配置文件") {
				t.Errorf("错误应指明解析失败: %v", err)
			}
		}
	})
}

func assertRedacted(t *testing.T, msg, secret string) {
	t.Helper()
	if strings.Contains(msg, secret) {
		t.Errorf("错误信息泄露 Key 明文: %s", msg)
	}
	// 超过 4 位的明文前缀也不允许。
	if strings.Contains(msg, secret[:5]) {
		t.Errorf("错误信息泄露超过 4 位 Key 前缀: %s", msg)
	}
}

func TestOfficialSiteSection(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `
listen: "127.0.0.1:8080"
official_site:
  base_url: "https://llmgate.example.com"
`))
	if err != nil {
		t.Fatalf("合法 official_site 段应可加载: %v", err)
	}
	if got := cfg.OfficialSite.EffectiveBaseURL(); got != "https://llmgate.example.com" {
		t.Errorf("EffectiveBaseURL = %q", got)
	}
	cfg, err = config.Load(writeCfg(t, "listen: \"127.0.0.1:8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OfficialSite.EffectiveBaseURL(); got != config.DefaultOfficialSiteBaseURL {
		t.Errorf("默认官网 = %q", got)
	}
	for _, bad := range []string{
		"llmgate.example.com",
		"ftp://llmgate.example.com",
		"https://user@example.com",
		"https://llmgate.example.com/mirror",
		"https://llmgate.example.com?channel=stable",
		"https://llmgate.example.com#updates",
	} {
		y := "listen: \"127.0.0.1:8080\"\nofficial_site:\n  base_url: \"" + bad + "\"\n"
		if _, err := config.Load(writeCfg(t, y)); err == nil || !strings.Contains(err.Error(), "official_site.base_url") {
			t.Errorf("非法官网地址 %q 应被拒绝，得到 %v", bad, err)
		}
	}
}

func TestTLSSection(t *testing.T) {
	const valid = `
listen: "127.0.0.1:8080"
tls:
  listen: "127.0.0.1:8443"
  cert_file: "/etc/llmgate/tls/cert.pem"
  key_file: "/etc/llmgate/tls/key.pem"
`
	cfg, err := config.Load(writeCfg(t, valid))
	if err != nil {
		t.Fatalf("合法 tls 段应可加载: %v", err)
	}
	if !cfg.TLS.Enabled() || cfg.TLS.Listen != "127.0.0.1:8443" {
		t.Errorf("TLS 配置未生效: %+v", cfg.TLS)
	}

	for name, section := range map[string]string{
		"缺私钥":       "tls:\n  listen: \"127.0.0.1:8443\"\n  cert_file: \"/tmp/cert.pem\"\n",
		"缺证书":       "tls:\n  listen: \"127.0.0.1:8443\"\n  key_file: \"/tmp/key.pem\"\n",
		"缺监听":       "tls:\n  cert_file: \"/tmp/cert.pem\"\n  key_file: \"/tmp/key.pem\"\n",
		"监听端口非法":    "tls:\n  listen: \"127.0.0.1:0\"\n  cert_file: \"/tmp/cert.pem\"\n  key_file: \"/tmp/key.pem\"\n",
		"与 HTTP 相同": "tls:\n  listen: \"127.0.0.1:8080\"\n  cert_file: \"/tmp/cert.pem\"\n  key_file: \"/tmp/key.pem\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			yaml := "listen: \"127.0.0.1:8080\"\n" + section
			if _, err := config.Load(writeCfg(t, yaml)); err == nil || !strings.Contains(err.Error(), "tls") {
				t.Fatalf("非法 TLS 配置应被拒绝，得到 %v", err)
			}
		})
	}
}

// usage_days 四件套（iteration-9 Phase 2）。与 history_days 的两处刻意不同都
// 要钉住：**没有 0 这个关闭档**，**下限 31 天**（短于一个最长自然月，
// 重启播种就播不出完整的当月消费，月预算会静默缩水）。
func TestUsageDays(t *testing.T) {
	// 整行省略取缺省。
	cfg, err := config.Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UsageDays != nil {
		t.Errorf("未配置时 UsageDays = %v, 期望 nil", *cfg.UsageDays)
	}
	if got := cfg.UsageDaysOrDefault(); got != config.DefaultUsageDays {
		t.Errorf("UsageDaysOrDefault = %d, 期望 %d", got, config.DefaultUsageDays)
	}

	// 合法取值原样生效。
	cfg, err = config.Load(writeCfg(t, validYAML+"\nusage_days: 120\n"))
	if err != nil {
		t.Fatalf("Load(usage_days: 120): %v", err)
	}
	if got := cfg.UsageDaysOrDefault(); got != 120 {
		t.Errorf("UsageDaysOrDefault = %d, 期望 120", got)
	}

	// 越界一律启动失败（0 也是越界——计量没有关闭档）。
	for _, bad := range []string{"0", "1", "30", "367", "-1"} {
		if _, err := config.Load(writeCfg(t, validYAML+"\nusage_days: "+bad+"\n")); err == nil {
			t.Errorf("usage_days: %s 应校验失败", bad)
		} else if !strings.Contains(err.Error(), "usage_days") {
			t.Errorf("usage_days: %s 的错误未点名字段: %v", bad, err)
		}
	}

	// 绕过 Load 手工构造的路径（测试、装配层）由 OrDefault 兜底：
	// 小于下限的保留期绝不交给 Meter，否则月预算播种残缺。
	tooShort := 1
	tooLong := 9999
	if got := (&config.Config{UsageDays: &tooShort}).UsageDaysOrDefault(); got != config.DefaultUsageDays {
		t.Errorf("下限兜底 = %d, 期望 %d", got, config.DefaultUsageDays)
	}
	if got := (&config.Config{UsageDays: &tooLong}).UsageDaysOrDefault(); got != config.MaxUsageDays {
		t.Errorf("上限兜底 = %d, 期望 %d", got, config.MaxUsageDays)
	}
}
