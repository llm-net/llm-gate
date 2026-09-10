package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fileChange struct {
	path        string
	before      os.FileInfo
	hash, link  string
	data        []byte // nil means remove; configuration contents stay in memory only
	label       string
	emptyParent bool
}

type uninstallPlan struct {
	changes     []fileChange
	guards      []fileChange
	absent      []string
	dirs        []string
	keep        []string
	blocked     []string
	state       installState
	self        *fileChange
	noop        bool
	pathChanges []pathAddition
}

func readSmallFile(path string) ([]byte, error) {
	if err := noLinkPath(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if len(b) > 4<<20 {
		return nil, fmt.Errorf("配置文件过大，拒绝清理：%s", path)
	}
	return b, err
}

func (p *uninstallPlan) rewrite(path string, before, after []byte, label string) error {
	if bytes.Equal(before, after) {
		return nil
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := noLinkPath(path); err != nil {
		return err
	}
	p.changes = append(p.changes, fileChange{path: path, before: st, hash: digest(before), data: after, label: label})
	return nil
}

func (c fileChange) check() error {
	parent := c.path
	if c.link != "" {
		parent = filepath.Dir(parent)
	}
	if err := noLinkPath(parent); err != nil {
		return err
	}
	st, err := os.Lstat(c.path)
	if err != nil {
		return err
	}
	if !os.SameFile(c.before, st) || st.Size() != c.before.Size() || !st.ModTime().Equal(c.before.ModTime()) || st.Mode() != c.before.Mode() {
		return fmt.Errorf("文件已改变，请重新预览：%s", c.path)
	}
	if c.link != "" {
		target, err := os.Readlink(c.path)
		if err != nil {
			return err
		}
		if target != c.link {
			return fmt.Errorf("链接已改变：%s", c.path)
		}
	}
	if c.hash != "" {
		got, err := pathDigest(c.path)
		if err != nil {
			return err
		}
		if got != c.hash {
			return fmt.Errorf("文件内容已改变，请重新预览：%s", c.path)
		}
	}
	return nil
}

func (p *uninstallPlan) remove(path string, expected *ownedNode, label string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		if expected != nil && expected.Kind != "dir" {
			p.blocked = append(p.blocked, "受管文件类型已改变，请检查后重试："+path)
			return nil
		}
		if err := noLinkPath(path); err != nil {
			return err
		}
		p.dirs = append(p.dirs, path)
		return nil
	}
	c := fileChange{path: path, before: st, label: label}
	if st.Mode()&os.ModeSymlink != 0 {
		if err := noLinkPath(filepath.Dir(path)); err != nil {
			return err
		}
		c.link, err = os.Readlink(path)
		if err != nil {
			return err
		}
	} else {
		if err := noLinkPath(path); err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("不支持的文件类型：%s", path)
		}
	}
	if expected != nil {
		if expected.Kind == "file" && st.Mode().IsRegular() {
			c.hash, err = pathDigest(path)
			if err != nil {
				return err
			}
		}
		if (expected.Kind == "file" && (c.hash == "" || expected.Hash != c.hash)) || (expected.Kind == "link" && (c.link == "" || c.link != expected.Link)) || expected.Kind == "dir" {
			p.blocked = append(p.blocked, "受管文件已修改，请检查后重试："+path)
			return nil
		}
	}
	p.changes = append(p.changes, c)
	return nil
}

func (p *uninstallPlan) applyFiles() error {
	for _, c := range p.changes {
		if err := c.check(); err != nil {
			return err
		}
	}
	for _, c := range p.changes {
		if err := c.check(); err != nil {
			return err
		}
		var err error
		if c.data == nil {
			err = os.Remove(c.path)
		} else {
			mode := os.FileMode(0600)
			if c.label == "恢复 Shell PATH" {
				mode = c.before.Mode().Perm()
			}
			err = writeUserFile(c.path, c.data, mode)
		}
		if err != nil {
			return fmt.Errorf("%s失败：%w", c.label, err)
		}
	}
	// Only empty directories can disappear. A file added since preview survives.
	sort.Slice(p.dirs, func(i, j int) bool { return len(p.dirs[i]) > len(p.dirs[j]) })
	seen := map[string]bool{}
	for _, dir := range p.dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := noLinkPath(dir); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			p.keep = append(p.keep, "保留非空目录："+dir)
			continue
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

func cleanJSONFields(value any, prefix string, fields map[string]string) (any, bool) {
	b, _ := json.Marshal(value)
	if fields[prefix] == digest(b) {
		return nil, true
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return value, false
	}
	for k, v := range obj {
		path := prefix + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
		if next, remove := cleanJSONFields(v, path, fields); remove {
			delete(obj, k)
		} else {
			obj[k] = next
		}
	}
	return obj, len(obj) == 0
}

func cleanGrokBlock(b []byte) ([]byte, error) {
	const start = "# >>> SOC AGENT grok managed block >>>"
	const end = "# <<< SOC AGENT grok managed block <<<"
	text := string(b)
	i, j := strings.Index(text, start), strings.Index(text, end)
	if i < 0 && j < 0 {
		return b, nil
	}
	if i < 0 || j < i || strings.Count(text, start) != 1 || strings.Count(text, end) != 1 {
		return nil, errors.New("Grok 受管块边界损坏，请先修复配置")
	}
	if i > 0 && text[i-1] != '\n' {
		return nil, errors.New("Grok 受管块边界不在独立行")
	}
	j += len(end)
	if j < len(text) && text[j] == '\r' {
		j++
	}
	if j < len(text) && text[j] == '\n' {
		j++
	}
	return []byte(text[:i] + text[j:]), nil
}

func (a *app) planDerived(p *uninstallPlan, name string, purge bool) error {
	for _, file := range generatedNames(name) {
		rel := filepath.Join("tools", name, "derived", file)
		path := filepath.Join(p.state.Root, rel)
		_, recordedFile := p.state.Generated[rel]
		_, linkedProfile := a.cfg.Tools[name]
		if purge && (recordedFile || linkedProfile || p.state.Profiles[name]) {
			if err := p.remove(path, nil, "删除受管偏好与接入配置"); err != nil {
				return err
			}
			delete(p.state.Generated, rel)
			continue
		}
		b, err := readSmallFile(path)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		r, recorded := p.state.Generated[rel]
		if _, linked := a.cfg.Tools[name]; !recorded && !linked && !p.state.Profiles[name] {
			p.keep = append(p.keep, "保留未确认归属的配置："+path)
			continue
		}
		after := b
		switch {
		case name == "grok":
			after, err = cleanGrokBlock(b)
		case recorded && name != "cursor" && r.Hash == digest(b):
			after = nil
		case strings.HasSuffix(file, ".json") && !strings.Contains(file, "catalog"):
			var obj map[string]any
			if json.Unmarshal(b, &obj) != nil || obj == nil {
				return fmt.Errorf("%s 配置损坏，无法安全移除接入字段", name)
			}
			if !recorded {
				r = legacyGenerated(a, name, file, obj, b)
			}
			clean, empty := cleanJSONFields(obj, "", r.Fields)
			if empty {
				after = nil
			} else {
				after, err = json.MarshalIndent(clean, "", "  ")
				after = append(after, '\n')
			}
		case name == "codex" && file == "config.toml":
			text := string(b)
			if !recorded {
				r = legacyGenerated(a, name, file, nil, b)
			}
			for _, key := range []string{"model", "model_provider", "model_catalog_json"} {
				if v, ok := tomlTopLevel(text, key); ok && r.Fields[key] == digest([]byte(v)) {
					text = tomlRemoveTopLevel(text, key)
				}
			}
			i, j := tomlSectionBounds(text, sharedProviderHeader)
			if i >= 0 && r.Fields[sharedProviderHeader] == digest([]byte(strings.TrimSpace(text[i:j]))) {
				text = tomlRemoveSection(text, sharedProviderHeader)
			}
			after = []byte(text)
		default:
			p.keep = append(p.keep, "保留未确认归属的配置："+path)
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(after)) == 0 {
			after = nil
		}
		if a.cfg.APIKey != "" && bytes.Contains(after, []byte(a.cfg.APIKey)) {
			return fmt.Errorf("%s 配置含已改写的 gate 凭据，请先修复接入字段", name)
		}
		if err := p.rewrite(path, b, after, "清理接入配置"); err != nil {
			return err
		}
		delete(p.state.Generated, rel)
	}
	if purge {
		if err := a.planPurgeData(p, name); err != nil {
			return err
		}
	}
	p.dirs = append(p.dirs, a.derivedDir(name))
	return nil
}

// Old installations have no field hashes. Recognize only adapter-specific
// markers, and never infer ownership from the name of a user-created file alone.
func legacyGenerated(a *app, name, file string, obj map[string]any, b []byte) generatedFile {
	r := generatedFile{Fields: map[string]string{}}
	switch name {
	case "cursor":
		r.Fields["/network/useHttp1ForAgent"] = digest([]byte("true"))
	case "claude":
		env, _ := obj["env"].(map[string]any)
		if env["CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT"] == "1" || env["ANTHROPIC_AUTH_TOKEN"] == a.cfg.APIKey {
			owned := map[string]any{}
			for _, k := range []string{"model", "availableModels"} {
				if v, ok := obj[k]; ok {
					owned[k] = v
				}
			}
			ownedEnv := map[string]any{}
			for k, v := range env {
				if strings.HasPrefix(k, "ANTHROPIC_DEFAULT_") || k == "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT" || k == "ANTHROPIC_BASE_URL" || k == "ANTHROPIC_AUTH_TOKEN" || k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_OAUTH_TOKEN" {
					ownedEnv[k] = v
				}
			}
			owned["env"] = ownedEnv
			jsonFieldHashes(owned, "", r.Fields)
		}
	case "opencode":
		providers, _ := obj["provider"].(map[string]any)
		id := a.openCodeProviderID()
		if provider, ok := providers[id].(map[string]any); ok && provider["name"] == "LLM Gate" {
			owned := map[string]any{"provider": map[string]any{id: provider}}
			for _, k := range []string{"model", "small_model", "enabled_providers", "autoupdate", "$schema"} {
				if v, ok := obj[k]; ok {
					owned[k] = v
				}
			}
			jsonFieldHashes(owned, "", r.Fields)
		}
	case "codex":
		i, j := tomlSectionBounds(string(b), sharedProviderHeader)
		if i >= 0 && strings.Contains(string(b[i:j]), `env_key = "LLMGATE_API_KEY"`) && strings.Contains(string(b[i:j]), tomlQuote(a.cfg.BaseURL+"/agents/codex/v1")) {
			r = describeGenerated(name, file, b)
		}
	}
	return r
}

func purgeNames(name string) []string {
	switch name {
	case "codex":
		return []string{"sessions", "archived_sessions", "log", "logs", "tmp", "cache", "history.jsonl", "session_index.jsonl", "state_5.sqlite", "state_5.sqlite-wal", "state_5.sqlite-shm", "models_cache.json", "version.json", "auth.json"}
	case "grok":
		return []string{"sessions", "history", "cache", "logs", "history.jsonl"}
	case "claude":
		return []string{"projects", "todos", "tasks", "debug", "cache", "statsig", "plugins", "session-env", "history.jsonl", ".credentials.json", "stats-cache.json", "backups"}
	case "cursor":
		return []string{"data", "cli-config.json"}
	case "opencode":
		return []string{"xdg"}
	}
	return nil
}

func (a *app) planPurgeData(p *uninstallPlan, name string) error {
	for _, child := range purgeNames(name) {
		dir := filepath.Join(a.derivedDir(name), child)
		if err := noLinkPath(dir); err != nil {
			return err
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			// Never read sessions, even to hash them. File identity is enough to
			// ensure that a purge operates on the previewed state namespace.
			for i, c := range p.changes {
				if c.path == path {
					p.changes[i].data = nil
					return nil
				}
			}
			return p.remove(path, nil, "删除会话/缓存")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *app) planPrograms(p *uninstallPlan, name string) error {
	program, ok := p.state.Programs[name]
	if !ok {
		return nil
	}
	if _, err := a.programRoot(name, program.Entry); err != nil {
		return err
	}
	keys := make([]string, 0, len(program.Nodes))
	for rel := range program.Nodes {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		path, err := safeRelative(p.state.Root, rel)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(rel, filepath.Join("tools", name)+string(filepath.Separator)) || strings.Contains(rel, string(filepath.Separator)+"derived"+string(filepath.Separator)) {
			return errors.New("程序归属记录包含非程序路径")
		}
		// Each inventory entry must lie in one of the adapter's fixed program trees.
		allowed := filepath.Join("tools", name, "bin") + string(filepath.Separator)
		if name == "cursor" {
			allowed = filepath.Join("tools", name, "app") + string(filepath.Separator)
		}
		if name == "codex" {
			allowed = filepath.Join("tools", "codex", "packages", "standalone", "releases") + string(filepath.Separator)
		}
		if !strings.HasPrefix(rel+string(filepath.Separator), allowed) {
			return errors.New("程序清单超出固定安装目录")
		}
		n := program.Nodes[rel]
		if err := p.remove(path, &n, "删除受管程序"); err != nil {
			return err
		}
	}
	delete(p.state.Programs, name)
	return nil
}

func (a *app) buildUninstallPlan(name string, purge bool) (*uninstallPlan, error) {
	s, err := readInstallState(a.root)
	if err != nil {
		return nil, err
	}
	p := &uninstallPlan{state: s}
	for _, file := range []string{"install-state.json", "config.json"} {
		path := filepath.Join(s.Root, file)
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			p.absent = append(p.absent, path)
			continue
		}
		if err != nil {
			return nil, err
		}
		hash, err := pathDigest(path)
		if err != nil {
			return nil, err
		}
		p.guards = append(p.guards, fileChange{path: path, before: st, hash: hash})
	}
	// Recognize a still-connected legacy installation, with no PATH probing.
	for tool, st := range a.cfg.Tools {
		p.state.Profiles[tool] = true
		if _, ok := s.Programs[tool]; ok || st.Origin != "gate" {
			continue
		}
		dir, err := a.programRoot(tool, st.BinaryPath)
		if err != nil {
			return nil, err
		}
		nodes, err := snapshotTree(s.Root, dir)
		if err != nil {
			return nil, err
		}
		if len(nodes) > 0 {
			p.state.Programs[tool] = ownedProgram{Entry: st.BinaryPath, Nodes: nodes}
		}
	}
	if name != "" {
		if _, ok := p.state.Programs[name]; !ok {
			p.noop = true
			if st, ok := a.cfg.Tools[name]; ok && st.Origin == "external" {
				p.keep = append(p.keep, fmt.Sprintf("外部安装：%s；请沿原渠道卸载。gate %s disconnect 只撤销接入。", st.BinaryPath, name))
			} else {
				p.keep = append(p.keep, name+"：未发现受管安装")
			}
			return p, nil
		}
	}
	if name == "" || name == "codex" {
		if err := a.planShared(p); err != nil {
			return nil, err
		}
	}
	names := toolNames
	if name != "" {
		names = []string{name}
	}
	for _, tool := range names {
		if st, ok := a.cfg.Tools[tool]; ok && st.Origin == "external" {
			p.keep = append(p.keep, "保留外部程序："+st.BinaryPath)
		}
		_, owned := p.state.Programs[tool]
		if err := a.planDerived(p, tool, purge && (name != "" || owned || p.state.Profiles[tool])); err != nil {
			return nil, err
		}
		if name != "" || purge {
			if err := a.planPrograms(p, tool); err != nil {
				return nil, err
			}
		} else if owned {
			p.keep = append(p.keep, "保留受管程序："+p.state.Programs[tool].Entry)
		}
		if !purge || (!owned && !p.state.Profiles[tool]) {
			p.keep = append(p.keep, "保留会话和偏好："+a.derivedDir(tool))
		}
	}
	// Build the connection-state edit last. It is not committed before shared
	// restoration and credential cleanup have all succeeded.
	configPath := filepath.Join(s.Root, "config.json")
	b, err := readSmallFile(configPath)
	if err != nil {
		return nil, err
	}
	var after []byte
	if name != "" {
		cfg := a.cfg
		cfg.Tools = map[string]toolState{}
		for k, v := range a.cfg.Tools {
			if k != name {
				cfg.Tools[k] = v
			}
		}
		if name == "codex" {
			cfg.Shared = nil
		}
		after, err = json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return nil, err
		}
		after = append(after, '\n')
	}
	if b != nil {
		if err := p.rewrite(configPath, b, after, "清理连接记录"); err != nil {
			return nil, err
		}
	}
	if name == "" {
		target, err := a.currentExecutable()
		if err != nil {
			return nil, err
		}
		if s.Executable != "" && s.Executable != target {
			return nil, errors.New("当前 gate 与安装记录路径不匹配；请使用该安装的 gate 卸载")
		}
		if err := noLinkPath(target); err != nil {
			return nil, err
		}
		st, err := os.Lstat(target)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, errors.New("gate 程序不是普通文件")
		}
		hash, err := pathDigest(target)
		if err != nil {
			return nil, err
		}
		// A verified current executable is also the bounded legacy fallback.
		p.self = &fileChange{path: target, before: st, hash: hash, label: "删除 gate 程序"}
		locatorPath := target + ".install.json"
		if locator, err := readSmallFile(locatorPath); err != nil {
			return nil, err
		} else if locator != nil {
			resolved, err := uninstallRootForExecutable(s.Root, target)
			if err != nil || resolved != s.Root {
				return nil, errors.New("安装定位记录不匹配，拒绝清理")
			}
			if err := p.rewrite(locatorPath, locator, nil, "删除安装定位记录"); err != nil {
				return nil, err
			}
		}
		p.self.emptyParent = s.DirectoryOwned && !sharedInstallDirectory(filepath.Dir(target))
		if err := p.planPathCleanup(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (a *app) currentExecutable() (string, error) {
	if a.selfPath != "" {
		return filepath.Abs(a.selfPath)
	} // injected by offline tests
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func (p *uninstallPlan) preview(out io.Writer) {
	for _, c := range p.changes {
		fmt.Fprintf(out, "%s：%s\n", c.label, c.path)
	}
	if p.self != nil {
		fmt.Fprintln(out, "删除 gate 程序："+p.self.path)
	}
	for _, item := range p.keep {
		fmt.Fprintln(out, item)
	}
	for _, item := range p.blocked {
		fmt.Fprintln(out, "阻塞："+item)
	}
}

func (a *app) uninstall(name string, args []string, in io.Reader) error {
	purge, dry, yes := false, false, false
	for _, arg := range args {
		switch arg {
		case "--purge":
			purge = true
		case "--dry-run":
			dry = true
		case "--yes":
			yes = true
		default:
			return errors.New("用法: gate [<工具>] uninstall [--purge] [--dry-run] [--yes]")
		}
	}
	p, err := a.buildUninstallPlan(name, purge)
	if err != nil {
		fmt.Fprintln(a.out, "阻塞：", err)
		return errors.New("卸载预检未通过；未删除任何文件")
	}
	p.preview(a.out)
	if len(p.blocked) > 0 {
		return errors.New("请处理上述阻塞项后重试")
	}
	if p.noop {
		return nil
	}
	if err := probeOperation(a.root); err != nil {
		return err
	}
	if err := checkProgramUse(p); err != nil {
		return err
	}
	if dry {
		fmt.Fprintln(a.out, "预览完成；未写入文件，未访问网络。")
		return nil
	}
	if !yes {
		f, ok := in.(*os.File)
		if !ok || !isTerminalFile(f) {
			fmt.Fprintln(a.err, "非交互卸载必须指定 --yes；查看范围请使用 --dry-run。")
			return exitStatusError{code: 2}
		}
		fmt.Fprint(a.out, "确认执行以上卸载？[y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), "y") && !strings.EqualFold(strings.TrimSpace(line), "yes") {
			fmt.Fprintln(a.out, "已取消，未作修改。")
			return nil
		}
	}
	lease, err := acquireOperation(a.root, false)
	if err != nil {
		return err
	}
	defer lease.close()
	if err := checkProgramUse(p); err != nil {
		return err
	}
	// All previewed file identities are rechecked before creating a journal.
	for _, c := range p.guards {
		if err := c.check(); err != nil {
			return err
		}
	}
	for _, path := range p.absent {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("安装或连接记录已改变，请重新预览")
		}
	}
	for _, c := range p.changes {
		if err := c.check(); err != nil {
			return err
		}
	}
	if p.self != nil {
		if err := p.self.check(); err != nil {
			return err
		}
	}
	finish, cancel, err := prepareSelfRemoval(p.self, a.out, a.err)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return err
	}
	defer cancel()
	p.state.Pending = "uninstall"
	if name != "" {
		p.state.Pending += " " + name
	}
	if purge {
		p.state.Pending += " --purge"
	}
	journal, err := readInstallState(a.root)
	if err != nil {
		return err
	}
	journal.Pending = p.state.Pending
	if err := journal.save(); err != nil {
		return err
	}
	if err := p.applyFiles(); err != nil {
		return fmt.Errorf("卸载未完成，保留归属记录，请重试同一命令：%w", err)
	}
	if err := p.applyPathCleanup(); err != nil {
		return err
	}
	p.state.Pending = ""
	if p.self != nil {
		p.state.Executable = ""
		p.state.ExecutableHash = ""
	}
	if err := p.state.save(); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if p.self == nil {
		fmt.Fprintln(a.out, "受管程序卸载完成。")
	} else {
		printSelfRemovalResult(a.out)
	}
	for _, item := range p.keep {
		fmt.Fprintln(a.out, item)
	}
	return nil
}

func (a *app) disconnectDerived(name string) error {
	s, err := readInstallState(a.root)
	if err != nil {
		return err
	}
	// Keep ownership across disconnect, including a connected legacy program.
	if st, ok := a.cfg.Tools[name]; ok && st.Origin == "gate" {
		if _, ok := s.Programs[name]; !ok {
			if err := a.recordProgram(name, st.BinaryPath); err != nil {
				return err
			}
			s, err = readInstallState(a.root)
			if err != nil {
				return err
			}
		}
	}
	p := &uninstallPlan{state: s}
	if err := a.planDerived(p, name, false); err != nil {
		return err
	}
	if err := p.applyFiles(); err != nil {
		return err
	}
	delete(a.cfg.Tools, name)
	if err := a.save(); err != nil {
		return err
	}
	if err := p.state.save(); err != nil {
		return err
	}
	fmt.Fprintln(a.out, name, "已断开；程序、会话与用户偏好已保留。")
	return nil
}
