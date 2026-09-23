package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gate media 是 gate 与设备之间的私有协议 /gate-helper/v1/media/* 的命令行入口。
// gate 不带任何模型知识：模型、操作、输入角色与参数都由设备的能力表裁决，
// --param 原样透传。Key 只从已保存的 config.json 读出并放进 Authorization 头，
// 不接受命令行参数形式的 Key。

const (
	mediaAPIBase = "/gate-helper/v1/media"
	// 设备的提交体上限是 80 MiB；超过它的单个文件不可能被受理，读入前就拒绝。
	mediaMaxInputBytes  = 80 << 20
	mediaMaxPromptBytes = 1 << 20
	mediaMaxJSONBytes   = 4 << 20
	mediaMaxResultBytes = 4 << 30

	mediaSubmitTimeout   = 10 * time.Minute
	mediaWaitTimeout     = 90 * time.Second // 设备陪等一帧最长 30 秒
	mediaDownloadTimeout = 30 * time.Minute
	mediaWaitMaxFailures = 5
)

// 测试把它们调小；陪等请求异常快返回或失败时靠它们避免空转。
var (
	mediaWaitMinInterval = time.Second
	mediaWaitRetryDelay  = 2 * time.Second
)

var (
	mediaJobIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	mediaIntRE   = regexp.MustCompile(`^-?[0-9]+$`)
	mediaExtRE   = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
)

const mediaUsage = `用法:
  gate media models [--json]
      这把 Key 可用的图像 / 视频模型：输入角色、参数、计费类别
  gate media generate --model <id> [--operation edit|extend]
      [--prompt "…" | --prompt-file <文件>]    都不给且标准输入不是终端时读标准输入
      [--first-frame <文件>] [--last-frame <文件>] [--ref <文件>]... [--source-video <文件>]
      [--param k=v]... [--count n] [--out <名字>] [--keep] [--no-wait]
      缺省陪等到终态，把结果下载到当前目录，随后删除设备上的任务；
      --keep 保留设备上的任务，--no-wait 只逐行打印任务 id。
      --param 原样交给设备校验：true/false 为布尔，十进制整数为数字，
      同一个键给多次折成字符串数组，其余为字符串。
  gate media wait <任务 id> [--out <名字>] [--keep]
  gate media status <任务 id>
  gate media cancel <任务 id>
`

type mediaJob struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	Error     string `json:"error"`
	MediaFile string `json:"media_file"`
}

func (j mediaJob) pending() bool { return j.Status == "running" || j.Status == "queued" }

type mediaAPIError struct {
	status     int
	code       string
	message    string
	retryAfter string
}

func (e *mediaAPIError) Error() string {
	msg := e.message
	if msg == "" {
		msg = fmt.Sprintf("设备返回 HTTP %d", e.status)
	}
	if e.code != "" {
		msg += "（" + e.code + "）"
	}
	if e.retryAfter != "" {
		if _, err := strconv.Atoi(e.retryAfter); err == nil {
			msg += "；请在 " + e.retryAfter + " 秒后重试"
		} else {
			msg += "；请在 " + e.retryAfter + " 之后重试"
		}
	}
	return msg
}

type mediaUsageError struct{ msg string }

func (e *mediaUsageError) Error() string { return e.msg }

func mediaUsagef(format string, args ...any) error {
	return &mediaUsageError{msg: fmt.Sprintf(format, args...)}
}

// media 分发 gate media 的子命令并返回进程退出码。
func (a *app) media(args []string, in io.Reader) int {
	if len(args) == 0 {
		fmt.Fprint(a.err, mediaUsage)
		return 2
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(a.out, mediaUsage)
		return 0
	case "models":
		err = a.mediaModels(args[1:])
	case "generate":
		err = a.mediaGenerate(args[1:], in)
	case "wait":
		err = a.mediaWait(args[1:])
	case "status":
		err = a.mediaStatus(args[1:])
	case "cancel":
		err = a.mediaCancel(args[1:])
	default:
		err = mediaUsagef("未知的 media 子命令 %q", args[0])
	}
	if err == nil {
		return 0
	}
	var usage *mediaUsageError
	if errors.As(err, &usage) {
		fmt.Fprintln(a.err, "gate:", err)
		fmt.Fprint(a.err, mediaUsage)
		return 2
	}
	if !errors.Is(err, errMediaJobsFailed) {
		fmt.Fprintln(a.err, "gate:", err)
	}
	return 1
}

// errMediaJobsFailed 表示任务级失败已经逐条写到 stderr，只需要非 0 退出码。
var errMediaJobsFailed = errors.New("媒体任务失败")

// ---- HTTP ----

func (a *app) mediaHTTPClient(timeout time.Duration) *http.Client {
	client := *a.hc
	client.Timeout = timeout
	return &client
}

func (a *app) mediaRequest(method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, a.cfg.BaseURL+mediaAPIBase+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	req.Header.Set("X-LLMGate-Client", "gate/"+version)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func mediaErrorFromResponse(resp *http.Response) error {
	e := &mediaAPIError{status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		e.code, e.message = parsed.Error.Code, strings.TrimSpace(parsed.Error.Message)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		e.retryAfter = strings.TrimSpace(resp.Header.Get("Retry-After"))
	}
	return e
}

// mediaDo 发一次 JSON 请求并返回 2xx 的响应正文。
func (a *app) mediaDo(method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	req, err := a.mediaRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.mediaHTTPClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, mediaErrorFromResponse(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, mediaMaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > mediaMaxJSONBytes {
		return nil, errors.New("设备响应超过大小限制")
	}
	return b, nil
}

func (a *app) mediaJobCall(method, id, suffix string, timeout time.Duration) (mediaJob, error) {
	var job mediaJob
	var body []byte
	if method == http.MethodPost {
		body = []byte("{}")
	}
	b, err := a.mediaDo(method, "/jobs/"+url.PathEscape(id)+suffix, body, timeout)
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(b, &job); err != nil {
		return job, fmt.Errorf("解析任务: %w", err)
	}
	if job.ID == "" {
		job.ID = id
	}
	return job, nil
}

// ---- 参数解析 ----

type mediaFlags struct {
	values map[string][]string
	bools  map[string]bool
	rest   []string
}

// parseMediaFlags 接受 --k v 与 --k=v；valued / boolean 之外的 -- 参数一律报用法错误。
func parseMediaFlags(args []string, valued, boolean []string) (mediaFlags, error) {
	f := mediaFlags{values: map[string][]string{}, bools: map[string]bool{}}
	has := func(list []string, name string) bool {
		for _, item := range list {
			if item == name {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			f.rest = append(f.rest, arg)
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		switch {
		case has(boolean, name):
			if inline {
				return f, mediaUsagef("%s 不带值", name)
			}
			f.bools[name] = true
		case has(valued, name):
			if !inline {
				if i+1 >= len(args) {
					return f, mediaUsagef("%s 缺少值", name)
				}
				i++
				value = args[i]
			}
			f.values[name] = append(f.values[name], value)
		default:
			return f, mediaUsagef("未知参数 %s", name)
		}
	}
	return f, nil
}

func (f mediaFlags) single(name string) (string, bool, error) {
	v := f.values[name]
	switch len(v) {
	case 0:
		return "", false, nil
	case 1:
		return v[0], true, nil
	}
	return "", false, mediaUsagef("%s 只能给一次", name)
}

// mediaParams 把 --param k=v 折成提交体的 params：单次出现按字面推断为布尔、
// 整数或字符串；同一个键出现多次恒为字符串数组。不按逗号拆分，也不认识任何参数名。
func mediaParams(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	var order []string
	raw := map[string][]string{}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, mediaUsagef("--param 需要 k=v 形式，收到 %q", pair)
		}
		if _, seen := raw[k]; !seen {
			order = append(order, k)
		}
		raw[k] = append(raw[k], v)
	}
	out := make(map[string]any, len(order))
	for _, k := range order {
		vals := raw[k]
		if len(vals) > 1 {
			out[k] = vals
			continue
		}
		out[k] = mediaScalar(vals[0])
	}
	return out, nil
}

func mediaScalar(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if mediaIntRE.MatchString(v) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return v
}

var mediaMIMEByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".mp4":  "video/mp4",
}

func mediaDataURI(path string) (string, error) {
	mimeType, ok := mediaMIMEByExt[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return "", fmt.Errorf("%s: 不支持的扩展名（支持 png、jpg、jpeg、webp、gif、mp4）", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, mediaMaxInputBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if len(b) > mediaMaxInputBytes {
		return "", fmt.Errorf("%s: 文件超过设备提交体上限", path)
	}
	if len(b) == 0 {
		return "", fmt.Errorf("%s: 文件为空", path)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// mediaPrompt 按 --prompt、--prompt-file、非终端标准输入的顺序取提示词；
// 都没有时返回空串，由设备裁决提示词是否可省。
func mediaPrompt(f mediaFlags, in io.Reader) (string, error) {
	prompt, hasPrompt, err := f.single("--prompt")
	if err != nil {
		return "", err
	}
	file, hasFile, err := f.single("--prompt-file")
	if err != nil {
		return "", err
	}
	if hasPrompt && hasFile {
		return "", mediaUsagef("--prompt 与 --prompt-file 只能给一个")
	}
	if hasPrompt {
		return prompt, nil
	}
	var src io.Reader
	switch {
	case hasFile:
		fh, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		src = fh
	case in == nil:
		return "", nil
	default:
		if fh, ok := in.(*os.File); ok {
			st, err := fh.Stat()
			if err != nil || st.Mode()&os.ModeCharDevice != 0 {
				return "", nil
			}
		}
		src = in
	}
	b, err := io.ReadAll(io.LimitReader(src, mediaMaxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取提示词: %w", err)
	}
	if len(b) > mediaMaxPromptBytes {
		return "", errors.New("提示词超过 1 MiB")
	}
	return strings.TrimSpace(string(b)), nil
}

// ---- 子命令 ----

type mediaSubmit struct {
	Model     string         `json:"model"`
	Operation string         `json:"operation,omitempty"`
	Prompt    string         `json:"prompt,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Count     int            `json:"count,omitempty"`
}

func (a *app) mediaGenerate(args []string, in io.Reader) error {
	f, err := parseMediaFlags(args,
		[]string{"--model", "--operation", "--prompt", "--prompt-file", "--first-frame", "--last-frame",
			"--ref", "--source-video", "--param", "--count", "--out"},
		[]string{"--keep", "--no-wait"})
	if err != nil {
		return err
	}
	if len(f.rest) > 0 {
		return mediaUsagef("多余的参数 %q", f.rest[0])
	}
	var body mediaSubmit
	var ok bool
	if body.Model, ok, err = f.single("--model"); err != nil {
		return err
	} else if !ok || body.Model == "" {
		return mediaUsagef("缺少 --model")
	}
	if body.Operation, _, err = f.single("--operation"); err != nil {
		return err
	}
	if raw, given, err := f.single("--count"); err != nil {
		return err
	} else if given {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 {
			return mediaUsagef("--count 需要正整数")
		}
		body.Count = n
	}
	out, _, err := f.single("--out")
	if err != nil {
		return err
	}
	keep, noWait := f.bools["--keep"], f.bools["--no-wait"]
	if noWait && (out != "" || keep) {
		return mediaUsagef("--no-wait 不下载结果，不能与 --out 或 --keep 同用")
	}
	if body.Params, err = mediaParams(f.values["--param"]); err != nil {
		return err
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	inputs := map[string]any{}
	for flagName, role := range map[string]string{
		"--first-frame": "first_frame", "--last-frame": "last_frame", "--source-video": "source_video",
	} {
		path, given, err := f.single(flagName)
		if err != nil {
			return err
		}
		if !given {
			continue
		}
		uri, err := mediaDataURI(path)
		if err != nil {
			return err
		}
		inputs[role] = uri
	}
	if refs := f.values["--ref"]; len(refs) > 0 {
		uris := make([]string, 0, len(refs))
		for _, path := range refs {
			uri, err := mediaDataURI(path)
			if err != nil {
				return err
			}
			uris = append(uris, uri)
		}
		inputs["reference_images"] = uris
	}
	if len(inputs) > 0 {
		body.Inputs = inputs
	}
	if body.Prompt, err = mediaPrompt(f, in); err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	respBody, err := a.mediaDo(http.MethodPost, "/jobs", payload, mediaSubmitTimeout)
	if err != nil {
		return err
	}
	var accepted struct {
		Jobs []mediaJob `json:"jobs"`
	}
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		return fmt.Errorf("解析提交结果: %w", err)
	}
	if len(accepted.Jobs) == 0 {
		return errors.New("设备没有返回任务")
	}
	for _, job := range accepted.Jobs {
		if !mediaJobIDRE.MatchString(job.ID) {
			return errors.New("设备返回了无法识别的任务 id")
		}
	}
	if noWait {
		for _, job := range accepted.Jobs {
			fmt.Fprintln(a.out, job.ID)
		}
		return nil
	}
	for _, job := range accepted.Jobs {
		fmt.Fprintf(a.err, "已提交任务 %s，等待结果；中断后可用 gate media wait %s 继续\n", job.ID, job.ID)
	}
	failed := false
	for _, job := range accepted.Jobs {
		if err := a.mediaFinish(job, out, keep); err != nil {
			failed = true
			if !errors.Is(err, errMediaJobsFailed) {
				fmt.Fprintf(a.err, "gate: 任务 %s: %v\n", job.ID, err)
			}
		}
	}
	if failed {
		return errMediaJobsFailed
	}
	return nil
}

func mediaJobIDArg(rest []string, usage string) (string, error) {
	if len(rest) != 1 {
		return "", mediaUsagef("用法: %s", usage)
	}
	if !mediaJobIDRE.MatchString(rest[0]) {
		return "", mediaUsagef("任务 id 形状不对: %q", rest[0])
	}
	return rest[0], nil
}

func (a *app) mediaWait(args []string) error {
	f, err := parseMediaFlags(args, []string{"--out"}, []string{"--keep"})
	if err != nil {
		return err
	}
	id, err := mediaJobIDArg(f.rest, "gate media wait <任务 id> [--out <名字>] [--keep]")
	if err != nil {
		return err
	}
	out, _, err := f.single("--out")
	if err != nil {
		return err
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	return a.mediaFinish(mediaJob{ID: id, Status: "running"}, out, f.bools["--keep"])
}

func (a *app) mediaStatus(args []string) error {
	f, err := parseMediaFlags(args, nil, nil)
	if err != nil {
		return err
	}
	id, err := mediaJobIDArg(f.rest, "gate media status <任务 id>")
	if err != nil {
		return err
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	job, err := a.mediaJobCall(http.MethodGet, id, "", a.hc.Timeout)
	if err != nil {
		return err
	}
	line := job.ID + "\t" + job.Status + "\t" + job.Model
	if job.Error != "" {
		line += "\t" + mediaOneLine(job.Error)
	}
	fmt.Fprintln(a.out, line)
	return nil
}

func (a *app) mediaCancel(args []string) error {
	f, err := parseMediaFlags(args, nil, nil)
	if err != nil {
		return err
	}
	id, err := mediaJobIDArg(f.rest, "gate media cancel <任务 id>")
	if err != nil {
		return err
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	if err := a.mediaDelete(id); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "已删除任务", id)
	return nil
}

func (a *app) mediaDelete(id string) error {
	_, err := a.mediaDo(http.MethodDelete, "/jobs/"+url.PathEscape(id), nil, a.hc.Timeout)
	return err
}

// mediaFinish 陪等一个任务到终态：成功则下载到当前目录，随后（除非 keep）删除设备上的任务；
// 失败把 error 写到 stderr 并返回 errMediaJobsFailed。
func (a *app) mediaFinish(job mediaJob, out string, keep bool) error {
	id := job.ID
	failures := 0
	for job.pending() {
		started := time.Now()
		next, err := a.mediaJobCall(http.MethodPost, id, "/wait", mediaWaitTimeout)
		if err != nil {
			var apiErr *mediaAPIError
			if errors.As(err, &apiErr) && apiErr.status < 500 && apiErr.status != http.StatusTooManyRequests {
				return err
			}
			failures++
			if failures >= mediaWaitMaxFailures {
				return fmt.Errorf("陪等连续失败，任务仍在设备上: %w", err)
			}
			time.Sleep(mediaWaitRetryDelay)
			continue
		}
		failures = 0
		job = next
		if job.pending() {
			if rest := mediaWaitMinInterval - time.Since(started); rest > 0 {
				time.Sleep(rest)
			}
		}
	}
	if job.Status != "succeeded" {
		reason := mediaOneLine(job.Error)
		if reason == "" {
			reason = "没有错误说明"
		}
		fmt.Fprintf(a.err, "gate: 任务 %s %s: %s\n", id, job.Status, reason)
		if !keep {
			a.mediaDeleteQuietly(id)
		}
		return errMediaJobsFailed
	}
	base := out
	if base == "" {
		base = id
	}
	path, err := a.mediaDownload(job, base)
	if err != nil {
		return fmt.Errorf("下载结果失败，任务仍在设备上: %w", err)
	}
	fmt.Fprintln(a.out, path)
	if !keep {
		a.mediaDeleteQuietly(id)
	}
	return nil
}

func (a *app) mediaDeleteQuietly(id string) {
	if err := a.mediaDelete(id); err != nil {
		fmt.Fprintf(a.err, "gate: 设备上的任务 %s 未能删除: %v\n", id, err)
	}
}

var mediaExtByType = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
}

// mediaResultExt 取结果扩展名：Content-Disposition 的 filename → Content-Type → 任务的 media_file。
func mediaResultExt(h http.Header, job mediaJob) string {
	if _, params, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil {
		if ext := filepath.Ext(params["filename"]); mediaExtRE.MatchString(ext) {
			return strings.ToLower(ext)
		}
	}
	if t, _, err := mime.ParseMediaType(h.Get("Content-Type")); err == nil {
		if ext, ok := mediaExtByType[strings.ToLower(t)]; ok {
			return ext
		}
	}
	if ext := filepath.Ext(job.MediaFile); mediaExtRE.MatchString(ext) {
		return strings.ToLower(ext)
	}
	return ""
}

// mediaCreateUnique 以 O_EXCL 占住 base+ext；重名依次试 base-2、base-3…，不覆盖已有文件。
func mediaCreateUnique(base, ext string) (*os.File, string, error) {
	if ext != "" && strings.EqualFold(filepath.Ext(base), ext) {
		base = base[:len(base)-len(ext)]
	}
	for n := 1; n <= 10000; n++ {
		path := base + ext
		if n > 1 {
			path = base + "-" + strconv.Itoa(n) + ext
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			return f, path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("同名文件过多")
}

func (a *app) mediaDownload(job mediaJob, base string) (string, error) {
	req, err := a.mediaRequest(http.MethodGet, "/jobs/"+url.PathEscape(job.ID)+"/download", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := a.mediaHTTPClient(mediaDownloadTimeout).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", mediaErrorFromResponse(resp)
	}
	f, path, err := mediaCreateUnique(base, mediaResultExt(resp.Header, job))
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, mediaMaxResultBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > mediaMaxResultBytes {
		err = errors.New("结果超过大小限制")
	}
	if err == nil && n == 0 {
		err = errors.New("结果为空")
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func mediaOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---- models ----

type mediaModelsResponse struct {
	RunningPerKey int          `json:"running_per_key"`
	Models        []mediaModel `json:"models"`
}

type mediaModel struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	Billing    string           `json:"billing"`
	Note       string           `json:"note"`
	Available  bool             `json:"available"`
	ReasonCode string           `json:"reason_code"`
	Reason     string           `json:"reason"`
	Operations []mediaOperation `json:"operations"`
}

type mediaOperation struct {
	Name               string       `json:"name"`
	Label              string       `json:"label"`
	Inputs             []mediaInput `json:"inputs"`
	PromptOptionalWith []string     `json:"prompt_optional_with"`
	Params             []mediaParam `json:"params"`
}

type mediaInput struct {
	Role     string   `json:"role"`
	Max      int      `json:"max"`
	MaxBytes int64    `json:"max_bytes"`
	Formats  []string `json:"formats"`
	Required bool     `json:"required"`
}

type mediaParam struct {
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Values    []string    `json:"values"`
	Min       json.Number `json:"min"`
	Max       json.Number `json:"max"`
	Unit      string      `json:"unit"`
	MaxItems  int         `json:"max_items"`
	MaxLength int         `json:"max_length"`
	Required  bool        `json:"required"`
}

func (a *app) mediaModels(args []string) error {
	f, err := parseMediaFlags(args, nil, []string{"--json"})
	if err != nil {
		return err
	}
	if len(f.rest) > 0 {
		return mediaUsagef("多余的参数 %q", f.rest[0])
	}
	if err := a.requireDevice(); err != nil {
		return err
	}
	body, err := a.mediaDo(http.MethodGet, "/models", nil, a.hc.Timeout)
	if err != nil {
		return err
	}
	if f.bools["--json"] {
		a.out.Write(body)
		if !bytes.HasSuffix(body, []byte("\n")) {
			fmt.Fprintln(a.out)
		}
		return nil
	}
	var got mediaModelsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("解析模型能力表: %w", err)
	}
	if len(got.Models) == 0 {
		fmt.Fprintln(a.out, "这把 Key 没有可列出的图像 / 视频模型。")
		return nil
	}
	if got.RunningPerKey > 0 {
		fmt.Fprintf(a.out, "每把 Key 同时进行的任务上限：%d\n\n", got.RunningPerKey)
	}
	for i, m := range got.Models {
		if i > 0 {
			fmt.Fprintln(a.out)
		}
		state := "可用"
		if !m.Available {
			state = "不可用"
			if reason := mediaOneLine(m.Reason); reason != "" {
				state += "：" + reason
			}
			if m.ReasonCode != "" {
				state += "（" + m.ReasonCode + "）"
			}
		}
		fmt.Fprintf(a.out, "%s  %s  %s  %s\n", m.ID, m.Kind, m.Billing, state)
		if note := mediaOneLine(m.Note); note != "" {
			fmt.Fprintf(a.out, "  %s\n", note)
		}
		for _, op := range m.Operations {
			title := op.Name
			if op.Label != "" {
				title += "（" + op.Label + "）"
			}
			fmt.Fprintf(a.out, "  %s\n", title)
			if len(op.PromptOptionalWith) == 0 {
				fmt.Fprintln(a.out, "    提示词: 必填")
			} else {
				fmt.Fprintf(a.out, "    提示词: 带 %s 时可省\n", strings.Join(op.PromptOptionalWith, " / "))
			}
			for _, in := range op.Inputs {
				fmt.Fprintf(a.out, "    输入 %s\n", mediaInputSummary(in))
			}
			for _, p := range op.Params {
				fmt.Fprintf(a.out, "    参数 %s\n", mediaParamSummary(p))
			}
		}
	}
	return nil
}

func mediaInputSummary(in mediaInput) string {
	parts := []string{in.Role}
	if in.Max > 0 {
		parts = append(parts, "最多 "+strconv.Itoa(in.Max)+" 个")
	}
	if len(in.Formats) > 0 {
		parts = append(parts, strings.Join(in.Formats, "/"))
	}
	if in.MaxBytes > 0 {
		parts = append(parts, "每个不超过 "+mediaByteSize(in.MaxBytes))
	}
	if in.Required {
		parts = append(parts, "必填")
	}
	return strings.Join(parts, "  ")
}

func mediaParamSummary(p mediaParam) string {
	parts := []string{p.Name, p.Type}
	switch {
	case len(p.Values) > 0:
		parts = append(parts, strings.Join(p.Values, " | "))
	case p.Min != "" || p.Max != "":
		parts = append(parts, strings.TrimSpace(p.Min.String()+"–"+p.Max.String()+" "+p.Unit))
	}
	if p.MaxItems > 0 {
		parts = append(parts, "最多 "+strconv.Itoa(p.MaxItems)+" 项")
	}
	if p.MaxLength > 0 {
		parts = append(parts, "最长 "+strconv.Itoa(p.MaxLength))
	}
	if p.Required {
		parts = append(parts, "必填")
	}
	return strings.Join(parts, "  ")
}

func mediaByteSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}
	return strconv.FormatInt(n, 10) + " 字节"
}
