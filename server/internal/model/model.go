// Package model holds every type shared across the service.
//
// 这是唯一的共享类型真源：其他包只读不改（见 docs/go-service-design.md §5）。
// JSON 键名与旧版 config.yaml 一一对应（snake_case），前端与 API 直接复用，
// 不做第二次映射——多一层映射就多一处漂移。
package model

import "time"

// ---------- 配置 ----------

// Config 是运行期配置全文，落盘为 data/config.json。
type Config struct {
	Version  int            `json:"version"`
	User     UserConfig     `json:"user"`
	Checkin  CheckinConfig  `json:"checkin"`
	Vacation VacationConfig `json:"vacation"`
	Notify   NotifyConfig   `json:"notify"`
	Server   ServerConfig   `json:"server"`
}

// UserConfig 认证信息：token 与学号密码二选一（有 token 优先）。
type UserConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// CheckinConfig 位置 + 时间调度。locations 非空时完全接管 longitude/latitude/actual_location。
type CheckinConfig struct {
	Longitude      string     `json:"longitude"`
	Latitude       string     `json:"latitude"`
	ActualLocation string     `json:"actual_location"`
	Locations      []Location `json:"locations"`

	// 位置抖动半径（米）。两个键都保留：新键非空时优先，旧键兜底，见 fzu.JitterRadius。
	// 用指针区分"未配置"与"显式 0"：显式 0 = 关闭抖动，缺失 = 默认 200 米。
	LocationJitterM *float64 `json:"location_jitter_m,omitempty"`
	JitterRadiusM   *float64 `json:"jitter_radius_m,omitempty"`

	Schedule ScheduleConfig `json:"schedule"`
}

// Location 位置池的一项。Weight 为 nil 时按 1 处理（<=0 也按 1，不是"永不选中"）。
type Location struct {
	Longitude      string   `json:"longitude"`
	Latitude       string   `json:"latitude"`
	ActualLocation string   `json:"actual_location"`
	Weight         *float64 `json:"weight,omitempty"`
}

// ScheduleConfig 时刻一律用 "HH:MM" 或 "HH:MM:SS" 字符串表达，避免 YAML/JSON 的
// 数字进制歧义（旧版 YAML 把 21:35 解析成 1295 就是这类坑）。
type ScheduleConfig struct {
	WindowStart string   `json:"window_start"`
	WindowEnd   string   `json:"window_end"`
	Times       []string `json:"times"`

	TimeJitterMinutes *float64 `json:"time_jitter_minutes,omitempty"`
	MaxWaitMinutes    *float64 `json:"max_wait_minutes,omitempty"`
	ToleranceMinutes  *float64 `json:"tolerance_minutes,omitempty"`
}

// VacationConfig 假期跳过：区间内不签、不推、不留运行记录。
type VacationConfig struct {
	Notify     bool            `json:"notify"`
	SkipRanges []VacationRange `json:"skip_ranges"`
}

// VacationRange 日期区间，Start/End 为 "2006-01-02"。
type VacationRange struct {
	Name  string `json:"name"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// NotifyConfig 推送通道：填一种即可。
type NotifyConfig struct {
	Type          string `json:"type"` // serverchan|pushplus|bark|wecom|none
	ServerChanKey string `json:"serverchan_key"`
	PushPlusToken string `json:"pushplus_token"`
	BarkURL       string `json:"bark_url"`
	WecomWebhook  string `json:"wecom_webhook"`
}

// ServerConfig 服务自身参数。
type ServerConfig struct {
	Listen       string   `json:"listen"`        // 默认 127.0.0.1:8787
	AllowOrigins []string `json:"allow_origins"` // 跨源白名单，空 = 仅同源
	DataDir      string   `json:"data_dir"`      // 默认 ./data
}

// ---------- 运行状态 ----------

// State 是 data/state.json：只放"重启后仍需知道"的最小事实。
// 不缓存签到是否成功的权威结论——那由学校接口的 FState 决定，这里只用于去重与展示。
type State struct {
	ConfigVersion   int64               `json:"config_version"`
	LastSuccessDate string              `json:"last_success_date"` // 2006-01-02
	LastAttemptAt   time.Time           `json:"last_attempt_at"`
	LastRunID       string              `json:"last_run_id"`
	LastSlotKey     string              `json:"last_slot_key"` // date|HH:MM|target，防同一档重复触发
	Notified        map[string][]string `json:"notified"`      // date -> 已推送过的 status 列表

	// DayCache 是最近一次 init 响应的摘要，只为让仪表盘在重启后也能立即显示
	// "今日计划/迟到线"，不必为了一次刷新就去打学校接口——15 秒一次的轮询若直连
	// 上游，等于把用户的账号暴露在异常流量特征里。
	DayCache *DayPlan `json:"day_cache,omitempty"`
}

// DayPlan 今日考勤计划的缓存视图，来源是 init.action 的 paramsData + schoolData。
type DayPlan struct {
	Date      string    `json:"date"` // 2006-01-02（北京时间）
	Start     string    `json:"start"`
	End       string    `json:"end"`
	LateLine  string    `json:"late_line"`
	HasPlan   bool      `json:"has_plan"`
	Checked   bool      `json:"checked"`
	FetchedAt time.Time `json:"fetched_at"`
}

// ---------- 运行记录 ----------

// RunStatus 一次运行的结论枚举。
const (
	StatusSuccess    = "success"
	StatusAlready    = "already_checked"
	StatusNoPlan     = "no_plan"
	StatusOutOfRange = "out_of_range"
	StatusVacation   = "vacation"
	StatusSkipped    = "skipped"
	StatusError      = "error"
)

// RunRecord 一次运行，一行 JSON 落 data/runs.jsonl。
type RunRecord struct {
	ID         string       `json:"id"`
	Mode       string       `json:"mode"` // scheduled|force|manual|probe
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Decision   string       `json:"decision"` // on_target|late_fallback|force:... 等
	Slot       string       `json:"slot"`     // 配置时刻 HH:MM
	Target     string       `json:"target"`   // 目标时刻 HH:MM:SS
	Status     string       `json:"status"`
	LatencyMS  int64        `json:"latency_ms"`
	IsLate     bool         `json:"is_late"`
	Location   *RunLocation `json:"location,omitempty"`
	School     *RunSchool   `json:"school,omitempty"`
	Notify     *RunNotify   `json:"notify,omitempty"`
	Error      string       `json:"error,omitempty"`
	Logs       []string     `json:"logs,omitempty"`
}

// RunLocation 本次上报的位置（脱敏后仍可复盘：坐标是用户自己填的公共地点）。
type RunLocation struct {
	Lon            float64 `json:"lon"`
	Lat            float64 `json:"lat"`
	Label          string  `json:"label"` // single | locations[i]
	Jittered       bool    `json:"jittered"`
	ActualLocation string  `json:"actual_location"`
}

// RunSchool check.action 的结果摘要。
type RunSchool struct {
	Campus  string `json:"campus"`
	Matched bool   `json:"matched"`
}

// RunNotify 推送结果。
type RunNotify struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
}

// LogEvent 内存日志环 + SSE 的单条事件。
type LogEvent struct {
	TS    time.Time `json:"ts"`
	Level string    `json:"level"` // debug|info|warn|error
	RunID string    `json:"run_id,omitempty"`
	Msg   string    `json:"msg"`
}

// ---------- 上游数据结构 ----------

// Campus check.action 命中项。
type Campus struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ClockInParams clockIn.action 的参数，字段名与学校接口一致。
type ClockInParams struct {
	CampusID       string  `json:"campus"`
	Lon            float64 `json:"lon"`
	Lat            float64 `json:"lat"`
	StartTime      string  `json:"startTime"`
	EndTime        string  `json:"endTime"`
	ActualLocation string  `json:"actualLocation"`
	CaIsNo         string  `json:"caIsNo"`
	CollUnit       string  `json:"collUnit"`
	Way            int     `json:"way"`
	IsLate         int     `json:"isLate"`
}

// Place 本次解析出的基准位置（位置池抽签或单点）。
type Place struct {
	Lon            float64
	Lat            float64
	ActualLocation string
	Label          string // "single" 或 "locations[0]"
}

// CheckinResult 一次打卡的中间结果，供 flow 组装 RunRecord。
type CheckinResult struct {
	Place    Place
	Jittered bool
	Matched  bool
	Campus   string
	IsLate   bool
}
