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
//
// SessionEpoch 是**会话代次**：会话 Cookie 的载荷里带着签发时的代次，authed 校验
// 时必须与这里相等。改管理密码、点「退出登录」都会换一个新代次 —— 这样"改了密码
// 但旧 Cookie 还能用 7 天"、"点了退出但复制走的 Cookie 依然有效"这两件事同时消失。
// 它是随机值而不是时间戳：时间戳会让"同一秒签发的两个代次"相等。
type authDoc struct {
	PasswordHash  string `json:"password_hash"`
	InitializedAt string `json:"initialized_at"`
	SessionEpoch  string `json:"session_epoch,omitempty"`
}

// Store 是 data/ 目录的唯一入口。Dir 是绝对路径（Open 时解析），
// 便于日志与错误信息里直接给出可点的位置。
//
// 关于「配置版本」：Store 内部维护一个 version 计数（配置改过几次），它是给上层判断
// "配置变过"用的信号（调度器重新规划、SSE/前端缓存失效、/status 排障），不属于配置
// 内容本身——config.json 里没有版本字段，这个计数只落在 state.json 的 config_version，
// 也不会被渲染成「配置版本 vN」出现在界面上。
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
	// sealer 用 secret.key 派生出的密钥封装 config.json 里的敏感字段。
	// 它只负责"落盘形态"，内存里的 s.cfg 永远是明文——上层不需要知道这件事。
	sealer *secure.Sealer
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
	// 数据目录里放的是加密后的凭据与那把钥匙：先把目录权限收紧到"只有本用户"，
	// 再往里写任何东西。Windows 上 0600 不生效（Go 只能改只读位），所以这一步
	// 在 Windows 上走 icacls，失败只告警不阻断——起不来的代价比权限不完美更大。
	if err := hardenDataDir(abs); err != nil {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: fmt.Sprintf("收紧数据目录权限失败（%v）；请确认 %s 只有本账号可访问", err, abs)})
	}
	// Open 期间没有并发读者，下列 load* 都假定"已持有写锁"（此处即无锁）。
	if err := s.loadSecret(); err != nil {
		return nil, err
	}
	sealer, err := secure.NewSealer(secure.DeriveKey("fzu-config-seal-v1", s.secret))
	if err != nil {
		return nil, fmt.Errorf("初始化配置加密失败: %w", err)
	}
	s.sealer = sealer
	s.loadAuth()
	s.loadConfig()
	// loadState 会从 state.json 的 config_version 接管内部计数；配置先读是为了
	// 拿到旧版 config.json 里那个已废弃的 version 作为计数的起点（见 loadConfig）。
	s.loadState()
	if s.version <= 0 {
		s.version = 1
	}
	// 让 State() 与 ConfigVersion() 从启动第一刻就是同一个数：state.json 里那一格
	// 只是内部计数的投影，内存里也不该出现两个不同的值。
	s.state.ConfigVersion = s.version
	s.loadRuns()

	s.appendLogLocked(model.LogEvent{
		Level: "info",
		Msg:   fmt.Sprintf("数据目录就绪 dir=%s", abs),
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

// ConfigVersion 返回"配置改过几次"的内部计数。
//
// 只给三处用：调度器唤醒/重新规划的判断、SSE 与前端缓存失效、/status 的排障字段。
// 它不来自 config.json，也不代表任何对外展示的「配置版本」——配置文件里没有版本。
func (s *Store) ConfigVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// SaveConfig 原子落盘并把内部计数 +1。
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
//
// 顺序是"先配置，再计数"：配置文件里没有版本字段，计数只落在 state.json 的
// config_version。写配置成功、写计数失败时不把错误抛给调用方——配置确实已经生效，
// 报"保存失败"只会骗用户再覆盖一次；计数下一次 SaveState 或下一次保存会自愈。
func (s *Store) saveConfigLocked(cfg model.Config, warnings []string) (int64, error) {
	next := s.version + 1
	if err := s.writeConfig(cfg); err != nil {
		return s.version, err
	}
	s.cfg = cfg
	s.version = next
	s.state.ConfigVersion = next
	if err := s.writeState(s.state); err != nil {
		s.appendLogLocked(model.LogEvent{
			Level: "warn",
			Msg:   fmt.Sprintf("配置已生效，但内部计数写入 %s 失败（重启后可能回退）：%v", stateFileName, err),
		})
	}
	for _, w := range warnings {
		s.appendLogLocked(model.LogEvent{Level: "warn", Msg: "配置警告：" + w})
	}
	s.appendLogLocked(model.LogEvent{
		Level: "info",
		Msg:   fmt.Sprintf("配置已保存 警告=%d 条", len(warnings)),
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
	s.unsealConfig(&cfg)
	s.cfg = cfg
	// 旧版 config.json 里带一个 "version" 字段。配置现在不再记录版本，但那份文件里
	// 的数字是"改过几次"的唯一历史证据：读出来当内部计数的起点，升级后 /status 的
	// config_version 才不会突然归零。它只用于这里，之后由 state.json 接管。
	// 注意：解析走的是 encoding/json 默认行为（未知字段忽略），所以带 version 的
	// 旧文件、不带它的新文件都能读；这里刻意不开 DisallowUnknownFields。
	s.version = legacyConfigVersion(raw)
}

// legacyConfigVersion 从旧配置文件里抠出已废弃的 version 字段。
//
// 解析失败（例如 version 被手写成了字符串）、负数或缺失一律按 0（无可继承的历史）
// 处理：它的用途只是让计数连续，绝不因为读不懂这一格就去干扰配置加载本身。
func legacyConfigVersion(raw []byte) int64 {
	var probe struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0
	}
	if probe.Version < 0 {
		return 0
	}
	return probe.Version
}

func (s *Store) resetConfig() {
	// 计数不在这里重置：它属于 state.json，由 loadState 接管。
	s.cfg = config.DefaultConfig()
}

// sealConfig 返回一份"敏感字段已加密"的配置副本；入参不会被改动。
//
// 加密发生在**落盘这一层**，内存里的 s.cfg 始终是明文：上层（flow / 前端 API /
// 调度器）不需要知道磁盘上长什么样，也就不会有人绕开这里自己拼 JSON 写文件。
func (s *Store) sealConfig(cfg model.Config) (model.Config, error) {
	if s.sealer == nil {
		// 只有"没走 Open"的零值 Store 才会这样。宁可报错，也不能悄悄把明文写回磁盘
		// ——那正是这次改动要消灭的东西。
		return cfg, errors.New("配置加密器未初始化（Store 必须经 Open 构造）")
	}
	out := config.Clone(cfg)
	for _, f := range config.SecretFields(&out) {
		v := *f.Value
		if v == "" {
			continue // 空字段不留密文，配置文件仍然可读可 diff
		}
		sealed, err := s.sealer.Seal(v)
		if err != nil {
			return cfg, fmt.Errorf("加密 %s 失败: %w", f.Path, err)
		}
		*f.Value = sealed
	}
	return out, nil
}

// unsealConfig 就地解密敏感字段。
//
// 解不开时**清空该字段并留一条 error 日志**，而不是让整个服务起不来：
// secret.key 被换过 / 从别的机器拷来 config.json 都会走到这一步，此时用户需要的是
// "去面板重填凭据"，不是一台永远起不来的机器。清空后 config.CredentialWarning
// 会在配置页顶部直接说明"凭据没配、不会自动签到"。
func (s *Store) unsealConfig(cfg *model.Config) {
	for _, f := range config.SecretFields(cfg) {
		if !secure.IsSealed(*f.Value) {
			continue // 升级前的明文配置：读得动，下次保存自动变成密文
		}
		plain, err := s.sealer.Open(*f.Value)
		if err != nil {
			s.appendLogLocked(model.LogEvent{
				Level: "error",
				Msg: fmt.Sprintf("%s 解密失败（%v）：已清空该项，请在面板重新填写；原文件另存为 config.json.bak",
					f.Path, err),
			})
			*f.Value = ""
			continue
		}
		*f.Value = plain
	}
}

// writeConfig 落盘配置：先备份旧文件，再原子写新文件。
//
// 备份的是**磁盘上的原文**（已加密），所以 .bak 也不会多出一份明文凭据——
// 这是"改过密码以后旧密码还留在 .bak 里"那类问题的根治点。
func (s *Store) writeConfig(cfg model.Config) error {
	path := filepath.Join(s.Dir, configFileName)
	sealed, err := s.sealConfig(cfg)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}
	raw = append(raw, '\n')
	if old, rerr := os.ReadFile(path); rerr == nil {
		// 升级路径的关键一步：从明文老配置迁过来的**第一次**保存，磁盘上那份 old
		// 里还是明文凭据。照抄成 .bak 等于"顺手多留一份明文，而且用户以为已经加密了"。
		// 这一份不备份，并在日志里说清楚（备份的目的是回滚，而回滚到明文正是要消灭的东西）。
		if hasPlaintextSecrets(old) {
			s.appendLogLocked(model.LogEvent{
				Level: "warn",
				Msg: "检测到磁盘上是旧版明文配置：本次保存不写 config.json.bak（避免多留一份明文凭据）。" +
					"如需备份，请在这一步之前手动拷走并妥善保管。",
			})
		} else if berr := s.writeFile(filepath.Join(s.Dir, configBakName), old); berr != nil {
			return fmt.Errorf("备份旧配置失败（%s）: %w", configBakName, berr)
		}
	}
	if werr := s.writeFile(path, raw); werr != nil {
		return fmt.Errorf("写入 %s 失败: %w", configFileName, werr)
	}
	return nil
}

// hasPlaintextSecrets 判断一段 config.json 原文里是否还有未加密的敏感字段。
//
// 解析失败一律按"有明文"处理：宁可少留一份备份，也不要把一段看不懂的内容
// 当成"应该没问题"抄进 .bak。
func hasPlaintextSecrets(raw []byte) bool {
	var cfg model.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return true
	}
	for _, f := range config.SecretFields(&cfg) {
		v := strings.TrimSpace(*f.Value)
		if v != "" && !secure.IsSealed(v) {
			return true
		}
	}
	return false
}

// ---------- 状态 ----------

func (s *Store) State() model.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

// SaveState 整份落盘 state.json。
//
// config_version 由 store 自己维护：调用方通常是"读一份 State() → 改几个字段 → 整份
// 写回"，读与写之间可能刚好保存过一次配置，那份旧副本里的计数会把它写回去。
// 所以这里一律用内部计数覆盖它——state.json 里那一格永远只是内部计数的投影，
// 不由调用方决定（也顺手让计数在写失败后自我修复）。
func (s *Store) SaveState(st model.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.ConfigVersion = s.version
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
	// 内部计数以 state.json 为准；没有（或 <=0）就继续用旧配置里继承来的那个，
	// 最后由 Open 兜到 1。顺序不能反：state.json 是这份计数唯一的落盘位置。
	if st.ConfigVersion > 0 {
		s.version = st.ConfigVersion
	}
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

// SessionEpoch 返回当前会话代次。未初始化时为空串（此时也没有任何会话）。
func (s *Store) SessionEpoch() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth.SessionEpoch
}

// SetPasswordHash 写入管理密码哈希。initialized_at 只在第一次写入时落定：
// 它是"这套部署的出生时间"，重置密码不该把它改掉。
//
// 换密码必然换会话代次：所有已签发的 Cookie 立刻失效。没有这一步，一次"改密码"
// 只是把新口令记下来，被偷走的旧 Cookie 还能继续用满 7 天。
func (s *Store) SetPasswordHash(h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writePasswordHashLocked(h, true); err != nil {
		return err
	}
	s.appendLogLocked(model.LogEvent{Level: "info", Msg: "管理密码已更新，所有已登录会话已失效"})
	return nil
}

// RehashPassword 只把哈希换成更强的参数，**不**换会话代次。
//
// 用途是登录成功后的参数升级（12 万轮 → 60 万轮）：密码本身没变，把用户踢下线
// 属于副作用。它与 SetPasswordHash 的唯一区别就在这里，所以两条路都指向同一个
// 写入实现，只由一个 rotate 参数分叉。
func (s *Store) RehashPassword(h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writePasswordHashLocked(h, false); err != nil {
		return err
	}
	s.appendLogLocked(model.LogEvent{Level: "info", Msg: fmt.Sprintf("管理密码哈希已按当前参数重算（迭代 %d）", secure.PBKDF2Iterations)})
	return nil
}

// RotateSessionEpoch 让所有已签发的会话立即失效（"退出登录"用）。
//
// 单管理员面板上这是最省心的语义：点一次退出 = 所有浏览器/手机都要重新登录。
// 代价是"手机点退出会把电脑也踢下线"，换来的是"复制走的 Cookie 不能再用"——
// 对一个存着学校密码的面板，这个交换是划算的。
func (s *Store) RotateSessionEpoch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth.PasswordHash == "" {
		return nil // 未初始化：没有会话可失效，也不用造一个代次出来
	}
	doc := s.auth
	doc.SessionEpoch = secure.RandomHex(16)
	if err := s.writeAuth(doc); err != nil {
		return err
	}
	s.auth = doc
	return nil
}

// writePasswordHashLocked 是两条写哈希路径的唯一实现（调用方必须已持写锁）。
func (s *Store) writePasswordHashLocked(h string, rotate bool) error {
	doc := s.auth
	doc.PasswordHash = h
	if doc.InitializedAt == "" {
		doc.InitializedAt = time.Now().UTC().Format(time.RFC3339)
	}
	// 代次为空（首次设置密码、或从没有该字段的旧 auth.json 升级上来）时无条件生成：
	// 没有代次的会话载荷会被 authed 判为无效，等于谁都进不来。
	if rotate || doc.SessionEpoch == "" {
		doc.SessionEpoch = secure.RandomHex(16)
	}
	if err := s.writeAuth(doc); err != nil {
		return err
	}
	s.auth = doc
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
	// 旧 auth.json 里没有 session_epoch：补一个并落盘，否则"换密码/退出即失效"这套
	// 机制在升级后的第一次启动里没有可比对的基准值。
	//
	// 升级前签发的 Cookie 载荷里没有 sv 字段，一律会被 authed 拒掉——这是一次性
	// 的重新登录代价，换的是"代次之后签发的每个 Cookie 都可撤销"。日志里说清楚，
	// 免得用户以为是登录坏了。
	if doc.PasswordHash != "" && doc.SessionEpoch == "" {
		doc.SessionEpoch = secure.RandomHex(16)
		if err := s.writeAuth(doc); err != nil {
			s.appendLogLocked(model.LogEvent{Level: "warn", Msg: fmt.Sprintf("写入会话代次失败（%v）：本次进程内所有旧 Cookie 都会被判无效", err)})
		} else {
			s.auth = doc
			s.appendLogLocked(model.LogEvent{Level: "info", Msg: "已为既有部署生成会话代次：升级前签发的登录 Cookie 需重新登录一次"})
		}
	}
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
