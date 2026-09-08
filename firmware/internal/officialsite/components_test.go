package officialsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchComponentIndexNeedsBothFiles(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch r.URL.Path {
		case "/updates/components/cloudflared/stable.json":
			w.Write([]byte(`{"schema":"x"}`))
		case "/updates/components/cloudflared/stable.json.sig":
			w.Write([]byte(`{"signature":"y"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	index, sig, err := c.FetchComponentIndex(context.Background(), "cloudflared")
	if err != nil {
		t.Fatal(err)
	}
	if string(index) != `{"schema":"x"}` || string(sig) != `{"signature":"y"}` {
		t.Fatalf("取回内容不对: %q %q", index, sig)
	}
	if len(gotPaths) != 2 {
		t.Fatalf("应各取一次: %v", gotPaths)
	}
	// 缺签名 = 失败（没有签名的清单不构成允许决策）。
	if _, _, err := c.FetchComponentIndex(context.Background(), "mihomo"); err == nil {
		t.Fatal("清单不存在时应失败")
	}
}

// tlsPair 起两台自签 HTTPS 服务（前端 release 站 + 制品站），把它们的证书装进
// 只在本测试进程里生效的信任根。主机名统一走 127.0.0.1——策略只看主机串，测试
// 用「不同端口」区分两台在策略上是同一主机；跨主机拒绝由 checkArtifactURL 的
// 纯函数用例覆盖。
func tlsPair(t *testing.T, payload []byte) (front, asset *httptest.Server, pool *x509.CertPool) {
	t.Helper()
	asset = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			http.Error(w, "leaked headers", http.StatusBadRequest)
			return
		}
		w.Write(payload)
	}))
	front = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			http.Redirect(w, r, asset.URL+"/blob", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, "http://"+strings.TrimPrefix(asset.URL, "https://")+"/blob", http.StatusFound)
		case "/elsewhere":
			http.Redirect(w, r, "https://evil.example/blob", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, front.URL+"/loop", http.StatusFound)
		case "/direct":
			w.Write(payload)
		case "/big":
			w.Write(append(payload, 'x'))
		default:
			http.NotFound(w, r)
		}
	}))
	pool = x509.NewCertPool()
	pool.AddCert(front.Certificate())
	pool.AddCert(asset.Certificate())
	t.Cleanup(front.Close)
	t.Cleanup(asset.Close)
	return front, asset, pool
}

func TestFetchComponentArtifactPolicy(t *testing.T) {
	payload := bytes.Repeat([]byte("cf"), 4096)
	front, asset, pool := tlsPair(t, payload)
	c := NewClient("https://example.invalid", nil)
	c.artifactTLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(front.URL, "https://"))
	pol := ArtifactPolicy{AllowedHosts: []string{host}, MaxBytes: int64(len(payload))}
	want := sha256.Sum256(payload)

	t.Run("首跳与重定向都要过策略", func(t *testing.T) {
		for _, path := range []string{"/downgrade", "/elsewhere"} {
			_, _, err := c.FetchComponentArtifact(context.Background(), front.URL+path, testPolicy(pol, front, asset), &bytes.Buffer{})
			if !errors.Is(err, ErrArtifactPolicy) {
				t.Errorf("%s: 应被策略拒绝，得到 %v", path, err)
			}
		}
		// 重定向目标不在允许集合（只放行前端站）：一跳就被拒。
		_, _, err := c.FetchComponentArtifact(context.Background(), front.URL+"/ok", testPolicy(pol, front), &bytes.Buffer{})
		if !errors.Is(err, ErrArtifactPolicy) {
			t.Errorf("跳到未允许主机应被拒: %v", err)
		}
		_, _, err = c.FetchComponentArtifact(context.Background(), front.URL+"/loop", testPolicy(pol, front, asset), &bytes.Buffer{})
		if !errors.Is(err, ErrArtifactPolicy) {
			t.Errorf("循环重定向应按跳数封顶拒绝: %v", err)
		}
	})
	t.Run("直下与一跳重定向", func(t *testing.T) {
		for _, path := range []string{"/direct", "/ok"} {
			var buf bytes.Buffer
			sum, n, err := c.FetchComponentArtifact(context.Background(), front.URL+path, testPolicy(pol, front, asset), &buf)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if n != int64(len(payload)) || sum != hex.EncodeToString(want[:]) || !bytes.Equal(buf.Bytes(), payload) {
				t.Fatalf("%s: 下载内容或摘要不对（n=%d sum=%s）", path, n, sum)
			}
		}
	})
	t.Run("超长与 404", func(t *testing.T) {
		_, _, err := c.FetchComponentArtifact(context.Background(), front.URL+"/big", testPolicy(pol, front, asset), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "字节上限") {
			t.Errorf("超过上限应失败: %v", err)
		}
		_, _, err = c.FetchComponentArtifact(context.Background(), front.URL+"/missing", testPolicy(pol, front, asset), &bytes.Buffer{})
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusNotFound {
			t.Errorf("404 应映射为 HTTPError: %v", err)
		}
	})
	t.Run("前置校验", func(t *testing.T) {
		_, _, err := c.FetchComponentArtifact(context.Background(), "http://"+host+"/direct", pol, &bytes.Buffer{})
		if !errors.Is(err, ErrArtifactPolicy) {
			t.Errorf("HTTP 首跳应被拒: %v", err)
		}
		_, _, err = c.FetchComponentArtifact(context.Background(), front.URL+"/direct", testPolicy(ArtifactPolicy{AllowedHosts: pol.AllowedHosts}, front), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "字节上限") {
			t.Errorf("缺字节上限应在拨号前失败: %v", err)
		}
	})
}

// testPolicy 把 httptest 服务的「主机:端口」整体作为允许主机——生产策略拒绝显式
// 端口（checkArtifactURL 的 Port() != "" 分支），本地 TLS 服务却绕不开端口；
// 这里通过 allowTestHosts 钩子放开。
func testPolicy(pol ArtifactPolicy, srvs ...*httptest.Server) ArtifactPolicy {
	pol.allowTestHosts = nil
	for _, srv := range srvs {
		pol.allowTestHosts = append(pol.allowTestHosts, strings.TrimPrefix(srv.URL, "https://"))
	}
	return pol
}

func TestCheckArtifactURL(t *testing.T) {
	pol := ArtifactPolicy{AllowedHosts: []string{"github.com", "objects.githubusercontent.com"}, MaxBytes: 1}
	ok := []string{
		"https://github.com/cloudflare/cloudflared/releases/download/2026.8.2/cloudflared-linux-arm64",
		"https://objects.githubusercontent.com/github-production-release-asset/x?y=z",
		"https://GitHub.com/x",
	}
	for _, u := range ok {
		if err := checkArtifactURL(u, pol); err != nil {
			t.Errorf("%s 应通过: %v", u, err)
		}
	}
	bad := []string{
		"http://github.com/x",
		"https://github.com:8443/x",
		"https://evil.example/x",
		"https://github.com.evil.example/x",
		"https://u:p@github.com/x",
		"",
	}
	for _, u := range bad {
		if err := checkArtifactURL(u, pol); !errors.Is(err, ErrArtifactPolicy) {
			t.Errorf("%q 应被拒: %v", u, err)
		}
	}
}
