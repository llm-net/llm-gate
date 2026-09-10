package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *app) recordInstaller(args []string) error {
	target, err := a.currentExecutable()
	if err != nil {
		return err
	}
	if err := a.recordExecutable(target); err != nil {
		return err
	}
	if len(args) == 0 {
		if os.Getenv("GATE_INSTALL_DIR_CREATED") == "1" {
			s, err := readInstallState(a.root)
			if err != nil {
				return err
			}
			s.DirectoryOwned = true
			return s.save()
		}
		return nil
	}
	s, err := readInstallState(a.root)
	if err != nil {
		return err
	}
	var item pathAddition
	switch {
	case len(args) == 4 && args[0] == "--path-block":
		item = pathAddition{File: args[1], Directory: args[2], Block: "\n# gate: LLM Gate 开发工具引导器\n" + args[3] + "\n"}
		if !filepath.IsAbs(item.File) || !filepath.IsAbs(item.Directory) || !strings.HasPrefix(item.Block, "\n# gate: LLM Gate 开发工具引导器\n") || strings.Count(item.Block, "\n") != 3 {
			return errors.New("无效的安装器 PATH 记录")
		}
		if err := noLinkPath(item.File); err != nil {
			return err
		}
	case len(args) == 2 && args[0] == "--user-path":
		item = pathAddition{Directory: args[1]}
	default:
		return errors.New("无效的安装器归属参数")
	}
	if item.Directory != filepath.Dir(target) {
		return errors.New("PATH 记录与当前 gate 目录不一致")
	}
	for _, old := range s.Path {
		if old == item {
			return nil
		}
	}
	s.Path = append(s.Path, item)
	return s.save()
}

func sharedInstallDirectory(dir string) bool {
	home, _ := os.UserHomeDir()
	for _, shared := range []string{home, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), "/usr/local/bin", "/usr/bin", "/bin"} {
		if filepath.Clean(dir) == filepath.Clean(shared) {
			return true
		}
	}
	return false
}

func (p *uninstallPlan) planPathCleanup() error {
	for _, item := range p.state.Path {
		if item.Directory != filepath.Dir(p.self.path) || sharedInstallDirectory(item.Directory) {
			p.keep = append(p.keep, "保留共享目录及 PATH："+item.Directory)
			continue
		}
		if err := noLinkPath(item.Directory); err != nil {
			return err
		}
		entries, err := os.ReadDir(item.Directory)
		if err != nil {
			return err
		}
		emptyAfter := true
		for _, entry := range entries {
			path := filepath.Join(item.Directory, entry.Name())
			removed := path == p.self.path
			for _, c := range p.changes {
				if c.path == path && c.data == nil {
					removed = true
				}
			}
			if !removed {
				emptyAfter = false
			}
		}
		if !emptyAfter {
			p.keep = append(p.keep, "保留非空安装目录及 PATH："+item.Directory)
			continue
		}
		if item.File == "" {
			p.pathChanges = append(p.pathChanges, item)
			continue
		}
		if !filepath.IsAbs(item.File) || !strings.HasPrefix(item.Block, "\n# gate: LLM Gate 开发工具引导器\n") || strings.Count(item.Block, "\n") != 3 {
			return errors.New("Shell PATH 归属记录形态无效")
		}
		b, err := readSmallFile(item.File)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		if !strings.Contains(string(b), "# gate: LLM Gate 开发工具引导器") {
			continue
		}
		if strings.Count(string(b), item.Block) != 1 {
			p.blocked = append(p.blocked, "Shell PATH 块已被修改："+item.File)
			continue
		}
		after := []byte(strings.Replace(string(b), item.Block, "", 1))
		if err := p.rewrite(item.File, b, after, "恢复 Shell PATH"); err != nil {
			return err
		}
	}
	return nil
}

// Remove exactly one matching segment. Empty segments, case, spacing and the
// order of every other segment are preserved byte for byte.
func removeUserPathSegment(raw, dir string) (string, bool) {
	parts := strings.Split(raw, ";")
	for i, part := range parts {
		if strings.EqualFold(strings.TrimRight(part, `\/`), strings.TrimRight(dir, `\/`)) {
			return strings.Join(append(parts[:i:i], parts[i+1:]...), ";"), true
		}
	}
	return raw, false
}

func (p *uninstallPlan) applyPathCleanup() error {
	for _, item := range p.pathChanges {
		raw, err := readUserPath()
		if err != nil {
			return fmt.Errorf("读取用户 PATH 失败，保留 gate，请重试：%w", err)
		}
		next, changed := removeUserPathSegment(raw, item.Directory)
		if changed {
			if err := writeUserPath(next); err != nil {
				return fmt.Errorf("恢复用户 PATH 失败，保留 gate，请重试：%w", err)
			}
		}
	}
	// Keep records for retained shared directories; future installs may still
	// share those PATH entries. Successfully restored blocks need no retry.
	var retained []pathAddition
	for _, item := range p.state.Path {
		removed := false
		for _, change := range p.changes {
			if change.label == "恢复 Shell PATH" && change.path == item.File {
				removed = true
			}
		}
		for _, change := range p.pathChanges {
			if change == item {
				removed = true
			}
		}
		if !removed {
			retained = append(retained, item)
		}
	}
	p.state.Path = retained
	return nil
}
