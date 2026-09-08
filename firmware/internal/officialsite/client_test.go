package officialsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

type fixedModel string

func (m fixedModel) Model() (string, error) { return string(m), nil }

func TestDataFilesUseStaticPathsETagAndSchemas(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(OfficialPricingPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"price-v7"` {
			w.Header().Set("ETag", `"price-v7"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"price-v7"`)
		io.WriteString(w, `{"schema":"llmgate.official-pricing/v1","version":7,"models":[{"name":"m","kind":"text","pricing":{"in":1,"out":1}}]}`)
	})
	mux.HandleFunc(PlatformModelsPath, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"schema":"llmgate.platform-models/v2","version":4,"platforms":[{"type":"deepseek","models":[]}]}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := NewClient(ts.URL, nil)

	price, err := c.FetchOfficialPricing(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchOfficialPricing: %v", err)
	}
	if price.URL != ts.URL+OfficialPricingPath || price.ETag != `"price-v7"` || price.Doc.Version != 7 {
		t.Fatalf("price = %+v", price)
	}
	if _, err := c.FetchOfficialPricing(context.Background(), `"price-v7"`); err != ErrNotModified {
		t.Fatalf("条件 GET = %v，期望 ErrNotModified", err)
	}
	models, err := c.FetchPlatformModels(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchPlatformModels: %v", err)
	}
	if models.URL != ts.URL+PlatformModelsPath || models.Doc.Version != 4 {
		t.Fatalf("models = %+v", models)
	}
}

func TestDataFileRejectsWrongSchemaAndOversize(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"wrong schema": {`{"schema":"other/v1","models":[{"name":"m"}]}`, "形态不符"},
		"oversize":     {strings.Repeat("x", OfficialPricingMaxBytes+1), "超过"},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer ts.Close()
			_, err := NewClient(ts.URL, nil).FetchOfficialPricing(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestFirmwareCheckFiltersBoardAndMinVersion(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "2608201045-1111"
	t.Cleanup(func() { buildinfo.Version = old })
	sha := strings.Repeat("a", 64)
	index := fmt.Sprintf(`{
  "schema":"%s","channel":"stable","releases":[
    {"version":"2608290930-9999","hardwareModels":["other"],"artifactUrl":"/updates/firmware/v9.bin","artifactSha256":"%s","sizeBytes":9},
    {"version":"2608240815-4444","hardwareModels":["h618-x98h"],"artifactUrl":"/updates/firmware/v4.bin","artifactSha256":"%s","sizeBytes":4,"minVersion":"2608221230-2500"},
    {"version":"2608211200-2222","hardwareModels":["h618-x98h"],"artifactUrl":"/updates/firmware/v2.bin","artifactSha256":"%s","sizeBytes":2,"minVersion":"2608201045-1111"}
  ]}`, FirmwareSchema, sha, sha, sha)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != FirmwareIndexPath {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, index)
	}))
	defer ts.Close()

	// 条目都没写 platform：按 linux-arm64 理解，所以这里把客户端也钉在 arm64 上——
	// 测试在 amd64 开发机与 arm64 板上都要同一结果。
	got, err := NewClient(ts.URL, fixedModel("h618-x98h"), WithPlatform("linux-arm64")).FirmwareCheck(context.Background())
	if err != nil {
		t.Fatalf("FirmwareCheck: %v", err)
	}
	if got.Current != "2608201045-1111" || !got.UpgradeAvailable || got.Latest == nil || got.Latest.Version != "2608211200-2222" {
		t.Fatalf("latest = %+v", got)
	}
	if got.Newest == nil || got.Newest.Version != "2608240815-4444" {
		t.Fatalf("newest = %+v", got.Newest)
	}
}

// TestFirmwareCheckFiltersPlatform 钉住双平台索引的筛法：同一版本 arm64 / amd64 两条并列，
// 设备只取本机平台那条；没写 platform 的条目算 linux-arm64；通用主机（型号空）只收
// 不限型号的条目。
func TestFirmwareCheckFiltersPlatform(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "2608201045-1111"
	t.Cleanup(func() { buildinfo.Version = old })
	shaARM, shaAMD := strings.Repeat("a", 64), strings.Repeat("b", 64)
	index := fmt.Sprintf(`{
  "schema":"%s","channel":"stable","releases":[
    {"version":"2609052022-4a8e","platform":"linux-arm64","artifactUrl":"/fw/arm64","artifactSha256":"%s","sizeBytes":9},
    {"version":"2609052022-4a8e","platform":"linux-amd64","artifactUrl":"/fw/amd64","artifactSha256":"%s","sizeBytes":8},
    {"version":"2609012045-ac25","artifactUrl":"/fw/legacy-arm64","artifactSha256":"%s","sizeBytes":7},
    {"version":"2609061200-cccc","platform":"linux-amd64","hardwareModels":["some-board"],"artifactUrl":"/fw/amd64-board","artifactSha256":"%s","sizeBytes":6}
  ]}`, FirmwareSchema, shaARM, shaAMD, shaARM, shaAMD)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, index)
	}))
	defer ts.Close()

	arm, err := NewClient(ts.URL, fixedModel("h618-x98h"), WithPlatform("linux-arm64")).FirmwareCheck(context.Background())
	if err != nil {
		t.Fatalf("arm64 FirmwareCheck: %v", err)
	}
	if arm.Latest == nil || arm.Latest.ArtifactURL != "/fw/arm64" || arm.Latest.ArtifactSHA256 != shaARM {
		t.Fatalf("arm64 latest = %+v", arm.Latest)
	}

	// 通用 x86-64 主机：型号空。拿 amd64 那条，不拿限了型号的 amd64 条目，也不拿 arm64 条目。
	amd, err := NewClient(ts.URL, FixedModel(""), WithPlatform("linux-amd64")).FirmwareCheck(context.Background())
	if err != nil {
		t.Fatalf("amd64 FirmwareCheck: %v", err)
	}
	if amd.Latest == nil || amd.Latest.ArtifactURL != "/fw/amd64" || amd.Latest.ArtifactSHA256 != shaAMD || amd.Latest.Platform != "linux-amd64" {
		t.Fatalf("amd64 latest = %+v", amd.Latest)
	}
	if amd.Newest == nil || amd.Newest.ArtifactURL != "/fw/amd64" {
		t.Fatalf("amd64 newest = %+v（限型号的条目不该给通用主机）", amd.Newest)
	}

	// 认得出型号的 amd64 主机能拿到限了它型号的条目。
	board, err := NewClient(ts.URL, fixedModel("some-board"), WithPlatform("linux-amd64")).FirmwareCheck(context.Background())
	if err != nil {
		t.Fatalf("board FirmwareCheck: %v", err)
	}
	if board.Latest == nil || board.Latest.ArtifactURL != "/fw/amd64-board" {
		t.Fatalf("board latest = %+v", board.Latest)
	}

	// 没有本机平台的条目：不报错，只是没有可升级版本。
	none, err := NewClient(ts.URL, FixedModel(""), WithPlatform("linux-riscv64")).FirmwareCheck(context.Background())
	if err != nil {
		t.Fatalf("riscv64 FirmwareCheck: %v", err)
	}
	if none.UpgradeAvailable || none.Latest != nil || none.Newest != nil {
		t.Fatalf("riscv64 = %+v", none)
	}

	// 缺省平台就是本机：官网索引里的现网条目在本机上按 HostPlatform 筛。
	if HostPlatform() != runtime.GOOS+"-"+runtime.GOARCH {
		t.Fatalf("HostPlatform = %q", HostPlatform())
	}
}

// TestCompareVersion 钉住版本名称的排序规则（firmware/Makefile 定义的
// YYMMDDHHMM-哈希）：十位时间戳是唯一的排序键，同一分钟的两个包互不构成升级。
// 点分整数的 vX.Y.Z 形状也收，用于清单里手写的 minVersion。
func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2608221732-2d81", "2608221629-1de9-d", 1},
		{"2608221629-1de9-d", "2608221732-2d81", -1},
		{"2608221732-1de9", "2608221732-2d81", 0},   // 同一分钟：哈希不分先后
		{"2608221732-1de9-d", "2608221732-1de9", 0}, // 脏标记同样不参与比较
		{"2608221733-1de9", "2608221732-2d81", 1},   // 分钟是最小可分辨粒度
		{"2601010000-0000", "v9.9.9", 1},            // 十位时间戳恒新过点分整数
		{"v1.0.0", "v0.9.1", 1},                     // 点分整数之间仍按段比
		{"v1.0.0", "v1.0.0", 0},
	}
	for _, tc := range cases {
		if got := compareVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersion(%q, %q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFirmwareDownloadHashesAndValidatesURL(t *testing.T) {
	payload := []byte("fake firmware bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/updates/firmware/v2.bin" {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	}))
	defer ts.Close()
	c := NewClient(ts.URL, nil)
	var dst bytes.Buffer
	sum, size, err := c.FetchFirmware(context.Background(), "/updates/firmware/v2.bin", &dst)
	if err != nil {
		t.Fatalf("FetchFirmware: %v", err)
	}
	wantSum := sha256.Sum256(payload)
	if sum != hex.EncodeToString(wantSum[:]) || size != int64(len(payload)) || !bytes.Equal(dst.Bytes(), payload) {
		t.Fatalf("sum=%q size=%d body=%q", sum, size, dst.Bytes())
	}
	for _, bad := range []string{"http://example.com/fw.bin", "https://user:pass@example.com/fw.bin", "ftp://example.com/fw.bin"} {
		if _, _, err := c.FetchFirmware(context.Background(), bad, io.Discard); err == nil {
			t.Errorf("不安全地址 %q 应被拒绝", bad)
		}
	}
}

func TestAppsRelayRestrictsPathAndBrowserHeaders(t *testing.T) {
	var seen *http.Request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		io.WriteString(w, "ok")
	}))
	defer ts.Close()
	c := NewClient(ts.URL, nil)
	h := make(http.Header)
	h.Set("Accept", "text/html")
	h.Set("If-None-Match", `"apps-v1"`)
	h.Set("Cookie", "admin=secret")
	h.Set("Accept-Language", "zh-CN")
	h.Set("User-Agent", "customer-browser")
	resp, err := c.OpenApps(context.Background(), AppsRequest{
		Method: http.MethodGet, Path: "/apps/assets/app.js", Query: "v=1", Header: h,
	})
	if err != nil {
		t.Fatalf("OpenApps: %v", err)
	}
	resp.Body.Close()
	if seen == nil || seen.URL.Path != "/apps/assets/app.js" || seen.URL.RawQuery != "v=1" {
		t.Fatalf("seen = %+v", seen)
	}
	if seen.Header.Get("Accept") != "text/html" || seen.Header.Get("If-None-Match") != `"apps-v1"` {
		t.Fatalf("白名单头未转发: %v", seen.Header)
	}
	for _, key := range []string{"Cookie", "Accept-Language", "User-Agent"} {
		if seen.Header.Get(key) != "" {
			t.Errorf("%s 不应转发，got %q", key, seen.Header.Get(key))
		}
	}
	for _, bad := range []string{"/", "/admin/", "/apps", "/apps/../admin/", "/apps//asset.js"} {
		if _, err := c.OpenApps(context.Background(), AppsRequest{Method: http.MethodGet, Path: bad}); err != ErrAppsPath {
			t.Errorf("path %q err=%v，期望 ErrAppsPath", bad, err)
		}
	}
}
