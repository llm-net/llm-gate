package update

// 升级管理器的可执行验收：advisory 情报流、下载/上传汇合校验（摘要锚 +
// 包内版本）、引擎转发与 staged 对齐。官网与引擎都是假实现，全程离线。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/fwimage"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

type fakeWebsite struct {
	check *officialsite.FirmwareCheckResult
	image []byte
	err   error
}

func (f *fakeWebsite) FirmwareCheck(context.Context) (*officialsite.FirmwareCheckResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.check, nil
}
func (f *fakeWebsite) FetchFirmware(_ context.Context, _ string, w io.Writer) (string, int64, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	if _, err := w.Write(f.image); err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(f.image)
	return hex.EncodeToString(sum[:]), int64(len(f.image)), nil
}

type fakeEngine struct {
	installs []updated.InstallRequest
	status   *updated.Status
	err      error
}

func (f *fakeEngine) Status(context.Context) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}
func (f *fakeEngine) Install(_ context.Context, req updated.InstallRequest) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.installs = append(f.installs, req)
	return f.status, nil
}
func (f *fakeEngine) Rollback(context.Context) (*updated.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}

func newTestManager(t *testing.T, site *fakeWebsite, engine *fakeEngine, version string) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), site, engine, slog.New(slog.DiscardHandler))
	m.verify = func(string) (*fwimage.Info, error) { return &fwimage.Info{Version: version}, nil }
	return m
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownloadStagesVerifiedImage(t *testing.T) {
	image := []byte("firmware-bytes")
	site := &fakeWebsite{
		image: image,
		check: &officialsite.FirmwareCheckResult{
			UpgradeAvailable: true,
			Latest: &officialsite.FirmwareRelease{
				Version: "v1.2.3", ArtifactURL: "/updates/firmware/llmgate-v1.2.3-linux-arm64",
				ArtifactSHA256: digestOf(image),
			},
		},
	}
	m := newTestManager(t, site, &fakeEngine{}, "v1.2.3")

	if err := m.Download(context.Background()); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("没有情报时应拒绝下载，得到 %v", err)
	}
	if _, err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStaged(t, m)
	man, ok := m.Staged()
	if !ok || man.Version != "v1.2.3" || man.Source != "website" || man.SHA256 != digestOf(image) {
		t.Fatalf("staged 清单不对: %+v ok=%v", man, ok)
	}
}

func TestDownloadRejectsDigestMismatch(t *testing.T) {
	image := []byte("firmware-bytes")
	site := &fakeWebsite{
		image: image,
		check: &officialsite.FirmwareCheckResult{
			UpgradeAvailable: true,
			Latest: &officialsite.FirmwareRelease{
				Version: "v1.2.3", ArtifactURL: "/x",
				ArtifactSHA256: digestOf([]byte("something-else")),
			},
		},
	}
	m := newTestManager(t, site, &fakeEngine{}, "v1.2.3")
	if _, err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Download(context.Background()); err == nil {
		t.Fatal("摘要不符应报错")
	}
	if _, ok := m.Staged(); ok {
		t.Fatal("摘要不符不得留下 staged 包")
	}
	if _, msg := m.Downloading(); msg == "" {
		t.Fatal("失败原因应留在状态里")
	}
}

func TestUploadDemandsEmbeddedVersion(t *testing.T) {
	m := newTestManager(t, &fakeWebsite{}, &fakeEngine{}, "")
	if _, err := m.Upload(bytes.NewReader([]byte("some-binary"))); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("包内无版本应拒绝，得到 %v", err)
	}
	m2 := newTestManager(t, &fakeWebsite{}, &fakeEngine{}, "v2.0.0")
	man, err := m2.Upload(bytes.NewReader([]byte("some-binary")))
	if err != nil {
		t.Fatal(err)
	}
	if man.Version != "v2.0.0" || man.Source != "upload" {
		t.Fatalf("上传清单不对: %+v", man)
	}
}

func TestInstallForwardsStagedToEngine(t *testing.T) {
	engine := &fakeEngine{status: &updated.Status{Phase: updated.PhaseInstalling, Busy: true}}
	m := newTestManager(t, &fakeWebsite{}, engine, "v2.0.0")
	if _, err := m.Install(context.Background()); !errors.Is(err, ErrNoStaged) {
		t.Fatalf("无 staged 包应拒绝安装，得到 %v", err)
	}
	if _, err := m.Upload(bytes.NewReader([]byte("bin"))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.installs) != 1 || engine.installs[0].Version != "v2.0.0" || engine.installs[0].SHA256 != digestOf([]byte("bin")) {
		t.Fatalf("转发给引擎的请求不对: %+v", engine.installs)
	}
}

func TestReconcileClearsInstalledStaged(t *testing.T) {
	m := newTestManager(t, &fakeWebsite{}, &fakeEngine{}, "v2.0.0")
	if _, err := m.Upload(bytes.NewReader([]byte("bin"))); err != nil {
		t.Fatal(err)
	}
	m.Reconcile(&updated.Status{LastResult: &updated.Result{
		Kind: updated.KindInstall, Outcome: updated.OutcomeSuccess, ToVersion: "v2.0.0",
	}})
	if _, ok := m.Staged(); ok {
		t.Fatal("安装成功后 staged 包应被清理")
	}
	// 回退结局不清 staged（那可能是一份还没装的新包）。
	if _, err := m.Upload(bytes.NewReader([]byte("bin"))); err != nil {
		t.Fatal(err)
	}
	m.Reconcile(&updated.Status{LastResult: &updated.Result{
		Kind: updated.KindRollback, Outcome: updated.OutcomeSuccess, ToVersion: "v1.0.0",
	}})
	if _, ok := m.Staged(); !ok {
		t.Fatal("回退结局不应清掉 staged 包")
	}
}

func waitStaged(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if busy, _ := m.Downloading(); !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("下载未在期限内结束")
}
