// Package modeldata 把公开数据仓库 github.com/llm-net/llm-model-data 的正式发布
// （远端 `data-YYYY.MM.DD.N` tag）合成为设备消费的**一份**模型目录文件
// model-catalog.json（形态见 internal/platformcatalog）。
//
// 分工一句话：数据仓库记「服务商 → 产品 → 模型」的事实（身份、协议面、价格），
// 不知道 LLM Gate 有哪些适配器；本包持「哪个产品接到设备的哪个平台 / 订阅」
// 的映射（mapping.go）与「原币种十进制 → 整数微元」的换算（build.go），
// 输出一份只含固件认识的词汇的文件。映射与换算规则都在源码里，改动可评审。
//
// 版本判定按数据仓库 docs/RELEASE.md：只认 `data-YYYY.MM.DD.N`，按
// (年, 月, 日, N) 整数比较取最新；不看提交时间、不跟随 main。合成文件的
// version = YYYYMMDD×1000 + N，与 tag 单调同序，设备侧「版本号大者胜」照旧成立。
package modeldata

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repo 是数据仓库的一个本地检出；全部读取经 git 按 ref 取，工作树状态不参与。
type Repo struct {
	Dir string
}

// Tag 是一个正式数据版本。
type Tag struct {
	Name                string
	Year, Month, Day, N int
}

var tagRE = regexp.MustCompile(`^data-([0-9]{4})\.([0-9]{2})\.([0-9]{2})\.([1-9][0-9]*)$`)

// ParseTag 解析正式 tag 名；不合格式或日期无效返回 false。
func ParseTag(name string) (Tag, bool) {
	m := tagRE.FindStringSubmatch(name)
	if m == nil {
		return Tag{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	n, _ := strconv.Atoi(m[4])
	if _, err := time.Parse("2006-01-02", fmt.Sprintf("%04d-%02d-%02d", y, mo, d)); err != nil {
		return Tag{}, false
	}
	return Tag{Name: name, Year: y, Month: mo, Day: d, N: n}, true
}

// Less 按 (年, 月, 日, N) 比较。
func (t Tag) Less(o Tag) bool {
	if t.Year != o.Year {
		return t.Year < o.Year
	}
	if t.Month != o.Month {
		return t.Month < o.Month
	}
	if t.Day != o.Day {
		return t.Day < o.Day
	}
	return t.N < o.N
}

// Date 是 tag 的发布日期（YYYY-MM-DD）。
func (t Tag) Date() string { return fmt.Sprintf("%04d-%02d-%02d", t.Year, t.Month, t.Day) }

// Version 是写进合成文件的整数版本：YYYYMMDD×1000 + N。N ≥ 1000 时报错——
// 一天发布上千次不是本仓库的现实，编码却要保持单调，宁可拒绝。
func (t Tag) Version() (int64, error) {
	if t.N >= 1000 {
		return 0, fmt.Errorf("tag %s 的序号 %d 超出版本编码范围（< 1000）", t.Name, t.N)
	}
	return int64(t.Year)*10000*1000 + int64(t.Month)*100*1000 + int64(t.Day)*1000 + int64(t.N), nil
}

func (r Repo) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// Fetch 从 origin 拉取全部 tag（不改工作树、不动分支）。
func (r Repo) Fetch() error {
	_, err := r.git("fetch", "--tags", "--quiet", "origin")
	return err
}

// Tags 列出本地已知的正式数据 tag，按版本升序。
func (r Repo) Tags() ([]Tag, error) {
	out, err := r.git("tag", "--list", "data-*")
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for _, line := range strings.Split(string(out), "\n") {
		if t, ok := ParseTag(strings.TrimSpace(line)); ok {
			tags = append(tags, t)
		}
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Less(tags[j]) })
	return tags, nil
}

// LatestTag 取最新正式发布；一个都没有时报错（不回退到 main）。
func (r Repo) LatestTag() (Tag, error) {
	tags, err := r.Tags()
	if err != nil {
		return Tag{}, err
	}
	if len(tags) == 0 {
		return Tag{}, errors.New("数据仓库没有任何 data-YYYY.MM.DD.N 正式发布 tag")
	}
	return tags[len(tags)-1], nil
}

// Commit 解出 ref 指向的完整提交 SHA（annotated tag 解引用到提交）。
func (r Repo) Commit(ref string) (string, error) {
	out, err := r.git("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadFile 读 ref 下的一个文件。
func (r Repo) ReadFile(ref, path string) ([]byte, error) {
	return r.git("show", ref+":"+path)
}

// ListFiles 列出 ref 下 dir 里的全部文件路径（递归）。
func (r Repo) ListFiles(ref, dir string) ([]string, error) {
	out, err := r.git("ls-tree", "-r", "--name-only", ref, dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
