package codexappserver

// 整包解包守卫：官方 `codex-app-server-package-*.tar.gz` 是一棵小目录树（见包注释）。这里把它
// 流式解到一个目录里，同时钉住形态——路径只能落在包根的 codex-package.json 与三个允许的
// 子目录下、只能是目录或普通文件（符号链接 / 硬链接 / 设备节点一律拒）、总量与文件数封顶
//（tar 炸弹）、入口文件长度与摘要必须与清单一致、每个可执行文件都得是本机架构的 ELF、
// 包自述的版本与入口必须与清单条目一致。任何一项不成立都判为「不是官方包」，解出来的
// 半成品由调用方整目录删掉。

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
)

// packageMeta 是 codex-package.json 里本固件关心的几个键（其余键忽略，不当作错误）。
type packageMeta struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Entrypoint    string `json:"entrypoint"`
}

// packageResult 是一次成功解包的读数。
type packageResult struct {
	EntrySize   int64
	EntrySHA256 string
	TotalBytes  int64
	Files       int
}

// cleanMemberPath 把 tar 条目名规范成包内相对路径（去掉 `./` 前缀），拒绝绝对路径、`..`
// 与空名。返回值用 `/` 分隔。
func cleanMemberPath(name string) (string, error) {
	p := path.Clean(strings.TrimPrefix(name, "./"))
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", errors.New("组件制品 tar 包含越界路径，不是官方包")
	}
	return p, nil
}

// allowedMember 判定一条相对路径是否在包布局允许的位置：包根只许 codex-package.json，
// 其余必须在 bin/ codex-path/ codex-resources/ 之下。
func allowedMember(rel string, isDir bool) bool {
	top, rest, hasRest := strings.Cut(rel, "/")
	if !hasRest {
		if isDir {
			return packageTopDirs[top]
		}
		return top == PackageMetaFile
	}
	return packageTopDirs[top] && rest != ""
}

// extractPackage 把 tgzPath 解到 dstDir（须不存在或为空目录；目录 0700、文件 0700/0600
// 按 tar 里的执行位区分，属主为当前进程）。entryLimit 是清单声明的入口文件长度：入口文件
// 解包字节数封顶在它 +1，超出即失败；整包另受 maxPackageTotalBytes / maxPackageFiles 约束。
// version 是清单版本，用来比对包自述。
func extractPackage(tgzPath, dstDir, version string, entryLimit int64) (*packageResult, error) {
	in, err := os.Open(tgzPath)
	if err != nil {
		return nil, fmt.Errorf("读取组件制品失败: %w", err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return nil, errors.New("组件制品不是合法的 tar.gz 包")
	}
	defer gz.Close()
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建解包目录失败: %w", err)
	}

	tr := tar.NewReader(gz)
	res := &packageResult{}
	var (
		metaRaw    []byte
		seen       = map[string]bool{}
		entryFound bool
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("组件制品不是合法的 tar.gz 包")
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, err := cleanMemberPath(hdr.Name)
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if !allowedMember(rel, true) {
				return nil, errors.New("组件制品 tar 包内容与官方包布局不符")
			}
			if err := os.MkdirAll(filepath.Join(dstDir, filepath.FromSlash(rel)), 0o700); err != nil {
				return nil, fmt.Errorf("创建解包目录失败: %w", err)
			}
			continue
		case tar.TypeReg:
		default:
			return nil, errors.New("组件制品 tar 包含非普通文件条目，不是官方包")
		}
		if !allowedMember(rel, false) {
			return nil, errors.New("组件制品 tar 包内容与官方包布局不符")
		}
		if seen[rel] {
			return nil, errors.New("组件制品 tar 包重复声明同一文件，不是官方包")
		}
		seen[rel] = true
		res.Files++
		if res.Files > maxPackageFiles {
			return nil, errors.New("组件制品 tar 包文件数超过上限")
		}

		// 剩余预算：整包总量与（入口文件的）清单声明长度取严者。
		limit := maxPackageTotalBytes - res.TotalBytes
		isEntry := rel == EntrypointPath
		if isEntry && entryLimit < limit {
			limit = entryLimit
		}
		if limit < 0 {
			limit = 0
		}
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, fmt.Errorf("创建解包目录失败: %w", err)
		}
		mode := os.FileMode(0o600)
		if hdr.Mode&0o111 != 0 {
			mode = 0o700
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, mode)
		if err != nil {
			return nil, fmt.Errorf("创建解包文件失败: %w", err)
		}
		hasher := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(tr, limit+1))
		if cerr := out.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			return nil, errors.New("解包组件制品失败")
		}
		if n > limit {
			if isEntry && n > entryLimit {
				return nil, fmt.Errorf("解包后长度超过清单声明 %d", entryLimit)
			}
			return nil, errors.New("组件制品解包总量超过上限")
		}
		res.TotalBytes += n
		switch {
		case isEntry:
			entryFound = true
			res.EntrySize = n
			res.EntrySHA256 = hex.EncodeToString(hasher.Sum(nil))
		case rel == PackageMetaFile:
			metaRaw, err = os.ReadFile(dst)
			if err != nil {
				return nil, errors.New("解包组件制品失败")
			}
		}
		// 每个带执行位的文件都得是本机架构的 ELF（入口、code-mode host、rg、bwrap、zsh）。
		if mode == 0o700 {
			if err := elfcheck.Verify(dst); err != nil {
				return nil, fmt.Errorf("包内 %s: %w", rel, err)
			}
		}
	}
	if !entryFound {
		return nil, errors.New("组件制品 tar 包里没有 " + EntrypointPath)
	}
	if metaRaw == nil {
		return nil, errors.New("组件制品 tar 包里没有 " + PackageMetaFile)
	}
	var meta packageMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, errors.New("组件制品的 " + PackageMetaFile + " 不是合法 JSON")
	}
	if meta.LayoutVersion != packageLayoutVersion {
		return nil, fmt.Errorf("组件制品包布局版本 %d 不是本固件认识的 %d", meta.LayoutVersion, packageLayoutVersion)
	}
	if meta.Version != version {
		return nil, errors.New("组件制品自述版本与清单条目不符")
	}
	if meta.Entrypoint != EntrypointPath {
		return nil, errors.New("组件制品自述入口不是 " + EntrypointPath)
	}
	return res, nil
}
