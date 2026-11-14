// Package config 是配置的唯一出入口：默认模板、规范化、校验、脱敏、YAML 导入导出。
//
// 贯穿全包的一条原则是「能修的就地修，修不了才报错」，这是从 Python 版继承下来的
// 风格：签到是每晚一次的窗口任务，配置里一个手滑（时刻没加引号、坐标写成
// "119.2046O"）如果在 21:35 那一刻才炸，代价就是一整晚漏签。所以除"没有凭据 /
// 没有坐标"这种必然失败的情形外，其余一律降级成默认值 + 警告，旧配置照样能跑。
// 而必失败的那两种，报错文本必须能直接照抄改好 —— 用户不该为了改一个坐标去读源码。
package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"fzucheckin/internal/model"
)

const (
	// MaskSentinel 是脱敏占位值。选一个不可能是真实凭据的字符组合而不是 "****"
	// （后者可能恰好出现在真实 token 里）；它同时承担协议语义：前端回传它表示
	// "保持原值不动"，回传空串表示"清空这一项"。
	MaskSentinel = "••••"

	// AMAPPicker 是坐标的唯一正确来源：高德坐标拾取器，GCJ-02，与打卡上报坐标系一致。
	AMAPPicker = "https://lbs.amap.com/tools/picker"

	DefaultWindowStart = "21:00"
	DefaultWindowEnd   = "23:59"
	DefaultListen      = "127.0.0.1:8787"
	DefaultDataDir     = "./data"

	defaultTimes = "21:35"
)

// notifyTypes 是受支持的推送通道，顺序即文档顺序（报错时按这个列表告诉用户能填什么）。
var notifyTypes = []string{"serverchan", "pushplus", "bark", "wecom", "none"}

// sensitivePaths 是全部需要脱敏的配置路径。坐标不在其中：坐标是用户自己填的公共
// 地点（宿舍楼），前端要回显给用户确认，脱敏掉反而让人没法核对。
var sensitivePaths = []string{
	"user.password",
	"user.token",
	"notify.serverchan_key",
	"notify.pushplus_token",
	"notify.bark_url",
	"notify.wecom_webhook",
}

// ---------- 默认模板 ----------

// DefaultConfig 是"安全默认值"：窗口/时刻/等待/抖动都按旧版 Python 口径，
// 凭据与坐标留空（首次启动进初始化向导），假期按《福州大学 2026—2027 学年校历》
// 预填寒暑假 —— 预填的是"确定的校历事实"，不是猜的假期。
func DefaultConfig() model.Config {
	return model.Config{
		Version: 1,
		Checkin: model.CheckinConfig{
			JitterRadiusM: floatPtr(200),
			Schedule: model.ScheduleConfig{
				WindowStart:       DefaultWindowStart,
				WindowEnd:         DefaultWindowEnd,
				Times:             []string{defaultTimes},
				TimeJitterMinutes: floatPtr(0),
				MaxWaitMinutes:    floatPtr(12),
				ToleranceMinutes:  floatPtr(5),
			},
		},
		Vacation: model.VacationConfig{
			Notify: false,
			SkipRanges: []model.VacationRange{
				{Name: "寒假", Start: "2027-01-23", End: "2027-02-21"},
				{Name: "暑假", Start: "2027-07-03", End: "2027-08-29"},
			},
		},
		Notify: model.NotifyConfig{Type: "none"},
		Server: model.ServerConfig{Listen: DefaultListen, DataDir: DefaultDataDir, AllowOrigins: []string{}},
	}
}

// ---------- 深拷贝 ----------

// Clone 深拷贝配置。Config 里有多个切片与指针（Locations/Times/SkipRanges/
// 三个 *float64），浅拷贝会让"改副本"顺手改掉原对象：store 与 HTTP 处理器同时
// 持有同一份配置时，那就是数据竞争（-race 会直接报）与难以复现的串扰。
func Clone(cfg model.Config) model.Config {
	out := cfg
	if cfg.Checkin.Locations != nil {
		out.Checkin.Locations = make([]model.Location, len(cfg.Checkin.Locations))
		for i, loc := range cfg.Checkin.Locations {
			out.Checkin.Locations[i] = loc
			out.Checkin.Locations[i].Weight = cloneFloat(loc.Weight)
		}
	}
	out.Checkin.LocationJitterM = cloneFloat(cfg.Checkin.LocationJitterM)
	out.Checkin.JitterRadiusM = cloneFloat(cfg.Checkin.JitterRadiusM)
	if cfg.Checkin.Schedule.Times != nil {
		out.Checkin.Schedule.Times = append([]string(nil), cfg.Checkin.Schedule.Times...)
	}
	out.Checkin.Schedule.TimeJitterMinutes = cloneFloat(cfg.Checkin.Schedule.TimeJitterMinutes)
	out.Checkin.Schedule.MaxWaitMinutes = cloneFloat(cfg.Checkin.Schedule.MaxWaitMinutes)
	out.Checkin.Schedule.ToleranceMinutes = cloneFloat(cfg.Checkin.Schedule.ToleranceMinutes)
	if cfg.Vacation.SkipRanges != nil {
		out.Vacation.SkipRanges = append([]model.VacationRange(nil), cfg.Vacation.SkipRanges...)
	}
	if cfg.Server.AllowOrigins != nil {
		out.Server.AllowOrigins = append([]string(nil), cfg.Server.AllowOrigins...)
	}
	return out
}

func cloneFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func floatPtr(v float64) *float64 { return &v }

// ---------- 规范化 ----------

// warnings 收集"改了但不算失败"的问题，交给前端逐条展示。
type warnings struct{ list []string }

func (w *warnings) add(format string, args ...any) {
	w.list = append(w.list, fmt.Sprintf(format, args...))
}

// Normalize 就地修配置并返回"改了什么"。返回新值而不是原地改，是为了让调用方
// 拿到一份可回滚的副本；内部先 Clone，因此不会污染调用方传进来的配置。
func Normalize(cfg model.Config) (model.Config, []string) {
	cfg = Clone(cfg)
	w := &warnings{}
	normalizeUser(&cfg, w)
	normalizeCheckin(&cfg, w)
	normalizeVacation(&cfg, w)
	normalizeNotify(&cfg, w)
	normalizeServer(&cfg, w)
	return cfg, w.list
}

func normalizeUser(cfg *model.Config, w *warnings) {
	cfg.User.Username = strings.TrimSpace(cfg.User.Username)
	cfg.User.Token = strings.TrimSpace(cfg.User.Token)
	// 密码不动：首尾空格可能就是密码的一部分，替用户"修好"等于让登录必然失败。
	_ = w
}

func normalizeCheckin(cfg *model.Config, w *warnings) {
	c := &cfg.Checkin
	c.Longitude = strings.TrimSpace(c.Longitude)
	c.Latitude = strings.TrimSpace(c.Latitude)
	c.ActualLocation = strings.TrimSpace(c.ActualLocation)

	// 半径两个键分别收敛：负数当笔误（→200），显式 0 是"关闭抖动"，必须保留。
	c.LocationJitterM = fixRadius(c.LocationJitterM, "checkin.location_jitter_m", w)
	c.JitterRadiusM = fixRadius(c.JitterRadiusM, "checkin.jitter_radius_m", w)

	normalizeLocations(c, w)

	// 单点坐标只在没有位置池时才有意义：locations 非空时它被完整忽略，
	// 这时报"单点坐标不是数字"只会让用户去改一段本来就没用的配置。
	if len(c.Locations) == 0 {
		for _, f := range []struct{ key, val string }{
			{"longitude", c.Longitude},
			{"latitude", c.Latitude},
		} {
			if f.val == "" {
				continue
			}
			if _, ok := parseCoordinate(f.val); !ok {
				full := "checkin." + f.key
				w.add("%s = %q 不是数字：坐标只能用纯数字，建议加引号写成 %s: \"119.20463\"。"+
					"坐标来源：高德坐标拾取器 %s", full, f.val, full, AMAPPicker)
			}
		}
	}

	s := &c.Schedule
	s.WindowStart = strings.TrimSpace(s.WindowStart)
	s.WindowEnd = strings.TrimSpace(s.WindowEnd)
	start := normalizeClock(s.WindowStart, "window_start", DefaultWindowStart, w)
	end := normalizeClock(s.WindowEnd, "window_end", DefaultWindowEnd, w)
	s.WindowStart, s.WindowEnd = start, end
	// 这一步必须无条件比较：即使某一边写坏回退成了默认值，也可能出现"终点不晚于起点"
	// 的组合（如 window_start: 23:59 且 window_end 缺失）。漏掉它会让整晚的时间判断失效。
	if clockSeconds(end) <= clockSeconds(start) {
		w.add("checkin.schedule.window_end（%s）不晚于 window_start（%s）：不支持跨日窗口，"+
			"已回退默认 %s-%s。", end, start, DefaultWindowStart, DefaultWindowEnd)
		s.WindowStart, s.WindowEnd = DefaultWindowStart, DefaultWindowEnd
	}

	s.Times = normalizeTimes(s.Times, w)

	s.TimeJitterMinutes = fixMinutes(s.TimeJitterMinutes, "checkin.schedule.time_jitter_minutes", w)
	s.MaxWaitMinutes = fixMinutes(s.MaxWaitMinutes, "checkin.schedule.max_wait_minutes", w)
	s.ToleranceMinutes = fixMinutes(s.ToleranceMinutes, "checkin.schedule.tolerance_minutes", w)
}

// normalizeLocations 把位置池收敛成"每项都有可用经纬度"。
// 位置池只是可选增强：写坏了应该退回单点继续签到，而不是让整晚签到因为多缩进
// 一行而失败；索引按 1 开始报，方便用户数着行改。
func normalizeLocations(c *model.CheckinConfig, w *warnings) {
	if len(c.Locations) == 0 {
		return
	}
	kept := make([]model.Location, 0, len(c.Locations))
	var dropped []int
	for i, loc := range c.Locations {
		idx := i + 1
		loc.Longitude = strings.TrimSpace(loc.Longitude)
		loc.Latitude = strings.TrimSpace(loc.Latitude)
		loc.ActualLocation = strings.TrimSpace(loc.ActualLocation)
		_, okLon := parseCoordinate(loc.Longitude)
		_, okLat := parseCoordinate(loc.Latitude)
		if !okLon || !okLat {
			dropped = append(dropped, idx)
			continue
		}
		loc.Weight = normalizeWeight(loc.Weight, idx, w)
		kept = append(kept, loc)
	}
	if len(dropped) > 0 {
		w.add("checkin.locations 第 %s 项没有可用经纬度（每项都要有 longitude / latitude，且是纯数字），"+
			"已忽略这些项。坐标来源：%s", joinInts(dropped, "、"), AMAPPicker)
	}
	c.Locations = kept
	// locations 非空时单点三项被忽略：保留原值便于用户随时回退到单点，但不再参与校验。
}

// normalizeWeight 把非法权重收敛成 1：权重 <=0 或 NaN 若原样下传，抽签实现一旦
// 用"总和为 0 就永不选中"的写法，就会变成"这一项永远抽不到"，而用户的意图只是
// "少去几次"。
func normalizeWeight(weight *float64, idx int, w *warnings) *float64 {
	if weight == nil {
		return nil // nil 由下游按 1 处理，不强制落成 1，避免配置里凭空多出 weight: 1
	}
	v := *weight
	if math.IsNaN(v) || math.IsInf(v, 0) {
		w.add("checkin.locations 第 %d 项的 weight 不是有效数字，已按 1 处理。", idx)
		return floatPtr(1)
	}
	if v <= 0 {
		w.add("checkin.locations 第 %d 项的 weight = %s 不是正数，已按 1 处理（想让这一项更容易被抽中就填大于 1 的数）。",
			idx, formatNum(v))
		return floatPtr(1)
	}
	return weight
}

// normalizeClock 把窗口边界收敛成 "HH:MM"；写坏/留空一律回退默认并给出改法。
func normalizeClock(raw, key, def string, w *warnings) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return def
	}
	if fixed, ok := restoreSexagesimal(text); ok {
		w.add("checkin.schedule.%s 写成了未加引号的 %s（YAML 按 60 进制整数解析成 %s），已还原；"+
			"建议写成 %s: \"%s\"（务必加引号）。", key, fixed, text, key, fixed)
		text = fixed
	}
	if canon, ok := parseClock(text); ok {
		return canon
	}
	w.add("checkin.schedule.%s = %q 不像时刻，已回退默认 %s。正确写法：%s: \"%s\"（HH:MM，加引号最稳）。",
		key, raw, def, key, def)
	return def
}

// normalizeTimes 逐项解析时刻：坏的丢弃并指出第几项，全坏才回退默认。
func normalizeTimes(raw []string, w *warnings) []string {
	times := make([]string, 0, len(raw))
	var bad []string
	for i, item := range raw {
		idx := i + 1
		text := strings.TrimSpace(item)
		if text == "" {
			bad = append(bad, fmt.Sprintf("第 %d 项（空值）", idx))
			continue
		}
		if fixed, ok := restoreSexagesimal(text); ok {
			w.add("checkin.schedule.times 第 %d 项 %s 是未加引号的时刻（YAML 按 60 进制整数解析），"+
				"已还原成 \"%s\"；建议写成 times: [\"21:35\", \"22:10\"]（务必加引号）。", idx, text, fixed)
			text = fixed
		}
		canon, ok := parseClock(text)
		if !ok {
			bad = append(bad, fmt.Sprintf("第 %d 项 %q", idx, item))
			continue
		}
		times = append(times, canon)
	}
	if len(bad) > 0 {
		w.add("checkin.schedule.times 里这些值不是 HH:MM 时刻，已忽略：%s。正确写法：times: [\"21:35\", \"22:10\"]",
			strings.Join(bad, "、"))
	}
	if len(times) == 0 {
		if len(raw) > 0 {
			w.add("checkin.schedule.times 里的时刻全部无法解析，已回退默认时刻 [\"%s\"]。", defaultTimes)
		}
		times = []string{defaultTimes}
	}
	return times
}

func normalizeVacation(cfg *model.Config, w *warnings) {
	ranges := make([]model.VacationRange, 0, len(cfg.Vacation.SkipRanges))
	for i, r := range cfg.Vacation.SkipRanges {
		idx := i + 1
		r.Name = strings.TrimSpace(r.Name)
		r.Start = strings.TrimSpace(r.Start)
		r.End = strings.TrimSpace(r.End)
		st, errStart := time.Parse("2006-01-02", r.Start)
		en, errEnd := time.Parse("2006-01-02", r.End)
		switch {
		case errStart != nil || errEnd != nil:
			w.add("vacation.skip_ranges 第 %d 项日期写法不对，已忽略该项（start=%q end=%q）。"+
				"正确写法：start: \"2027-01-23\"、end: \"2027-02-21\"（YYYY-MM-DD，务必加引号）。",
				idx, r.Start, r.End)
			continue
		case en.Before(st):
			w.add("vacation.skip_ranges 第 %d 项结束日期早于开始日期，已忽略该项（start=%q end=%q）。",
				idx, r.Start, r.End)
			continue
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("假期 %s~%s", r.Start, r.End)
		}
		ranges = append(ranges, r)
	}
	cfg.Vacation.SkipRanges = ranges
}

func normalizeNotify(cfg *model.Config, w *warnings) {
	n := &cfg.Notify
	n.Type = strings.ToLower(strings.TrimSpace(n.Type))
	n.ServerChanKey = strings.TrimSpace(n.ServerChanKey)
	n.PushPlusToken = strings.TrimSpace(n.PushPlusToken)
	n.BarkURL = strings.TrimSpace(n.BarkURL)
	n.WecomWebhook = strings.TrimSpace(n.WecomWebhook)
	switch {
	case n.Type == "":
		n.Type = "none"
	case !validNotifyType(n.Type):
		w.add("notify.type = %q 不是受支持的推送通道，已按 none（不推送）处理。可选：%s。",
			n.Type, strings.Join(notifyTypes, " | "))
		n.Type = "none"
	}
}

func normalizeServer(cfg *model.Config, w *warnings) {
	s := &cfg.Server
	s.Listen = strings.TrimSpace(s.Listen)
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	s.DataDir = strings.TrimSpace(s.DataDir)
	if s.DataDir == "" {
		s.DataDir = DefaultDataDir
	}
	origins := make([]string, 0, len(s.AllowOrigins))
	for _, o := range s.AllowOrigins {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	s.AllowOrigins = origins
	_ = w
}

// ---------- 校验 ----------

// Validate 只对"必然打不了卡"的情形硬失败：没有凭据、没有可用坐标。
// 其余问题（时刻写坏、权重非法、窗口早晚颠倒）已在 Normalize 里降级成默认值 +
// 警告，这里再拦一次只会把用户挡在门外而不会让签到变得可能。
// 失败的报错带两种正确写法，用户照抄即可。
func Validate(cfg model.Config) error {
	var problems []string

	hasToken := strings.TrimSpace(cfg.User.Token) != ""
	hasAccount := strings.TrimSpace(cfg.User.Username) != "" && strings.TrimSpace(cfg.User.Password) != ""
	if !hasToken && !hasAccount {
		problems = append(problems, strings.Join([]string{
			"认证信息没配好：user.token 与「user.username + user.password」二选一，至少要有一项。",
			"  · 已经有晚点名 token（H5 免登 token，会过期）：",
			"      user:",
			"        token: \"粘贴 token\"",
			"  · 没有 token（推荐，token 过期会自动用账密重新登录）：",
			"      user:",
			"        username: \"学号\"",
			"        password: \"当前能登录智汇福大 App 的密码\"",
		}, "\n"))
	}

	if !hasUsableLocation(cfg.Checkin) {
		lines := []string{
			"签到位置没配好：checkin 段里既没有可用的 longitude / latitude，也没有非空的 locations 列表，无法打卡。",
			"  坐标来源：高德坐标拾取器 " + AMAPPicker,
			"  搜「福州大学旗山校区」或直接点选你的宿舍楼 → 复制「经度,纬度」",
			"  写法一 · 单点（旧配置，最小可用）：",
			"    checkin:",
			"      longitude: \"119.20463\"",
			"      latitude: \"26.04920\"",
			"      actual_location: \"福州大学旗山校区生活区X号楼\"",
			"  写法二 · 位置池（多个位置随机挑一个，每项都要有坐标）：",
			"    checkin:",
			"      locations:",
			"        - longitude: \"119.20463\"",
			"          latitude: \"26.04920\"",
			"          actual_location: \"福州大学旗山校区生活区X号楼\"",
			"          weight: 1",
			"        - longitude: \"119.20510\"",
			"          latitude: \"26.04880\"",
			"          actual_location: \"福州大学旗山校区教学楼\"",
		}
		for _, f := range []struct{ key, val string }{
			{"longitude", cfg.Checkin.Longitude},
			{"latitude", cfg.Checkin.Latitude},
		} {
			v := strings.TrimSpace(f.val)
			if v == "" {
				continue
			}
			if _, ok := parseCoordinate(v); !ok {
				lines = append(lines, fmt.Sprintf("  × 当前 checkin.%s = %q 不是数字（坐标要纯数字，加引号更稳妥）", f.key, v))
			}
		}
		problems = append(problems, strings.Join(lines, "\n"))
	}

	if t := strings.TrimSpace(cfg.Notify.Type); t != "" && !validNotifyType(strings.ToLower(t)) {
		problems = append(problems, fmt.Sprintf(
			"notify.type = %q 不是受支持的推送通道，可选：%s（不推送填 none）。", t, strings.Join(notifyTypes, " | ")))
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func hasUsableLocation(c model.CheckinConfig) bool {
	for _, loc := range c.Locations {
		if _, ok := parseCoordinate(loc.Longitude); !ok {
			continue
		}
		if _, ok := parseCoordinate(loc.Latitude); !ok {
			continue
		}
		return true
	}
	_, okLon := parseCoordinate(c.Longitude)
	_, okLat := parseCoordinate(c.Latitude)
	return okLon && okLat
}

func validNotifyType(t string) bool {
	for _, v := range notifyTypes {
		if t == v {
			return true
		}
	}
	return false
}

// ---------- 脱敏 ----------

// secretField 返回可写的字段地址；路径是稳定字符串，便于前端按 key 判断"已配置"。
func secretField(cfg *model.Config, path string) *string {
	switch path {
	case "user.password":
		return &cfg.User.Password
	case "user.token":
		return &cfg.User.Token
	case "notify.serverchan_key":
		return &cfg.Notify.ServerChanKey
	case "notify.pushplus_token":
		return &cfg.Notify.PushPlusToken
	case "notify.bark_url":
		return &cfg.Notify.BarkURL
	case "notify.wecom_webhook":
		return &cfg.Notify.WecomWebhook
	}
	return nil
}

// Mask 把凭据换成哨兵值，只用于响应/导出。
//
// 空值保持空值：如果空字段也被写成哨兵，前端就没法表达"清空"——MergeMasked 会
// 把它当成"保持原值"，用户永远删不掉一个填错的 key。
func Mask(cfg model.Config) model.Config {
	out := Clone(cfg)
	for _, path := range sensitivePaths {
		if f := secretField(&out, path); f != nil && strings.TrimSpace(*f) != "" {
			*f = MaskSentinel
		}
	}
	return out
}

// MaskedFields 告诉前端哪些敏感字段"原本非空"（显示为"已配置"）。
// 全部路径都会出现在返回里，没配的为 false，前端不必区分"缺 key"和"false"。
func MaskedFields(cfg model.Config) map[string]bool {
	out := make(map[string]bool, len(sensitivePaths))
	for _, path := range sensitivePaths {
		f := secretField(&cfg, path)
		out[path] = f != nil && strings.TrimSpace(*f) != ""
	}
	return out
}

// MergeMasked 把前端回传的配置合到旧配置上。
//
// 协议：字段值 == MaskSentinel 表示"保持旧值"（前端拿到的是哨兵，没改就原样回传）；
// 空串表示"清空"（用户显式删掉）。两条都要有，否则要么改不动、要么删不掉。
func MergeMasked(oldCfg, incoming model.Config) model.Config {
	out := Clone(incoming)
	for _, path := range sensitivePaths {
		dst := secretField(&out, path)
		src := secretField(&oldCfg, path)
		if dst == nil || src == nil {
			continue
		}
		if *dst == MaskSentinel {
			*dst = *src
		}
	}
	return out
}

// ---------- 时刻与数字 ----------

// parseClock 把 "21:35" / "21:35:00" / "9:05" 解析成规范文本 "HH:MM[:SS]"。
// 容忍首尾空白与全角冒号（手机复制粘贴配置很常见），秒为 0 时不输出 :00。
func parseClock(text string) (string, bool) {
	raw := strings.TrimSpace(text)
	raw = strings.ReplaceAll(raw, "：", ":")
	if raw == "" {
		return "", false
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return "", false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 2 {
			return "", false
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return "", false
			}
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return "", false
		}
		nums[i] = v
	}
	hour, minute := nums[0], nums[1]
	second := 0
	if len(nums) == 3 {
		second = nums[2]
	}
	if hour > 23 || minute > 59 || second > 59 {
		return "", false
	}
	if second == 0 {
		return fmt.Sprintf("%02d:%02d", hour, minute), true
	}
	return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second), true
}

// clockSeconds 把 "HH:MM[:SS]" 换成当天秒数，用来比较窗口早晚。
func clockSeconds(text string) int {
	parts := strings.Split(strings.ReplaceAll(text, "：", ":"), ":")
	if len(parts) < 2 {
		return 0
	}
	h, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	s := 0
	if len(parts) >= 3 {
		s, _ = strconv.Atoi(strings.TrimSpace(parts[2]))
	}
	return h*3600 + m*60 + s
}

// restoreSexagesimal 还原"未加引号的时刻"：YAML 1.1 把 21:35 解析成 60 进制整数
// 1295，到我们手里只剩一个纯数字。按 Python 版同口径还原：不超过 23:59 的分钟数
// 读成"时:分"，更大且不超过一天的秒数读成"时:分:秒"。
func restoreSexagesimal(text string) (string, bool) {
	body := strings.TrimSpace(text)
	if body == "" {
		return "", false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return "", false
		}
	}
	v, err := strconv.Atoi(body)
	if err != nil {
		return "", false
	}
	if v <= 23*60+59 {
		return fmt.Sprintf("%02d:%02d", v/60, v%60), true
	}
	if v <= 23*3600+59*60+59 {
		return fmt.Sprintf("%02d:%02d:%02d", v/3600, v%3600/60, v%60), true
	}
	return "", false
}

// parseCoordinate 把坐标文本转成 float；空串、非数字、NaN/Inf 都不算"可用坐标"。
func parseCoordinate(text string) (float64, bool) {
	s := strings.TrimSpace(text)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func fixRadius(p *float64, key string, w *warnings) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		w.add("%s 不是有效数字（NaN/Inf），已按「未配置」处理，实际使用默认 200 米。", key)
		return nil
	case v < 0:
		w.add("%s = %s 是负数，已按默认 200 米处理；想关闭位置抖动请显式写 0。", key, formatNum(v))
		return floatPtr(200)
	}
	return p
}

func fixMinutes(p *float64, key string, w *warnings) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		w.add("%s 不是有效数字（NaN/Inf），已按「未配置」处理，使用默认值。", key)
		return nil
	case v < 0:
		w.add("%s = %s 是负数，已按 0 处理。", key, formatNum(v))
		return floatPtr(0)
	}
	return p
}

// formatNum 输出人能读的数字：整数不带小数点，其余用最短往返表示。
func formatNum(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatFloat(v float64) string { return formatNum(v) }

func joinInts(nums []int, sep string) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, sep)
}

// ---------- YAML 导入 ----------

// knownSections 是旧 config.yaml 认得的顶层段；别的一律按"写错段名"警告。
var knownSections = map[string]bool{
	"user": true, "checkin": true, "vacation": true, "notify": true, "server": true,
}

// ImportYAML 把旧 config.yaml 解析成完整配置（内存对象，不落盘）。
//
// 三条口径，都是为了让"旧配置继续可用"：
//  1. 起点是 DefaultConfig，所以文件里没写的段（server.listen 之类）不会变成空值；
//  2. 但 vacation.skip_ranges 不继承默认值 —— 导入一份没写假期的旧配置，若凭空
//     塞进寒暑假区间，用户会在 2027 年 1 月发现"为什么不签了"，这是最坏的静默失败；
//  3. 解析完立刻跑 Normalize，让"未加引号的 21:35"之类当场被修好并给出警告，
//     前端预览看到的就是保存后真正生效的形态。
func ImportYAML(text string) (model.Config, []string, error) {
	root, err := parseYAML(text)
	if err != nil {
		return model.Config{}, nil, err
	}
	if root == nil || root.kind == yamlKindNull {
		return model.Config{}, nil, &yamlParseError{line: 1, msg: "内容为空，没有可导入的配置段（需要 user / checkin / vacation / notify）"}
	}
	if root.kind != yamlKindMap {
		return model.Config{}, nil, &yamlParseError{line: root.line, msg: "顶层必须是映射（key: value），例如 user / checkin / vacation / notify"}
	}
	cfg := DefaultConfig()
	cfg.Vacation.SkipRanges = nil
	cfg.Checkin.Locations = nil
	w := &warnings{}
	applyYAML(&cfg, root, w)
	cfg, normWarnings := Normalize(cfg)
	return cfg, append(w.list, normWarnings...), nil
}

func applyYAML(cfg *model.Config, root *yamlNode, w *warnings) {
	for _, key := range root.keys {
		if !knownSections[key] {
			w.add("YAML 里的 %q 不是本服务认识的配置段，已忽略（认得的是 user / checkin / vacation / notify / server）。", key)
		}
	}

	if n := root.get("user"); n != nil {
		if n.kind != yamlKindMap {
			w.add("user 段应该是映射（key: value），当前写成了%s，已按空段处理（需要 token 或 学号+密码）。", kindName(n))
		} else {
			setStr(&cfg.User.Token, n.get("token"), "user.token", w)
			setStr(&cfg.User.Username, n.get("username"), "user.username", w)
			setStr(&cfg.User.Password, n.get("password"), "user.password", w)
		}
	}

	if n := root.get("checkin"); n != nil && n.kind == yamlKindMap {
		setStr(&cfg.Checkin.Longitude, n.get("longitude"), "checkin.longitude", w)
		setStr(&cfg.Checkin.Latitude, n.get("latitude"), "checkin.latitude", w)
		setStr(&cfg.Checkin.ActualLocation, n.get("actual_location"), "checkin.actual_location", w)
		setNumPtr(&cfg.Checkin.JitterRadiusM, n.get("jitter_radius_m"), "checkin.jitter_radius_m", w)
		setNumPtr(&cfg.Checkin.LocationJitterM, n.get("location_jitter_m"), "checkin.location_jitter_m", w)
		applyYAMLLocations(cfg, n.get("locations"), w)
		applyYAMLSchedule(cfg, n.get("schedule"), w)
	} else if n != nil {
		w.add("checkin 段应该是映射（key: value），当前写成了%s，已按空段处理。", kindName(n))
	}

	if n := root.get("vacation"); n != nil {
		if n.kind != yamlKindMap {
			w.add("vacation 段应该是映射（key: value），当前写成了%s，已按空段处理。", kindName(n))
		} else {
			setBool(&cfg.Vacation.Notify, n.get("notify"), "vacation.notify", w)
			applyYAMLVacationRanges(cfg, n.get("skip_ranges"), w)
		}
	}

	if n := root.get("notify"); n != nil {
		if n.kind != yamlKindMap {
			w.add("notify 段应该是映射（key: value），当前写成了%s，已按空段处理。", kindName(n))
		} else {
			setStr(&cfg.Notify.Type, n.get("type"), "notify.type", w)
			setStr(&cfg.Notify.ServerChanKey, n.get("serverchan_key"), "notify.serverchan_key", w)
			setStr(&cfg.Notify.PushPlusToken, n.get("pushplus_token"), "notify.pushplus_token", w)
			setStr(&cfg.Notify.BarkURL, n.get("bark_url"), "notify.bark_url", w)
			setStr(&cfg.Notify.WecomWebhook, n.get("wecom_webhook"), "notify.wecom_webhook", w)
		}
	}

	if n := root.get("server"); n != nil {
		if n.kind != yamlKindMap {
			w.add("server 段应该是映射（key: value），当前写成了%s，已按空段处理。", kindName(n))
		} else {
			setStr(&cfg.Server.Listen, n.get("listen"), "server.listen", w)
			setStr(&cfg.Server.DataDir, n.get("data_dir"), "server.data_dir", w)
			setStrList(&cfg.Server.AllowOrigins, n.get("allow_origins"), "server.allow_origins", w)
		}
	}
}

func applyYAMLSchedule(cfg *model.Config, n *yamlNode, w *warnings) {
	if n == nil {
		return
	}
	if n.kind != yamlKindMap {
		w.add("checkin.schedule 应该是一个映射（key: value），当前写成了%s，本次忽略该段，"+
			"按默认时刻 [\"21:35\"]、窗口 21:00-23:59 签到。", kindName(n))
		return
	}
	s := &cfg.Checkin.Schedule
	setStr(&s.WindowStart, n.get("window_start"), "checkin.schedule.window_start", w)
	setStr(&s.WindowEnd, n.get("window_end"), "checkin.schedule.window_end", w)
	setStrList(&s.Times, n.get("times"), "checkin.schedule.times", w)
	setNumPtr(&s.TimeJitterMinutes, n.get("time_jitter_minutes"), "checkin.schedule.time_jitter_minutes", w)
	setNumPtr(&s.MaxWaitMinutes, n.get("max_wait_minutes"), "checkin.schedule.max_wait_minutes", w)
	setNumPtr(&s.ToleranceMinutes, n.get("tolerance_minutes"), "checkin.schedule.tolerance_minutes", w)
}

func applyYAMLLocations(cfg *model.Config, n *yamlNode, w *warnings) {
	if n == nil {
		return
	}
	if n.kind != yamlKindSeq {
		w.add("checkin.locations 应该是列表（每项一个坐标点），当前写成了%s，本次忽略该段，"+
			"改用 checkin.longitude / latitude。正确写法：\n"+
			"        locations:\n"+
			"          - longitude: \"119.20463\"\n"+
			"            latitude: \"26.04920\"\n"+
			"            actual_location: \"福州大学旗山校区生活区X号楼\"", kindName(n))
		return
	}
	locs := make([]model.Location, 0, len(n.items))
	for _, item := range n.items {
		var loc model.Location
		if item != nil && item.kind == yamlKindMap {
			setStr(&loc.Longitude, item.get("longitude"), "checkin.locations[].longitude", w)
			setStr(&loc.Latitude, item.get("latitude"), "checkin.locations[].latitude", w)
			setStr(&loc.ActualLocation, item.get("actual_location"), "checkin.locations[].actual_location", w)
			setNumPtr(&loc.Weight, item.get("weight"), "checkin.locations[].weight", w)
		}
		// 非映射项也占一个位置：留空后由 Normalize 按"第几项"剔除并报索引，
		// 这样提示里的序号与用户数 YAML 列表的序号一致。
		locs = append(locs, loc)
	}
	cfg.Checkin.Locations = locs
}

func applyYAMLVacationRanges(cfg *model.Config, n *yamlNode, w *warnings) {
	if n == nil {
		return
	}
	if n.kind != yamlKindSeq {
		w.add("vacation.skip_ranges 应该是列表（每项 name / start / end），当前写成了%s，已忽略该段（不跳过任何假期）。", kindName(n))
		return
	}
	ranges := make([]model.VacationRange, 0, len(n.items))
	for i, item := range n.items {
		prefix := fmt.Sprintf("vacation.skip_ranges[%d]", i+1)
		var r model.VacationRange
		if item != nil && item.kind == yamlKindMap {
			setStr(&r.Name, item.get("name"), prefix+".name", w)
			setStr(&r.Start, item.get("start"), prefix+".start", w)
			setStr(&r.End, item.get("end"), prefix+".end", w)
		}
		ranges = append(ranges, r)
	}
	cfg.Vacation.SkipRanges = ranges
}

func setStr(dst *string, n *yamlNode, path string, w *warnings) {
	if n.isNull() {
		return
	}
	text, ok := n.asText()
	if !ok {
		w.add("%s 应该是文本，当前写成了%s，已忽略该项。", path, kindName(n))
		return
	}
	*dst = text
}

func setNumPtr(dst **float64, n *yamlNode, path string, w *warnings) {
	if n.isNull() {
		return
	}
	if text, ok := n.asText(); ok && strings.TrimSpace(text) == "" {
		return // 留空 = 未配置，与"写错"不是一回事，不该报警告
	}
	v, ok := n.asNumber()
	if !ok {
		w.add("%s 应该是数字，当前是 %s，已忽略该项（使用默认值）。", path, nodePreview(n))
		return
	}
	*dst = floatPtr(v)
}

func setBool(dst *bool, n *yamlNode, path string, w *warnings) {
	if n.isNull() {
		return
	}
	v, ok := n.asBool()
	if !ok {
		w.add("%s 应该是 true / false，当前是 %s，已忽略该项。", path, nodePreview(n))
		return
	}
	*dst = v
}

func setStrList(dst *[]string, n *yamlNode, path string, w *warnings) {
	if n.isNull() {
		return
	}
	if n.kind != yamlKindSeq {
		hint := "times: [\"21:35\", \"22:10\"]"
		if path != "checkin.schedule.times" {
			hint = path + ": [\"值1\", \"值2\"]"
		}
		w.add("%s 应该是列表，例如 %s，当前写成了%s，已忽略该项。", path, hint, kindName(n))
		return
	}
	out := make([]string, 0, len(n.items))
	for _, item := range n.items {
		text, ok := item.asText()
		if !ok {
			continue // 空项直接略过，等价于用户没写
		}
		out = append(out, text)
	}
	*dst = out
}

func kindName(n *yamlNode) string {
	if n == nil {
		return "空"
	}
	switch n.kind {
	case yamlKindNull:
		return "空值"
	case yamlKindMap:
		return "映射"
	case yamlKindSeq:
		return "列表"
	case yamlKindScalar:
		switch n.skind {
		case scalarBool:
			return "布尔值"
		case scalarInt, scalarFloat:
			return "数字"
		default:
			return "字符串"
		}
	}
	return "未知结构"
}

func nodePreview(n *yamlNode) string {
	if n == nil {
		return "空"
	}
	if n.kind == yamlKindScalar {
		return strconv.Quote(n.text)
	}
	return kindName(n)
}

// ---------- YAML 导出 ----------

// ToYAML 把配置导出成可被 ImportYAML 读回的 YAML。
//
// 调用方保证已脱敏（GET /config/export 必须导出脱敏版，否则一次导出就把凭据
// 泄漏到剪贴板/日志里）。这里再兜一层：万一传进来的是原值，输出顶部会带一行
// 醒目警告，而不是默默把 token 写进文件。
func ToYAML(cfg model.Config) string {
	var b strings.Builder
	b.WriteString("# fzu-auto-checkin 配置导出\n")
	b.WriteString("# 时刻一律加引号：不加引号时 YAML 会按 60 进制整数解析（21:35 → 1295）。\n")
	if hasPlainSecret(cfg) {
		b.WriteString("# 警告：本文件包含明文凭据（调用方未脱敏），转发给他人前请先脱敏。\n")
	}

	b.WriteString("user:\n")
	fmt.Fprintf(&b, "  token: %s\n", yamlQuote(cfg.User.Token))
	fmt.Fprintf(&b, "  username: %s\n", yamlQuote(cfg.User.Username))
	fmt.Fprintf(&b, "  password: %s\n", yamlQuote(cfg.User.Password))

	b.WriteString("checkin:\n")
	fmt.Fprintf(&b, "  longitude: %s\n", yamlQuote(cfg.Checkin.Longitude))
	fmt.Fprintf(&b, "  latitude: %s\n", yamlQuote(cfg.Checkin.Latitude))
	fmt.Fprintf(&b, "  actual_location: %s\n", yamlQuote(cfg.Checkin.ActualLocation))
	if len(cfg.Checkin.Locations) == 0 {
		b.WriteString("  locations: []\n")
	} else {
		b.WriteString("  locations:\n")
		for _, loc := range cfg.Checkin.Locations {
			fmt.Fprintf(&b, "    - longitude: %s\n", yamlQuote(loc.Longitude))
			fmt.Fprintf(&b, "      latitude: %s\n", yamlQuote(loc.Latitude))
			fmt.Fprintf(&b, "      actual_location: %s\n", yamlQuote(loc.ActualLocation))
			if loc.Weight != nil {
				fmt.Fprintf(&b, "      weight: %s\n", formatNum(*loc.Weight))
			}
		}
	}
	fmt.Fprintf(&b, "  jitter_radius_m: %s\n", yamlNum(cfg.Checkin.JitterRadiusM))
	fmt.Fprintf(&b, "  location_jitter_m: %s\n", yamlNum(cfg.Checkin.LocationJitterM))

	s := cfg.Checkin.Schedule
	b.WriteString("  schedule:\n")
	fmt.Fprintf(&b, "    window_start: %s\n", yamlQuote(s.WindowStart))
	fmt.Fprintf(&b, "    window_end: %s\n", yamlQuote(s.WindowEnd))
	fmt.Fprintf(&b, "    times: %s\n", yamlStringList(s.Times))
	fmt.Fprintf(&b, "    time_jitter_minutes: %s\n", yamlNum(s.TimeJitterMinutes))
	fmt.Fprintf(&b, "    max_wait_minutes: %s\n", yamlNum(s.MaxWaitMinutes))
	fmt.Fprintf(&b, "    tolerance_minutes: %s\n", yamlNum(s.ToleranceMinutes))

	b.WriteString("vacation:\n")
	fmt.Fprintf(&b, "  notify: %t\n", cfg.Vacation.Notify)
	if len(cfg.Vacation.SkipRanges) == 0 {
		b.WriteString("  skip_ranges: []\n")
	} else {
		b.WriteString("  skip_ranges:\n")
		for _, r := range cfg.Vacation.SkipRanges {
			fmt.Fprintf(&b, "    - name: %s\n", yamlQuote(r.Name))
			fmt.Fprintf(&b, "      start: %s\n", yamlQuote(r.Start))
			fmt.Fprintf(&b, "      end: %s\n", yamlQuote(r.End))
		}
	}

	b.WriteString("notify:\n")
	fmt.Fprintf(&b, "  type: %s\n", yamlQuote(cfg.Notify.Type))
	fmt.Fprintf(&b, "  serverchan_key: %s\n", yamlQuote(cfg.Notify.ServerChanKey))
	fmt.Fprintf(&b, "  pushplus_token: %s\n", yamlQuote(cfg.Notify.PushPlusToken))
	fmt.Fprintf(&b, "  bark_url: %s\n", yamlQuote(cfg.Notify.BarkURL))
	fmt.Fprintf(&b, "  wecom_webhook: %s\n", yamlQuote(cfg.Notify.WecomWebhook))

	b.WriteString("server:\n")
	fmt.Fprintf(&b, "  listen: %s\n", yamlQuote(cfg.Server.Listen))
	fmt.Fprintf(&b, "  data_dir: %s\n", yamlQuote(cfg.Server.DataDir))
	if len(cfg.Server.AllowOrigins) == 0 {
		b.WriteString("  allow_origins: []\n")
	} else {
		b.WriteString("  allow_origins:\n")
		for _, o := range cfg.Server.AllowOrigins {
			fmt.Fprintf(&b, "    - %s\n", yamlQuote(o))
		}
	}
	return b.String()
}

func hasPlainSecret(cfg model.Config) bool {
	for _, path := range sensitivePaths {
		f := secretField(&cfg, path)
		if f == nil || strings.TrimSpace(*f) == "" {
			continue
		}
		if *f != MaskSentinel {
			return true
		}
	}
	return false
}

// yamlQuote 一律用双引号：裸标量在 YAML 里会踩到 21:35 / on / null / 2027-01-23
// 这些隐式类型，加引号是最省心的写法，也保证本解析器能原样读回。
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlNum 用 ~ 表示"未配置"：本解析器把它读回空值，语义与 nil 指针一一对应。
func yamlNum(p *float64) string {
	if p == nil {
		return "~"
	}
	return formatNum(*p)
}

func yamlStringList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = yamlQuote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
