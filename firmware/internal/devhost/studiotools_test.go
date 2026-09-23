package devhost_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
)

func settleStudioJobs(t *testing.T, m *devhost.Manager, id int64) devhost.DevToolQueueState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := m.DevToolJobs(id)
	for {
		active := false
		for _, j := range st.Jobs {
			active = active || !j.Done()
		}
		if !active {
			return st
		}
		var err error
		st, err = m.WaitDevToolJobs(ctx, id, st.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStudioToolQueueInstallAndReadiness(t *testing.T) {
	for _, pm := range []string{"apt-get", "apk", "pacman", "dnf", "yum", "zypper"} {
		t.Run(pm, func(t *testing.T) {
			f := newFixture(t, devhosttest.Options{Username: "dev", Pkg: pm})
			host := f.enroll(t, "dev", false)
			specs := []devhost.DevToolSpec{{Tool: "studio/ffmpeg", Action: devhost.DevToolInstall}, {Tool: "studio/fonts-cjk", Action: devhost.DevToolInstall}}
			if _, err := f.dev.EnqueueToolJobs(context.Background(), host.ID, specs, ""); err == nil || code(t, err) != devhost.CodeSudoFailed {
				t.Fatalf("missing sudo: %v", err)
			}
			if len(f.dev.DevToolJobs(host.ID).Jobs) != 0 {
				t.Fatal("rejected batch queued jobs")
			}
			if _, err := f.dev.EnqueueToolJobs(context.Background(), host.ID, specs, password); err != nil {
				t.Fatal(err)
			}
			st := settleStudioJobs(t, f.dev, host.ID)
			if st.Tools == nil || !st.Tools.Studio.Ready || st.Tools.Studio.ImageMagick.Installed {
				t.Fatalf("required tools: %+v", st)
			}
			for _, j := range st.Jobs {
				if j.Status != devhost.DevToolJobSucceeded {
					t.Fatalf("failed: %+v", j)
				}
			}
			if st.Jobs[1].StartedAt.Before(st.Jobs[0].FinishedAt) {
				t.Fatal("package commands overlapped")
			}
			if _, err := f.dev.EnqueueToolJobs(context.Background(), host.ID, []devhost.DevToolSpec{{Tool: "studio/imagemagick", Action: devhost.DevToolInstall}}, password); err != nil {
				t.Fatal(err)
			}
			st = settleStudioJobs(t, f.dev, host.ID)
			if !st.Tools.Studio.ImageMagick.Installed || !st.Tools.Studio.Ready {
				t.Fatalf("optional tool: %+v", st.Tools.Studio)
			}
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), password) {
				t.Fatal("password in queue frame")
			}
			fontsCmd := map[string]string{"apt-get": "fonts-noto-cjk", "apk": "font-noto-cjk", "pacman": "noto-fonts-cjk",
				"dnf": "google-noto-sans-cjk-ttc-fonts", "yum": "google-noto-sans-cjk-ttc-fonts", "zypper": "noto-sans-cjk-fonts"}[pm]
			sawFonts := false
			for _, e := range f.host.Execs() {
				if strings.Contains(e.Command, password) {
					t.Fatal("password in argv")
				}
				if strings.Contains(e.Command, "fontconfig") {
					sawFonts = true
					if !strings.Contains(e.Command, fontsCmd) || !strings.Contains(e.Command, "fc-cache -f") {
						t.Fatalf("font install for %s: %s", pm, e.Command)
					}
				}
				if strings.Contains(e.Command, " install ") || strings.Contains(e.Command, " add ") || strings.Contains(e.Command, "pacman -Sy") {
					if !strings.HasPrefix(e.Command, "sudo -S -p '' ") {
						t.Fatalf("missing sudo: %s", e.Command)
					}
				}
			}
			if !sawFonts {
				t.Fatal("no font install command ran")
			}
		})
	}
}

func TestStudioToolQueueValidationAndFailedReprobe(t *testing.T) {
	f := newFixture(t, devhosttest.Options{Username: "root", Pkg: "apt-get", FFmpeg: "ffmpeg limited", FFprobe: true,
		Command: func(command, stdin string) (string, int, bool) {
			// A successful package manager does not imply usable media capabilities.
			if strings.Contains(command, "apt-get install") {
				return "package already installed\n", 0, true
			}
			return "", 0, false
		}})
	host := f.enroll(t, "root", false)
	for _, specs := range [][]devhost.DevToolSpec{
		{{Tool: "studio/ffmpeg", Action: devhost.DevToolInstall}, {Tool: "studio/unknown", Action: devhost.DevToolInstall}},
		{{Tool: "studio/fonts-cjk", Action: devhost.DevToolUninstall}},
	} {
		if _, err := f.dev.EnqueueToolJobs(context.Background(), host.ID, specs, ""); err == nil {
			t.Fatal("invalid batch accepted")
		}
		if len(f.dev.DevToolJobs(host.ID).Jobs) != 0 {
			t.Fatal("partially queued invalid batch")
		}
	}
	_, err := f.dev.EnqueueToolJobs(context.Background(), host.ID, []devhost.DevToolSpec{{Tool: "studio/ffmpeg", Action: devhost.DevToolInstall}}, "")
	if err != nil {
		t.Fatal(err)
	}
	st := settleStudioJobs(t, f.dev, host.ID)
	if st.Jobs[0].Status != devhost.DevToolJobFailed || st.Jobs[0].ErrorCode != devhost.CodeToolMissing || !strings.Contains(st.Jobs[0].Error, "subtitles") {
		t.Fatalf("limited ffmpeg should fail: %+v", st.Jobs)
	}
	if st.Tools == nil || st.Tools.Studio.Ready || !st.Tools.Studio.FFmpeg.Installed || st.ToolsAt.IsZero() {
		t.Fatalf("failed action lost readings: %+v", st)
	}
}

func TestStudioToolQueueSharesGateAndCancels(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := newFixture(t, devhosttest.Options{Username: "dev", Pkg: "apt-get", Command: func(command, stdin string) (string, int, bool) {
		if strings.Contains(command, "/gate-helper/install.sh") {
			once.Do(func() { close(started) })
			<-release
		}
		return "", 0, false
	}})
	host := f.enroll(t, "dev", false)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ctx := context.Background()
	if _, err := f.dev.EnqueueGateInstall(ctx, host.ID, devhost.GateInstallSpec{BaseURL: "http://device.test", Key: "sk_fake12345"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not start")
	}
	specs := []devhost.DevToolSpec{{Tool: "studio/fonts-cjk", Action: devhost.DevToolInstall}, {Tool: "studio/ffmpeg", Action: devhost.DevToolInstall}}
	if _, err := f.dev.EnqueueToolJobs(ctx, host.ID, specs, password); err != nil {
		t.Fatal(err)
	}
	st, err := f.dev.EnqueueToolJobs(ctx, host.ID, specs, password)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 3 || st.Jobs[1].Status != devhost.DevToolJobQueued || st.Jobs[2].Status != devhost.DevToolJobQueued {
		t.Fatalf("not serialized/deduplicated: %+v", st.Jobs)
	}
	if _, err := f.dev.CancelDevToolJob(host.ID, st.Jobs[1].ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	st = settleStudioJobs(t, f.dev, host.ID)
	if st.Jobs[1].Status != devhost.DevToolJobCancelled || st.Jobs[2].Status != devhost.DevToolJobSucceeded || st.Tools.Studio.FontsCJK.Count != 0 {
		t.Fatalf("cancelled job executed: %+v", st)
	}
}
