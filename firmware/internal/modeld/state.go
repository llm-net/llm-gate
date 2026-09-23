package modeld

// 持久化状态：state.json（算力服务器、对外 API 设置、令牌摘要）与 tasks.json（任务队列）。
// 两份都整份原子写；任务量小（每份至多 taskKeep 条已结束记录），不上数据库。
//
// §15.1：令牌只存 SHA-256 摘要与前缀；任务的 input / output 留在 tasks.json 里（那是任务本身的
// 数据、不是日志），不进任何日志行。

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// stateFile 是 state.json 的形状。
type stateFile struct {
	API      apiSettings `json:"api"`
	Backends []*Backend  `json:"backends"`
	Tokens   []*Token    `json:"tokens"`
}

// apiSettings 是对外 API 的设置。
type apiSettings struct {
	// Listen 是监听地址；空 = 不开。Set 为真表示状态里已显式设过（否则用安装时的缺省）。
	Listen string `json:"listen"`
	Set    bool   `json:"set"`
}

// Token 是对外 API 的一把令牌（只存摘要）。
type Token struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used_at,omitzero"`
}

// TokenView 是给界面看的令牌（不含摘要）。
type TokenView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used_at,omitempty"`
}

type state struct {
	dir string

	mu       sync.Mutex
	api      apiSettings
	backends []*Backend
	tokens   []*Token
	tasks    []*Task
	seq      int64
	// defaultListen 是安装时给的缺省监听地址。
	defaultListen string
}

func loadState(dir, defaultListen string) (*state, error) {
	st := &state{dir: dir, defaultListen: defaultListen}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		var f stateFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, errors.New("state.json 损坏：" + err.Error())
		}
		st.api, st.backends, st.tokens = f.API, f.Backends, f.Tokens
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw, err = os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err == nil {
		var f struct {
			Tasks []*Task `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, errors.New("tasks.json 损坏：" + err.Error())
		}
		st.tasks = f.Tasks
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, t := range st.tasks {
		if t.Seq > st.seq {
			st.seq = t.Seq
		}
	}
	return st, nil
}

// saveLocked 写 state.json（调用方持锁）。
func (st *state) saveLocked() error {
	f := stateFile{API: st.api, Backends: st.backends, Tokens: st.tokens}
	if f.Backends == nil {
		f.Backends = []*Backend{}
	}
	if f.Tokens == nil {
		f.Tokens = []*Token{}
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	return writeFileAtomic(filepath.Join(st.dir, "state.json"), append(raw, '\n'), 0o600)
}

// saveTasksLocked 写 tasks.json（调用方持锁）。
func (st *state) saveTasksLocked() error {
	tasks := st.tasks
	if tasks == nil {
		tasks = []*Task{}
	}
	raw, _ := json.MarshalIndent(map[string]any{"tasks": tasks}, "", "  ")
	return writeFileAtomic(filepath.Join(st.dir, "tasks.json"), append(raw, '\n'), 0o600)
}

func (st *state) apiListen() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.api.Set {
		return st.api.Listen
	}
	return st.defaultListen
}

func (st *state) setAPIListen(listen string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.api = apiSettings{Listen: listen, Set: true}
	return st.saveLocked()
}

// ---- 令牌 ----

const tokenPrefix = "msk_"

func (st *state) tokenCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.tokens)
}

func (st *state) tokenViews() []TokenView {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]TokenView, 0, len(st.tokens))
	for _, t := range st.tokens {
		v := TokenView{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339)}
		if !t.LastUsed.IsZero() {
			v.LastUsed = t.LastUsed.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// createToken 签一把新令牌，明文只在返回值里出现一次。
func (st *state) createToken(name string, now time.Time) (view TokenView, plaintext string, err error) {
	var b [32]byte
	rand.Read(b[:])
	plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plaintext))
	t := &Token{ID: newID("tok_"), Name: name, Prefix: plaintext[:len(tokenPrefix)+6], Digest: hex.EncodeToString(sum[:]), CreatedAt: now}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tokens = append(st.tokens, t)
	if err := st.saveLocked(); err != nil {
		st.tokens = st.tokens[:len(st.tokens)-1]
		return TokenView{}, "", err
	}
	return TokenView{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339)}, plaintext, nil
}

func (st *state) deleteToken(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, t := range st.tokens {
		if t.ID == id {
			st.tokens = append(st.tokens[:i], st.tokens[i+1:]...)
			return st.saveLocked()
		}
	}
	return notFound("令牌不存在")
}

// verifyToken 校验一把令牌明文；命中即记最近使用时刻（不每次落盘：只在分钟粒度变化时写）。
func (st *state) verifyToken(plaintext string, now time.Time) (*Token, bool) {
	if plaintext == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(plaintext))
	digest := hex.EncodeToString(sum[:])
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, t := range st.tokens {
		if subtle.ConstantTimeCompare([]byte(t.Digest), []byte(digest)) == 1 {
			if now.Sub(t.LastUsed) >= time.Minute {
				t.LastUsed = now
				_ = st.saveLocked()
			}
			return t, true
		}
	}
	return nil, false
}
