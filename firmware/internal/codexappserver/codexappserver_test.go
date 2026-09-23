package codexappserver

// Codex App Server 组件的离线验收：签名清单（两种官方地址、tar.gz 声明、许可证、防回退）、
// 整包解包守卫（布局外文件 / 越界路径 / 符号链接 / 缺入口 / 缺自述 / 非 ELF / 超长都拒）、
// 管理器全流程（检查清单 → 下载或上传 → 安装 → 升级 → 回退 → 卸载），假引擎 / 假官网，全部离线。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// ---- 夹具 ----

// fakeELF 是 elfcheck 认的本机架构 ELF 头（与 cloudflared / mihomo 测试同一造法）。
func fakeELF(version string) []byte {
	b := make([]byte, 64+len(version))
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b[16], b[17] = 2, 0 // e_type EXEC
	switch HostPlatform() {
	case "linux-arm64":
		b[18], b[19] = 0xb7, 0
	default:
		b[18], b[19] = 0x3e, 0
	}
	b[20] = 1
	copy(b[64:], version)
	return b
}

type tarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
	mode     int64 // 0 = 0755
}

func tgzBytes(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o755
		}
		hdr := &tar.Header{Name: e.name, Mode: mode, Size: int64(len(e.body)), Typeflag: tf, Linkname: e.linkname}
		if tf != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// packageMetaJSON 是官方包自述 codex-package.json 的正文。
func packageMetaJSON(version string) []byte {
	return []byte(`{"layoutVersion":1,"name":"codex-app-server","version":"` + version + `","entrypoint":"bin/codex-app-server","resourcesDir":"codex-resources","pathDir":"codex-path"}`)
}

// packageEntries 是官方整包的目录树：入口 ELF、code-mode host、自述、rg、bwrap、zsh。
func packageEntries(version string, elf []byte) []tarEntry {
	return []tarEntry{
		{name: "bin/", typeflag: tar.TypeDir},
		{name: "bin/codex-app-server", body: elf},
		{name: "bin/codex-code-mode-host", body: fakeELF("host-" + version)},
		{name: "codex-package.json", body: packageMetaJSON(version), mode: 0o644},
		{name: "codex-path/", typeflag: tar.TypeDir},
		{name: "codex-path/rg", body: fakeELF("rg")},
		{name: "codex-resources/", typeflag: tar.TypeDir},
		{name: "codex-resources/bwrap", body: fakeELF("bwrap")},
		{name: "codex-resources/zsh/bin/", typeflag: tar.TypeDir},
		{name: "codex-resources/zsh/bin/zsh", body: fakeELF("zsh")},
	}
}

// officialTGZ 是官方形态的整包：version 写进 codex-package.json，elf 是入口文件正文。
func officialTGZ(t *testing.T, version string, elf []byte) []byte {
	t.Helper()
	return tgzBytes(t, packageEntries(version, elf)...)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type signer struct {
	priv ed25519.PrivateKey
	keys map[string]ed25519.PublicKey
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{priv: priv, keys: map[string]ed25519.PublicKey{"test-key": pub}}
}

func (s *signer) sign(t *testing.T, idx Index) (raw, sig []byte) {
	t.Helper()
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	return raw, s.signRaw(raw)
}

func (s *signer) signRaw(raw []byte) []byte {
	sf := signatureFile{Schema: SignatureSchema, KeyID: "test-key", Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, raw))}
	sig, _ := json.Marshal(sf)
	return sig
}

func officialURL(version string) string {
	return "https://releases.openai.com/codex/releases/" + version + "/" + AssetName(HostPlatform()) + ".tar.gz"
}

func release(version string, tgz, elf []byte) Release {
	return Release{
		Version: version, Platform: HostPlatform(),
		ArtifactURL:    officialURL(version),
		ArtifactSHA256: sum(tgz), SizeBytes: int64(len(tgz)), Packaging: "tar.gz",
		UnpackedSHA256: sum(elf), UnpackedSizeBytes: int64(len(elf)),
		License: "Apache-2.0", SourceURL: "https://github.com/openai/codex/tree/rust-v" + version,
		MinComponentManager: ComponentManagerVersion, AllowInstall: true, AllowUpdate: true,
	}
}

func index(rev int64, rels ...Release) Index {
	return Index{Schema: IndexSchema, Component: ComponentName, Channel: "stable", Revision: rev, UpdatedAt: "2026-09-10T00:00:00Z", Releases: rels}
}

type fakeSettings struct {
	mu    sync.Mutex
	plain map[string]string
}

func newFakeSettings() *fakeSettings { return &fakeSettings{plain: map[string]string{}} }

func (f *fakeSettings) GetSetting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plain[k], nil
}

func (f *fakeSettings) SetSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[k] = v
	return nil
}

type fakeEngine struct {
	mu        sync.Mutex
	installed *updated.ComponentSlot
	previous  *updated.ComponentSlot
	names     []string
}

func (f *fakeEngine) status() *updated.ComponentStatus {
	st := &updated.ComponentStatus{Name: ComponentName}
	if f.installed != nil {
		st.Installed, st.CurrentSlot, st.Current = true, "a", f.installed
	}
	if f.previous != nil {
		st.PreviousSlot, st.Previous = "b", f.previous
	}
	return st
}

func (f *fakeEngine) ComponentStatus(_ context.Context, name string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	return f.status(), nil
}

func (f *fakeEngine) ComponentInstall(_ context.Context, name string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	// 引擎收到的是目录包：入口在 bin/codex-app-server，旁边必须有 code-mode host 与自述。
	raw, err := os.ReadFile(req.Path + "/bin/codex-app-server")
	if err != nil {
		return nil, err
	}
	for _, sibling := range []string{"/bin/codex-code-mode-host", "/codex-package.json", "/codex-resources/bwrap", "/codex-path/rg"} {
		if _, err := os.Stat(req.Path + sibling); err != nil {
			return nil, err
		}
	}
	if sum(raw) != req.SHA256 {
		return nil, errors.New("摘要不符")
	}
	f.previous = f.installed
	f.installed = &updated.ComponentSlot{Version: req.Version, SHA256: req.SHA256, SizeBytes: int64(len(raw))}
	return f.status(), nil
}

func (f *fakeEngine) ComponentRollback(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.previous == nil {
		return nil, &updated.EngineError{Status: 409, Message: updated.ErrNoPreviousSlot.Error()}
	}
	f.installed, f.previous = f.previous, f.installed
	return f.status(), nil
}

func (f *fakeEngine) ComponentRemove(context.Context, string) (*updated.ComponentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed, f.previous = nil, nil
	return f.status(), nil
}

type fakeWebsite struct {
	index, sig []byte
	artifact   []byte
	component  string
	policy     officialsite.ArtifactPolicy
}

func (w *fakeWebsite) FetchComponentIndex(_ context.Context, component string) ([]byte, []byte, error) {
	w.component = component
	if w.index == nil {
		return nil, nil, errors.New("无法连接 LLM Gate官网")
	}
	return w.index, w.sig, nil
}

func (w *fakeWebsite) FetchComponentArtifact(_ context.Context, _ string, pol officialsite.ArtifactPolicy, out io.Writer) (string, int64, error) {
	w.policy = pol
	if int64(len(w.artifact)) > pol.MaxBytes {
		return "", 0, errors.New("超过上限")
	}
	n, err := out.Write(w.artifact)
	return sum(w.artifact), int64(n), err
}

// ---- 清单 ----

func TestManifestPolicy(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("0.154.0")
	tgz := officialTGZ(t, "0.154.0", elf)
	good := release("0.154.0", tgz, elf)

	raw, sig := s.sign(t, index(1, good))
	v, err := ParseIndex(raw, sig, s.keys)
	if err != nil {
		t.Fatalf("合法清单被拒: %v", err)
	}
	if v.Index.Releases[0].Version != "0.154.0" || v.KeyID != "test-key" {
		t.Fatalf("解析结果不对: %+v", v.Index)
	}
	// GitHub 镜像地址同样是官方来源。
	gh := good
	gh.ArtifactURL = "https://github.com/openai/codex/releases/download/rust-v0.154.0/" + AssetName(HostPlatform()) + ".tar.gz"
	raw, sig = s.sign(t, index(1, gh))
	if _, err := ParseIndex(raw, sig, s.keys); err != nil {
		t.Fatalf("GitHub 官方地址应放行: %v", err)
	}

	bad := map[string]func(r *Release){
		"其他主机": func(r *Release) {
			r.ArtifactURL = "https://example.com/codex/releases/0.154.0/" + AssetName(HostPlatform()) + ".tar.gz"
		},
		"版本目录不符": func(r *Release) { r.ArtifactURL = strings.Replace(r.ArtifactURL, "/0.154.0/", "/0.153.0/", 1) },
		"单文件 asset": func(r *Release) {
			r.ArtifactURL = "https://releases.openai.com/codex/releases/0.154.0/codex-app-server-x86_64-unknown-linux-musl.tar.gz"
		},
		"别的 asset": func(r *Release) {
			r.ArtifactURL = "https://releases.openai.com/codex/releases/0.154.0/codex-x86_64-unknown-linux-musl.tar.gz"
		},
		"zst 包":   func(r *Release) { r.ArtifactURL = strings.TrimSuffix(r.ArtifactURL, ".tar.gz") + ".zst" },
		"带 query": func(r *Release) { r.ArtifactURL += "?x=1" },
		"http":    func(r *Release) { r.ArtifactURL = "http" + strings.TrimPrefix(r.ArtifactURL, "https") },
		"许可证":     func(r *Release) { r.License = "MIT" },
		"打包形态":    func(r *Release) { r.Packaging = "gzip" },
		"版本形态":    func(r *Release) { r.Version = "rust-v0.154.0" },
		"平台":      func(r *Release) { r.Platform = "linux-386" },
		"摘要大写":    func(r *Release) { r.ArtifactSHA256 = strings.ToUpper(r.ArtifactSHA256) },
		"制品过大":    func(r *Release) { r.SizeBytes = maxArtifactBytes + 1 },
		"解包过大":    func(r *Release) { r.UnpackedSizeBytes = maxUnpackedBytes + 1 },
		"缺管理器版本":  func(r *Release) { r.MinComponentManager = 0 },
	}
	for name, mutate := range bad {
		r := good
		mutate(&r)
		raw, sig := s.sign(t, index(1, r))
		if _, err := ParseIndex(raw, sig, s.keys); err == nil {
			t.Errorf("%s 应被拒", name)
		} else {
			var me *ManifestError
			if !errors.As(err, &me) {
				t.Errorf("%s 应是 ManifestError: %v", name, err)
			}
		}
	}
	// 组件名 / 通道 / 签名。
	other := index(1, good)
	other.Component = "mihomo"
	raw, sig = s.sign(t, other)
	if _, err := ParseIndex(raw, sig, s.keys); err == nil {
		t.Error("别的组件的清单应被拒")
	}
	raw, sig = s.sign(t, index(1, good))
	if _, err := ParseIndex(append(raw, ' '), sig, s.keys); err == nil {
		t.Error("篡改正文应验签失败")
	}
	if _, err := ParseIndex(raw, sig, map[string]ed25519.PublicKey{}); err == nil {
		t.Error("不认识的 key 应被拒")
	}
	// 建议筛选：未装取最高可装版本；已装 0.154.0 时只有更高版本才是 latest；阻断版本只进 newest。
	newer := release("0.155.0", tgz, elf)
	blocked := release("0.156.0", tgz, elf)
	blocked.Blocked = true
	idx := index(2, good, newer, blocked)
	adv := idx.selectRelease(HostPlatform(), "")
	if adv.Latest == nil || adv.Latest.Version != "0.155.0" || adv.Newest == nil || adv.Newest.Version != "0.156.0" {
		t.Fatalf("未装时建议不对: %+v", adv)
	}
	adv = idx.selectRelease(HostPlatform(), "0.155.0")
	if adv.Latest != nil || adv.InstalledBlocked {
		t.Fatalf("已装最新时不该再建议: %+v", adv)
	}
	adv = idx.selectRelease(HostPlatform(), "0.156.0")
	if !adv.InstalledBlocked {
		t.Fatal("已装被阻断版本应标 installed_blocked")
	}
	future := release("0.157.0", tgz, elf)
	future.MinComponentManager = ComponentManagerVersion + 1
	idx3 := index(3, good, future)
	adv = idx3.selectRelease(HostPlatform(), "0.154.0")
	if adv.Latest != nil || !adv.NeedsFirmware {
		t.Fatalf("要求更高管理器版本的条目应标 needs_firmware: %+v", adv)
	}
}

// ---- 解包守卫 ----

func TestExtractPackageGuards(t *testing.T) {
	elf := fakeELF("0.154.0")
	write := func(b []byte) string {
		p := t.TempDir() + "/a.tgz"
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	extract := func(b []byte, limit int64) (*packageResult, error) {
		return extractPackage(write(b), t.TempDir()+"/out", "0.154.0", limit)
	}
	// 官方形态：整棵树；条目带 ./ 前缀、没有目录条目也认。
	good := packageEntries("0.154.0", elf)
	dotted := make([]tarEntry, 0, len(good))
	filesOnly := make([]tarEntry, 0, len(good))
	for _, e := range good {
		d := e
		d.name = "./" + e.name
		dotted = append(dotted, d)
		if e.typeflag != tar.TypeDir {
			filesOnly = append(filesOnly, e)
		}
	}
	for name, entries := range map[string][]tarEntry{"官方": good, "./ 前缀": dotted, "无目录条目": filesOnly} {
		dst := t.TempDir() + "/out"
		res, err := extractPackage(write(tgzBytes(t, entries...)), dst, "0.154.0", int64(len(elf)))
		if err != nil || res.EntrySize != int64(len(elf)) || res.EntrySHA256 != sum(elf) || res.Files != 6 {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		// 解出来的文件只对本进程可读；可执行文件保留执行位（引擎按它定 0755/0644）。
		info, err := os.Stat(dst + "/bin/codex-code-mode-host")
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: code-mode host 权限 %v %v", name, info, err)
		}
		if info, err := os.Stat(dst + "/codex-package.json"); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: 自述权限 %v %v", name, info, err)
		}
		if info, err := os.Stat(dst + "/codex-resources/zsh/bin/zsh"); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: zsh 权限 %v %v", name, info, err)
		}
	}
	without := func(name string) []tarEntry {
		var out []tarEntry
		for _, e := range good {
			if e.name != name {
				out = append(out, e)
			}
		}
		return out
	}
	with := func(extra ...tarEntry) []tarEntry { return append(append([]tarEntry{}, good...), extra...) }
	cases := map[string][]byte{
		"包根多余文件":     tgzBytes(t, with(tarEntry{name: "README", body: []byte("x")})...),
		"布局外目录":      tgzBytes(t, with(tarEntry{name: "lib/x.so", body: fakeELF("x")})...),
		"越界路径":       tgzBytes(t, with(tarEntry{name: "bin/../../x", body: []byte("x")})...),
		"绝对路径":       tgzBytes(t, with(tarEntry{name: "/etc/passwd", body: []byte("x")})...),
		"符号链接":       tgzBytes(t, with(tarEntry{name: "bin/sh", typeflag: tar.TypeSymlink, linkname: "/bin/sh"})...),
		"重复文件":       tgzBytes(t, with(tarEntry{name: "bin/codex-app-server", body: elf})...),
		"缺入口":        tgzBytes(t, without("bin/codex-app-server")...),
		"缺自述":        tgzBytes(t, without("codex-package.json")...),
		"自述版本不符":     tgzBytes(t, append(without("codex-package.json"), tarEntry{name: "codex-package.json", body: packageMetaJSON("0.153.0"), mode: 0o644})...),
		"自述布局版本":     tgzBytes(t, append(without("codex-package.json"), tarEntry{name: "codex-package.json", body: []byte(`{"layoutVersion":2,"version":"0.154.0","entrypoint":"bin/codex-app-server"}`), mode: 0o644})...),
		"自述入口不符":     tgzBytes(t, append(without("codex-package.json"), tarEntry{name: "codex-package.json", body: []byte(`{"layoutVersion":1,"version":"0.154.0","entrypoint":"bin/codex"}`), mode: 0o644})...),
		"伴侣不是 ELF":   tgzBytes(t, append(without("bin/codex-code-mode-host"), tarEntry{name: "bin/codex-code-mode-host", body: []byte("#!/bin/sh\n")})...),
		"单文件旧 asset": tgzBytes(t, tarEntry{name: "codex-app-server-" + HostPlatform(), body: elf}),
		"空包":         tgzBytes(t),
		"不是 gz":      []byte("plain"),
	}
	for name, b := range cases {
		if _, err := extract(b, int64(len(elf))); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	// 入口超过声明长度：封顶失败，不把整个文件解出来。
	if _, err := extract(tgzBytes(t, good...), int64(len(elf))-1); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超长应拒: %v", err)
	}
}

// ---- 管理器 ----

func TestManagerFlow(t *testing.T) {
	s := newSigner(t)
	elf1, elf2 := fakeELF("0.154.0"), fakeELF("0.155.0")
	tgz1, tgz2 := officialTGZ(t, "0.154.0", elf1), officialTGZ(t, "0.155.0", elf2)
	raw, sig := s.sign(t, index(1, release("0.154.0", tgz1, elf1)))
	st := newFakeSettings()
	eng := &fakeEngine{}
	web := &fakeWebsite{index: raw, sig: sig, artifact: tgz1}
	dir := t.TempDir()
	m := NewManager(Options{DataDir: dir, Settings: st, Website: web, Engine: eng, Keys: s.keys})
	ctx := context.Background()

	if got := m.Status(ctx); got.State != "not_installed" || !got.WebsiteEnabled || !got.EngineAvailable || got.Advisory != nil {
		t.Fatalf("初始读数: %+v", got)
	}
	if err := m.Download(ctx); !errors.Is(err, ErrNoAdvisory) {
		t.Fatalf("没有清单就下载应拒: %v", err)
	}
	if _, err := m.Upload(bytes.NewReader(tgz1)); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("没有清单就上传应拒: %v", err)
	}
	adv, err := m.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if web.component != ComponentName {
		t.Fatalf("应按组件名取清单: %q", web.component)
	}
	if adv.Latest == nil || adv.Latest.Version != "0.154.0" || adv.Source != "website" {
		t.Fatalf("建议不对: %+v", adv)
	}
	if got := m.Status(ctx); got.State != "not_installed" || got.Advisory == nil || got.Advisory.Latest == nil {
		t.Fatalf("有清单后读数: %+v", got)
	}
	if err := m.Download(ctx); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if web.policy.MaxBytes != int64(len(tgz1)) || len(web.policy.AllowedHosts) == 0 || web.policy.AllowedHosts[0] != "releases.openai.com" {
		t.Fatalf("下载策略不对: %+v", web.policy)
	}
	staged, ok := m.StagedManifest()
	if !ok || staged.Version != "0.154.0" || staged.SHA256 != sum(elf1) || staged.SizeBytes != int64(len(elf1)) || staged.Source != "website" || staged.Files != 6 {
		t.Fatalf("staging 不对: %+v %v", staged, ok)
	}
	stagingDir := dir + "/components/" + ComponentName + "/staging"
	if info, err := os.Stat(stagingDir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("staging 应是 0700 目录: %v %v", info, err)
	}
	if info, err := os.Stat(stagingDir + "/bin/codex-app-server"); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("staging 入口文件不该对他人可读: %v %v", info, err)
	}
	if _, err := os.Stat(dir + "/components/" + ComponentName + "/unpack.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("解包临时目录应已换成 staging: %v", err)
	}
	if got := m.Status(ctx); got.State != "staged" {
		t.Fatalf("staged 读数: %+v", got)
	}
	comp, err := m.Install(ctx)
	if err != nil || !comp.Installed || comp.Current.Version != "0.154.0" {
		t.Fatalf("Install: %+v %v", comp, err)
	}
	if _, ok := m.StagedManifest(); ok {
		t.Fatal("安装成功应丢弃 staging")
	}
	if _, err := os.Stat(stagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("丢弃 staging 应删整个目录: %v", err)
	}
	if got := m.Status(ctx); got.State != "installed" || got.Version != "0.154.0" || got.Advisory.Latest != nil {
		t.Fatalf("安装后读数: %+v", got)
	}
	// 引擎收到的组件名恒为 codex-app-server。
	for _, n := range eng.names {
		if n != ComponentName {
			t.Fatalf("引擎收到别的组件名: %q", n)
		}
	}

	// 新清单：0.155.0 可更新；同 revision 换内容被拒；revision 回退被拒。
	raw2, sig2 := s.sign(t, index(2, release("0.154.0", tgz1, elf1), release("0.155.0", tgz2, elf2)))
	if _, err := m.ImportManifest(ctx, raw2, sig2); err != nil {
		t.Fatalf("导入 revision 2: %v", err)
	}
	if got := m.Status(ctx); got.State != "update_available" || got.Advisory.Latest.Version != "0.155.0" || got.Advisory.Source != "upload" {
		t.Fatalf("可更新读数: %+v", got)
	}
	raw2b, sig2b := s.sign(t, index(2, release("0.155.0", tgz2, elf2)))
	if _, err := m.ImportManifest(ctx, raw2b, sig2b); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("同 revision 换内容应拒: %v", err)
	}
	if _, err := m.ImportManifest(ctx, raw, sig); !errors.Is(err, ErrManifestStale) {
		t.Fatalf("revision 回退应拒: %v", err)
	}
	// 手动上传：不在清单的包拒；命中 0.155.0 的包就绪。
	if _, err := m.Upload(bytes.NewReader(officialTGZ(t, "9.9.9", fakeELF("9.9.9")))); !errors.Is(err, ErrUploadNotListed) {
		t.Fatalf("不在清单的上传应拒: %v", err)
	}
	up, err := m.Upload(bytes.NewReader(tgz2))
	if err != nil || up.Version != "0.155.0" || up.Source != "upload" {
		t.Fatalf("Upload: %+v %v", up, err)
	}
	comp, err = m.Install(ctx)
	if err != nil || comp.Current.Version != "0.155.0" || comp.Previous == nil || comp.Previous.Version != "0.154.0" {
		t.Fatalf("升级: %+v %v", comp, err)
	}
	if got := m.Status(ctx); got.State != "installed" || got.PreviousVersion != "0.154.0" {
		t.Fatalf("升级后读数: %+v", got)
	}
	comp, err = m.Rollback(ctx)
	if err != nil || comp.Current.Version != "0.154.0" {
		t.Fatalf("回退: %+v %v", comp, err)
	}
	// 卸载没有守卫：直接删槽位并丢弃 staging；清单保留。
	web.artifact = tgz2
	if err := m.Download(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := m.Status(ctx)
	if got.Installed || got.State != "not_installed" || got.Staged != nil || got.Advisory == nil || got.Advisory.Revision != 2 {
		t.Fatalf("卸载后读数: %+v", got)
	}
	// 再来一次「空卸载」幂等。
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("空卸载: %v", err)
	}
	// 重新装配：已存清单从磁盘恢复。
	m2 := NewManager(Options{DataDir: dir, Settings: st, Engine: eng, Keys: s.keys})
	if got := m2.Status(ctx); got.WebsiteEnabled || got.Advisory == nil || got.Advisory.Source != "stored" {
		t.Fatalf("重装配读数: %+v", got)
	}
}

func TestManagerRejectsCorruptArtifacts(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("0.154.0")
	tgz := officialTGZ(t, "0.154.0", elf)
	rel := release("0.154.0", tgz, elf)
	// 清单声明的解包摘要与包内不符：下载后 staging 不成立。
	wrong := rel
	wrong.UnpackedSHA256 = sum([]byte("other"))
	raw, sig := s.sign(t, index(1, wrong))
	m := NewManager(Options{DataDir: t.TempDir(), Settings: newFakeSettings(), Engine: &fakeEngine{},
		Website: &fakeWebsite{index: raw, sig: sig, artifact: tgz}, Keys: s.keys})
	ctx := context.Background()
	if _, err := m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Download(ctx); err == nil || !strings.Contains(err.Error(), "解包后摘要") {
		t.Fatalf("解包摘要不符应拒: %v", err)
	}
	if _, ok := m.StagedManifest(); ok {
		t.Fatal("被拒的制品不该就绪")
	}
	if got := m.Status(ctx); got.DownloadError == "" || got.State != "not_installed" {
		t.Fatalf("失败后读数: %+v", got)
	}
	// 入口不是 ELF：形状闸门拒。
	notELF := officialTGZ(t, "0.154.0", []byte("#!/bin/sh\n"))
	rel2 := release("0.154.0", notELF, []byte("#!/bin/sh\n"))
	raw, sig = s.sign(t, index(2, rel2))
	if _, err := m.ImportManifest(ctx, raw, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upload(bytes.NewReader(notELF)); err == nil {
		t.Fatal("非 ELF 入口应被拒")
	}
	// 只有单个 codex-app-server 文件的旧 asset 形态：命中清单也装不进去（缺 code-mode host 等伴侣）。
	single := tgzBytes(t, tarEntry{name: "codex-app-server-" + HostPlatform(), body: elf})
	rel3 := release("0.154.0", single, elf)
	raw, sig = s.sign(t, index(3, rel3))
	if _, err := m.ImportManifest(ctx, raw, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upload(bytes.NewReader(single)); err == nil || !strings.Contains(err.Error(), "官方包") {
		t.Fatalf("单文件包应被拒: %v", err)
	}
	if _, err := m.Install(ctx); !errors.Is(err, ErrNoStaged) {
		t.Fatalf("没有就绪制品的安装应拒: %v", err)
	}
}
