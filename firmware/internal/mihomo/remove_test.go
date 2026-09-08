// remove_test.go 钉住组件卸载的守卫：内核启用中 Remove 答 ErrComponentInUse 且不动引擎；
// 停用后卸载成功、读数回到 not_installed，订阅与节点选择保留（「第三方组件」页只管组件本身）。
package mihomo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoveRefusesWhileEnabled(t *testing.T) {
	s := newSigner(t)
	elf := fakeELF("1.19.30")
	gz := gzipBytes(t, elf)
	raw, sig := s.sign(t, index(1, release("1.19.30", gz, elf)))
	st := newFakeSettings()
	eng := &fakeEngine{}
	eg := &fakeEgress{core: true}
	sub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, subscriptionFixture)
	}))
	defer sub.Close()
	m := NewManager(Options{
		DataDir: t.TempDir(), Settings: st, Engine: eng, Egress: eg, Keys: s.keys,
		Website: &fakeWebsite{index: raw, sig: sig, artifact: gz},
		Fetch:   sub.Client(),
	})
	ctx := context.Background()

	// 没装过也能「卸载」：引擎幂等，读数仍是 not_installed。
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("空卸载: %v", err)
	}
	if _, err := m.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Download(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetSubscription(ctx, sub.URL+"/subscribe/1/token-secret/clash/"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(ctx, true); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := m.Remove(ctx); !errors.Is(err, ErrComponentInUse) {
		t.Fatalf("启用中卸载应答 ErrComponentInUse: %v", err)
	}
	if st := m.Status(ctx); !st.Enabled || !st.Component.Installed || !eng.running {
		t.Fatalf("被拒的卸载不该动内核或组件: enabled=%v installed=%v running=%v", st.Enabled, st.Component.Installed, eng.running)
	}
	if err := m.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatalf("停用后卸载: %v", err)
	}
	st2 := m.Status(ctx)
	if st2.Component.Installed || st2.Component.State != "not_installed" {
		t.Fatalf("卸载后读数: %+v", st2.Component)
	}
	if !st2.Subscription.Set || len(st2.Nodes) == 0 {
		t.Fatalf("卸载不应清订阅与节点: %+v", st2.Subscription)
	}
	if err := m.Enable(ctx, true); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("卸载后再启用应差组件: %v", err)
	}
	if strings.Contains(ErrComponentInUse.Error(), "token") {
		t.Fatal("哨兵错误文本不该提凭据")
	}
}
