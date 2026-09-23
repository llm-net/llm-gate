package agenthost_test

// 纳管流程的离线验收：假主机在进程内跑真 SSH（internal/agenthost/agenthosttest），
// 设备侧走完整的「拨号 → 认证 → 装公钥 → 配免密 sudo → 用证书复验」。
// 不出网、不起外部进程、不碰本机 SSH 配置。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const testPassword = "n0t-a-real-password"

// fleet 按地址把拨号路由到各台假主机，并能把某一台标成「连不上」。
type fleet struct {
	mu    sync.Mutex
	hosts map[string]*agenthosttest.Server
	down  map[string]bool
}

func newFleet() *fleet {
	return &fleet{hosts: map[string]*agenthosttest.Server{}, down: map[string]bool{}}
}

func (f *fleet) add(name string, s *agenthosttest.Server) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[name] = s
}

func (f *fleet) setDown(name string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[name] = down
}

func (f *fleet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	srv, ok := f.hosts[host]
	down := f.down[host]
	f.mu.Unlock()
	if !ok || down {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}
	return srv.Dial(ctx, network, address)
}

type env struct {
	t     *testing.T
	st    *store.Store
	mgr   *agenthost.Manager
	fleet *fleet
	buf   *bytes.Buffer
	dir   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	buf := &bytes.Buffer{}
	f := newFleet()
	return &env{
		t:   t,
		st:  st,
		dir: dir,
		buf: buf,
		mgr: agenthost.New(agenthost.Options{
			Store:  st,
			Logger: logging.New(buf, slog.LevelDebug),
			Dial:   f.dial,
		}),
		fleet: f,
	}
}

// host 起一台假主机并登记到路由表。
func (e *env) host(name string, opt agenthosttest.Options) *agenthosttest.Server {
	e.t.Helper()
	if opt.Password == "" {
		opt.Password = testPassword
	}
	s := agenthosttest.New(e.t, opt)
	e.fleet.add(name, s)
	return s
}

func (e *env) cert() *agenthost.Certificate {
	e.t.Helper()
	c, err := e.mgr.GenerateCertificate(context.Background())
	if err != nil {
		e.t.Fatalf("生成证书: %v", err)
	}
	return c
}

func (e *env) enroll(address, name string, sudo bool) (*store.AgentHost, error) {
	return e.mgr.Enroll(context.Background(), agenthost.EnrollRequest{
		Name: name, Kind: store.AgentHostKindWorker, Address: address, Port: 22, Username: "admin",
		Password: testPassword, ConfigureSudo: sudo,
	})
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *agenthost.Error
	if !errors.As(err, &e) {
		t.Fatalf("错误不是 *agenthost.Error: %v", err)
	}
	return e.Code
}

func TestEnrollInstallsKeyAndConfiguresSudo(t *testing.T) {
	e := newEnv(t)
	srv := e.host("board1", agenthosttest.Options{Username: "admin"})
	cert := e.cert()

	host, err := e.enroll("board1", "板子一", true)
	if err != nil {
		t.Fatalf("纳管: %v", err)
	}
	if host.Status != store.AgentHostStatusReady || host.LastError != "" {
		t.Fatalf("状态 = %q，错误 = %q", host.Status, host.LastError)
	}
	if !srv.HasKey(cert.PublicKey) {
		t.Fatal("公钥没装到主机上")
	}
	if host.KeyFingerprint != cert.Fingerprint {
		t.Fatalf("已装指纹 = %q，证书 = %q", host.KeyFingerprint, cert.Fingerprint)
	}
	if host.HostKey != srv.HostKeyLine() {
		t.Fatal("主机公钥没钉住")
	}
	if !host.SudoNoPasswd || !srv.SudoNoPasswd() {
		t.Fatal("免密 sudo 没配上")
	}
	if want := "/etc/sudoers.d/90-llmgate-admin"; srv.SudoersFile() != want {
		t.Fatalf("sudoers 文件 = %q，期望 %q", srv.SudoersFile(), want)
	}
	if host.System == "" {
		t.Fatal("没记下系统自述")
	}
	// 复验那一步必须是用证书登录的（口令从此不再需要）。
	sawKeyAuth := false
	for _, ex := range srv.Execs() {
		if ex.PublicKeyAuth {
			sawKeyAuth = true
		}
		if strings.Contains(ex.Command, testPassword) {
			t.Fatal("口令出现在远端命令行里")
		}
	}
	if !sawKeyAuth {
		t.Fatal("没有一条命令是凭证书登录后跑的")
	}
}

// 口令是一次性的：不入库、不进日志。私钥同理只在 settings 密文里。
func TestEnrollLeaksNoSecrets(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin"})
	e.cert()
	if _, err := e.enroll("board1", "板子一", true); err != nil {
		t.Fatalf("纳管: %v", err)
	}
	if strings.Contains(e.buf.String(), testPassword) {
		t.Fatal("口令写进了日志")
	}
	if strings.Contains(e.buf.String(), "PRIVATE KEY") {
		t.Fatal("私钥写进了日志")
	}
	for _, name := range []string{"llmgate.db", "llmgate.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(e.dir, name))
		if err != nil {
			continue
		}
		if bytes.Contains(raw, []byte(testPassword)) {
			t.Fatalf("口令进了 %s", name)
		}
		if bytes.Contains(raw, []byte("PRIVATE KEY")) {
			t.Fatalf("私钥明文进了 %s", name)
		}
	}
}

func TestEnrollRequiresCertificate(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin"})
	_, err := e.enroll("board1", "板子一", true)
	if err == nil || code(t, err) != agenthost.CodeCertificateMissing {
		t.Fatalf("err = %v", err)
	}
	hosts, err := e.mgr.List(context.Background())
	if err != nil {
		t.Fatalf("列出: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatal("没有证书时不该建出主机行")
	}
}

func TestEnrollWrongPasswordKeepsRowWithReason(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin", Password: "another"})
	e.cert()
	_, err := e.enroll("board1", "板子一", true)
	if err == nil || code(t, err) != agenthost.CodeAuthFailed {
		t.Fatalf("err = %v", err)
	}
	hosts, err := e.mgr.List(context.Background())
	if err != nil {
		t.Fatalf("列出: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Status != store.AgentHostStatusError || hosts[0].LastError == "" {
		t.Fatalf("行 = %+v", hosts)
	}
}

func TestEnrollWithoutSudoOption(t *testing.T) {
	e := newEnv(t)
	srv := e.host("board1", agenthosttest.Options{Username: "admin"})
	e.cert()
	host, err := e.enroll("board1", "板子一", false)
	if err != nil {
		t.Fatalf("纳管: %v", err)
	}
	if host.SudoNoPasswd || srv.SudoersFile() != "" {
		t.Fatal("没勾选时不该动 sudoers")
	}
	if host.Status != store.AgentHostStatusReady {
		t.Fatalf("状态 = %q", host.Status)
	}
}

func TestEnrollReportsMissingSudo(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin", NoSudo: true})
	e.cert()
	_, err := e.enroll("board1", "板子一", true)
	if err == nil || code(t, err) != agenthost.CodeSudoFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestHostKeyPinnedAndChangeRefused(t *testing.T) {
	e := newEnv(t)
	first := e.host("board1", agenthosttest.Options{Username: "admin"})
	e.cert()
	host, err := e.enroll("board1", "板子一", true)
	if err != nil {
		t.Fatalf("纳管: %v", err)
	}
	// 主机「重装」了：同一地址换了一把主机公钥，且不再认识设备的证书。
	first.Close()
	e.host("board1", agenthosttest.Options{Username: "admin"})
	if _, err := e.mgr.Check(context.Background(), host.ID); err == nil || code(t, err) != agenthost.CodeHostKeyChanged {
		t.Fatalf("err = %v", err)
	}
	// 管理员确认后可以重新纳管并接受新的主机公钥。
	again, err := e.mgr.Enroll(context.Background(), agenthost.EnrollRequest{
		ID: host.ID, Password: testPassword, ConfigureSudo: true, AcceptNewHostKey: true,
	})
	if err != nil {
		t.Fatalf("重新纳管: %v", err)
	}
	if again.HostKey == host.HostKey {
		t.Fatal("主机公钥没更新")
	}
	if again.Status != store.AgentHostStatusReady {
		t.Fatalf("状态 = %q：%s", again.Status, again.LastError)
	}
}

func TestRotateCertificatePushesAndLeavesStragglers(t *testing.T) {
	e := newEnv(t)
	one := e.host("board1", agenthosttest.Options{Username: "admin"})
	two := e.host("board2", agenthosttest.Options{Username: "admin"})
	old := e.cert()
	if _, err := e.enroll("board1", "board1", true); err != nil {
		t.Fatalf("纳管 1: %v", err)
	}
	if _, err := e.enroll("board2", "board2", true); err != nil {
		t.Fatalf("纳管 2: %v", err)
	}
	// 第二台在轮换证书时不在线。
	e.fleet.setDown("board2", true)
	next, results, err := e.mgr.RotateCertificate(context.Background())
	if err != nil {
		t.Fatalf("轮换证书: %v", err)
	}
	if next.Fingerprint == old.Fingerprint {
		t.Fatal("证书没换")
	}
	if len(results) != 2 {
		t.Fatalf("结果 = %+v", results)
	}
	byName := map[string]agenthost.HostResult{}
	for _, r := range results {
		byName[r.Address] = r
	}
	if !byName["board1"].OK {
		t.Fatalf("board1 应当成功：%s", byName["board1"].Error)
	}
	if byName["board2"].OK || byName["board2"].Error == "" {
		t.Fatal("board2 应当失败并给出原因")
	}
	if !one.HasKey(next.PublicKey) || one.HasKey(old.PublicKey) {
		t.Fatal("board1 没换成新公钥")
	}
	if !two.HasKey(old.PublicKey) {
		t.Fatal("board2 不该被动过")
	}
	// 掉队的那台：检查说得出补救动作，补发之后就对齐了。
	hosts, err := e.mgr.List(context.Background())
	if err != nil {
		t.Fatalf("列出: %v", err)
	}
	var straggler store.AgentHost
	for _, h := range hosts {
		if h.Address == "board2" {
			straggler = h
		}
	}
	if straggler.KeyFingerprint != old.Fingerprint {
		t.Fatalf("掉队主机的指纹 = %q", straggler.KeyFingerprint)
	}
	e.fleet.setDown("board2", false)
	checked, err := e.mgr.Check(context.Background(), straggler.ID)
	if err == nil {
		t.Fatalf("旧公钥的主机该报「证书待更新」，却成功了：%+v", checked)
	}
	if !strings.Contains(err.Error(), "补发证书") {
		t.Fatalf("原因 = %q", err.Error())
	}
	pushed, err := e.mgr.Push(context.Background(), straggler.ID)
	if err != nil {
		t.Fatalf("补发: %v", err)
	}
	if pushed.KeyFingerprint != next.Fingerprint || pushed.Status != store.AgentHostStatusReady {
		t.Fatalf("补发后 = %+v", pushed)
	}
	if !two.HasKey(next.PublicKey) || two.HasKey(old.PublicKey) {
		t.Fatal("board2 的 authorized_keys 没换干净")
	}
}

func TestRotateRequiresCertificate(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.mgr.RotateCertificate(context.Background()); err == nil || code(t, err) != agenthost.CodeCertificateMissing {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateCertificateOnlyOnce(t *testing.T) {
	e := newEnv(t)
	first := e.cert()
	_, err := e.mgr.GenerateCertificate(context.Background())
	if err == nil || code(t, err) != agenthost.CodeCertificateExists {
		t.Fatalf("err = %v", err)
	}
	current, err := e.mgr.CurrentCertificate(context.Background())
	if err != nil {
		t.Fatalf("读证书: %v", err)
	}
	if current.Fingerprint != first.Fingerprint {
		t.Fatal("重复生成把证书换掉了")
	}
	if !strings.HasPrefix(current.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(current.PublicKey, " llmgate-agent") {
		t.Fatalf("公钥形状 = %q", current.PublicKey)
	}
}

func TestRemoveTakesKeysOffTheHost(t *testing.T) {
	e := newEnv(t)
	srv := e.host("board1", agenthosttest.Options{Username: "admin"})
	cert := e.cert()
	host, err := e.enroll("board1", "板子一", true)
	if err != nil {
		t.Fatalf("纳管: %v", err)
	}
	cleaned, err := e.mgr.Remove(context.Background(), host.ID, true)
	if err != nil {
		t.Fatalf("删除: %v", err)
	}
	if !cleaned {
		t.Fatal("该主机在线，公钥应当被摘掉")
	}
	if srv.HasKey(cert.PublicKey) {
		t.Fatal("公钥还留在主机上")
	}
	if srv.SudoNoPasswd() {
		t.Fatal("免密 sudo 条目没删")
	}
	if _, err := e.st.GetAgentHost(context.Background(), host.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("行还在：%v", err)
	}
}

// 主机够不着时删除照样进行：设备侧的行必须删得掉。
func TestRemoveWhenHostUnreachable(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin"})
	e.cert()
	host, err := e.enroll("board1", "板子一", true)
	if err != nil {
		t.Fatalf("纳管: %v", err)
	}
	e.fleet.setDown("board1", true)
	cleaned, err := e.mgr.Remove(context.Background(), host.ID, true)
	if err != nil {
		t.Fatalf("删除: %v", err)
	}
	if cleaned {
		t.Fatal("主机连不上，不该报告已清理")
	}
	if _, err := e.st.GetAgentHost(context.Background(), host.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("行还在：%v", err)
	}
}

func TestDuplicateHostRejected(t *testing.T) {
	e := newEnv(t)
	e.host("board1", agenthosttest.Options{Username: "admin"})
	e.cert()
	if _, err := e.enroll("board1", "board1", true); err != nil {
		t.Fatalf("纳管: %v", err)
	}
	_, err := e.enroll("board1", "board1", true)
	if err == nil || code(t, err) != agenthost.CodeHostExists {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidHostInput(t *testing.T) {
	e := newEnv(t)
	e.cert()
	worker := store.AgentHostKindWorker
	for _, req := range []agenthost.EnrollRequest{
		{Kind: worker, Address: "board 1", Username: "admin", Password: testPassword},
		{Kind: worker, Address: "board1", Username: "Admin", Password: testPassword},
		{Kind: worker, Address: "board1", Username: "admin", Port: 70000, Password: testPassword},
		{Kind: worker, Address: "board1", Username: "admin", Password: testPassword, Name: strings.Repeat("名", 33)},
		// 类型必填、只认受控纳管 / 工作节点。
		{Address: "board1", Username: "admin", Password: testPassword},
		{Kind: "owner", Address: "board1", Username: "admin", Password: testPassword},
	} {
		if _, err := e.mgr.Enroll(context.Background(), req); err == nil || code(t, err) != agenthost.CodeInvalidHost {
			t.Fatalf("%+v 应当被拒：%v", req, err)
		}
	}
}

func TestUnreachableHostReportsReason(t *testing.T) {
	e := newEnv(t)
	e.cert()
	_, err := e.mgr.Enroll(context.Background(), agenthost.EnrollRequest{
		Kind: store.AgentHostKindWorker, Address: "nowhere", Port: 22, Username: "admin", Password: testPassword,
	})
	if err == nil || code(t, err) != agenthost.CodeUnreachable {
		t.Fatalf("err = %v", err)
	}
}
