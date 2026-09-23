package studio

// 创作工作空间的目录：落在设备数据目录下（<data_dir>/workspaces/<id>/）或一台工作节点上
// （~/workspaces/<name>，经守护进程 devd 读写），操作集在 storage.go。每个空间固定三个子目录，
// 文件一律以「目录/文件名」的路径引用（Path），根目录与别的子目录不进清单：
//   - agent/  智能体文档：智能体自己维护的项目说明（agent/PROJECT.md）、计划、提示词记录、备忘；
//   - docs/   创作文档：交付给管理员的脚本、分镜、文案、字幕等文本；
//   - media/  媒体：管理员上传的与智能体生成的图像 / 视频 / 音频，生成结果只落在这里。
// 目录按种类分工（DirAllows）：文本文件只能写进 agent/ 或 docs/，其余文件只能写进 media/；这条
// 规则约束经本包的写入（上传、write_text、生成、改名），目录里已有的文件不论放对没放对都进清单。
//
// 目录是真值，studio_files 表只是附注（种类、尺寸、来源、生成它的提示词 / 模型 / 参数，主键里的
// name 存的就是路径）；列目录时对账——目录里有、表里没有的补一行 unknown，表里有、目录里没有的
// 删行。缩略图**一律缓存在设备上** <data_dir>/workspaces/<id>/.thumbs/<目录>/<文件名>.jpg（设备上的
// 空间里它就是隐藏子目录）；以 . 开头的名字保留给设备自己用，不接受上传。
//
// 目录写操作（上传 / 落盘 / 删 / 改名 / 对账）按工作空间互斥，先写临时文件再改名，半成品不会
// 被当成结果。文件路径进日志只记路径，正文不记（§15.1）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	// WorkspacesDirName 是数据目录下放创作工作空间（及主机上空间的缩略图缓存）的子目录名。
	WorkspacesDirName = "workspaces"
	thumbsDirName     = ".thumbs"
	// MaxUploadBytes 是单个上传文件的上限（与媒体生成的结果文件上限同量级）。
	MaxUploadBytes = 512 << 20
	// MaxTextBytes 是智能体读写文本文件的上限。
	MaxTextBytes = 256 << 10
	// MaxFileNameRunes 是文件名长度上限。
	MaxFileNameRunes = 120
	// MaxListFiles 是一次清单返回的上限：目录再大也不该把页面撑爆。
	MaxListFiles = 2000
	// ThumbShortEdge 是目录预览图的目标短边；等比缩小、不裁切，小图不放大。
	ThumbShortEdge = mediagen.ThumbShortEdge
	// thumbSourceLimit 是为了量尺寸 / 做缩略图愿意整份读进内存的图像上限。
	thumbSourceLimit = 32 << 20
	// analyzePerList 是一次列目录里最多补量 / 补缩略图的图像数（主机上的每张要经 SSH 取回）。
	analyzePerList = 8
)

// 工作空间里的三个子目录。
const (
	// DirAgent 放智能体自己的文档：项目说明、计划、提示词记录、备忘。
	DirAgent = "agent"
	// DirDocs 放创作文档：脚本、分镜、文案、字幕等交付给管理员的文本。
	DirDocs = "docs"
	// DirMedia 放媒体：上传或生成的图像 / 视频 / 音频。
	DirMedia = "media"
)

// Dirs 是三个子目录的固定顺序（清单、开发者指令与页面都按它排）。
var Dirs = []string{DirAgent, DirDocs, DirMedia}

// BriefFileName 是项目说明文件的路径：智能体维护、页面可看。
const BriefFileName = DirAgent + "/PROJECT.md"

// ValidDir 报告 dir 是不是三个子目录之一。
func ValidDir(dir string) bool {
	return dir == DirAgent || dir == DirDocs || dir == DirMedia
}

// DirAllows 报告种类为 kind 的文件能不能写进 dir：文本只能进 agent/ 或 docs/，其余只能进 media/。
func DirAllows(dir, kind string) bool {
	if kind == store.StudioFileText {
		return dir == DirAgent || dir == DirDocs
	}
	return dir == DirMedia
}

// DefaultDir 是种类为 kind 的文件缺省该放的目录（上传时页面不指定目录就按它）。
func DefaultDir(kind string) string {
	if kind == store.StudioFileText {
		return DirDocs
	}
	return DirMedia
}

// JoinPath 把目录与文件名拼成路径。
func JoinPath(dir, name string) string { return dir + "/" + name }

// SplitPath 把路径拆成目录与文件名（不校验；没有 / 时目录为空）。
func SplitPath(p string) (dir, name string) {
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// ValidatePath 收窄一条文件路径：「目录/文件名」，目录是三个子目录之一，文件名经 ValidateFileName。
func ValidatePath(p string) (dir, name string, err error) {
	dir, name = SplitPath(p)
	if !ValidDir(dir) {
		return "", "", &Error{Code: CodeFileInvalid, Msg: "文件路径须是 agent/、docs/ 或 media/ 之下的「目录/文件名」"}
	}
	if err := ValidateFileName(name); err != nil {
		return "", "", err
	}
	return dir, name, nil
}

// placementError 是把文件写进不该放的目录时的拒绝。
func placementError(dir, kind string) error {
	if kind == store.StudioFileText {
		return &Error{Code: CodeFileInvalid, Msg: "文本文件只能放在 agent/ 或 docs/ 目录，不能放进 " + dir + "/"}
	}
	return &Error{Code: CodeFileInvalid, Msg: "图像 / 视频 / 音频等媒体文件只能放在 media/ 目录，不能放进 " + dir + "/"}
}

func workspacesRoot(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, WorkspacesDirName)
}

// Root 是放全部创作工作空间的目录（空 = 没配数据目录，只在窄测试里出现）。
func (m *Manager) Root() string { return m.root }

// Dir 是设备上的目录：设备落点的工作空间就是它的目录，主机落点的只放缩略图缓存。
func (m *Manager) Dir(ws *store.Workspace) string {
	if !ws.OnHost() && ws.Path != "" {
		return ws.Path
	}
	return filepath.Join(m.root, ws.ID)
}

// PathFor 是新建设备落点的工作空间时要写进行里的路径。
func (m *Manager) PathFor(id string) string { return filepath.Join(m.root, id) }

// RemoveDir 删掉设备上属于这个工作空间的目录（设备落点的整个目录、主机落点的缩略图缓存）；
// 只认 root 之下的路径。主机上的目录由 devhost.RemoveWorkspaceDir 收。
func (m *Manager) RemoveDir(ws *store.Workspace) error {
	dir := m.Dir(ws)
	if m.root == "" || !strings.HasPrefix(dir, m.root+string(filepath.Separator)) {
		return errors.New("拒绝删除工作空间目录以外的路径")
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除工作空间目录: %w", err)
	}
	return nil
}

func (m *Manager) lockFS(wsID string) *sync.Mutex {
	m.mu.Lock()
	mu, ok := m.fsMu[wsID]
	if !ok {
		mu = &sync.Mutex{}
		m.fsMu[wsID] = mu
	}
	m.mu.Unlock()
	mu.Lock()
	return mu
}

// ValidateFileName 收窄文件名（路径里目录之后的那一段）：1–MaxFileNameRunes 个字符，不含路径
// 分隔符、控制字符与 NUL，不以 . 开头（隐藏名保留给设备），首尾不是空白。
func ValidateFileName(name string) error {
	if name == "" || !utf8.ValidString(name) {
		return &Error{Code: CodeFileInvalid, Msg: "文件名不能为空"}
	}
	if utf8.RuneCountInString(name) > MaxFileNameRunes || len(name) > 255 {
		return &Error{Code: CodeFileInvalid, Msg: fmt.Sprintf("文件名最多 %d 个字符", MaxFileNameRunes)}
	}
	if strings.HasPrefix(name, ".") {
		return &Error{Code: CodeFileInvalid, Msg: "文件名不能以 . 开头"}
	}
	if name != strings.TrimSpace(name) {
		return &Error{Code: CodeFileInvalid, Msg: "文件名首尾不能有空白"}
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 || unicode.IsControl(r) {
			return &Error{Code: CodeFileInvalid, Msg: "文件名不能含路径分隔符或控制字符"}
		}
	}
	return nil
}

// FileKind 按扩展名判文件种类（传路径或文件名都行）。
func FileKind(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "png", "jpg", "jpeg", "webp", "gif":
		return store.StudioFileImage
	case "mp4", "webm", "mov", "m4v":
		return store.StudioFileVideo
	case "mp3", "wav", "m4a", "aac", "ogg", "flac":
		return store.StudioFileAudio
	case "md", "txt", "json", "csv", "srt", "vtt", "yaml", "yml", "xml", "html", "htm", "css", "js", "ts", "py", "sh", "toml", "ini":
		return store.StudioFileText
	}
	return store.StudioFileOther
}

// MimeFor 按扩展名给 MIME（认不出时 application/octet-stream）。
func MimeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".txt", ".srt", ".vtt":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ListFiles 列三个子目录并与附注表对账，回带附注的清单（按创建时间排序，最多 MaxListFiles 项；
// 每项的 Name 是路径）。缺失的子目录顺手建出来。
func (m *Manager) ListFiles(ctx context.Context, ws *store.Workspace) ([]store.StudioFile, error) {
	st, err := m.storage(ws)
	if err != nil {
		return nil, err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	return m.listLocked(ctx, ws, st)
}

func (m *Manager) listLocked(ctx context.Context, ws *store.Workspace, st Storage) ([]store.StudioFile, error) {
	type pathEntry struct {
		Path string
		Entry
	}
	var entries []pathEntry
	for _, dir := range Dirs {
		list, err := st.List(ctx, dir)
		if err != nil {
			return nil, err
		}
		for _, e := range list {
			entries = append(entries, pathEntry{Path: JoinPath(dir, e.Name), Entry: e})
		}
	}
	rows, err := m.st.ListStudioFiles(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	known := make(map[string]store.StudioFile, len(rows))
	for _, r := range rows {
		known[r.Name] = r
	}
	present := map[string]bool{}
	out := make([]store.StudioFile, 0, len(entries))
	analyzed := 0
	for _, e := range entries {
		present[e.Path] = true
		row, ok := known[e.Path]
		mod := fileTime(e.ModTime)
		changed := !ok || row.Bytes != e.Size || (!row.ModTime.IsZero() && !row.ModTime.Equal(mod))
		if changed {
			// 目录里出现、表里没有（或大小 / 修改时刻对不上：不经本包改过）的文件：附注归零，
			// 补一行 unknown。
			row = store.StudioFile{WorkspaceID: ws.ID, Name: e.Path, Kind: FileKind(e.Path), Mime: MimeFor(e.Path), Bytes: e.Size,
				Origin: store.StudioFileOriginUnknown, CreatedAt: e.ModTime, ModTime: mod}
			_ = os.Remove(m.thumbPath(ws, e.Path))
		}
		// 还没记过修改时刻的存量行：按此刻的补上，附注照旧。
		backfill := !changed && row.ModTime.IsZero()
		if backfill {
			row.ModTime = mod
		}
		// 图像的尺寸与缩略图：新文件（或改过的）补量，每次列目录最多 analyzePerList 张。
		if row.Kind == store.StudioFileImage && (changed || row.Width == 0 || !m.HasThumb(ws, row.Name)) && analyzed < analyzePerList {
			analyzed++
			if w, h, ok := m.analyzeImage(ctx, ws, st, row.Name, row.Bytes); ok {
				row.Width, row.Height = w, h
				changed = changed || known[e.Path].Width != w || known[e.Path].Height != h
			}
		}
		if changed || backfill {
			saved, err := m.st.UpsertStudioFile(ctx, row)
			if err != nil {
				return nil, err
			}
			row = *saved
		}
		out = append(out, row)
	}
	for name := range known {
		if !present[name] {
			_ = m.st.DeleteStudioFile(ctx, ws.ID, name)
			_ = os.Remove(m.thumbPath(ws, name))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > MaxListFiles {
		out = out[:MaxListFiles]
	}
	return out, nil
}

// analyzeImage 读一张图像（≤ thumbSourceLimit）量尺寸并写缩略图；解不开的格式（WebP）只量得到
// 尺寸时也回尺寸。调用方持目录锁。
func (m *Manager) analyzeImage(ctx context.Context, ws *store.Workspace, st Storage, p string, size int64) (int, int, bool) {
	if size > thumbSourceLimit {
		return 0, 0, false
	}
	obj, err := st.Open(ctx, p)
	if err != nil {
		return 0, 0, false
	}
	data, err := io.ReadAll(io.LimitReader(obj.Reader(), thumbSourceLimit+1))
	obj.Close()
	if err != nil || int64(len(data)) > thumbSourceLimit {
		return 0, 0, false
	}
	w, h := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h = cfg.Width, cfg.Height
	}
	if thumb, err := mediagen.MakeThumb(bytes.NewReader(data), ThumbShortEdge); err == nil {
		_ = m.writeThumb(ws, p, thumb)
	} else {
		m.log.Info("设备未能生成工作空间图像的缩略图", "file", p, "reason", err.Error())
	}
	return w, h, w > 0
}

// FileMeta 是落盘时写进附注的来源信息。
type FileMeta struct {
	Origin   string
	Provider string
	Model    string
	Prompt   string
	Params   string
	ChatID   string
	RunID    string
}

// SaveFile 把 r 的内容写成路径 p 的文件（同名覆盖），并写附注、生成缩略图。目录须与文件种类
// 相配（DirAllows）。limit 是允许的字节上限（超限即 CodeFileTooLarge，半成品不留）。
func (m *Manager) SaveFile(ctx context.Context, ws *store.Workspace, p string, r io.Reader, limit int64, meta FileMeta) (*store.StudioFile, error) {
	dir, _, err := ValidatePath(p)
	if err != nil {
		return nil, err
	}
	if kind := FileKind(p); !DirAllows(dir, kind) {
		return nil, placementError(dir, kind)
	}
	st, err := m.storage(ws)
	if err != nil {
		return nil, err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	return m.saveLocked(ctx, ws, st, p, r, limit, meta)
}

func (m *Manager) saveLocked(ctx context.Context, ws *store.Workspace, st Storage, p string, r io.Reader, limit int64, meta FileMeta) (*store.StudioFile, error) {
	if limit <= 0 {
		limit = MaxUploadBytes
	}
	put, err := st.Put(ctx, p, newCapReader(r, limit))
	n := put.Size
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return nil, &Error{Code: CodeFileTooLarge, Msg: fmt.Sprintf("文件超过 %d MiB 上限", limit>>20)}
		}
		return nil, err
	}
	origin := meta.Origin
	if origin == "" {
		origin = store.StudioFileOriginUpload
	}
	f := store.StudioFile{WorkspaceID: ws.ID, Name: p, Kind: FileKind(p), Mime: MimeFor(p), Bytes: n, Origin: origin,
		Provider: meta.Provider, Model: meta.Model, Prompt: meta.Prompt, Params: meta.Params, ChatID: meta.ChatID, RunID: meta.RunID, CreatedAt: m.now(), ModTime: fileTime(put.ModTime)}
	_ = os.Remove(m.thumbPath(ws, p))
	if f.Kind == store.StudioFileImage {
		if w, h, ok := m.analyzeImage(ctx, ws, st, p, n); ok {
			f.Width, f.Height = w, h
		}
	}
	saved, err := m.st.UpsertStudioFile(ctx, f)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// fileTime 把文件修改时刻收成附注里存的精度（毫秒、UTC），对账时两边才比得上。
func fileTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Truncate(time.Millisecond).UTC()
}

// thumbPath 是路径 p 的缩略图在设备上的位置：<空间目录>/.thumbs/<目录>/<文件名>.jpg。
func (m *Manager) thumbPath(ws *store.Workspace, p string) string {
	return filepath.Join(m.Dir(ws), thumbsDirName, filepath.FromSlash(p)+".jpg")
}

// writeThumb 把缩略图字节写到设备上的缓存（先临时文件再改名）。
func (m *Manager) writeThumb(ws *store.Workspace, p string, data []byte) error {
	target := m.thumbPath(ws, p)
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".thumb-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// SaveThumb 收页面回传的封面帧（视频第一帧、WebP 等设备解不开的图像）：字节经重新解码、
// 缩放、编码后才落盘；已有缩略图时不改动。revision 钉住抓帧时的原件，防止同名覆盖后写入旧封面。
func (m *Manager) SaveThumb(ctx context.Context, ws *store.Workspace, p string, upload []byte, revision ...string) error {
	if _, _, err := ValidatePath(p); err != nil {
		return err
	}
	if kind := FileKind(p); kind != store.StudioFileImage && kind != store.StudioFileVideo {
		return &Error{Code: CodeFileInvalid, Msg: "只有图像和视频可以保存预览图"}
	}
	st, err := m.storage(ws)
	if err != nil {
		return err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	entry, err := thumbSourceEntry(ctx, st, p)
	if err != nil {
		return err
	}
	if len(revision) > 0 && revision[0] != "" && revision[0] != SourceRevision(store.StudioFile{Bytes: entry.Size, ModTime: entry.ModTime}) {
		return &Error{Code: CodeFileChanged, Msg: "素材已更新，请重新生成预览图"}
	}
	if m.HasThumb(ws, p) {
		return nil
	}
	data, err := mediagen.MakeThumb(bytes.NewReader(upload), ThumbShortEdge)
	if err != nil {
		return &Error{Code: CodeFileInvalid, Msg: "缩略图不是可识别的图像"}
	}
	return m.writeThumb(ws, p, data)
}

// OpenFile 打开路径 p 的文件（工具读取、页面内联显示 / 下载）。主机上的文件只能顺序读。
func (m *Manager) OpenFile(ctx context.Context, ws *store.Workspace, p string) (*mediagen.Media, error) {
	_, name, err := ValidatePath(p)
	if err != nil {
		return nil, err
	}
	st, err := m.storage(ws)
	if err != nil {
		return nil, err
	}
	obj, err := st.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	media := &mediagen.Media{ContentType: MimeFor(p), Filename: name, ModTime: obj.ModTime}
	if obj.Content != nil {
		media.Content = obj.Content
	} else {
		media.Body = obj.Body
	}
	return media, nil
}

// ServeFile 把一个文件写给 HTTP 响应：设备上的经 http.ServeContent（支持 Range，视频可拖动）；
// 主机上的把请求（含 Range）原样转给守护进程、响应流回来。disposition 为 inline / attachment。
func (m *Manager) ServeFile(w http.ResponseWriter, r *http.Request, ws *store.Workspace, p, disposition string) error {
	_, name, err := ValidatePath(p)
	if err != nil {
		return err
	}
	st, err := m.storage(ws)
	if err != nil {
		return err
	}
	hs, onHost := st.(hostStorage)
	if !onHost {
		media, err := m.OpenFile(r.Context(), ws, p)
		if err != nil {
			return err
		}
		defer media.Close()
		mediagen.ServeMedia(w, r, media, disposition)
		return nil
	}
	headers := http.Header{}
	if rg := r.Header.Get("Range"); rg != "" {
		headers.Set("Range", rg)
	}
	resp, err := hs.dev.ProxyStream(r.Context(), hs.hostID, http.MethodGet, "fs/raw", map[string][]string{"path": {hs.full(p)}}, nil, "", headers)
	if err != nil {
		return daemonErr("读取主机上的文件", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return daemonErr("读取主机上的文件", proxyErr(resp.StatusCode, raw))
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", MimeFor(p))
	w.Header().Set("Content-Disposition", disposition+"; filename="+strconv.Quote(name))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// OpenThumb 打开一个文件的缩略图；没有缩略图返回 CodeFileNotFound。
func (m *Manager) OpenThumb(ws *store.Workspace, p string) (*mediagen.Media, error) {
	_, name, err := ValidatePath(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(m.thumbPath(ws, p))
	if err != nil {
		return nil, &Error{Code: CodeFileNotFound, Msg: "没有缩略图"}
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, &Error{Code: CodeFileNotFound, Msg: "没有缩略图"}
	}
	return &mediagen.Media{ContentType: "image/jpeg", Filename: name + ".jpg", ModTime: info.ModTime(), Content: f}, nil
}

// HasThumb 报告一个文件有没有缩略图。
func (m *Manager) HasThumb(ws *store.Workspace, p string) bool {
	_, err := os.Stat(m.thumbPath(ws, p))
	return err == nil
}

// ReadFile 整份读一个文件（上限 limit 字节，超限报 CodeFileTooLarge）。
func (m *Manager) ReadFile(ctx context.Context, ws *store.Workspace, p string, limit int64) ([]byte, error) {
	media, err := m.OpenFile(ctx, ws, p)
	if err != nil {
		return nil, err
	}
	defer media.Close()
	var src io.Reader = media.Body
	if media.Content != nil {
		src = media.Content
	}
	data, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读文件: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, &Error{Code: CodeFileTooLarge, Msg: fmt.Sprintf("文件超过 %d KiB，不能整份读取", limit>>10)}
	}
	return data, nil
}

// DeleteFile 删目录里的一个文件（连缩略图与附注）。
func (m *Manager) DeleteFile(ctx context.Context, ws *store.Workspace, p string) error {
	if _, _, err := ValidatePath(p); err != nil {
		return err
	}
	st, err := m.storage(ws)
	if err != nil {
		return err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if err := st.Delete(ctx, p); err != nil {
		var se *Error
		if errors.As(err, &se) && se.Code == CodeFileNotFound {
			_ = m.st.DeleteStudioFile(ctx, ws.ID, p)
			_ = os.Remove(m.thumbPath(ws, p))
		}
		return err
	}
	_ = os.Remove(m.thumbPath(ws, p))
	return m.st.DeleteStudioFile(ctx, ws.ID, p)
}

// RenameFile 改名或在三个子目录之间挪动（目标已存在即 CodeFileExists；目标目录须与文件种类相配）。
func (m *Manager) RenameFile(ctx context.Context, ws *store.Workspace, from, to string) error {
	if _, _, err := ValidatePath(from); err != nil {
		return err
	}
	toDir, _, err := ValidatePath(to)
	if err != nil {
		return err
	}
	if kind := FileKind(to); !DirAllows(toDir, kind) {
		return placementError(toDir, kind)
	}
	if from == to {
		return nil
	}
	st, err := m.storage(ws)
	if err != nil {
		return err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if err := st.Rename(ctx, from, to); err != nil {
		return err
	}
	if target := m.thumbPath(ws, to); m.HasThumb(ws, from) {
		_ = os.MkdirAll(filepath.Dir(target), 0o700)
		_ = os.Rename(m.thumbPath(ws, from), target)
	}
	_ = m.st.DeleteStudioFile(ctx, ws.ID, to)
	if err := m.st.RenameStudioFile(ctx, ws.ID, from, to); err != nil {
		return err
	}
	row, err := m.st.GetStudioFile(ctx, ws.ID, to)
	if isNotFound(err) {
		// 目录里直接出现、还没对账过的文件（主机上命令产出）：改名后立即对账补一行 unknown，
		// 管理端点随即能读回附注。
		_, err = m.listLocked(ctx, ws, st)
		return err
	}
	if err != nil {
		return err
	}
	if FileKind(from) != FileKind(to) {
		// 扩展名变了：种类与 MIME 跟着变。
		row.Kind, row.Mime = FileKind(to), MimeFor(to)
		_, _ = m.st.UpsertStudioFile(ctx, *row)
	}
	return nil
}

// uniqueName 在子目录 dir 里给 base+ext 找一个没被占用的文件名：base.ext、base-2.ext、base-3.ext…
// 调用方须持目录锁。
func (m *Manager) uniqueName(ctx context.Context, st Storage, dir, base, ext string) string {
	taken := map[string]bool{}
	if entries, err := st.List(ctx, dir); err == nil {
		for _, e := range entries {
			taken[e.Name] = true
		}
	}
	for i := 1; i < 10000; i++ {
		name := base + ext
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		if !taken[name] {
			return name
		}
	}
	return fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext)
}

// EnsureBrief 在 agent/PROJECT.md 不存在时按工作空间的创作类型写入初始骨架（来源 agent，之后归
// 智能体维护）；已有即什么都不做。建空间时与对话新建时都调用：设备上的空间建目录后必须成功，
// 主机上的空间守护进程此刻不可用就先放过，下次新建对话再补。
func (m *Manager) EnsureBrief(ctx context.Context, ws *store.Workspace) error {
	st, err := m.storage(ws)
	if err != nil {
		return err
	}
	mu := m.lockFS(ws.ID)
	defer mu.Unlock()
	if ok, err := st.Exists(ctx, BriefFileName); err != nil {
		return err
	} else if ok {
		return nil
	}
	tpl := templateFor(ws.Template)
	_, err = m.saveLocked(ctx, ws, st, BriefFileName, strings.NewReader(tpl.Brief), MaxTextBytes, FileMeta{Origin: store.StudioFileOriginAgent})
	return err
}

// readBrief 读项目说明（没有即空）。
func (m *Manager) readBrief(ctx context.Context, ws *store.Workspace) string {
	data, err := m.ReadFile(ctx, ws, BriefFileName, MaxTextBytes)
	if err != nil {
		return ""
	}
	return string(data)
}
