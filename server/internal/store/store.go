// Package store 负责 data/ 目录里的全部持久化：配置、状态、运行历史、鉴权与日志环。
//
// 三条硬约束决定了这里几乎所有实现细节：
//
//  1. **不能因为一份坏文件就起不来**。服务挂了就意味着当晚不会签到，而"多签一次"
//     的代价远小于"漏签一整晚"（幂等由学校接口的 FState 兜底）。所以
//     config.json / state.json 解析失败时，一律改名成 *.corrupt-<unix> 留证、
//     回到默认值继续启动。
//  2. **原子写**。进程被强杀（Windows 上计划任务结束、Ctrl+C、蓝屏）时，直接覆盖
//     写会留下半截 JSON，下次启动就成了"损坏文件"。同目录 tmp + fsync + rename 在
//     同分区上是原子的：要么旧内容，要么新内容，没有中间态。
//  3. **单进程单写者**。全部方法由 sync.RWMutex 保护，runs.jsonl 用 O_APPEND
//     追加（拷到别的机器上也能用文本工具直接看），因此不需要文件锁。
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
	"fzucheckin/internal/secure"
)

const (
	configFileName = "config.json"
	configBakName  = "config.json.bak"
	stateFileName  = "state.json"
	runsFileName   = "runs.jsonl"
	runsOldName    = "runs.jsonl.1"
	authFileName   = "auth.json"
	secretFileName = "secret.key"

	// runsRotateLines：runs.jsonl 超过这个行数就轮转（spec §7）。
	// 只留一代历史（.1）：够复盘最近的异常，也不至于让 data/ 无限长大。
	runsRotateLines = 2000
	// runsMemoryCap：内存索引同样封顶。常驻服务跑几个月，历史不能全读进内存。
	runsMemoryCap = 2000
	// logRingCap：内存日志环容量（/api/v1/logs 与 SSE 的数据源）。
	logRingCap = 1000
)

// authDoc 是 auth.json 的形状。initialized_at 一经写入不再变化，
// 用来回答"这套部署是什么时候建的"。
type authDoc struct {
	PasswordHash  string `json:"password_hash"`
	InitializedAt string `json:"initialized_at"`
}

// Store 是 data/ 目录的唯一入口。Dir 是绝对路径（Open 时解析），
// 便于日志与错误信息里直接给出可点的位置。
type Store struct {
	Dir string

	mu      sync.RWMutex
	cfg     model.Config
	version int64
	state   model.State
	runs    []model.RunRecord // 旧 → 新
	index   map[string]model.RunRecord
	nLines  int              // runs.jsonl 当前行数，用于轮转判断
	logs    []model.LogEvent // 旧 → 新（对外快照时反序）
	auth    authDoc
	secret  []byte
}

// Open 打开（必要时创建）数据目录。
//
// 只有"目录建不出来"和"会话密钥写不进去"会返回错误：这两种情况服务没法安全运行，
// 早失败早暴露。其余一切（配置坏、状态坏、历史坏）都在内部降级并留痕。
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = config.DefaultDataDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("数据目录路径非法（%s）: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败（%s）: %w", abs, err)
	}

	s := &Store{Dir: abs, index: make(map[string]model.RunRecord)}
	// Open 期间没有并发读者，下列 load* 都假定"已持有写锁"（此处即无锁）。
	if err := s.loadSecret(); err != nil {
		return nil, err
	}
	s.loadAuth()
	s.loadConfig()
	s.loadState()
	s.loadRuns()

	s.appendLogLocked(model.LogEvent{
		Level: "info",
		Msg:   fmt.Sprintf("数据目录就绪 dir=%s config_version=%d", abs, s.version),
	})
	return s, nil
}

// ---------- 配置 ----------

// Config 返回配置的深拷贝：调用方随手改一改（接口里常这么写）不该改到 store 内部
// 状态，否则并发读写会直接变成数据竞争。
func (s *Store) Config() model.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return config.Clone(s.cfg)
}

// ConfigVersion 返回当前落盘的配置版本号。
func (s *Store) ConfigVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// SaveConfig 原子落盘并把版本号 +1。
//
// 旧文件先复制成 config.json.bak：改错了一档时刻想回退，是这套服务最常见的操作，
// 没有备份就只能凭记忆改回去。备份失败直接返回错误——宁可保存失败，也不能在
// 没有备份的情况下覆盖用户唯一的配置。
func (s *Store) SaveConfig(cfg model.Config, warnings []string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveConfigLocked(cfg, warnings)
}

// UpdateConfig 在写锁内重新读一遍最新配置，交给 fn 修改后才落盘。
//
// 为什么要有它：flow 在运行开始时读了一份配置快照，SSO 换到新 token 后若拿这份
// 快照整份写回，就会把这十几秒里用户通过 PUT /config 改的东西（甚至改过的密码）
// 静默覆盖掉——典型的 lost update。这里只允许改自己关心的字段（token），
// 其余字段始终以最新落盘值为准。
func (s *Store) UpdateConfig(fn func(cfg *model.Config) []string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		return s.version, nil
	}
	next := s.cfg // 结构体浅拷贝；fn 只应改值类型字段（token 之类的字符串）
	warnings := fn(&next)
	return s.saveConfigLocked(next, warnings)
}

// saveConfigLocked 是配置落盘的唯一实现（调用方必须已持有写锁）。
func (s *Store) saveConfigLocked(cfg model.Config, warnings []string) (int64, error) {
	next := s.version + 1
	cfg.Version = int(next)
	if err := s.writeConfig(cfg); err != nil {
		return s.version, err
	}
	s.cfg = cfg
	s.version = next
	for _, w := range warnings {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: "配置警告：" + w})
	}
	s.appendLogLocked(model.LogEvent{
		Level: "info",
		Msg:   fmt.Sprintf("配置已保存 config_version=%d 警告=%d 条", next, len(warnings)),
	})
	return next, nil
}

func (s *Store) loadConfig() {
	path := filepath.Join(s.Dir, configFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg:   fmt.Sprintf("读取 %s 失败（%v），本次按默认配置启动", configFileName, err),
			})
		}
		s.resetConfig()
		if werr := s.writeConfig(s.cfg); werr != nil {
			s.appendLogLocked(model.LogEvent{Level: "error", Msg: fmt.Sprintf("写入默认配置失败：%v", werr)})
		} else {
			s.appendLogLocked(model.LogEvent{Level: "info", Msg: "未找到配置，已写入安全默认模板（等待初始化向导）"})
		}
		return
	}

	var cfg model.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		name := s.quarantine(path, fmt.Sprintf("配置解析失败：%v", err))
		s.resetConfig()
		if werr := s.writeConfig(s.cfg); werr != nil {
			s.appendLogLocked(model.LogEvent{Level: "error", Msg: fmt.Sprintf("写入默认配置失败：%v", werr)})
		}
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg:   fmt.Sprintf("config.json 解析失败，已备份为 %s 并用默认配置启动（%v）", name, err),
		})
		return
	}
	if cfg.Version <= 0 {
		cfg.Version = 1
	}
	s.cfg = cfg
	s.version = int64(cfg.Version)
}

func (s *Store) resetConfig() {
	s.cfg = config.DefaultConfig()
	s.version = int64(s.cfg.Version)
}

// writeConfig 落盘配置：先备份旧文件，再原子写新文件。
func (s *Store) writeConfig(cfg model.Config) error {
	path := filepath.Join(s.Dir, configFileName)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}
	raw = append(raw, '\n')
	if old, rerr := os.ReadFile(path); rerr == nil {
		if berr := s.writeFile(filepath.Join(s.Dir, configBakName), old); berr != nil {
			return fmt.Errorf("备份旧配置失败（%s）: %w", configBakName, berr)
		}
	}
	if werr := s.writeFile(path, raw); werr != nil {
		return fmt.Errorf("写入 %s 失败: %w", configFileName, werr)
	}
	return nil
}

// ---------- 状态 ----------

func (s *Store) State() model.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func (s *Store) SaveState(st model.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeState(st); err != nil {
		return err
	}
	s.state = cloneState(st)
	return nil
}

func (s *Store) loadState() {
	path := filepath.Join(s.Dir, stateFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg:   fmt.Sprintf("读取 %s 失败（%v），本次按空状态启动", stateFileName, err),
			})
		}
		s.state = model.State{}
		return
	}
	var st model.State
	if err := json.Unmarshal(raw, &st); err != nil {
		name := s.quarantine(path, fmt.Sprintf("状态解析失败：%v", err))
		s.state = model.State{}
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg: fmt.Sprintf("state.json 解析失败，已备份为 %s 并按空状态启动（宁可多签一次，也不能启不来）：%v",
				name, err),
		})
		return
	}
	s.state = st
	if len(st.Notified) > 0 {
		s.appendLogLocked(model.LogEvent{
			Level: "info",
			Msg:   fmt.Sprintf("已恢复状态 last_success_date=%s 通知记录=%d 天", st.LastSuccessDate, len(st.Notified)),
		})
	}
}

func (s *Store) writeState(st model.State) error {
	path := filepath.Join(s.Dir, stateFileName)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("状态序列化失败: %w", err)
	}
	raw = append(raw, '\n')
	if werr := s.writeFile(path, raw); werr != nil {
		return fmt.Errorf("写入 %s 失败: %w", stateFileName, werr)
	}
	return nil
}

// ---------- 鉴权 ----------

func (s *Store) PasswordHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth.PasswordHash
}

// SetPasswordHash 写入管理密码哈希。initialized_at 只在第一次写入时落定：
// 它是"这套部署的出生时间"，重置密码不该把它改掉。
func (s *Store) SetPasswordHash(h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.auth
	doc.PasswordHash = h
	if doc.InitializedAt == "" {
		doc.InitializedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.writeAuth(doc); err != nil {
		return err
	}
	s.auth = doc
	s.appendLogLocked(model.LogEvent{Level: "info", Msg: "管理密码已更新"})
	return nil
}

func (s *Store) loadAuth() {
	path := filepath.Join(s.Dir, authFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg:   fmt.Sprintf("读取 %s 失败（%v），视为未初始化", authFileName, err),
			})
		}
		return
	}
	var doc authDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		name := s.quarantine(path, fmt.Sprintf("鉴权文件解析失败：%v", err))
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg:   fmt.Sprintf("%s 解析失败，已备份为 %s，需要重新设置管理密码", authFileName, name),
		})
		return
	}
	s.auth = doc
}

func (s *Store) writeAuth(doc authDoc) error {
	path := filepath.Join(s.Dir, authFileName)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("鉴权信息序列化失败: %w", err)
	}
	raw = append(raw, '\n')
	if werr := s.writeFile(path, raw); werr != nil {
		return fmt.Errorf("写入 %s 失败: %w", authFileName, werr)
	}
	return nil
}

// SessionKey 返回 secret.key 里的原始字节。
//
// 文件里存的是 secure.RandomHex(32) 的十六进制文本（64 字符），返回值就是这份
// 文本的字节：它稳定、够长、且与文件内容一一对应，Signer 用它做 HMAC 即可。
// 返回副本，调用方改写不会污染后续签名。
func (s *Store) SessionKey() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.secret) == 0 {
		return nil, errors.New("会话密钥不可用：secret.key 缺失或为空")
	}
	out := make([]byte, len(s.secret))
	copy(out, s.secret)
	return out, nil
}

// loadSecret 只在文件缺失/过短时生成新密钥。
// 重新生成会让所有已发出的登录 Cookie 立即失效——这是可接受的（比密钥可预测好），
// 但必须先确认文件真的坏到不能用，否则每次重启都把用户踢下线。
func (s *Store) loadSecret() error {
	path := filepath.Join(s.Dir, secretFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		if key := bytes.TrimSpace(raw); len(key) >= 16 {
			s.secret = append([]byte(nil), key...)
			return nil
		}
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg:   fmt.Sprintf("%s 内容过短（%d 字节），视为损坏并重新生成；已完成登录需要重新登录", secretFileName, len(bytes.TrimSpace(raw))),
		})
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s 失败: %w", secretFileName, err)
	}

	key := secure.RandomHex(32)
	if werr := s.writeFile(path, []byte(key+"\n")); werr != nil {
		return fmt.Errorf("写入 %s 失败: %w", secretFileName, werr)
	}
	s.secret = []byte(key)
	return nil
}

// ---------- 运行历史 ----------

// AppendRun 追加一行 RunRecord 并同步内存索引。
//
// 用 O_APPEND 单次 Write 追加：单进程单写者下，写到文件末尾是原子的，不需要
// 重写整个文件（2000 行重写一次就是几十毫秒的停顿，正好可能撞上签到时刻）。
func (s *Store) AppendRun(r model.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("运行记录序列化失败: %w", err)
	}
	line = append(line, '\n')
	path := filepath.Join(s.Dir, runsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", runsFileName, err)
	}
	if _, werr := f.Write(line); werr != nil {
		f.Close()
		return fmt.Errorf("写入 %s 失败: %w", runsFileName, werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("关闭 %s 失败: %w", runsFileName, cerr)
	}
	s.nLines++
	s.pushRunLocked(r)
	if s.nLines > runsRotateLines {
		s.rotateRunsLocked(path)
	}
	return nil
}

// Runs 返回最近 limit 条运行记录，新的在前；limit <= 0 表示全给。
// 只读内存索引：这是仪表盘每次刷新都要调的接口，不能每次都去解析文件。
func (s *Store) Runs(limit int) ([]model.RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.runs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]model.RunRecord, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, cloneRun(s.runs[n-1-i]))
	}
	return out, nil
}

func (s *Store) RunByID(id string) (model.RunRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.index[id]
	if !ok {
		return model.RunRecord{}, false
	}
	return cloneRun(r), true
}

func (s *Store) loadRuns() {
	path := filepath.Join(s.Dir, runsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg:   fmt.Sprintf("读取 %s 失败（%v），历史记录本次为空", runsFileName, err),
			})
		}
		return
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	count := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			count++
		}
	}
	s.nLines = count
	if count > runsRotateLines {
		// 轮转失败也不阻断启动：历史读不进内存不影响签到，日志里留下痕迹即可。
		if s.rotateRunsLocked(path) {
			s.appendLogLocked(model.LogEvent{
				Level: "info",
				Msg:   fmt.Sprintf("%s 已有 %d 行（上限 %d），已轮转为 %s", runsFileName, count, runsRotateLines, runsOldName),
			})
			return
		}
		// 轮转失败（目标被占用 / 权限不足）时不能顺手 return：文件其实完好，
		// 清空内存历史会让用户打开「历史」看到空列表，比"没轮转"更难解释。
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg:   fmt.Sprintf("%s 轮转失败（目标被占用？），本次改为继续加载现有历史", runsFileName),
		})
	}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var r model.RunRecord
		if uerr := json.Unmarshal([]byte(ln), &r); uerr != nil {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg:   fmt.Sprintf("跳过一行无法解析的 %s 记录：%v", runsFileName, uerr),
			})
			continue
		}
		s.pushRunLocked(r)
	}
}

func (s *Store) pushRunLocked(r model.RunRecord) {
	s.runs = append(s.runs, r)
	if r.ID != "" {
		s.index[r.ID] = r
	}
	if len(s.runs) > runsMemoryCap {
		drop := len(s.runs) - runsMemoryCap
		for _, old := range s.runs[:drop] {
			if old.ID != "" {
				// 只删"还指向这条旧记录"的索引，避免 ID 复用时误删新记录
				if cur, ok := s.index[old.ID]; ok && cur.StartedAt.Equal(old.StartedAt) {
					delete(s.index, old.ID)
				}
			}
		}
		s.runs = append(s.runs[:0], s.runs[drop:]...)
	}
}

// rotateRunsLocked 把 runs.jsonl 整份移到 runs.jsonl.1（覆盖旧的），再建一个空文件。
// 返回是否真的完成：调用方据此决定日志怎么写（"轮转失败"却记成功是最坏的假象）。
func (s *Store) rotateRunsLocked(path string) bool {
	old := filepath.Join(s.Dir, runsOldName)
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: fmt.Sprintf("删除旧 %s 失败：%v", runsOldName, err)})
	}
	if err := os.Rename(path, old); err != nil {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: fmt.Sprintf("轮转 %s 失败：%v", runsFileName, err)})
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		s.appendLogLocked(model.LogEvent{Level: "error", Msg: fmt.Sprintf("重建 %s 失败：%v", runsFileName, err)})
		return false
	}
	if cerr := f.Close(); cerr != nil {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: fmt.Sprintf("关闭 %s 失败：%v", runsFileName, cerr)})
	}
	s.nLines = 0
	return true
}

// ---------- 日志环 ----------

// Logs 返回内存日志环快照（新的在前）。limit <= 0 表示全给。
func (s *Store) Logs(limit int) []model.LogEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.logs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]model.LogEvent, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, s.logs[n-1-i])
	}
	return out
}

// AppendLog 写一条内存日志。零值时间戳补成当前时间：调用方常常只关心 msg，
// 让它们每次自己填 time.Now() 只会出现"日志时间差了几毫秒"的噪声。
func (s *Store) AppendLog(e model.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLogLocked(e)
}

func (s *Store) appendLogLocked(e model.LogEvent) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if e.Level == "" {
		e.Level = "info"
	}
	s.logs = append(s.logs, e)
	if len(s.logs) > logRingCap {
		drop := len(s.logs) - logRingCap
		s.logs = append(s.logs[:0], s.logs[drop:]...)
	}
}

// ---------- 落盘原语 ----------

// writeFile 原子写：同目录 tmp → fsync → rename。
//
// Windows 上 0600 只影响只读位，"权限没生效"不是错误（安全边界由数据目录的 ACL
// 与"默认只监听 127.0.0.1"承担），因此这里不因权限失败而中断。
func (s *Store) writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		os.Remove(tmp)
		return werr
	}
	// fsync：rename 之后如果数据还在页缓存里，断电会得到"文件名对了、内容是空/半截"的文件
	if serr := f.Sync(); serr != nil {
		f.Close()
		os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		os.Remove(tmp)
		return rerr
	}
	_ = os.Chmod(path, 0o600) // Windows 上基本无效果，忽略失败即可
	return nil
}

// quarantine 把损坏文件改名留证并返回新文件名（失败返回空串）。
// 用 unix 时间戳而不是递增序号：同一秒内多次损坏也能靠后缀继续区分，且不再依赖
// 目录枚举（目录里文件多时枚举很慢）。
func (s *Store) quarantine(path, reason string) string {
	base := path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
	for i := 1; i <= 100; i++ {
		if _, err := os.Stat(base); os.IsNotExist(err) {
			break
		}
		base = path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + strconv.Itoa(i)
	}
	if err := os.Rename(path, base); err != nil {
		s.appendLogLocked(model.LogEvent{
			Level: "error",
			Msg:   fmt.Sprintf("备份损坏文件 %s 失败（%v）；原因：%s", filepath.Base(path), err, reason),
		})
		return ""
	}
	return filepath.Base(base)
}

// ---------- 深拷贝 ----------

func cloneRun(r model.RunRecord) model.RunRecord {
	if r.Location != nil {
		v := *r.Location
		r.Location = &v
	}
	if r.School != nil {
		v := *r.School
		r.School = &v
	}
	if r.Notify != nil {
		v := *r.Notify
		r.Notify = &v
	}
	if r.Logs != nil {
		r.Logs = append([]string(nil), r.Logs...)
	}
	return r
}

func cloneState(st model.State) model.State {
	if st.Notified != nil {
		m := make(map[string][]string, len(st.Notified))
		for k, v := range st.Notified {
			m[k] = append([]string(nil), v...)
		}
		st.Notified = m
	}
	return st
}
