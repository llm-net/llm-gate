// fetch.go 把大制品的取回收成一条公共路径：带进度、可续传。
//
// 盒子那头 /{tool}-helper/cli/* 是白名单透传，字节从官方源经设备中继过来，
// 每个字节都要过一遍设备的出网口。真机上 159 MiB 的 grok 制品按分钟计，
// 期间 io.Copy 一声不吭，跟卡死分不出来；设备侧中继又有分钟级的正文上限，
// 被掐一次就把已经收下的一百多兆全丢了。
//
// 所以这里做两件事：按固定间隔把已收字节写给用户；把落点定成按 URL 定名的
// 续传件，重试或重跑时用 Range 从断点接着传。五个 helper 的透传面本来就转发
// Range/Accept-Ranges/Content-Range 并认 206，续传不需要盒子那边改动。
package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// artifactFetchAttempts 是单次取回内的自动重试上限。设备侧中继有分钟级
	// 正文上限，慢链路上一件大制品可能被掐好几刀；每次重试都从已落盘字节接着
	// 传，所以重试是推进而不是重来。
	artifactFetchAttempts = 5
)

var (
	// artifactFetchBackoff 是重试前的固定等待。var 仅供测试压到 0。
	artifactFetchBackoff = 2 * time.Second
	// artifactProgressTTY / artifactProgressPlain 是两种输出节奏：终端上用
	// \r 原地刷新，可以密一些；重定向进日志或 CI 时只能一行行追加，放稀。
	// var 仅供测试压短。
	artifactProgressTTY   = 1 * time.Second
	artifactProgressPlain = 15 * time.Second
)

// errRestartFetch 表示半截文件已经没法接着传，本轮已经清空、要从头再来。
var errRestartFetch = errors.New("restart fetch")

// retryableError 标记「再来一次可能成功」的失败：链路被掐、正文短了、上游
// 5xx。4xx 与校验不符不进这一类——重试只会白烧带宽。
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func retryableFetch(err error) error { return retryableError{err: err} }

func isRetryableFetch(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}

// artifactFetch 描述一次大制品取回。零值不可用。
type artifactFetch struct {
	client *http.Client
	url    string
	// label 是进度行里的人读名字，例如 "Grok"。具体版本由调用方在开始前
	// 单独打印，这里保持短，窄终端上 \r 刷新才不折行。
	label string
	// dir 是续传件所在目录，恒与最终落点同一文件系统。
	dir string
	// prefix 是续传件基名前缀（如 ".grok-download"），同工具其它版本留下的
	// 续传件按它清理。
	prefix string
	// size 大于 0 时是官方元数据给出的精确字节数。
	size int64
	// digest 非空时是期望的 hex sha256。
	digest string
	// max 是单件上限，与盒子透传面的上限一致。
	max int64
	// mode 是续传件权限：可执行件 0700，归档 0600。
	mode os.FileMode
}

// partPath 把续传件定名到 URL 上：制品名恒带版本，所以同一个 URL 的半截文件
// 可以安全接着传，换了版本自然换文件，不会把两个版本的字节接到一起。
func (f artifactFetch) partPath() string {
	sum := sha256.Sum256([]byte(f.url))
	return filepath.Join(f.dir, fmt.Sprintf("%s-%x.part", f.prefix, sum[:6]))
}

// cleanStalePart 清掉同一工具其它版本留下的续传件：换版本后旧的半截文件再也
// 接不上，留着只占盘。
func (f artifactFetch) cleanStalePart(keep string) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, f.prefix+"-") || !strings.HasSuffix(name, ".part") {
			continue
		}
		if p := filepath.Join(f.dir, name); p != keep {
			_ = os.Remove(p)
		}
	}
}

// fetchArtifact 把 f.url 的正文取进按 URL 定名的续传件并校验，返回该文件路径；
// 调用方负责改名或解包。链路失败时续传件**保留**，下次接着传；校验不符时清空，
// 免得毒到下一次。
func (a *app) fetchArtifact(f artifactFetch) (string, error) {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return "", err
	}
	if err := securePath(f.dir, true); err != nil {
		return "", err
	}
	partPath := f.partPath()
	file, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, f.mode)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := file.Chmod(f.mode); err != nil {
		return "", err
	}
	if err := securePath(partPath, false); err != nil {
		return "", err
	}

	prog := newProgressWriter(a.out, f.label, f.size)
	// 半截文件只在「链路失败」后留着接着传。尺寸超限、4xx、摘要不符这类
	// 终端失败续传也救不回来，留着既占盘又会毒到下一次，一律删掉——失败的
	// 取回不留痕。
	fail := func(err error) (string, error) {
		prog.abort()
		if !isRetryableFetch(err) {
			_ = file.Close()
			_ = os.Remove(partPath)
		}
		return "", err
	}

	var lastErr error
	stalled := 0
	for attempt := 1; attempt <= artifactFetchAttempts; attempt++ {
		have, err := fileEnd(file)
		if err != nil {
			return fail(err)
		}
		// 半截文件比上限或比官方元数据还长：只可能是坏件或换了制品，丢掉重来。
		if have > f.max || (f.size > 0 && have > f.size) {
			if err := resetFile(file); err != nil {
				return fail(err)
			}
			have = 0
		}
		if f.size > 0 && have == f.size {
			// 上一轮已经收全，只是没走到校验或改名。直接进校验。
			lastErr = nil
			break
		}
		done, err := a.fetchAttempt(f, file, have, prog)
		if done {
			lastErr = nil
			break
		}
		lastErr = err
		if errors.Is(err, errRestartFetch) {
			stalled = 0
			continue
		}
		if !isRetryableFetch(err) {
			return fail(err)
		}
		after, sizeErr := fileEnd(file)
		if sizeErr != nil {
			return fail(sizeErr)
		}
		// 连着两轮一个字节都没推进，就不是「被掐断」而是这条路走不通了。
		if after > have {
			stalled = 0
		} else if stalled++; stalled >= 2 {
			break
		}
		if attempt < artifactFetchAttempts {
			prog.note(fmt.Sprintf("%s 链路中断（已收 %s），续传重试…", f.label, humanBytes(after)))
			time.Sleep(artifactFetchBackoff)
		}
	}
	if lastErr != nil {
		return fail(lastErr)
	}

	if err := file.Sync(); err != nil {
		return fail(err)
	}
	got, err := fileEnd(file)
	if err != nil {
		return fail(err)
	}
	if got == 0 {
		return fail(fmt.Errorf("%s 制品为空", f.label))
	}
	if f.size > 0 && got != f.size {
		return fail(fmt.Errorf("%s 下载大小与官方元数据不匹配", f.label))
	}
	if f.digest != "" {
		// 续传把字节分几段收，摘要只能在收全之后整个文件过一遍——本地盘读
		// 一百多兆是秒级，相对分钟级的下载可以忽略。
		sum, err := fileDigest(file)
		if err != nil {
			return fail(err)
		}
		if !strings.EqualFold(sum, f.digest) {
			return fail(fmt.Errorf("%s SHA-256 校验失败", f.label))
		}
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	prog.finish()
	f.cleanStalePart(partPath)
	return partPath, nil
}

// fetchAttempt 跑一次取回：have 大于 0 时用 Range 从断点接着要。返回是否已经收全。
func (a *app) fetchAttempt(f artifactFetch, file *os.File, have int64, prog *progressWriter) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, f.url, nil)
	if err != nil {
		return false, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return false, retryableFetch(fmt.Errorf("下载 %s: %w", f.label, err))
	}
	defer resp.Body.Close()

	total := f.size
	switch resp.StatusCode {
	case http.StatusOK:
		// 对方没认 Range（或本来就是从头要）：已经收下的字节作废，从头写。
		if have > 0 {
			if err := resetFile(file); err != nil {
				return false, err
			}
			have = 0
		}
	case http.StatusPartialContent:
		if have == 0 {
			return false, fmt.Errorf("下载 %s：没要断点却收到 206", f.label)
		}
		start, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != have {
			// 起点对不上就不能接，接上去是错位的字节。
			return false, fmt.Errorf("下载 %s：断点 %d 与 Content-Range %q 对不上",
				f.label, have, resp.Header.Get("Content-Range"))
		}
		if size > 0 {
			total = size
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 盒子透传面对非 2xx 只回状态码、不带 Content-Range，分不清是「已经
		// 收全」还是「半截文件比制品还长」。丢掉重来是唯一安全的处置。
		if err := resetFile(file); err != nil {
			return false, err
		}
		return false, errRestartFetch
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("下载 %s 返回 HTTP %d", f.label, resp.StatusCode)
		if resp.StatusCode >= 500 {
			return false, retryableFetch(err)
		}
		return false, err
	}

	if total == 0 && resp.ContentLength >= 0 {
		total = have + resp.ContentLength
	}
	if total > f.max {
		return false, fmt.Errorf("%s 制品超过大小限制", f.label)
	}
	if f.size > 0 && resp.ContentLength >= 0 && have+resp.ContentLength != f.size {
		return false, fmt.Errorf("%s 下载大小与官方元数据不匹配", f.label)
	}

	prog.restart(have, total)
	if _, err := file.Seek(have, io.SeekStart); err != nil {
		return false, err
	}
	n, copyErr := io.Copy(io.MultiWriter(file, prog), io.LimitReader(resp.Body, f.max+1-have))
	// 成功失败都要把已落盘字节钉稳：下一轮按文件大小定断点，长度必须和内容
	// 对得上，否则续传会从一段没真正写下去的位置接。
	syncErr := file.Sync()
	got := have + n
	if got > f.max {
		return false, fmt.Errorf("%s 制品超过大小限制", f.label)
	}
	if copyErr != nil {
		return false, retryableFetch(fmt.Errorf("接收 %s: %w", f.label, copyErr))
	}
	if syncErr != nil {
		return false, syncErr
	}
	if total > 0 && got != total {
		return false, retryableFetch(fmt.Errorf("接收 %s：收到 %d 字节，期望 %d", f.label, got, total))
	}
	// 上一轮可能留下更长的尾巴（换了制品又刚好起点对得上），按实收长度截平。
	if err := file.Truncate(got); err != nil {
		return false, err
	}
	return true, nil
}

// fileEnd 报告文件当前长度，也就是下一次续传的断点。
func fileEnd(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// resetFile 清空续传件，回到从头下载。
func resetFile(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

// fileDigest 整file过一遍 sha256，返回 hex。
func fileDigest(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// parseContentRange 解 "bytes <start>-<end>/<total>"；total 是 "*" 时按未知返回 0。
func parseContentRange(v string) (start, total int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, false
	}
	span, sizePart, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	startPart, _, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startPart), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if s := strings.TrimSpace(sizePart); s != "*" {
		total, err = strconv.ParseInt(s, 10, 64)
		if err != nil || total < 0 {
			return 0, 0, false
		}
	}
	return start, total, true
}

// progressWriter 把已收字节按固定间隔写给用户。经盒子中继的大制品按分钟计，
// 没有这行输出时终端上完全静默，看起来跟卡死一样。
//
// 速率与剩余时间全走整数运算：这里只是给人看的进度，不进任何校验，也不该
// 引进浮点。
type progressWriter struct {
	out   io.Writer
	label string
	every time.Duration
	tty   bool

	total int64 // 制品总字节；0 表示还不知道
	done  int64 // 续传件当前长度
	// fetched 只数本次进程真正收下来的字节，续传时不含已经在盘上的前缀，
	// 用来算速率与最后那行平均值。
	fetched int64

	start    time.Time // 整次取回的起点，只给「用时」用
	segStart time.Time // 本段的起点，给速率与剩余时间用
	segBase  int64
	last     time.Time

	width int  // 终端上上一行的宽度，用来擦掉变短后的残尾
	dirty bool // 终端上还有一行没收尾的 \r
}

func newProgressWriter(out io.Writer, label string, total int64) *progressWriter {
	tty := isTerminal(out)
	every := artifactProgressPlain
	if tty {
		every = artifactProgressTTY
	}
	now := time.Now()
	return &progressWriter{
		out: out, label: label, every: every, tty: tty, total: total,
		start: now, segStart: now, last: now,
	}
}

// restart 起一段新的传输：把计数对齐到已经落盘的断点，速率从这里重新起算。
func (p *progressWriter) restart(have, total int64) {
	if p == nil {
		return
	}
	if total > 0 {
		p.total = total
	}
	p.done = have
	p.segBase = have
	now := time.Now()
	p.segStart = now
	p.last = now
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if p == nil {
		return len(b), nil
	}
	p.done += int64(len(b))
	p.fetched += int64(len(b))
	if time.Since(p.last) >= p.every {
		p.last = time.Now()
		p.emit(p.line())
	}
	return len(b), nil
}

// rate 返回本段的字节/秒；样本太短就先不报，免得开头几十毫秒算出个天文数字。
func (p *progressWriter) rate() int64 {
	ms := time.Since(p.segStart).Milliseconds()
	if ms < 500 {
		return 0
	}
	return (p.done - p.segBase) * 1000 / ms
}

func (p *progressWriter) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s  %s", p.label, humanBytes(p.done))
	if p.total > 0 {
		fmt.Fprintf(&b, " / %s  %d%%", humanBytes(p.total), p.done*100/p.total)
	}
	if rate := p.rate(); rate > 0 {
		fmt.Fprintf(&b, "  %s/s", humanBytes(rate))
		if p.total > p.done {
			fmt.Fprintf(&b, "  剩余 %s", humanDuration(time.Duration((p.total-p.done)/rate)*time.Second))
		}
	}
	return b.String()
}

// finish 收尾：补一行总结，终端上顺带把 \r 那行换行收掉。
func (p *progressWriter) finish() {
	if p == nil || p.out == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s  %s 取回完成", p.label, humanBytes(p.done))
	if p.fetched < p.done {
		fmt.Fprintf(&b, "（本次续传 %s）", humanBytes(p.fetched))
	}
	fmt.Fprintf(&b, "，用时 %s", humanDuration(time.Since(p.start)))
	if ms := time.Since(p.start).Milliseconds(); ms >= 500 && p.fetched > 0 {
		fmt.Fprintf(&b, "，平均 %s/s", humanBytes(p.fetched*1000/ms))
	}
	p.emit(b.String())
	p.endLine()
}

// note 在进度行之间插一条消息。终端上先把 \r 那行收掉，免得被盖成半截。
func (p *progressWriter) note(msg string) {
	if p == nil || p.out == nil {
		return
	}
	p.endLine()
	fmt.Fprintf(p.out, "  %s\n", msg)
}

// abort 只负责把终端上没收尾的 \r 行换行，错误正文由调用方打印。
func (p *progressWriter) abort() {
	if p == nil {
		return
	}
	p.endLine()
}

func (p *progressWriter) emit(text string) {
	if p.out == nil {
		return
	}
	if !p.tty {
		fmt.Fprintln(p.out, text)
		return
	}
	pad := ""
	if n := p.width - len([]rune(text)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	p.width = len([]rune(text))
	fmt.Fprintf(p.out, "\r%s%s", text, pad)
	p.dirty = true
}

func (p *progressWriter) endLine() {
	if p.tty && p.dirty {
		fmt.Fprintln(p.out)
		p.dirty = false
	}
	p.width = 0
}

// isTerminal 只认真正的字符设备：输出被重定向进文件、管道或测试缓冲时不能用
// \r 原地刷新，只能一行行追加。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// humanBytes 把字节数写成人读单位，只给进度行用，不参与任何校验。
func humanBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%d.%02d GiB", n/gib, n%gib*100/gib)
	case n >= mib:
		return fmt.Sprintf("%d.%d MiB", n/mib, n%mib*10/mib)
	case n >= kib:
		return fmt.Sprintf("%d KiB", n/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanDuration(d time.Duration) string {
	sec := int64(d / time.Second)
	if sec < 0 {
		sec = 0
	}
	if sec >= 60 {
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%ds", sec)
}
