# fzu-auto-checkin · Go 服务化重写设计方案 v1.0

> 状态：**已定稿，作为实现与验收的唯一契约**。任何与本文件冲突的代码以本文件为准；确需变更必须在此文件内改并标注理由。
> 目标读者：实现该服务的智能体团队 + 人工验收者。

---

## 0. 一句话

把「一次性 Python 脚本」重写为**单二进制 Go 服务**：一个常驻进程内置调度器（替代 GitHub cron）+ 一套 REST API + 一份零构建的原生前端（HTML/JS/CSS），**零第三方依赖**，空闲内存预算 ≤ 30 MB。

## 1. 目标与非目标

### 1.1 目标（DoD 必达）

| # | 目标 | 可验证判据 |
|---|---|---|
| G1 | 前后端分离 | 后端只暴露 `/api/v1/*` JSON；前端是 `server/web/static` 下的静态资源，可用任意静态服务器托管（切 `server.allow_origins` 即可跨源） |
| G2 | 功能对齐 Python 版 | 4 个学校接口、SSO 换 token、位置池加权、圆内均匀抖动、窗口/多档/时间抖动/迟到补签、假期跳过、4 种推送、幂等不重复打卡 |
| G3 | 零第三方依赖 | `go.mod` 无 `require`；`go mod graph` 只有自身；二进制不链 cgo |
| G4 | 高性能低内存 | 空闲 RSS ≤ 30 MB；`/api/v1/status` p99 ≤ 5 ms（本地）；100 并发 SSE 连接 RSS ≤ 60 MB |
| G5 | 免 cron 自调度 | 进程内调度器按北京时间算下一档，单飞不并发，`SIGINT/SIGTERM` 优雅退出（等待进行中的签到最多 60 s） |
| G6 | 安全默认 | 首次启动强制设置管理密码；密码 PBKDF2-HMAC-SHA256(600k，登录时自动升级老哈希)；Session 用 HMAC 签名 Cookie + 可撤销的会话代次；**配置读取一律回哨兵**（真实值走 `POST /config/reveal`，要再输一次管理密码）；**凭据落盘 AES-256-GCM 加密**；日志禁止打印 token/密码/Cookie/推送 key |
| G7 | 可观测 | 结构化日志（stdout + 内存环 + SSE 实时推送）、运行历史 JSONL、`/api/v1/health` 暴露内存/goroutine/uptime |

### 1.2 非目标（明确不做）

- 多用户 / 多账号代挂、付费托管、任何形式的账号共享。
- 数据库依赖（SQLite/Redis/PG）：单用户场景 JSON + JSONL 足够，且避免 cgo/第三方。
- 前端构建链（npm/vite/webpack）：原生 ESM，浏览器直接跑，`go:embed` 打包。
- 伪造定位、隐藏离校状态：与 Python 版同一底线，坐标必须由用户填入真实位置。

## 2. 约束与技术选型

硬约束：**Go 1.22.12 已装、不得引入第三方库**。下表逐能力论证 stdlib 足够：

| 能力 | Python 原实现 | Go stdlib 方案 | 说明 |
|---|---|---|---|
| YAML 配置 | `PyYAML` | **不引入 YAML 库**：主配置改 JSON（`encoding/json`），另实现 **YAML 子集解析器**（仅支持本项目的映射/列表/标量/引号/注释/行内 `[]`），带 40 例测试，仅用于导入旧 `config.yaml` | 见 §6.3 |
| AES-128-CBC（接口参数） | `pycryptodome` | `crypto/aes` + `crypto/cipher.NewCBCEncrypter` | 手工 PKCS7 |
| AES-128-ECB（SSO 密码） | 同上 | `aes.NewCipher` + 16 字节分块手工 ECB（Go 1.22 无 `NewECBEncrypter`） | 见 §11.2 |
| HTTP 客户端/服务端 | `requests` | `net/http` | 复用 `http.Transport`，`MaxIdleConnsPerHost=4` |
| 路由 | — | `http.ServeMux`（Go 1.22 支持 `"POST /api/v1/x"` 方法+路径模式） | 无需 chi/gin |
| 静态资源打包 | — | `embed.FS` | 单二进制 |
| 配置落盘/历史 | 文件+secret | 原子写（临时文件+`os.Rename`）+ `bufio` 追加 JSONL | 见 §7 |
| 密码哈希 | — | 手工 PBKDF2-HMAC-SHA256（RFC 8018，约 30 行） | 有 RFC 测试向量 |
| Session | — | `crypto/hmac` + `encoding/base64`（`payload.signature`） | 无状态签名 + 会话代次 `sv`：进程重启仍有效，但改密码/退出即时作废 |
| 配置加密 | — | `crypto/aes` + `cipher.NewGCM`（标准库，无第三方） | 落盘 AES-256-GCM，密钥由 `secret.key` 派生 |
| 实时日志 | — | SSE（`text/event-stream`，`http.Flusher`） | 无需 WebSocket 库 |
| 随机数 | `random` | `math/rand/v2`（可注入 `*rand.Rand` 供测试复现） | 抖动不要求密码学强度 |
| 时区 | `zoneinfo`/offset | `time.FixedZone("CST", 8*3600)` | 服务器时区不参与任何判断 |

**唯一允许出现的非 stdlib 导入**：无。CI 门禁：`go list -deps ./... | grep -v '^fzucheckin' | grep -v '^internal/' | grep -vE '^(crypto|encoding|net|os|time|fmt|...)/'` 为空（见 §15.5 脚本化实现）。

## 3. 总体架构

```
                        ┌─────────────────────────── 浏览器（原生 ESM，零构建）
                        │  web/static/index.html + app.js + style.css
                        │  登录 / 仪表盘 / 配置 / 日志(SSE) / 历史
                        └───────────────┬───────────────────────────
                              HTTPS/CORS │ /api/v1/*  JSON + SSE
┌───────────────────────────────────────▼─────────────────────────────────────┐
│ 单个 Go 进程（fzucheckin.exe）                                                │
│                                                                              │
│  internal/server   ── router(ServeMux) · 中间件(日志/恢复/鉴权/CSRF/CORS)      │
│                    ── handlers: config/status/runs/logs(SSE)/auth/health      │
│                    ── scheduler: 60s 扫描 + 目标定时器 + 单飞 + 优雅退出       │
│                    ── flow: 假期 → token → init → 位置 → check → clockIn → 推送 │
│  internal/fzu      ── AttnClient(4 接口) · SSO 登录 · 位置解析/抖动 · 通知      │
│  internal/schedule ── 纯函数：窗口 / 多档 / 时间抖动 / 钳制 / 决策（可重放）      │
│  internal/secure   ── AES-CBC/ECB · PKCS7 · PBKDF2 · HMAC · 随机源              │
│  internal/config   ── 校验 / 默认值 / 脱敏 / YAML 子集导入                       │
│  internal/store    ── 原子写 config.json+state.json · runs.jsonl · 内存日志环     │
│  internal/model    ── 全部共享类型（禁止其他人改）                                │
└───────────────────────────────────┬──────────────────────────────────────────┘
                                    │ HTTPS（Token 头 / AES 参数）
                     ┌──────────────▼───────────────┐
                     │ yzsxg.fzu.edu.cn (4 接口)     │
                     │ sso.fzu.edu.cn (CAS 登录)     │
                     │ 推送网关 4 种                  │
                     └──────────────────────────────┘
```

**「前后端分离」的落地判据**：后端不认识 HTML；前端只认 JSON API 与 SSE。同一进程托管静态资源只是「顺带方便」，把 `web/static` 丢给 nginx、后端只监听 API，功能完全等价（配置 `server.allow_origins` + Cookie `SameSite=None; Secure` 由反向代理终结 TLS 时生效）。

## 4. 目录结构（实现即此布局，不改）

```
server/
├── go.mod                       # module fzucheckin / go 1.22 / 零 require
├── cmd/fzucheckin/main.go       # 参数解析、日志初始化、store.Open、server.New、信号处理
├── internal/
│   ├── model/model.go           # 共享类型（唯一真源，其他包只读不改）
│   ├── schedule/schedule.go     # Parse/InWindow/PlanSlots/ResolveRun/NowCN/Settings/Slot/Decision
│   ├── schedule/schedule_test.go
│   ├── secure/secure.go         # AES/PKCS7/PBKDF2/HMAC/Random
│   ├── secure/secure_test.go
│   ├── fzu/client.go            # AttnClient: Init/ServerTimeMs/Check/ClockIn
│   ├── fzu/login.go             # SSO 登录
│   ├── fzu/location.go          # 位置池/单点解析、圆内抖动、半径解析
│   ├── fzu/notify.go            # 4 渠道推送
│   ├── fzu/vacation.go          # 假期区间匹配
│   ├── fzu/*_test.go            # httptest 假上游
│   ├── config/config.go         # Normalize/Validate/Mask/Unmask/YAMLImport
│   ├── config/yamlmini.go       # YAML 子集解析器
│   ├── config/*_test.go
│   ├── store/store.go           # Open/Config/SaveConfig/Runs/AppendRun/Logs/密码与 SessionKey
│   ├── store/store_test.go
│   ├── server/server.go         # New/Deps/Handler/Run(ctx)
│   ├── server/router.go         # 全部路由与中间件
│   ├── server/handlers.go       # 各 handler
│   ├── server/scheduler.go      # 调度循环
│   ├── server/flow.go           # 一次签到的完整编排
│   ├── server/sse.go            # 日志/事件广播
│   └── server/*_test.go         # httptest 端到端
├── web/
│   ├── embed.go                 # //go:embed static（embed 只能引用同目录/子目录，故此处而非 internal/server）
│   └── static/{index.html,app.js,style.css,favicon.svg}
├── testdata/
│   ├── schedule_parity.json     # Python→Go 决策一致性语料（323 例，由 scripts/gen_parity.py 生成）
│   └── aes_vectors.json         # AES-CBC / AES-ECB 字节级对齐向量
└── data/                        # 运行时（git 忽略）：config.json/state.json/runs.jsonl/secret.key/auth.json
```

配套脚本（仓库根）：`scripts/gen_parity.py`（生成语料）、`scripts/smoke.ps1`（端到端冒烟）、`scripts/ui-verify.ps1` + `scripts/ui-verify.mjs`（Chrome/CDP 四视图验收）。

## 5. 领域模型与包契约

### 5.1 共享类型（`internal/model`，已落地文件）

键名与 Python 版 `config.yaml` **一一对应**（`snake_case`），前端与 API 直接复用同一套 JSON 键，不做二次映射。

### 5.2 包级导出签名（实现必须逐字一致，否则集成失败）

```go
// package schedule
type Slot struct {
    Configured time.Time // 配置原始时刻（今天的日期 + 配置时分）
    Target     time.Time // 抖动+钳制后的目标时刻
    Valid      bool      // 配置时刻本身是否在窗口内
    Jittered   bool
    Clamped    bool
    Reason     string    // ok|clamped_low|clamped_high|out_of_window
}
type Decision struct {
    Mode        string    // run|wait|skip
    Slot        *Slot
    Target      time.Time
    WaitSeconds float64
    Reason      string    // on_target|wait_until_target|wait_too_long|late_fallback|no_slot_fallback|out_of_window|force:...
}
type Settings struct {
    WindowStart, WindowEnd time.Time // 只取时分；日期无意义
    Times                  []time.Time
    JitterMinutes          float64
    MaxWaitMinutes         float64
    ToleranceMinutes       float64
    Warnings               []string
}
// 随机源抽象：跨语言一致性语料要求"两边消费同一串 u∈[0,1)"，
// 所以不用 *rand.Rand（Mersenne Twister 与 Python 不同），改用最小接口 + 可注入序列。
type Source interface{ Float64() float64 }
func GlobalSource() Source                 // 生产：math/rand/v2
func NewScriptSource(us []float64) Source  // 测试：循环消费给定序列（与 Python 假 rng 一致）

func Parse(cfg model.ScheduleConfig) Settings
func NowCN() time.Time
func InWindow(t time.Time, start, end time.Time) bool                 // end 整分钟按 :59.999 收尾
func VerifyInWindow(t, start, end time.Time) error
func PlanSlots(s Settings, now time.Time, src Source) []Slot           // src 为 nil 时用 GlobalSource
func ResolveRun(s Settings, now time.Time, src Source, slots []Slot) Decision
func DecisionText(d Decision) string                                   // 日志/推送可读文案
func NextRunAt(s Settings, slots []Slot, now time.Time) time.Time      // 仪表盘倒计时用；无未来档返回零值

// package secure
func EncryptParams(v any) (string, error)   // JSON→AES-128-CBC(K=IV=apexinfoapexinfo)→Base64
func DecryptParams(b64 string) (string, error)
func EncryptECBBlock(key, plain []byte) ([]byte, error) // PKCS7 + 分块 ECB
func PKCS7Pad(b []byte, block int) []byte
func PKCS7Unpad(b []byte, block int) ([]byte, error)
func PBKDF2SHA256(password, salt []byte, iter, keyLen int) []byte
func HashPassword(pw string) (string, error)             // "pbkdf2$sha256$600000$<b64salt>$<b64hash>"
func NeedsRehash(encoded string) bool                    // 迭代次数低于当前参数 → 登录后顺手升级
func VerifyPassword(encoded, pw string) bool
func RandomHex(n int) string
type Signer struct { Key []byte }
func (s Signer) Sign(msg []byte) string
func (s Signer) Verify(msg []byte, sig string) bool

// package fzu
type Client struct { HTTP *http.Client; BaseURL, Token string }   // BaseURL 可被测试覆盖
func NewClient(token string) *Client
func (c *Client) Init() (map[string]any, error)                    // 明文 JSON POST
func (c *Client) ServerTimeMs() (int64, error)
func (c *Client) Check(lon, lat float64, polygons []json.RawMessage) ([]Campus, error)
func (c *Client) ClockIn(p ClockInParams) (map[string]any, error)
func Login(username, password string) (string, error)
func LoginAt(ssoBase, service, username, password string) (string, error) // 测试注入
// Plan 是 init 响应的规范化视图：flow 只认它，不再摸 map[string]any。
type Plan struct {
    Start, End, LateLine string   // FStartTime / FEndTime / FLateTime（缺失则空）
    FState               string   // 未签到 | SUCCESS | 已签到
    Checked              bool     // FState ∈ {SUCCESS, 已签到}
    HasPlan              bool     // schoolData 非空
    CampusDefault        string   // userData.FDefaultCampus
    CollUnit             string   // userData.FCollUnit
    Way                  int      // userData.FWay，缺省 1
    Polygons             []json.RawMessage // 各 schoolData.FPosition 解析出的 range 数组
}
func ParseInit(init map[string]any) (Plan, error)
// 位置抖动的随机源同样走"抽签函数"，便于测试注入固定序列；fzu 不 import schedule。
type RandFunc func() float64
func ResolveLocation(cfg model.Config, draw RandFunc) (Place, error)
func JitterLocation(lon, lat, radiusM float64, draw RandFunc) (float64, float64)
func JitterRadius(cfg model.Config) float64
func Notify(cfg model.Config, title, content string, httpc *http.Client) error
func MatchedVacation(cfg model.Config, today time.Time) (string, bool)
func DoCheckin(c *Client, cfg model.Config, init map[string]any, rng *rand.Rand, logf func(string, ...any)) (CheckinResult, error)

// package config
func Normalize(cfg model.Config) (model.Config, []string)     // 修 YAML 60 进制、坏坐标等，返回警告
func Validate(cfg model.Config) error                        // 硬失败：无凭据/无坐标
func Mask(cfg model.Config) model.Config                     // 敏感字段→"••••"，读取与导出共用（GET /config 也走它）
func MergeMasked(oldCfg, incoming model.Config) model.Config // 收到 "••••" 视为"保持原值"
func SecretFields(cfg *model.Config) []SecretField           // 敏感字段路径 + 可写地址（加密/reveal 共用一张表）
func LookupSecret(cfg *model.Config, path string) (string, bool)
func ImportYAML(text string) (model.Config, []string, error)

// package store
type Store struct { Dir string }
func Open(dir string) (*Store, error)
func (s *Store) Config() model.Config
func (s *Store) SaveConfig(cfg model.Config, warnings []string) (int64, error) // 返回新版本号
func (s *Store) State() model.State
func (s *Store) SaveState(st model.State) error
func (s *Store) PasswordHash() string
func (s *Store) SetPasswordHash(h string) error
func (s *Store) SessionKey() ([]byte, error)
func (s *Store) AppendRun(r model.RunRecord) error
func (s *Store) Runs(limit int) ([]model.RunRecord, error)     // 新的在前
func (s *Store) Logs(limit int) []model.LogEvent               // 内存环
func (s *Store) AppendLog(e model.LogEvent)
```

### 5.3 依赖方向（禁止反向/循环）

`model ← schedule ← server`；`model ← secure ← fzu ← server`；`model ← config, store ← server`。
`schedule` 与 `secure` **不得** import `fzu/config/store/server`。

## 6. 配置规范

### 6.1 存储位置

运行期配置唯一真源：`data/config.json`（键与 §5.1 一致）。首次启动若无文件 → 写入一份安全默认模板（凭据为空），前端进入「初始化向导」。旧 `config.yaml` 通过 API 一次性导入，之后以 JSON 为准（避免两套格式长期漂移）。

### 6.2 校验规则（`config.Validate`，任一失败即 400 并给出可照抄的修法）

| 规则 | 失败信息要点 |
|---|---|
| 必须有 `user.token` 或 (`user.username` 且 `user.password`) | 二选一说明 + 推荐账密自动登录 |
| 必须有可用坐标：`locations` 非空 **或** `longitude`+`latitude` 都能被解析为浮点 | 附高德坐标拾取器链接 + 两种写法示例（照抄即可） |
| `times` 每项能被解析为 `HH:MM[:SS]`（容忍全角冒号、首尾空格）；空则落默认 `["21:35"]` | 指出第几项坏 |
| `window_end > window_start` | 不支持跨日，回退默认窗口并警告 |
| `time_jitter_minutes`/`max_wait_minutes`/`tolerance_minutes` ≥ 0，NaN/Inf/文字按默认 | 逐键警告 |
| `jitter_radius_m` 负数按默认 200 处理；显式 `0` 才关闭 | 提示"关闭请显式写 0" |
| `notify.type ∈ {serverchan,pushplus,bark,wecom,none}` | 枚举列出 |
| `locations[].weight` ≤0 或非法按 1（不是"永不选中"） | 提示 |
| 时刻不加引号被 YAML 解析成 60 进制整数（1295） | 自动还原为 21:35 并警告（与 Python 行为一致） |

### 6.3 YAML 子集解析器（仅导入用）

支持：`key: value`、嵌套映射（2 空格缩进）、`- item` 列表、`#` 注释与行尾注释（引号内的 `#` 不切）、单/双引号、行内 `[]`/`["a","b"]`、`~`/空值、布尔与数字（含 `21:35` 未加引号→按 60 进制整数再由 `Normalize` 还原）、UTF-8 BOM。
不支持并**明确报错行号**：锚点/别名（`&`/`*`）、多文档（`---`）、块标量（`|`/`>`）、flow map `{}`。错误信息含 `第 N 行`。

## 7. 存储规范

```
data/
├── config.json     # 配置（0600）：原子写 = 同目录 tmp + fsync + rename，旧文件保留为 config.json.bak
├── state.json      # 状态（0600）：{config_version, last_success_date, last_attempt_at, last_run_id, day_cache}
├── runs.jsonl      # 运行历史，一行一条 RunRecord；超过 2000 行启动时轮转为 runs.jsonl.1
├── secret.key      # 会话 HMAC 与配置加密的密钥材料（0600，首次随机生成；按用途用带标签的 SHA-256 派生）
└── auth.json       # {password_hash, initialized_at}（0600）
```

- **写并发**：`Store` 内部 `sync.RWMutex`；`AppendRun` 用 `O_APPEND`（单进程单写者，天然原子到行级）。
- **崩溃恢复**：启动时读取 `state.json` 失败 → 视为空状态并保留 `.corrupt` 副本，日志 WARN，不阻塞启动（宁可多签一次也不启不来；幂等由 `init` 的 `FState` 兜底）。
- **不存 token**：SSO 换来的 token 只写回 `config.json`（与 Python 一致），并标记 `config_version++`。
- **`state.day_cache`**：最近一次 `init` 的计划摘要（`model.DayPlan`）。它存在的唯一理由是让仪表盘重启后不必为了显示"今日计划/迟到线"去打学校接口——15 秒一次的轮询若直连上游，等于把用户的账号暴露在异常流量特征里。
- **备份/回滚**：`PUT /api/v1/config` 前把旧文件复制为 `config.json.bak`；提供 `POST /api/v1/config/rollback`（可选 P2）。
- **`server.data_dir` 只由启动参数决定**：`config.json` 本身就在数据目录里，读它之前必须先知道目录（鸡生蛋），所以 `-data`/`FZU_DATA_DIR` 是唯一真源；进程启动时会把解析后的**绝对路径写回配置**，避免界面上那个字段成为「改了不生效」的假值。
- **token 回写必须走 `Store.UpdateConfig(fn)`**：flow 的配置快照是运行开始时读的，若拿它整份写回，会把这十几秒里用户通过 `PUT /config` 改的密码/坐标覆盖掉（lost update，验收缺陷 #3）。`UpdateConfig` 在写锁内重读最新配置，只改 `user.token`。

## 8. 鉴权与安全

| 项 | 规格 |
|---|---|
| 首次启动 | `auth.json` 不存在 → `POST /api/v1/auth/setup {password}`（≥8 字符）唯一一次可用；已初始化则返回 409 |
| 密码存储 | `pbkdf2$sha256$600000$<b64 salt16>$<b64 32B>`；比较用 `subtle.ConstantTimeCompare`；登录成功后 `secure.NeedsRehash` 把老参数哈希顺手续到当前迭代（不换会话代次） |
| 改密码 | `POST /auth/password {current,new}`（≥8 字符，且不能与当前相同）；失败计入登录限速；成功后换会话代次并给自己续一张新 Cookie |
| 登录限速 | 同 IP 15 分钟内失败 5 次 → 429 + `Retry-After`；成功清零；计数器在内存（进程重启归零，可接受）。**身份默认取 TCP 来源 `RemoteAddr`**；`X-Forwarded-For` 只在 `FZU_TRUST_PROXY=1`（服务确实在可信反代后面）时采信——客户端可写的头不能当身份，否则换一个 XFF 就绕过了限速（验收实测：修复前同一来源第 7 次直接 200） |
| Session | Cookie `fzu_sess=<base64url(payload)>.<hmac>`；payload `{sub:"admin",iat,exp,sv}`；`HttpOnly; SameSite=Strict; Path=/; Max-Age=604800`；TLS 或 `X-Forwarded-Proto: https` 时追加 `Secure` |
| 会话撤销 | `sv` = `auth.json` 里的 `session_epoch`。改密码（`SetPasswordHash`）与退出登录（`RotateSessionEpoch`）都会换一个新代次，所有旧 Cookie 立即失效。重算哈希（`RehashPassword`）**不**换代次。升级前签发的 Cookie 没有 `sv`，会一次性失效、需要重新登录 |
| CSRF | 所有非 GET 请求必须带 `X-Requested-With: fzu-web`（前端统一 fetch 包装）+ SameSite=Strict 双保险；缺失返回 403 |
| CORS | 默认同源。`server.allow_origins` 非空时按白名单回显 `Access-Control-Allow-Origin`，`Access-Control-Allow-Credentials: true`，`Vary: Origin` |
| 脱敏 | `config.Mask`（读取与导出口径）：`user.password`/`user.token`/`user.cookie`/`notify.*_key|token|url|webhook` → `"••••"`；`GET /config` 与导出同口径，另返回 `masked_fields` 布尔供前端显示"已配置"。import 预览例外：回显的是用户自己粘进来的文本 |
| 按需显示 | `POST /config/reveal {field,password}`：要再输一次**管理密码**，一次只回一项真实值；字段名不认识 → 400 + 可选值清单；失败按 IP 限速（与登录独立计数）；日志只记 `field=`，不记值 |
| 落盘加密 | `store.sealConfig` 在写盘前把敏感字段用 AES-256-GCM 封装成 `enc:v1:<base64(nonce||ct||tag)>`，密钥 = `SHA-256("fzu-config-seal-v1"‖0‖secret.key)`；AAD 固定为 `enc:v1:`。内存里的配置始终是明文，`config.json.bak` 备份的是磁盘原文（密文）。不带前缀的值按历史明文读入（升级兼容），保存一次即自动加密。解不开时清空该字段 + error 日志，服务照常启动 |
| 数据目录 | `0700`/`0600`（Unix）；Windows 用 `icacls` 去继承 + 只授权本账号/SYSTEM/Administrators，并做建写读删自检（`FZU_SKIP_ACL=1` 跳过）。`secret.key` 与密文同目录，**能读目录的人就能解密**——这是本机边界的诚实表述，也是"别把 data/ 放进共享盘"的原因 |
| 日志 | 任何日志/SSE 事件禁止出现 token、密码、Cookie、推送 key 原文；只允许 `token=st***（len=64）` 形式。`logf` 自己做第二道 `redactSecrets`（调用方漏了也不会漏出去），且不截断（panic 栈要完整） |
| HTTP 加固 | `ReadHeaderTimeout=5s`、`ReadTimeout=15s`、`WriteTimeout=30s`（SSE 路由单独用 `0`）、`MaxHeaderBytes=64KB`、请求体 `http.MaxBytesReader 1MB` |
| 上游加固 | 客户端 `Timeout=15s`，`CheckRedirect` 最多 5 跳（SSO 需要），关闭 `http.DefaultClient` 复用 |

## 9. HTTP API 契约

统一前缀 `/api/v1`。响应 `Content-Type: application/json; charset=utf-8`。
错误体：`{"error":{"code":"config_invalid","message":"...","details":["..."]}}`。
状态码：`400` 校验失败 / `401` 未登录 / `403` CSRF 或未初始化 / `404` / `409` 冲突（初始化重复、已有运行中）/ `413` 请求体超过 1 MB / `422` 上游拒绝 / `429` 限速 / `500` 内部错误 / `502` 上游不可达。
响应头：所有 `/api/v1/*` 带 `Cache-Control: no-store`（配置里有学号与精确坐标，中间缓存不该留存）；CSP **只**加在自有页面响应上——API 若也带 `connect-src 'self'`，会把「前端放 nginx、后端只跑 API」的跨源部署打死。
请求体：`PUT /config` 容忍未知字段（前向兼容，返回 200 并落盘），但 `config` 缺失/为 null → 400；超过 1 MB → 413。

| 方法 | 路径 | 鉴权 | 请求 | 响应 |
|---|---|---|---|---|
| GET | `/api/v1/health` | 否 | — | `{"status":"ok","version":"1.0.0","uptime_s":123,"goroutines":12,"heap_mb":8.4}` |
| GET | `/api/v1/auth/session` | 否 | — | `{"initialized":bool,"authed":bool,"env":"dev"}` |
| POST | `/api/v1/auth/setup` | 否 | `{"password":"..."}` | `{"ok":true}` + Set-Cookie |
| POST | `/api/v1/auth/login` | 否 | `{"password":"..."}` | `{"ok":true}` + Set-Cookie |
| POST | `/api/v1/auth/logout` | 是 | — | `{"ok":true}`；同时轮换会话代次（所有设备失效） |
| POST | `/api/v1/auth/password` | 是 | `{"current":"...","new":"..."}` | `{"ok":true}` + 新 Set-Cookie；旧 Cookie 全部失效 |
| GET | `/api/v1/config` | 是 | — | `{"config":{masked},"warnings":[],"masked_fields":{...},"source":"json"}`（敏感字段是 `••••`） |
| PUT | `/api/v1/config` | 是 | `{"config":{...}}` | `{"ok":true,"warnings":[]}`；回传哨兵 = 保持原值，空串 = 清空 |
| POST | `/api/v1/config/reveal` | 是 | `{"field":"user.password","password":"<管理密码>"}` | `{"field":"user.password","value":"..."}`；一次一项，失败限速 429 |
| POST | `/api/v1/config/import-yaml` | 是 | `{"yaml":"..."}` | `{"config":{...},"warnings":[...]}`（不落盘，供前端预览；回显的是用户粘进来的原文） |
| GET | `/api/v1/config/export?format=yaml\|json` | 是 | — | 附件下载（导出为**脱敏**版，避免泄露到剪贴板/日志） |
| GET | `/api/v1/status` | 是 | — | 见 §9.1 |
| GET | `/api/v1/runs?limit=50` | 是 | — | `{"runs":[RunRecord...]}` |
| GET | `/api/v1/runs/{id}` | 是 | — | `{"run":RunRecord}` |
| POST | `/api/v1/runs` | 是 | `{"mode":"force"\|"normal","dry_run":bool}` | `202 {"run_id":"...","accepted":true}`；运行中 `409` |
| POST | `/api/v1/notify/test` | 是 | — | `{"ok":true,"channel":"serverchan"}` |
| GET | `/api/v1/logs?limit=200` | 是 | — | `{"logs":[LogEvent...]}`（内存环快照） |
| GET | `/api/v1/logs/stream` | 是 | — | SSE，见 §9.2 |

### 9.1 `GET /api/v1/status`

```json
{
  "now": "2026-11-14T21:04:09+08:00",
  "window": {"start": "21:00", "end": "23:59", "inside": true},
  "slots": [{"configured": "21:35", "target": "21:35:12", "valid": true,
             "jittered": false, "clamped": false, "reason": "ok", "state": "pending|done|missed"}],
  "next_run_at": "2026-11-14T21:35:12+08:00",
  "day": {"date": "2026-11-14", "checked_in": false, "last_success_date": "2026-11-13",
          "plan": {"start": "21:30", "end": "23:59", "late_line": "22:30", "has_plan": true}},
  "vacation": null,
  "scheduler": {"enabled": true, "stopped": false, "running_now": false, "last_decision": "wait_until_target"},
  "last_run": { "...": "RunRecord 或 null" },
  "version": "1.0.0", "config_version": 7
}
```
`slots[].state` 由 `state.json.last_success_date` + 当天 `runs.jsonl` 推导：今日已成功 → 全 `done`；目标已过且无成功 → `missed`；其余 `pending`。

### 9.2 SSE 事件

`GET /api/v1/logs/stream`，`Content-Type: text/event-stream`，每 20 s 发送 `: ping` 保活。

```
event: log
data: {"ts":"2026-11-14T21:35:12+08:00","level":"info","run_id":"20261114T213512-3f9a","msg":"clockIn 成功"}

event: run
data: {"id":"20261114T213512-3f9a","status":"success","slot":"21:35","target":"21:35:12"}
```

前端断线 3 s 退避重连（`EventSource` 自带，仅需 `Last-Event-ID` 忽略即可）；SSE 连接数上限 16（超出 503，防 fd 耗尽）。

### 9.3 `RunRecord`（`runs.jsonl` 一行）

```json
{"id":"20261114T213512-3f9a","mode":"scheduled","started_at":"...","finished_at":"...",
 "decision":"on_target","slot":"21:35","target":"21:35:12","status":"success",
 "latency_ms":1832,"is_late":false,
 "location":{"lon":119.20471,"lat":26.04912,"label":"locations[0]","jittered":true,"actual_location":"..."},
 "school":{"campus":"...","matched":true},
 "notify":{"channel":"serverchan","ok":true},
 "error":"","logs":["最大 20 行"]}
```
`status` 枚举：`success | already_checked | no_plan | out_of_range | vacation | skipped | error`。

## 10. 调度器规格（Python `src/schedule.py` 的逐条移植）

### 10.1 决策表（顺序即优先级，不得重排）

| # | 条件 | 结论 | `reason` |
|---|---|---|---|
| 1 | `!InWindow(now)` | skip | `out_of_window` |
| 2 | 存在 `target ≤ now` 且 `now-target ≤ tolerance` | **run 立刻签**（取最近的） | `on_target` |
| 3 | 存在 `target ≥ now` 且 `wait ≤ max_wait` | wait 到该 target | `wait_until_target` |
| 4 | 存在 `target ≥ now` 但等不起 | skip | `wait_too_long` |
| 5 | 所有档都已过且超出 tolerance | run（补签，取最晚档） | `late_fallback` |
| 6 | 无有效档（全在窗口外/解析失败） | run（窗口内兜底签一次） | `no_slot_fallback` |

`CHECKIN_FORCE`/手动补签 = `POST /api/v1/runs {mode:"force"}`：跳过等待与窗口外拦截，`reason` 前缀 `force:`，仍走执行前 `VerifyInWindow`（只 WARN 不拦截）。

### 10.2 时间抖动三重闸（与 Python 完全一致）

1. `delta = U(-J, +J)` 分钟；越界**重抽**，最多 8 次（`MAX_JITTER_REDRAWS`）；
2. 8 次仍越界 → **贴边**：低于起点 = `window_start + U(0,60s)`，高于终点 = `window_end_eff - U(0,60s)`；再 `clamp` 一次保证 `[start, end_eff]`；
3. 执行前 `VerifyInWindow` 二次校验。

`end_eff`：`window_end` 秒位为 0 时按 `:59.999` 收尾（写 `23:59` 表示到 `23:59:59` 都能签）。

### 10.3 循环与并发（`internal/server/scheduler.go`）

```
ticker 30s:
  if stopped || running_now: continue
  sched := schedule.Parse(cfg.checkin.schedule)
  slots := schedule.PlanSlots(sched, NowCN(), rng)      // 每轮重抽：允许抖动漂移
  d := schedule.ResolveRun(sched, NowCN(), nil, slots)
  switch d.Mode:
    skip  -> 记录 last_decision，等下一 tick
    wait  -> 设定 one-shot timer 到 d.Target（cap 到 max_wait），到点再 ResolveRun 重判
    run   -> running_now=true; go flow.Run(ctx, Request{Mode:"scheduled", Target:d.Target})
```

- **单飞**：`atomic.Bool` + `mutex`；`POST /api/v1/runs` 在运行中返回 `409`。
- **等待可取消**：`ctx.Done()` 立即返回（优雅退出不阻塞 12 分钟）。
- **时钟回拨**：若 `now` 早于上一 tick 超过 5 分钟 → 日志 WARN 并重新规划 slots（不信任缓存）。
- **优雅退出**：`signal.NotifyContext(SIGINT,SIGTERM)` → 停止 ticker → 等待进行中 flow 最多 60 s → 落盘 state → 退出码 0。
- **同一档不重复触发**：`state.json` 记 `last_slot_key=date|HH:MM|target`，命中则跳过（幂等第一层）；`FState` 检查是第二层。
- **失败退避 5 分钟**：调度器发起一次运行后 5 分钟内不再发起（`schedulerRunCooldown`）。没有它时上游 500 会让每 30 秒 tick 重打 init+SSO+check，半小时几十次请求——正是最该避免的异常流量特征。成功路径由「今日已签」闸兜住，冷却只影响失败重试节奏。
- **预检不算签到**：`mode=probe` 的运行即使结论是 `success`，也**不**写 `last_success_date` / `last_slot_key`，也不走推送。否则白天点一次「定位预检」会让调度器整晚判定 `already_checked_today`，当晚直接漏签（验收缺陷 #1）。
- **断电/重启**：启动即做一次「补跑判定」——若今天已过最后一档且未签到且在窗口内 → 按 `late_fallback` 走一次。

## 11. 学校接口客户端与 SSO

### 11.1 参数加密（与前端 `Encrypt()` 一致，逐字节对齐）

```
明文 = JSON(obj) 无空格（encoding/json 默认紧凑输出；浮点用 strconv 'g' 保证 119.20463 不被写成 119.20463000000001）
AES-128-CBC，KEY = IV = "apexinfoapexinfo"（16 字节 ASCII）
PKCS7 填充到 16 的倍数 → Base64Std
param 查询参数 = url.QueryEscape(url.QueryEscape(base64))    // 前端 axios 双重编码
```
Go 侧注意：`json.Marshal` 对 `float64` 用最短往返表示（与 Python `json.dumps` 一致），但**必须**让传入值先由 `strconv.ParseFloat` 得来，直接 `json.Marshal(map[string]any{"lon":119.20463})` 即输出 `119.20463`。测试用 Python 生成的固定向量校验（见 `testdata/aes_vectors.json`）。

### 11.2 SSO 登录链路（`fzu/login.go`）

1. `GET {sso}/login?service={attnCallback}` → 正则取 `id="login-croypto">(.*?)</p>` 与 `id="login-page-flowkey">(.*?)</p>`，缺任一 → 明确报「页面结构可能已变化」；
2. `password = Base64(AES-128-ECB(PKCS7(明文密码), key=Base64Decode(croypto)))`；
3. `POST {sso}/login?service=...`，form：`type=UsernamePassword&_eventId=submit&geolocation=&execution=&croypto=&username=&password=`，跟随重定向（≤5 跳）；
4. 从 `history` + 最终 URL 里用 `[?&]token=([A-Za-z0-9]{20,})` 取 token；
5. 失败时取 `id="login-error-msg"[^>]*>([^<]+)` 作为原因（常见「需要验证码」）。

### 11.3 打卡编排（`flow.Run`，与 Python `do_checkin` 等价）

```
1 假期命中 → status=vacation，静默（vacation.notify 才推）
2 token 空 → SSO 登录 → 回写 config（version++）；失败 → error + 推送
3 init → HTTP 500/无 data → no_plan（记日志，不推送轰炸）
4 userData.FState ∈ {SUCCESS, 已签到} → already_checked（静默）
5 schoolData 为空 → no_plan
6 ResolveLocation（池/单点）→ JitterRadius → 圆内取点（r=R√U）
7 check(polygons)：越界则重取 ≤3 次；仍失败回退基准点再 check；仍失败 → out_of_range
8 currentTimestamp → 北京时间 → 与 FLateTime/FEndTime 比较 → isLate
9 clockIn → 成功 status=success
10 notify（按 §12 去重）
11 落盘 RunRecord + state.last_success_date=今天
```

`init` 的 `schoolData[].FPosition` 是 JSON 字符串，需二次解析取 `range` 数组；解析失败按"该条无多边形"跳过（不整体失败）。

## 12. 通知规格

| 渠道 | 端点 | 成功判据 |
|---|---|---|
| serverchan | `POST https://sctapi.ftqq.com/{key}.send` form `title`/`desp` | HTTP 200 且 `code == 0` |
| pushplus | `POST https://www.pushplus.plus/send` JSON `{token,title,content}` | 200 且 `code == 200` |
| bark | `POST {bark_url}` JSON `{title,body}` | 200 |
| wecom | `POST {webhook}` JSON `{"msgtype":"text","text":{"content":"标题\n内容"}}` | 200 且 `errcode == 0` |

去重（替代 Python 里 21:45/22:00 的硬编码阈值）：
- **同一天同一状态只推一次**（`state.notified = {"2026-11-14": ["success"]}`）；`error` 允许重试时再推但 30 分钟内不重复；
- `success`/`out_of_range`/`error` 必推；`already_checked`/`skipped`/`vacation`(默认)/`no_plan`(当日首推一次) 静默。
- 推送失败**不影响** `RunRecord.status`，只写 `notify.ok=false` 与日志。

## 13. 前端规格（`server/web/static`）

- **技术**：原生 ESM（`<script type="module">`）+ 手写 CSS 变量主题，**零构建、零依赖**；`fetch` 统一封装（自动带 `X-Requested-With`、`credentials:'same-origin'`、401 → 跳登录）。
- **布局（v1.1，左右分栏）**：`#shell` 为 CSS Grid `248px minmax(0,1fr)`——左侧 `<aside class="sidebar">` 固定 `sticky` 全高（品牌 / `#nav` 竖排导航 / 底部「退出登录 + 版本号」），右侧 `#main` 内容区（`max-width 1180px` 居中）。`≤860px` 侧栏转为顶部横向条（导航横向滚动），`≤480px` 隐藏品牌文字、导航四等分。`<a class="skip">` 跳到主内容保留。
- **控件全部自绘**（不使用浏览器默认外观）：`select` / `checkbox` / `time` / `date` / `text` / `password` / `textarea` 一律 `appearance:none`，`number` 用 `appearance:textfield` + 隐藏 spin button（同样不是原生外观）；图标用 data URI SVG（`#` 需写成 `%23`）；`time/date` 把原生指示器 `opacity:0` 覆盖在自绘图标上方，点击仍能弹出原生选择器；复选框自绘方框 + `::after` 勾；自绘滚动条与 `:focus-visible` 焦点环；颜色对比度按 WCAG AA 复核（白字面统一 `--accent-btn`，浅色主题的日志 INFO 用压深的 `--accent-lv`）。
- **体积预算**：`index.html ≤ 40 KB`、`style.css ≤ 45 KB`、`app.js ≤ 80 KB`，三文件合计 **≤ 160 KB**（未压缩）；当前实测 28.8 / 34.8 / 76.3 KiB（合计 139.8 KiB）。体积门禁在 `zz_review_test.go` / `zz_layout_review_test.go` 里，超了就红。首屏之外还有一个**按需加载**的 `map-picker.js`（地图选点），单独上限 32 KiB，门禁在 `map_picker_test.go`——它不算进首屏三文件预算，因为它不点「在地图上选点」就一个字节都不下载。
- **视图**（单页 + hash 路由 `#/dashboard|config|logs|runs`）：
  1. `#/dashboard`：今日状态卡（已签/待签/假期）、计划时段与迟到线、下一档倒计时（前端本地每秒递减，数据来自 `status.next_run_at`）、`slots` 时间轴（pending/done/missed 三色）、按钮「立即签到」「定位预检」「推送测试」、最近 5 次运行表格。
  2. `#/config`：分段表单（账号 / 位置（单点或池，可增删行、权重）/ 时间调度 / 假期 / 推送 / 服务），行内校验与警告展示、保存（PUT）、`导入 YAML`（弹窗粘贴 → 预览 → 应用）、`导出`。
  3. `#/logs`：SSE 实时流（等价 `tail -f`）、级别过滤、自动滚动开关、复制/清屏；断线显示「重连中…」。
  4. `#/runs`：历史分页列表 + 单条详情（含坐标、校区、isLate、错误原文）。
  5. 未初始化 → 强制「设置管理密码」；未登录 → 登录页。
- **浏览器定位（v1.1）**：「位置」区提供「获取当前位置」按钮（`#btn-geo` + 状态行 `#geo-status`）。
  - 流程：`navigator.geolocation.getCurrentPosition({enableHighAccuracy:true, timeout:15000, maximumAge:0})` → **WGS-84 → GCJ-02** 转换 → 以 6 位小数填入经度/纬度 → 冒泡 `input` 事件（不自动保存、不覆盖上报文案）。
  - **必须做坐标转换**：浏览器给的是 WGS-84，高德/学校要 GCJ-02；福州实测偏移约 590 米，不转换会直接「定位不在签到范围」。境外坐标（`outOfChina`）原样返回。
  - **安全上下文**：`getCurrentPosition` 只在 HTTPS 或 `localhost/127.0.0.1` 下可用；`isSecureContext === false` 时给出可执行提示（改用 127.0.0.1 或配 HTTPS），不能表现为「点了没反应」。
  - 错误按 `error.code` 分档（拒绝权限 / 定位源不可用 / 超时），状态行 + toast 双通道；精度 > 100 m 追加「可能在室内」提示。
- **地图选点（v1.2）**：「位置」区有 `#btn-map-pick`（在地图上选点），位置池每行有 `.r-pick`；弹窗 `#map-picker` 由**动态 import** 的 `map-picker.js` 驱动（`app.js` 里 `import('./map-picker.js')`），首屏不下载、不用就零外部请求。
  - **不接高德 JS API，直接铺高德公开瓦片**：`webrd0{1-4}.is.autonavi.com`（街道，`style=8`，PNG）与 `webst0{1-4}.is.autonavi.com`（卫星，`style=6`，JPEG）。理由是 JS API 2.0 要 key + 安全密钥、SDK 会注入行内 `<style>`（与 `style-src 'self'` 冲突），POI 检索/逆地理编码还要再配一套按量计费的 Web 服务 key；瓦片免 key、免额度，且本身就是 GCJ-02 投影——选出来的坐标可以直上报。
  - **自绘地图引擎**（无第三方库）：Web 墨卡托投影、瓦片增量复用（拖动只改 `transform`，不重建 DOM，否则每帧闪）、指针拖动、滚轮锚点缩放、方向键 ±60 px 平移、`+/-` 缩放、点击落点、「用中心」精确微调；缩放 5–18 级（高德瓦片到 18 级为止）。
  - **坐标系**：高德图层原生 GCJ-02；连续 4 张瓦片失败自动切 OpenStreetMap（WGS-84），切图层时中心与已选点一起换算（`gcjToWgs` 用「减偏移量迭代两次」求逆），状态行标注「已从 WGS-84 换算」，回填前再用 app.js 注入的 `wgs84ToGcj02` 换回 GCJ-02。
  - **粘贴解析**：`经度,纬度`、`position=lng,lat`、`lng=/lat=`、高德 App「分享 → 复制链接」整段地址；写反成「纬度,经度」也能识别。
  - **上报地点文字（校区 + 详细地址 + 靠近 POI）**：`旗山校区 福建省福州市闽侯县乌龙江北大道2号靠近福州大学旗山校区生活区X号楼`，即「旗山校区 + 空格 + 省市区县路号 + 靠近 + POI 全称」（规则、反例与三端实现见 [address-format.md](address-format.md)，归一函数是 `model.NormalizeActualLocation`）。三条来源按优先级：① 用户自己填的**高德 Web 服务 key** → 落点即 `restapi.amap.com/v3/geocode/regeo`（`extensions=all`，取 `formatted_address` 与 `pois[0]/aois[0]` 拼「靠近…」；若 POI 已经写在前半段里，则合并成一句而不是重复一遍）；② 粘贴链接里的 `name=`；③ **点位记忆**（localStorage，按 4 位小数≈11 米取键，封顶 200 条）——没 key 的用户手写一次即可自动带出。key 只在 `localStorage`，不进配置、不随导出；只有落点变化且配了 key 才发请求（450 ms 防抖 + 请求序号防迟到覆盖），失败只影响文字、不影响坐标回填。
  - **回填语义**：6 位小数、补发冒泡 `input`（行内校验 / 位置池提示 / 徽标同步）、不自动保存；位置池的按钮只改它那一行。空输入打开时**不许**落到 `(0,0)`（`Number('') === 0` 是最容易踩的坑），此时不预置图钉、确认时明确报「还没选点」。
- **体验**：暗色为默认（跟随 `prefers-color-scheme`），键盘可达（所有交互 `button`/`label`），`aria-live="polite"` 播报运行结果与定位结果，无外部字体/CDN（离线可用）。
- **错误呈现**：`error.details[]` 逐条列出，附「怎么改」文本（与后端 §6.2 同源）。
- **CSP 约束（写前端时的三条硬线）**：不许 `style="…"` 内联属性、不许内联 `<style>`、不许外部**脚本/样式**资源；图标只能 CSS 绘制或 data URI；动态文本一律 `textContent`（无 `innerHTML`/`eval`）。地图选点是唯一例外，且只放行两类：`img-src` 白名单 `https://*.autonavi.com`、`https://tile.openstreetmap.org`（瓦片）；`connect-src` 白名单 `https://restapi.amap.com`（可选逆地理编码）。`script-src`/`style-src` 仍是 `'self'`，`map_picker_test.go` 把这条、以及「不许出现高德 JS SDK 域名/安全密钥」「key 不许调后端接口持久化」一起钉死。
## 14. 非功能预算

| 指标 | 预算 | 测量方法 |
|---|---|---|
| 空闲 RSS | ≤ 30 MB | 启动后 60 s `runtime.MemStats.Sys` 与服务 RSS 双口径 |
| 冷启动到可服务 | ≤ 200 ms | `time curl /api/v1/health` |
| `/api/v1/status` | p99 ≤ 5 ms（本地，1000 次） | 简单基准脚本 |
| 100 SSE 连接 | RSS ≤ 60 MB，不泄漏 | 脚本开 100 连接 60 s，观察 goroutine 数回落 |
| 二进制 | ≤ 12 MB（`-trimpath -ldflags="-s -w"`） | `Get-Item` |
| 单元测试 | `go test ./... -race` 全绿，覆盖率 ≥ 75%（核心包 ≥ 85%） | `go test -cover` |
| 静态检查 | `go vet ./...` 与 `gofmt -l .` 干净 | CI |

## 15. 测试策略

1. **表驱动单元**：`schedule` 覆盖 20:59:59 / 21:00:00 / 23:59:59 / 00:00:00 边界、六条决策、抖动越界重抽与贴边、`end_eff`、跨日拒绝、坏配置降级。
2. **跨实现一致性（关键）**：`scripts/gen_parity.py` 用现有 Python 实现生成 `testdata/schedule_parity.json`。做法：给 Python 注入一个「循环消费固定 u 序列」的假 rng，把用的 u 序列一起写进语料；Go 侧用 `schedule.NewScriptSource(us)` 复现同一序列，逐条断言 `mode/reason/target/wait_seconds/slots[]`。语料 ≥ 200 例（20 组 schedule × 多时刻 × 随机用例），覆盖六条决策与抖动越界钳制。
3. **加密向量**：Python 生成 `testdata/aes_vectors.json`（明文→密文 Base64）与 ECB 向量，Go 断言字节级相等。
4. **上游假服务**：`httptest.Server` 模拟 4 接口 + SSO 页面，覆盖：正常签到、已签到、无计划、500、越界重取、迟到标记、token 过期→重登。
5. **HTTP 层**：`httptest` 起 `server.Handler()`，覆盖未登录 401、CSRF 403、限速 429、配置校验 400、运行中 409、SSE 首帧与保活、脱敏不泄露。
6. **并发安全**：`go test -race ./...`；另加 `scheduler` 单测用 `-count=20` 压同一决策路径。
7. **崩溃恢复**：损坏 `state.json`/`config.json` → 启动不 panic，生成 `.corrupt`，服务可用。
8. **前端验收**（人工 + 脚本）：Chrome（不用 Edge）打开 `http://127.0.0.1:8787`，走完「设密码→登录→导入 YAML→改一档时间→保存→看倒计时→手动补签→看日志实时刷新」；截图存档。

## 16. 运维与部署

**一键启动（推荐，Windows）**：仓库根的双击入口 `start.cmd` → 调 `start.ps1`。

```powershell
.\start.ps1                 # 默认 http://127.0.0.1:14514，自动按需构建，前台运行，Ctrl+C 停止
.\start.ps1 -Status         # 只看健康状态（JSON），未运行退出码 1
.\start.ps1 -Port 14515     # 换端口
.\start.ps1 -NoBuild        # 用现有 exe，不重新构建
.\start.ps1 -Listen 0.0.0.0:14514   # 局域网可访问（会提示用本机 IP 打开）
.\start.ps1 -NoOpen         # 不自动打开 Chrome
```

脚本行为：定位仓库根 → 检查端口（已在运行则不重复启动，打印 URL 后退出 0；被别的程序占用则报进程名并退出 1）→ 按需 `go build -trimpath -ldflags="-s -w"` → 前台起服务 → 30 秒内轮询 `/api/v1/health` 就绪后打印 URL（并可用 Chrome 绝对路径打开）→ 服务输出逐行回显 + 落 `%TEMP%\fzucheckin-<port>-*.log`。**不得**用 `Start-Process http://…` 打开浏览器（本机 http 默认关联是 Edge）。

手动构建与运行：

```bash
# 构建（Windows）
go build -trimpath -ldflags="-s -w" -o fzucheckin.exe ./cmd/fzucheckin
# 运行（服务自身默认 8787；一键脚本默认 14514）
./fzucheckin.exe -listen 127.0.0.1:14514 -data ./data
# 或环境变量：FZU_LISTEN / FZU_DATA_DIR / FZU_LOG_LEVEL(debug|info|warn|error) / FZU_TRUST_PROXY=1
```

- **systemd（Linux）**：`Restart=on-failure`、`After=network-online.target`、`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ReadWritePaths=/var/lib/fzucheckin`。
- **Windows**：计划任务不需要了（内建调度器），只需「开机自启」+ 失败重启：`schtasks /create /tn fzucheckin /tr "...fzucheckin.exe -listen 127.0.0.1:8787" /sc onstart /ru SYSTEM`。
- **反向代理**：nginx 终结 TLS，`proxy_buffering off` 供 SSE，`proxy_read_timeout 300s`。
- **升级**：替换二进制 → 重启（配置/状态在 `data/`，向后兼容；`config.version` 用于迁移）。
- **诊断**：`/api/v1/health`（内存/goroutine/uptime）、`runs.jsonl`、SSE 日志；日志格式 `2026-11-14T21:35:12+08:00 INFO  run=... msg="..."`。

## 17. 里程碑与智能体分工（可并行，契约见 §5.2）

| 阶段 | 智能体 | 归属文件 | 交付物 | 完成判据 |
|---|---|---|---|---|
| P0（已由主控完成） | lead | `server/go.mod`、`internal/model/model.go`、本文件、`testdata/` 语料 | 契约冻结 | 三件套就位 |
| P1-A | **sched-agent** | `internal/schedule/**` | 纯函数调度 + 表驱动测试 | `go test ./internal/schedule -cover ≥ 85%`，parity 语料全过 |
| P1-B | **secure-agent** | `internal/secure/**` | AES/PKCS7/PBKDF2/HMAC/随机 + 向量测试 | Python 向量逐字节一致，PBKDF2 对 RFC 向量 |
| P1-C | **fzu-agent** | `internal/fzu/**` | 4 接口客户端、SSO、位置/抖动、通知、假期 + httptest | 假上游 12 个场景全过，不触网 |
| P1-D | **store-agent** | `internal/config/**`、`internal/store/**` | 校验/脱敏/合并、YAML 子集解析、原子持久化/历史/日志环 | 40 例 YAML、脱敏不泄露、损坏文件可恢复 |
| P2-E | **server-agent** | `internal/server/**`、`cmd/fzucheckin/**` | 路由/中间件/鉴权/调度循环/flow/SSE | httptest 覆盖 §15.5，`-race` 干净 |
| P2-F | **web-agent** | `web/static/**` | 4 视图原生前端 + fetch 封装 + SSE | 体积达标、无外部资源、接口字段与 §9 一致 |
| P3 | **lead + verify-agent** | 集成修补、`scripts/gen_parity.py`、e2e | 构建/测试/启动/浏览器验收 | §19 清单全勾 |

**协作纪律**：各 agent 只写自己归属的目录；`internal/model/model.go` 只读；如契约不足，先在自己包内加未导出实现，**不得**改他人包签名；集成冲突由 lead 统一裁决。

## 18. 风险登记册

| 风险 | 影响 | 对策 |
|---|---|---|
| 学校前端改版（接口/字段/密钥） | 全链路失效 | 错误分类明确 + 推送原文；协议集中在 `fzu/`，改动面单一 |
| SSO 要求验证码 | 自动登录失败 | 保留「手动 token」输入；报错直指验证码 |
| GitHub cron 延迟（Python 版痛点） | 漏签 | 本方案已用进程内调度器**彻底消除该依赖**（G5） |
| JSON 浮点序列化与 Python 不一致 | 接口校验失败 | 加密向量 + 打卡参数快照测试锁死格式 |
| 零依赖 YAML 解析不全 | 导入旧配置失败 | 只支持本项目用到的子集，超集明确报行号；主格式为 JSON |
| 单进程调度器 = 单点 | 进程挂掉不签 | systemd/计划任务自动拉起 + 启动即补跑判定 |
| 前端手写导致体验差 | 可用性 | 明确四视图 + 键盘/暗色/aria 要求，验收走真实浏览器 |

## 19. 验收清单（P3 逐项勾）

> 状态：2026-11-14 22:20（北京时间）实测。证据命令见 `docs/server-readme.md` §6。

- [x] `go build ./...`、`go vet ./...`、`gofmt -l .` 全干净
- [x] `go test ./... -race -cover` 全绿：schedule 97.5% / fzu 90.6% / config 89.8% / store 89.6% / server 80.7% / secure 83.8%（`-race` 用 Clang+MSVC 包装器跑通，见 README §6）
- [x] `go.mod` 无 `require`；`go list -deps` 无第三方（仅有 stdlib 及其内部 vendor 副本）
- [x] 323 条 Python↔Go 调度一致性语料全过
- [x] AES-CBC / AES-ECB 与 Python 输出逐字节一致（含中文 `\uXXXX` 转义与代理对）
- [x] 启动 → 初始化密码 → 登录 → 导入旧 `config.yaml` → 保存 → `status` 正确
- [x] 手动补签（`force`）走通并落 `runs.jsonl`（`TestForceRunEndToEnd` + 浏览器「立即签到」）
- [x] 假上游下六种 `status` 全部可达（success/already_checked/no_plan/out_of_range/vacation/error）
- [x] SSE 实时日志在浏览器可见，断线自动重连（`logs-conn` 显示「已连接」，快照+增量）
- [x] 未登录 401、无 CSRF 头 403、5 次错密 429（`Retry-After`）
- [x] 配置 GET 只回哨兵（断言响应里没有任何明文凭据、`masked_fields` 标出已配置项）；真实值走 `POST /config/reveal` 二次校验管理密码，一次一项、失败 429；导出仍是脱敏版
- [x] `config.json` 与 `.bak` 落盘无明文（`enc:v1:` 密文），换钥后服务仍能启动、凭据清空并留 error 日志
- [x] 改密码 / 退出登录后其它设备的旧 Cookie 立刻 401，本机由响应里的新 Cookie 续上
- [x] 进程空闲 RSS 10.6 MB（≤30 MB），二进制 6.28 MB（≤12 MB）
- [x] `SIGINT` 优雅退出，退出期间进行中的运行可完成（`TestRunGracefulShutdown` + 60s 兜底）
- [x] 浏览器（Chrome 157.0.8103.78 · CDP:9225）四视图走查 + 8 张截图存档（28 项断言全过）

---

## 20. 验收记录：独立验收智能体发现的缺陷与修复（2026-11-14）

`verify-agent` 以「证伪」为目标写了 26 个测试函数（41 个叶子用例），**故意保留会失败的回归用例**而不是放宽断言。8 条缺陷全部修复，其中 2 条回归用例修复后自动转绿。

| # | 级别 | 缺陷 | 影响 | 修复 |
|---|---|---|---|---|
| 1 | **P0** | `dry_run` 预检通过被记成 `success`，写脏 `last_success_date` | 白天点一次「定位预检」→ 当晚调度器判定「今日已签」→ **整晚漏签**，并且收到一条假的「✅ 签到成功」推送 | `finishRun` 的成功副作用加 `rec.Mode != "probe"`；`notifyRun` 对 probe 直接静默 |
| 2 | P1 | 登录限速身份无条件取 `X-Forwarded-For` | 同一来源失败 5 次被 429 后，加一个 XFF 头立刻 200 —— 限速形同不存在 | `clientIP` 默认只用 `RemoteAddr`；仅 `FZU_TRUST_PROXY=1`（可信反代）时采信 XFF；回归用例改为断言 429 |
| 3 | P1 | 运行期 SSO 回写 token 用旧配置快照整份覆盖 | 运行那十几秒内用户改的密码/坐标/推送 key 被静默抹掉（lost update） | 新增 `Store.UpdateConfig(fn)`：写锁内重读最新配置，只改 `user.token` |
| 4 | P2 | `runs.jsonl` 轮转失败时 `return`，内存历史被清空 | 文件其实完好，界面却显示「无历史」 | 轮转失败改为继续逐行加载并记 WARN |
| 5 | P2 | `server.data_dir` 是死配置项（界面能填，改了没用） | 用户以为改目录生效 | 启动时把实际绝对路径写回配置；文档明确「只由 `-data`/`FZU_DATA_DIR` 决定」 |
| 6 | P2 | `HEAD /api/v1/health` 返回 401 | 用 HEAD 做探针的负载均衡/监控会把健康服务判成挂了 | `apiIsPublic` 放行 `GET` 与 `HEAD` |
| 7 | P2 | `init` 失败被推成「今日无晚点名计划」 | 上游挂了那天用户会以为「今晚不用签」 | 保留 `no_plan` 状态但写入 `rec.Error`；推送标题改为「⚠️ 无法确认今日计划（上游异常）」 |
| 8 | P2 | 敏感 API 响应缺 `Cache-Control: no-store` | 配置/历史可能被浏览器或中间缓存留存 | API 中间件统一加 `no-store`；CSP 只加在自有页面响应上 |

**验收后复核**：`go test ./...` 全绿（server 覆盖率 84.8%）、`go test ./... -race` 全绿（schedule/secure/fzu/config/store/server 六包）、`go vet` 与 `gofmt -l` 干净、`scripts/smoke.ps1` 全过（RSS 10.7 MB）、Chrome 157 真实浏览器 28 项断言全过（CSP 去掉 `'unsafe-inline'` 后页面与 SSE 均正常）。

### 20.1 智能体分工与实际交付

| 智能体 | 归属 | 交付 |
|---|---|---|
| lead | 契约、`model`、`secure`、语料、集成与验收 | 设计方案、共享类型、AES/PBKDF2、`gen_parity.py`、`smoke.ps1`、`ui-verify.*`、缺陷修复与文档 |
| sched-agent | `internal/schedule` | 705 行调度内核 + 42 用例，**323/323 跨语言一致性**，覆盖率 97.5% |
| fzu-agent | `internal/fzu` | 4 接口客户端、SSO、位置池/抖动、4 通道推送、假期，78 用例，覆盖率 90.6% |
| store-agent | `internal/config`、`internal/store` | 零依赖 YAML 子集解析器（63 例）、校验/脱敏/合并、原子持久化/轮转/损坏恢复，覆盖率 89.8% / 89.6% |
| server-agent | `internal/server`、`cmd`、`web/embed.go` | 路由/中间件/鉴权/调度循环/flow/SSE，54 用例，覆盖率 84.8% |
| web-agent | `web/static` | 4 视图原生前端（96.2 KB / 120 KB 预算），Chrome 走查 53 项 |
| verify-agent | `zz_review_test.go` ×2 | 26 个对抗性测试函数、41 叶子用例、8 条缺陷（含 1 条 P0） |



---

## 21. v1.4.0 增补（2026-11-18）：一键启停、凭据可空、二次认证不再谎报

### 21.1 `POST /api/v1/control` 与 `state.paused`

| 项 | 约定 |
|---|---|
| 请求 | `{"paused": true|false}`；缺字段 / 类型错 / 非法 JSON → `400 bad_request` |
| 响应 | `{"paused":bool,"scheduler":{"enabled","stopped","paused","running_now","last_decision"}}` |
| 存储 | `state.json` 的 `paused`（**不放 config**：配置页整份回写会拿旧快照盖掉它） |
| 调度 | paused 分支排在状态机最前：不做启动补跑、不做窗口内复查、不做失败重试；阻塞在唯一阻塞点 `pauseUntil` 等 `s.wake`，进入/恢复各只打一条 INFO |
| 不受影响 | `POST /runs`（手动补签）、`POST /login/test`、`POST /login/refresh` |
| 前端 | 仪表盘 `#btn-auto-switch`（`role="switch"`），读 `/status.scheduler.paused`；请求期间禁用按钮、失败回滚并提示 |

### 21.2 凭据允许全空

`config.Validate` 不再把「token / cookie / 学号+密码 三选一」当硬错误——那等于"没填密码就存不下配置"，连坐标都改不了。
现在改为：`Normalize` 产出一条警告（`config.CredentialWarning`，`GET /config` 的 `warnings` 也会带上），运行期 `flowRun` 把它定性成 `skipped`（不换 token、不打上游、不写 `last_success_date`、不推送），调度器记 `last_decision=no_credentials` 并睡到下一个窗口起点。坐标缺失仍是硬错误。

### 21.3 校外 IP 的短信二次认证 = 独立语义，不是"结构变化"

2026-11-18 线上事故复盘（证据来自 `journalctl -u fzucheckin` 与两次手工复现）：

- 用真实学号密码在**阿里云 IP** 上 POST `https://sso.fzu.edu.cn/login?service=…` → `200`，返回的是一张二次认证页：
  `#sso-second=true`、`#second-auth-tip=您正在使用非校内ip登录，请进行手机验证…`、`#phone-number`、`#second-auth-user-id=<学号>`。
  学号出现在页面上恰好证明**账号密码是对的**，缺的只有手机验证码。
- 同一份凭据在**家庭宽带 IP** 上一次成功；同一份已认证的会话 Cookie 从机房 IP 请求登录页，返回的仍是这张二次认证页 → 密码通道与 Cookie 通道**都会被拦**。
- 同一份有效 token 从机房 IP 打 `init.action` 是 `200 success` → 晚点名 H5 接口不限校外 IP，只有 SSO 发票这一步被卡。

因此：

| 层 | 行为 |
|---|---|
| `fzu.ErrSecondAuth` / `SecondAuthError{Tip, Phone, UserID}` | 判据只看 `#sso-second=="true"` 或 `#second-auth-tip` 非空，200/401 都判；手机号打码（保 3+4，短号整体 `****`）；`Unwrap()` 让 `errors.Is/As` 穿包装 |
| `fzu.LoginAt` / `attemptCookieLogin` | 拿到页面**先判**二次认证（早于 `#login-croypto` 缺失与 `loginErrorHint`），返回同一哨兵 |
| `loginErrorHint` | 兜底不再写「登录页结构可能已变化」：按事实给状态码 + 字节数 + 标题 + 正文开头（凭据字样脱敏），401 单独说"密码被拒或被风控拦截" |
| `internal/server` | `POST /login/test`、`/login/refresh` 命中哨兵 → **409** + `reason=second_auth` + `second_auth{phone,tip}` + 三条人工路径；`mintToken` 合并 cookie 与密码两条通道的失败原因 |
| `flowRun` | `rec.Error` 同时带上「token 不被接受」与「换新为什么失败」；二次认证那条压短，保证 `maxErrorTextLen` 截断后仍留得住"要短信验证"这句 |

**设计取舍**：不实现自动短信验证。取证的 `type=smsLogin&_eventId=submit&execution=<flowkey>&smsCode=…` 只回 `401` + 同一张页，字段形状无法确证；继续试是拿学校风控记账，且"服务替人收验证码"等于把用户手机号绑到进程上。对外只提供三条人工路径（浏览器取 token / 校内网跑 / Cookie——后者仅校内网成立）。

> 2026-11-18 深夜补充：上面这条"不实现自动短信验证"已被推翻——把学校前端拆完之后，三个接口的形状都拿到了（见 §21.4）。**"替人收验证码"仍然不做**：服务只替人"提交"那 6 位数字，码永远只到用户手机上。

### 21.4 v1.4.1：把「短信二次验证」搬进面板

学校那套二次认证是标准 CAS + 金智 SSO 的实现，三个接口全部照着实测响应复刻（`internal/fzu/sms.go`）：

| 步骤 | 请求 | 关键点 |
|---|---|---|
| 发起 | `POST /login`（`type=UsernamePassword`，`password=AES-ECB(croypto)`） | 校外 IP 返回二次认证页：`#sso-second=true`、`#phone-number`、`#login-page-flowkey`（换了一把新的） |
| 发码 | `POST /api/protected/sms/publicNoToken/sendSmsCode` `{phone, businessNo:"0008"}` | 必须先过 CSRF 双提交头，否则回 `401 Unauthorized / Invalid request`；成功回 `{result:true, sendInterval:120, expireTime:300}` |
| 校验 | `POST /api/protected/sms/checkTokenResult` `{phone, token, delete:false}` | 错/过期 = `412 + errorCode 1320009`；没绑手机 = `1320010`；太频繁 = `412 + 8000068`（`errorMessage` 里带 `remaining` 秒） |
| 换票 | `GET /login?service=…` 或 `POST /login`（`type=smsLogin&username=<手机号>&password=<验证码>&execution=<flowkey>`） | **验证码的字段名是 `password`**，`smsCode` 只是前端模板引用名；`execution` 必须是二次认证页那一把 |

服务端 API（都要登录态 + `X-Requested-With: fzu-web`）：

```
POST /api/v1/login/sms/start   {}                → {challenge, phone, tip, expires_in, resend_after, sent}
POST /api/v1/login/sms/resend  {challenge}       → {resend_after, expires_in}｜429｜410
POST /api/v1/login/sms/verify  {challenge, code} → {token_hint, school_ok, plan}｜400 code_rejected(attempts_left)｜410
POST /api/v1/login/sms/cancel  {challenge}       → {ok:true}
```

工程约定：

- challenge 是**进程内**句柄（10 分钟 TTL、最多 5 条、成功即删、错 5 次即删）。它握着一条活的 CAS 会话（cookie jar + flowkey），落盘或跨实例迁移都会让学校侧认不出这条会话。
- 日志/响应/推送里只出现打码手机号（`138****1234`）、challenge 前 8 位与 `maskSecret(token)`；验证码与 flowkey 一律不进任何输出。
- **init 没过也照样落盘**（与本轮之前的 `/login/refresh` 不同）：这条 token 是用户等了一条短信换来的，上游一时 500 不代表 token 无效；面板显式回 `school_ok:false` 并附 init 原话。
- 409 的文案同步改成「点面板上的发送验证码」——不能再让用户去开 DevTools。
- 换票的收尾写了两种（会话已认证则直接 GET 发票；否则提交 smsLogin 表单，username 先手机号再学号），因为没拿到过真实验证码，无法把收尾收敛到一条。失败时错误里带每次尝试的 HTTP 码与"页面是否还是二次认证页"，便于下一轮定位。
- **2026-11-18 22:28 真机跑通整条链路**（阿里云 IP、真实账号）：`start`（发码）→ `verify`（用户把手机上的 6 位数字交给面板）→ 换到 token → `init` 通过 → 落盘 → 面板回 `school_ok:true`。这一步证明了"二次认证页 → 发码 → 校验 → 换票"在机房 IP 上确实走得通，不需要人工去浏览器。

### 21.5 v1.4.2 / v1.4.3：学校限流这件事，最后是"别去撞"而不是"重试"

真机连测把学校的发码节流摸清了（同一台手机）：

```
22:23:10  发送成功（第 1 条）
22:23:25  ✗ 13001016
22:24:05  ✗ 13001016
22:25:35  ✗ 13001016
22:27:06  ✗ 13001016
22:28:36  ✓ 发送成功（第 2 条）
```

两个结论，都写进了代码：

1. **学校回的 `sendInterval`（120 秒）不是真实冷却**：真实放行间隔在 4~5 分钟。v1.4.3 起按 `smsSendCooldown = 5 分钟`（按手机号、跨 challenge 记账）自己拦一道，`resend_after` 与提示文案都以它为准——否则用户会在第 2 分钟点一次、被学校拒一次，然后认定功能坏了。
2. **失败的两种形状都要认**：`{"code":412,"message":"13001016"}`（码在 `message`）与 `{"code":412,"data":{"errorCode":60008}}`（码在 `data`）。v1.4.2 之前只认后者，于是 13001016 退化成了"发验证码失败（code=412）：13001016"——就是用户报的那个 bug。

配套的省额度措施（v1.4.2 起）：

- 同一台手机已有**活着的一条 challenge** 时，`start` 直接复用（`reused:true, sent:false` + "上一条还能用"），不再打学校；
- 冷却期内即使没有可复用的 challenge，也直接回 429 并说清还要等多久，**一次都不请求学校**（打了也只会被拒，还多记一次风控）；
- 发码失败不记账（不占冷却），避免用户被自己的失败次数锁死。

### 21.6 v1.5.0/1.5.1：不再让用户点「发送验证码」，配置也不再有版本号

| 变更 | 做法 |
|---|---|
| 409 当场发码 | `writeSecondAuth` 变成 `*Server` 的方法：命中二次认证后直接调 `beginSecondAuth(本次尝试的学号密码)`，把 `challenge / sent / resend_after / expires_in` 塞进 409 的 body；发不出去就只填 `sms_error`，**409 的状态码与 `reason` 不变** |
| 凭据取哪一份 | 用「这次真正尝试的那组」而不是配置里那份（表单里临时测另一个账号时，发码不会跑到别人的手机上）；只有 Cookie 的路径回落到配置，并在缺学号密码时不发码、`sms_error` 讲清原因 |
| 前端只填数字 | 409 且 `challenge` 非空 → 面板自动展开、焦点进验证码框；输入满 6 位 **250ms 防抖 + 单飞**自动调 `/sms/verify`；400 清空重填并显示剩余次数；200 收起并刷新配置/仪表盘 |
| 只有 Cookie / 发码失败 | `challenge` 为空 → 不弹框，只显示 `sms_error` 与两条备用路（认证页 Cookie / 免登 token） |
| 配置不记版本 | `model.Config` 去掉 `Version` 字段，`config.json` 永远没有 `version`；内部计数只留在 `state.json.config_version`（`GET /status` 仍回，供排障，面板不再显示"配置版本 vN"）；`PUT /config` 响应不再带 `version`。读到旧文件里的 `version` 会当成未知键忽略，并把它的值当作内部计数的起点（不会丢历史连续性） |

顺带修掉的两处：

- `login_refresh` 的响应与日志里残留的 `version` 字段/文案删净；
- 学校拒发短信时（`13001016` / `60008` / `10000001`，以及任何 412）统一给"学校拒绝了发码 + 还能做什么"，不再把 `code=412：10000001` 这种半截原文丢给用户。
