package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MediaJob 是一条媒体生成任务（表 media_jobs）：设备自己向订阅或上游平台发起的一次图像 /
// 视频生成，编排在 internal/mediagen。一个任务一个结果文件；一次提交出多个候选时落成
// 同批（BatchID）的多条任务，各自独立到终态。
//
// 归属：每条任务都挂一把客户端 Key（KeyID / KeyDisplay 快照）——订阅授权、准入、并发上限
// 与用量都按它裁决；历史上没有归属密钥的旧行 KeyID 为 0。AccountID 是订阅后端生成时
// 钉死的订阅账号行，按量后端为 0（上游账户钉在 aigc_tasks 行里）。
type MediaJob struct {
	ID string `json:"id"`
	// Origin 是任务来源（MediaOrigin*）：页面列表只列 page，studio / cli 的任务只能凭 id 访问。
	Origin  string `json:"origin"`
	BatchID string `json:"batch_id"`
	// Owner 是来源自己的归属信息（JSON，studio：workspace_id / chat_id / run_id / name），
	// 内核不解释，也不给页面。
	Owner string `json:"-"`
	// Backend 是承载这次生成的后端名；Provider 列保留，两者同值。
	Backend    string `json:"backend"`
	Provider   string `json:"provider"`
	AccountID  int64  `json:"account_id,omitempty"`
	KeyID      int64  `json:"key_id,omitempty"`
	KeyDisplay string `json:"key_display,omitempty"`
	Kind       string `json:"kind"`
	Model      string `json:"model"`
	// Operation 是 generate / edit / extend（MediaOp*）。
	Operation string `json:"operation"`
	Prompt    string `json:"prompt,omitempty"`
	VendorID  string `json:"vendor_id,omitempty"`
	Status    string `json:"status"`
	MediaURL  string `json:"media_url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	// MediaFile 是保存在设备结果目录下的结果文件名（<ID>.<扩展名>）；空 = 没有本地文件。
	MediaFile string `json:"media_file,omitempty"`
	// ThumbFile 是结果缩略图（短边 480 的 JPEG）的文件名（<ID>.thumb.jpg）；空 = 没有缩略图。
	ThumbFile string `json:"thumb_file,omitempty"`
	Error     string `json:"error,omitempty" i18n:"text"`
	// Params 是提交时的生成参数快照（键名即各模型的参数名）；Inputs 是输入形态快照
	// （各角色带了几个），媒体本身不落行。
	Params map[string]any `json:"params,omitempty"`
	Inputs map[string]int `json:"inputs,omitempty"`
	// CreatedAt 是提交时间；UpdatedAt 是行最后一次写回；FinishedAt 是首次到终态的完成时间，
	// 由 UpdateMediaJob 在首次写成终态时记下，之后补文件、补缩略图的写回不再改动。
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// 任务来源。
const (
	MediaOriginPage   = "page"
	MediaOriginStudio = "studio"
	MediaOriginCLI    = "cli"
)

// 操作词汇。
const (
	MediaOpGenerate = "generate"
	MediaOpEdit     = "edit"
	MediaOpExtend   = "extend"
)

// 输入角色（inputs 快照与提交体的键名）。
const (
	MediaRoleFirstFrame      = "first_frame"
	MediaRoleLastFrame       = "last_frame"
	MediaRoleReferenceImages = "reference_images"
	MediaRoleSourceVideo     = "source_video"
)

// 任务状态：running 是设备自己还在生成（后台请求平台尚未返回），进程重启后没有人再接手，
// 启动时统一判失败；queued 是平台已受理、设备后台按间隔向平台查询进度，行里带平台任务 ID，
// 进程重启后编排层据此接着查。
const (
	MediaStatusRunning   = "running"
	MediaStatusQueued    = "queued"
	MediaStatusSucceeded = "succeeded"
	MediaStatusFailed    = "failed"
	MediaStatusExpired   = "expired"
)

// MediaStatusActive 是两种尚未到终态的状态：陪等、并发上限与重启接续都按它们裁决。
func MediaStatusActive(status string) bool {
	return status == MediaStatusRunning || status == MediaStatusQueued
}

// MediaStatusTerminal 判状态是否已到终态。
func MediaStatusTerminal(status string) bool {
	return status == MediaStatusSucceeded || status == MediaStatusFailed || status == MediaStatusExpired
}

// MarshalJSON 把任务行编码给页面：media_url 若是 data URI（内联 base64 结果，尚未落成本地
// 文件），只保留 "data:<mime>;base64," 头、不带正文——页面据非空判「有结果」，正文一律经
// …/media 端点取回。
func (j MediaJob) MarshalJSON() ([]byte, error) {
	type plain MediaJob
	if strings.HasPrefix(j.MediaURL, "data:") {
		if i := strings.IndexByte(j.MediaURL, ','); i >= 0 {
			j.MediaURL = j.MediaURL[:i+1]
		}
	}
	return json.Marshal(plain(j))
}

const mediaJobColumns = `id,origin,owner,batch_id,backend,provider,account_id,key_id,key_display,kind,model,operation,prompt,vendor_id,status,media_url,media_type,media_file,thumb_file,error,params,inputs,created_at,updated_at,finished_at`

func (s *Store) CreateMediaJob(ctx context.Context, j MediaJob) error {
	return createMediaJob(ctx, s.db, j)
}

// CreateMediaJobs 在一个事务里落同批任务：全部成功或一条不留。
func (s *Store) CreateMediaJobs(ctx context.Context, jobs []MediaJob) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是空操作
	for _, j := range jobs {
		if err := createMediaJob(ctx, tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func createMediaJob(ctx context.Context, db execer, j MediaJob) error {
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	if j.UpdatedAt.IsZero() {
		j.UpdatedAt = j.CreatedAt
	}
	if j.Origin == "" {
		j.Origin = MediaOriginPage
	}
	if j.Backend == "" {
		j.Backend = j.Provider
	}
	if j.Provider == "" {
		j.Provider = j.Backend
	}
	if j.Operation == "" {
		j.Operation = MediaOpGenerate
	}
	_, err := db.ExecContext(ctx, `INSERT INTO media_jobs (`+mediaJobColumns+`)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Origin, j.Owner, j.BatchID, j.Backend, j.Provider, j.AccountID, j.KeyID, j.KeyDisplay, j.Kind, j.Model, j.Operation, j.Prompt,
		j.VendorID, j.Status, j.MediaURL, j.MediaType, j.MediaFile, j.ThumbFile, j.Error, encodeJSONColumn(j.Params), encodeJSONColumn(j.Inputs),
		fmtTime(j.CreatedAt), fmtTime(j.UpdatedAt), fmtOptTime(j.FinishedAt))
	return err
}

// encodeJSONColumn 把快照编成列值：空 map 为空串。
func encodeJSONColumn[M ~map[string]V, V any](m M) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// legacyInputKeys 是改表前混在 params 里的输入形态键：读取时挪进 Inputs。
var legacyInputKeys = [...]string{MediaRoleFirstFrame, MediaRoleLastFrame, MediaRoleReferenceImages, MediaRoleSourceVideo}

// decodeMediaSnapshots 解 params / inputs 两列。旧行的 params 里混着输入形态（first_frame /
// last_frame / source_video 为 true、reference_images 为张数）与 operation：挪到各自的字段，
// 其余键原样保留。解不开的列当没有。
func decodeMediaSnapshots(j *MediaJob, params, inputs string) {
	if params != "" {
		var m map[string]any
		if json.Unmarshal([]byte(params), &m) == nil {
			legacy := map[string]int{}
			for _, k := range legacyInputKeys {
				switch v := m[k].(type) {
				case bool:
					if v {
						legacy[k] = 1
					}
					delete(m, k)
				case float64:
					if v > 0 {
						legacy[k] = int(v)
					}
					delete(m, k)
				}
			}
			if op, ok := m["operation"].(string); ok {
				if j.Operation == "" {
					j.Operation = op
				}
				delete(m, "operation")
			}
			if len(m) > 0 {
				j.Params = m
			}
			if len(legacy) > 0 {
				j.Inputs = legacy
			}
		}
	}
	if inputs != "" {
		var m map[string]int
		if json.Unmarshal([]byte(inputs), &m) == nil && len(m) > 0 {
			j.Inputs = m
		}
	}
	if j.Operation == "" {
		j.Operation = MediaOpGenerate
	}
}

// UpdateMediaJob 写回任务行；状态已到终态而行里还没有完成时间时，把这一刻记为完成时间
// （只在首次写成终态时发生，后续写回保留原值）。
func (s *Store) UpdateMediaJob(ctx context.Context, j MediaJob) error {
	now := time.Now()
	if j.FinishedAt == nil && MediaStatusTerminal(j.Status) {
		j.FinishedAt = &now
	}
	_, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET account_id=?, vendor_id=?, status=?, media_url=?, media_type=?, media_file=?, thumb_file=?, error=?, updated_at=?, finished_at=? WHERE id=?`,
		j.AccountID, j.VendorID, j.Status, j.MediaURL, j.MediaType, j.MediaFile, j.ThumbFile, j.Error, fmtTime(now), fmtOptTime(j.FinishedAt), j.ID)
	return err
}

// fmtOptTime 把可空时间写成列值：nil 为空串。
func fmtOptTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}

// FailRunningMediaJobs 把仍标记为 running 的任务判为失败并写入原因，返回改动行数。只在进程
// 启动时调用：后台生成的 goroutine 随进程消亡，行不会再更新。
func (s *Store) FailRunningMediaJobs(ctx context.Context, reason string) (int64, error) {
	now := fmtTime(time.Now())
	res, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET status='failed', error=?, updated_at=?, finished_at=? WHERE status=?`, reason, now, now, MediaStatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MediaJobFiles 列出仍被任务行引用的结果与缩略图文件名：编排层据此清掉目录里的孤儿文件。
func (s *Store) MediaJobFiles(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT media_file, thumb_file FROM media_jobs WHERE media_file<>'' OR thumb_file<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var media, thumb string
		if err := rows.Scan(&media, &thumb); err != nil {
			return nil, err
		}
		if media != "" {
			out[media] = true
		}
		if thumb != "" {
			out[thumb] = true
		}
	}
	return out, rows.Err()
}

// ListMediaJobsNeedingMedia 列出已成功但结果仍只在行里（data URI）或还没有缩略图的图像任务：
// 进程启动时编排层把它们补成本地文件与缩略图。视频的缩略图由页面回传封面帧，不在此列。
func (s *Store) ListMediaJobsNeedingMedia(ctx context.Context) ([]MediaJob, error) {
	return s.listMediaJobs(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs
	WHERE status='succeeded' AND ((media_file='' AND media_url LIKE 'data:%') OR (media_file<>'' AND thumb_file='' AND kind=?))
	ORDER BY created_at DESC`, ModelKindImage)
}

func (s *Store) GetMediaJob(ctx context.Context, id string) (*MediaJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs WHERE id=?`, id)
	return scanMediaJob(row)
}

// ListPageMediaJobs 列页面提交的任务（管理员视角，含各把 Key 发起的），最新 100 条；
// studio / cli 的任务不进任何列表。
func (s *Store) ListPageMediaJobs(ctx context.Context) ([]MediaJob, error) {
	return s.listMediaJobs(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs WHERE origin=? ORDER BY created_at DESC LIMIT 100`, MediaOriginPage)
}

// ListPageMediaJobsByKey 只列某把 Key 在页面提交的任务（Key 持有人视角），最新 100 条。
func (s *Store) ListPageMediaJobsByKey(ctx context.Context, keyID int64) ([]MediaJob, error) {
	return s.listMediaJobs(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs WHERE origin=? AND key_id=? ORDER BY created_at DESC LIMIT 100`, MediaOriginPage, keyID)
}

// CountActiveMediaJobsByKey 数某把 Key 仍未到终态的任务（全部来源合计）：并发上限按它裁决。
func (s *Store) CountActiveMediaJobsByKey(ctx context.Context, keyID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_jobs WHERE key_id=? AND status IN (?, ?)`, keyID, MediaStatusRunning, MediaStatusQueued).Scan(&n)
	return n, err
}

// ListQueuedMediaJobs 列出平台排队中、带平台任务 ID 的任务：进程启动后编排层为它们重新
// 开启后台查询。
func (s *Store) ListQueuedMediaJobs(ctx context.Context) ([]MediaJob, error) {
	return s.listMediaJobs(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs WHERE status=? AND vendor_id<>'' ORDER BY created_at`, MediaStatusQueued)
}

// ListTerminalMediaJobsByOrigin 列出某个来源已到终态、尚未被来源搬走的任务（旧→新）：
// 进程启动后编排层把它们交给来源注册的收尾器。
func (s *Store) ListTerminalMediaJobsByOrigin(ctx context.Context, origin string) ([]MediaJob, error) {
	return s.listMediaJobs(ctx, `SELECT `+mediaJobColumns+` FROM media_jobs WHERE origin=? AND status IN (?, ?, ?) ORDER BY created_at`,
		origin, MediaStatusSucceeded, MediaStatusFailed, MediaStatusExpired)
}

func (s *Store) listMediaJobs(ctx context.Context, query string, args ...any) ([]MediaJob, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaJob
	for rows.Next() {
		j, err := scanMediaJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

type mediaJobScanner interface{ Scan(...any) error }

func scanMediaJob(r mediaJobScanner) (*MediaJob, error) {
	var j MediaJob
	var created, updated, finished, params, inputs string
	if err := r.Scan(&j.ID, &j.Origin, &j.Owner, &j.BatchID, &j.Backend, &j.Provider, &j.AccountID, &j.KeyID, &j.KeyDisplay, &j.Kind, &j.Model, &j.Operation, &j.Prompt,
		&j.VendorID, &j.Status, &j.MediaURL, &j.MediaType, &j.MediaFile, &j.ThumbFile, &j.Error, &params, &inputs, &created, &updated, &finished); err != nil {
		return nil, err
	}
	decodeMediaSnapshots(&j, params, inputs)
	var err error
	if j.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if j.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	if finished != "" {
		ft, err := parseTime(finished)
		if err != nil {
			return nil, err
		}
		j.FinishedAt = &ft
	}
	return &j, nil
}

// DeleteMediaJob 删除一条任务记录；不存在时返回 sql.ErrNoRows。结果文件由编排层清扫。
func (s *Store) DeleteMediaJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM media_jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeletePageMediaJobs 清空页面提交的任务记录（管理员视角，含各把 Key 的），返回删除行数；
// studio / cli 的任务归各自的来源收尾，不在这里删。
func (s *Store) DeletePageMediaJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM media_jobs WHERE origin=?`, MediaOriginPage)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeletePageMediaJobsByKey 清空某把 Key 在页面提交的任务记录，返回删除行数。
func (s *Store) DeletePageMediaJobsByKey(ctx context.Context, keyID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM media_jobs WHERE origin=? AND key_id=?`, MediaOriginPage, keyID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LookupKeyAuthByID 按行 id 取与数据面鉴权同一份事实（归属、启停位、限额与按量额度）：
// 设备自己以某把 Key 的名义发起调用时（媒体生成），准入闸要的就是这一份快照。已归档的
// Key 与不存在同答 ErrNotFound。
func (s *Store) LookupKeyAuthByID(ctx context.Context, id int64) (*KeyAuth, error) {
	var (
		a                         KeyAuth
		prefix, last4             string
		keyDisabled               int
		keyDay, keyWeek, keyMonth sql.NullInt64
		rpm                       sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, display_prefix, display_last4, disabled,
		       budget_day_micro, budget_week_micro, budget_month_micro, rpm_limit,
		       metered_allowance_micro
		  FROM api_keys WHERE id = ? AND archived_at IS NULL`, id).
		Scan(&a.KeyID, &prefix, &last4, &keyDisabled, &keyDay, &keyWeek, &keyMonth, &rpm, &a.KeyMeteredAllowanceMicro)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("查询 Key: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查询 Key: %w", err)
	}
	a.KeyDisplay = KeyDisplay(prefix, last4)
	a.KeyDisabled = keyDisabled != 0
	a.KeyBudgetDayMicro = nullableInt64(keyDay)
	a.KeyBudgetWeekMicro = nullableInt64(keyWeek)
	a.KeyBudgetMonthMicro = nullableInt64(keyMonth)
	a.KeyRPMLimit = nullableInt64(rpm)
	return &a, nil
}
