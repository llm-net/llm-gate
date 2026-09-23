package devhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the actual POSIX probe with an isolated PATH: FFmpeg's build details and
// stderr must not become the version, and a missing binary must remain absent.
func TestFFmpegProbeScript(t *testing.T) {
	dir := t.TempDir()
	head, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(head, filepath.Join(dir, "head")); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n[ \"$1\" = -version ] || exit 1\nprintf 'ffmpeg version test-build\\nconfiguration: test\\n'\necho diagnostic >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, installed := range []bool{true, false} {
		if !installed {
			if err := os.Remove(binary); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", probeScript)
		cmd.Env = []string{"PATH=" + dir, "HOME=" + dir}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		got := parseProbe(nil, string(out)).Studio.FFmpeg.ToolPackage
		want := ToolPackage{}
		if installed {
			want = ToolPackage{Installed: true, Version: "ffmpeg version test-build"}
		}
		if got != want {
			t.Fatalf("installed=%v: got %+v, want %+v", installed, got, want)
		}
	}
}

func TestStudioProbeCapabilities(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"head", "awk", "sort"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	probe := func() StudioTools {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Env = []string{"PATH=" + dir, "HOME=" + dir}
		cmd.Stdin = strings.NewReader(probeScript)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "configuration:") {
			t.Fatalf("unexpected build details: %s", out)
		}
		return parseProbe(nil, string(out)).Studio
	}
	write("ffmpeg", `case "$*" in
  -version) printf 'ffmpeg version sample\nconfiguration: ignored\n';;
  *-encoders) printf ' V....D h264_v4l2m2m hardware\n A..... aac audio\n';;
  *-filters) printf ' ... subtitles V->V Render subtitles\n';;
  *) exit 1;;
esac`)
	write("ffprobe", "exit 0")
	write("fc-list", `printf 'Noto Sans CJK SC\nNoto Sans CJK SC\nNoto Serif CJK SC\n'`)
	write("convert", `echo 'Version: ImageMagick 6.9 test'`)
	st := probe()
	if !st.Ready || !st.ImageMagick.Installed || st.FontsCJK.Count != 2 || st.FontsCJK.Family != "Noto Sans CJK SC" {
		t.Fatalf("full probe: %+v", st)
	}
	// Prefer magick to convert, but ignore unrelated executables with the same name.
	write("magick", `echo 'Version: ImageMagick 7.1 test'`)
	if got := probe().ImageMagick.Version; !strings.Contains(got, "7.1") {
		t.Fatal(got)
	}
	for _, name := range []string{"magick", "convert"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if st := probe(); !st.Ready || st.ImageMagick.Installed {
		t.Fatalf("optional program affects readiness: %+v", st)
	}
	write("ffmpeg", `case "$*" in
  -version) echo 'ffmpeg version limited';;
  *-encoders) printf ' V....D libx264 encoder\n A..... aac audio\n';;
  *-filters) printf ' ... scale V->V Scale\n';;
esac`)
	if st := probe(); st.Ready || !st.FFmpeg.H264 || st.FFmpeg.Subtitles {
		t.Fatalf("missing subtitles: %+v", st)
	}
	if err := os.Remove(filepath.Join(dir, "fc-list")); err != nil {
		t.Fatal(err)
	}
	if st := probe(); st.FontsCJK.Count != 0 || st.FontsCJK.Family != "" {
		t.Fatalf("missing fontconfig: %+v", st)
	}
}

func TestStudioReadyRequiresEachCapability(t *testing.T) {
	full := "ffmpeg_version=ffmpeg test\nffprobe=yes\nffmpeg_h264=yes\nffmpeg_aac=yes\nffmpeg_subtitles=yes\nfonts_cjk_count=1\n"
	for _, line := range strings.Split(strings.TrimSpace(full), "\n") {
		st := parseProbe(nil, strings.ReplaceAll(full, line+"\n", "")).Studio
		if st.Ready {
			t.Errorf("ready without %s", line)
		}
	}
	if !parseProbe(nil, full).Studio.Ready {
		t.Fatal("complete capabilities should be ready")
	}
}
