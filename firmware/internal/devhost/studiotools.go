package devhost

import (
	"fmt"
	"strings"
)

// StudioTools is a live probe, never persisted. Optional tools do not affect Ready.
type StudioTools struct {
	FFmpeg      StudioFFmpeg
	FontsCJK    StudioFonts
	ImageMagick ToolPackage
	Ready       bool
}

type StudioFFmpeg struct {
	ToolPackage
	FFprobe   bool
	H264      bool
	AAC       bool
	Subtitles bool
}

func (f StudioFFmpeg) Ready() bool {
	return f.Installed && f.FFprobe && f.H264 && f.AAC && f.Subtitles
}

type StudioFonts struct {
	Count  int
	Family string
}

// Studio jobs share the host queue with gate; the namespace keeps row identities distinct.
func IsStudioTool(tool string) bool {
	return tool == "studio/ffmpeg" || tool == "studio/fonts-cjk" || tool == "studio/imagemagick"
}

// Fixed package names only. Install from the administrator's configured repositories;
// unavailable packages/codecs are reported without adding third-party repositories.
// Each row lists alternatives tried in order: the RPM families name the same thing differently
// (Fedora google-noto-sans-cjk-fonts / ffmpeg-free, RHEL-likes google-noto-sans-cjk-ttc-fonts,
// openSUSE noto-sans-cjk-fonts). The re-probe after the job decides success, not the package name.
var studioPackages = map[string]map[string][]string{
	"apt-get": {"ffmpeg": {"ffmpeg"}, "fonts-cjk": {"fontconfig fonts-noto-cjk"}, "imagemagick": {"imagemagick"}},
	"apk":     {"ffmpeg": {"ffmpeg"}, "fonts-cjk": {"fontconfig font-noto-cjk"}, "imagemagick": {"imagemagick"}},
	"pacman":  {"ffmpeg": {"ffmpeg"}, "fonts-cjk": {"fontconfig noto-fonts-cjk"}, "imagemagick": {"imagemagick"}},
	"dnf": {"ffmpeg": {"ffmpeg", "ffmpeg-free"},
		"fonts-cjk":   {"fontconfig google-noto-sans-cjk-fonts", "fontconfig google-noto-sans-cjk-ttc-fonts"},
		"imagemagick": {"ImageMagick"}},
	"yum": {"ffmpeg": {"ffmpeg"},
		"fonts-cjk":   {"fontconfig google-noto-sans-cjk-ttc-fonts", "fontconfig google-noto-sans-cjk-fonts"},
		"imagemagick": {"ImageMagick"}},
	"zypper": {"ffmpeg": {"ffmpeg"},
		"fonts-cjk":   {"fontconfig noto-sans-cjk-fonts", "fontconfig google-noto-sans-cjk-fonts"},
		"imagemagick": {"ImageMagick"}},
}

// fontCacheRefresh 在字体包装完后重建 fontconfig 缓存：有的发行版不在包脚本里刷新，复探的
// fc-list 就看不到新字体。缓存刷新失败不改变安装结果（以复探读数为准）。
const fontCacheRefresh = " && { fc-cache -f >/dev/null 2>&1 || true; }"

func packageCommand(manager, name string) (string, error) {
	template, ok := pkgInstallCommands[manager]
	if !ok {
		return "", &Error{Code: CodeToolMissing, Msg: fmt.Sprintf("主机上没有认得的包管理器（apt-get / dnf / yum / apk / pacman / zypper），请手工安装 %s。", name)}
	}
	if name == "git" || name == "tmux" {
		return fmt.Sprintf(template, name), nil
	}
	pkgs := studioPackages[manager][name]
	if len(pkgs) == 0 {
		return "", &Error{Code: CodeInvalidHost, Msg: "不认识的创作工具：" + name}
	}
	alts := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		alts = append(alts, fmt.Sprintf(template, pkg))
	}
	command := strings.Join(alts, " || ")
	if len(alts) > 1 {
		command = "{ " + command + "; }"
	}
	if name == "fonts-cjk" {
		command += fontCacheRefresh
	}
	return command, nil
}

func studioToolVersion(tool string, reading StudioTools) string {
	switch tool {
	case "studio/ffmpeg":
		return reading.FFmpeg.Version
	case "studio/fonts-cjk":
		return reading.FontsCJK.Family
	case "studio/imagemagick":
		return reading.ImageMagick.Version
	}
	return ""
}
