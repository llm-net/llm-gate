package devhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestToolJobSecretRedactsValueAndPointer(t *testing.T) {
	secret := toolJobSecret{base: "https://private.example", key: "fake-client-key", password: "fake-sudo-password"}
	for _, value := range []any{secret, &secret} {
		outputs := []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)}
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, string(b))
		var log bytes.Buffer
		slog.New(slog.NewJSONHandler(&log, nil)).Info("test", "secret", value)
		outputs = append(outputs, log.String())
		for _, output := range outputs {
			for _, forbidden := range []string{secret.base, secret.key, secret.password} {
				if strings.Contains(output, forbidden) {
					t.Fatal("secret exposed")
				}
			}
		}
	}
}

func TestCancelledStudioJobDropsPasswordAndAudits(t *testing.T) {
	m := New(Options{})
	q := m.queueFor(1)
	q.jobs = []*DevToolJob{{ID: 1, HostID: 1, Tool: "studio/fonts-cjk", Action: DevToolInstall, Status: DevToolJobQueued}}
	q.secrets[1] = toolJobSecret{password: "fake-password"}
	var notified DevToolJob
	m.SetDevToolHook(func(job DevToolJob, _ *Tools) { notified = job })
	st, err := m.CancelDevToolJob(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.secrets) != 0 || st.Jobs[0].Status != DevToolJobCancelled || notified.Status != DevToolJobCancelled {
		t.Fatal("cancel must remove secret and publish a completion event")
	}
}

// 行观察器把远端输出按 \n / \r 切成行，半截行留到下一段；空行不报。
func TestLineObserverSplitsOnNewlineAndCarriageReturn(t *testing.T) {
	var got []string
	o := &lineObserver{emit: func(line string) { got = append(got, line) }}
	o.Write([]byte("  codex  1.0 MB / 4.0 MB  25%\r  codex  2.0 MB / 4.0 MB  50%\n"))
	o.Write([]byte("half"))
	o.Write([]byte(" line\n\n  codex  4.0 MB 取回完成，用时 3 秒\n"))
	want := []string{"codex  1.0 MB / 4.0 MB  25%", "codex  2.0 MB / 4.0 MB  50%", "half line", "codex  4.0 MB 取回完成，用时 3 秒"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("行 = %q，想要 %q", got, want)
	}
}

// 进度百分比只认独立的 `NN%` 记号。
func TestPercentRE(t *testing.T) {
	for line, want := range map[string]string{
		"  codex  1.0 MB / 4.0 MB  25%  1.0 MB/s": "25",
		"  cursor  120.0 MB / 120.0 MB  100%":     "100",
		"downloading 5%":                          "5",
		"no percent here":                         "",
		"ratio 12.5%ish":                          "",
		"  codex  4.0 MB 取回完成，用时 3 秒":             "",
	} {
		m := percentRE.FindStringSubmatch(line)
		got := ""
		if m != nil {
			got = m[2]
		}
		if got != want {
			t.Errorf("%q → %q，想要 %q", line, got, want)
		}
	}
}
