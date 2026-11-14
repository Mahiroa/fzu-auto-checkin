// Package schedule 是签到时间调度的纯函数内核：窗口判定、多档规划、时间抖动与最终决策。
//
// 为什么单独成包：时间语义（北京时间、窗口端点、抖动具体落在哪一秒）一旦散进调度循环，
// 就没法离线复现一次决策。这里不联网、不 sleep、不打印、不看宿主机时区，随机源可注入，
// 所以任意时刻的结论都能用"一串固定 u"重放出来——这是能与 Python 原实现
// (src/schedule.py) 逐条对答案的前提，一致性语料见 server/testdata/schedule_parity.json。
//
// 契约：docs/go-service-design.md §5.2 / §10。
package schedule

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"fzucheckin/internal/model"
)

// 内置默认值：与 Python 版 src/schedule.py 顶部常量一一对应，改这里等于改契约。
const (
	defaultTime         = "21:35" // times 写空或全写坏时的兜底档
	defaultWindowStart  = "21:00"
	defaultWindowEnd    = "23:59"
	defaultMaxWaitMin   = 12.0
	defaultToleranceMin = 5.0

	// maxJitterRedraws 是"抖动越界后重抽"的次数上限，实际抽取次数是它 +1：
	// 先抽一次，越界才有"重抽"可谈。
	maxJitterRedraws = 8

	// clampJitterSeconds 是贴到窗口边界后的随机漂移上限（秒）。
	// 没有它，重抽失败的档位会永远精确卡在 21:00:00，比不抖动还假。
	clampJitterSeconds = 60.0
)

// mode / reason 是跨包、跨推送文案、跨运行记录共享的字符串字面量，
// 收敛成常量避免某一处手写拼错导致前端按 state 匹配不上。
const (
	modeRun  = "run"
	modeWait = "wait"
	modeSkip = "skip"

	reasonOK              = "ok"
	reasonClampedLow      = "clamped_low"
	reasonClampedHigh     = "clamped_high"
	reasonOutOfWindow     = "out_of_window"
	reasonOnTarget        = "on_target"
	reasonWaitUntilTarget = "wait_until_target"
	reasonWaitTooLong     = "wait_too_long"
	reasonLateFallback    = "late_fallback"
	reasonNoSlotFallback  = "no_slot_fallback"
)

// cn 是调度唯一的时间基准。宿主机时区（CI 上常是 UTC）绝不参与比较：
// 一旦让 time.Local 混进来，"21:00" 是几点就取决于跑在哪台机器上。
var cn = time.FixedZone("CST", 8*3600)

// refDay 承载 WindowStart / WindowEnd / Times 的日期部分。
// 这三个字段只取时分秒，给一个固定的非零日期是为了让零值 time.Time 继续表示"未配置"
// （InWindow 用零值判空），不至于把 00:00:00 当成合法窗口。
var refDay = time.Date(2000, 1, 1, 0, 0, 0, 0, cn)

// ============ 随机源 ============

// Source 是抖动所需的最小随机接口。刻意不用 *rand.Rand：
// 跨语言一致性要求两边消费同一串 u∈[0,1)，而 Go 的 ChaCha8 与 Python 的
// Mersenne Twister 永远抽不出同一串数，只能由调用方把 u 序列本身交进来。
type Source interface{ Float64() float64 }

// GlobalSource 返回生产用随机源（math/rand/v2 全局源，自带并发安全）。
func GlobalSource() Source { return globalSource{} }

type globalSource struct{}

func (globalSource) Float64() float64 { return rand.Float64() }

// scriptSource 循环消费一串 u，与 scripts/gen_parity.py 的 ScriptRNG 一一对应：
// 每被取用一格就前进一格，取到末尾回绕。游标用原子量，是为了让并发调用在 -race 下干净。
type scriptSource struct {
	us []float64
	n  atomic.Uint64
}

// NewScriptSource 用给定序列构造可重放的随机源（测试与一致性语料专用）。
func NewScriptSource(us []float64) Source {
	// 复制一份：调用方随后改自己的切片不该让已注入的序列漂移，
	// 否则"同一个 u 序列 → 同一个结果"的可重放性就没了。
	return &scriptSource{us: append([]float64(nil), us...)}
}

func (s *scriptSource) Float64() float64 {
	if len(s.us) == 0 {
		// 空序列没有"下一格"。取 0.5 而非 0：0 会把抖动 delta 固定拉到 -J
		// （等于每档都取下界），0.5 落在区间中点，最接近"没有抖动"的直觉。
		return 0.5
	}
	i := s.n.Add(1) - 1
	return s.us[i%uint64(len(s.us))]
}

// uniform 在 [low, high] 上取一个随机数。
// 式子必须与 Python 假 rng 的 uniform(low, high) = low + (high-low)*random() 完全一致：
// 两边只有对同一个 u 得到同一个值，语料才有意义。
func uniform(src Source, low, high float64) float64 {
	return low + (high-low)*src.Float64()
}

// ============ 时间基础设施 ============

// NowCN 返回带 +08:00 的当前时间：所有调度判断的唯一时间基准。
func NowCN() time.Time { return time.Now().In(cn) }

// timeOfDay 把时刻折算成"当日第几纳秒"。
// 窗口比较只关心时分秒，用当日偏移量比较可以彻底摆脱日期部分
// ——跨日窗口已明确不支持，日期在这里没有任何信息量。
func timeOfDay(t time.Time) time.Duration {
	d := t.In(cn)
	h, m, s := d.Clock()
	return time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second +
		time.Duration(d.Nanosecond())
}

// effectiveEnd 返回窗口终点的当日偏移，整分钟写法按该分钟最后一刻收尾。
//
// 为什么要延伸：配置里写 23:59 的直觉含义是"23:59:59 之前还来得及"。
// 若严格跟 23:59:00 比较，23:59:30 会被判越界，默认窗口就得写成 23:59:59——太反直觉。
func effectiveEnd(end time.Time) time.Duration {
	offset := timeOfDay(end)
	if end.In(cn).Second() == 0 && end.In(cn).Nanosecond() == 0 {
		return offset + 59*time.Second + 999999999
	}
	return offset
}

// onDay 把"时分秒"落到指定日期上（北京时间）。
// 档位时刻与窗口端点自身都不带日期，日期一律由调用方此刻的北京日期决定
// ——这就是"只支持同日窗口"在实现里的确切含义。
func onDay(day time.Time, clock time.Time) time.Time {
	d := day.In(cn)
	c := clock.In(cn)
	return time.Date(d.Year(), d.Month(), d.Day(), c.Hour(), c.Minute(), c.Second(), c.Nanosecond(), cn)
}

// minutesDuration 把"分钟数"折成 Duration。
// 用截断而非四舍五入：与 time.Duration 的整数语义一致，且与 Python
// timedelta 的微秒取整最多只差 1µs，远小于一致性测试的 5ms 容差。
func minutesDuration(minutes float64) time.Duration {
	return time.Duration(minutes * float64(time.Minute))
}

// hhmm 把时刻格式化成 "HH:MM"，只用于警告与错误文案。
func hhmm(t time.Time) string { return t.In(cn).Format("15:04") }

// ============ 配置解析 ============

// Settings 是 schedule 段规范化后的内部结构。
// 所有默认值只在这里落地：ResolveRun 之类函数拿到 Settings 就按字面执行，
// 于是"配置少一个键"绝不会变成某个调用点上的意外默认值。
type Settings struct {
	WindowStart, WindowEnd time.Time // 只取时分秒；约期日期无意义（固定 refDay）
	Times                  []time.Time
	JitterMinutes          float64
	MaxWaitMinutes         float64
	ToleranceMinutes       float64
	Warnings               []string // 降级提示：上层落日志/回显给前端，本包不打印
}

// parseClock 解析 "HH:MM" / "HH:MM:SS" → 时刻；非法或空返回 false。
//
// 容忍首尾空白与中文全角冒号「：」：手机复制粘贴配置时这两种脏字符最常见。
// 不返回 error 是刻意的——一档时刻写错只该丢掉这一档，不该让整次签到跑不起来。
func parseClock(text string) (time.Time, bool) {
	raw := strings.TrimSpace(strings.ReplaceAll(text, "：", ":"))
	if raw == "" {
		return time.Time{}, false
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return time.Time{}, false
	}
	// 逐段 TrimSpace：Python 的 int(" 35") 能过，Go 的 Atoi 不能，
	// 不对齐这一步会让 "21 : 35" 这类写法在两边结论不同。
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return time.Time{}, false
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return time.Time{}, false
	}
	second := 0
	if len(parts) == 3 {
		second, err = strconv.Atoi(strings.TrimSpace(parts[2]))
		if err != nil {
			return time.Time{}, false
		}
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return time.Time{}, false
	}
	return time.Date(refDay.Year(), refDay.Month(), refDay.Day(), hour, minute, second, 0, cn), true
}

// defaultTimes 是内置兜底档位。
// 走一遍 parseClock 而不是硬编码 time.Date：默认值本身若写错，测试会立刻发现，
// 否则"默认值也解析失败"这种事故只会在深夜静默漏签时暴露。
func defaultTimes() []time.Time {
	if t, ok := parseClock(defaultTime); ok {
		return []time.Time{t}
	}
	return []time.Time{time.Date(2000, 1, 1, 21, 35, 0, 0, cn)}
}

// parseClockOr 解析失败回退 def。
func parseClockOr(text string, def time.Time) time.Time {
	if t, ok := parseClock(text); ok {
		return t
	}
	return def
}

// parseTimes 规范化 times：坏值丢弃，全空/全坏回退默认档。
// 第二个返回值是坏项描述，第三个表示是否发生了"整段回退"——两者只用于生成警告。
func parseTimes(raw []string) (times []time.Time, dropped []string, fellBack bool) {
	times = make([]time.Time, 0, len(raw))
	for i, item := range raw {
		t, ok := parseClock(item)
		if !ok {
			dropped = append(dropped, fmt.Sprintf("times[%d]=%q", i, item))
			continue
		}
		times = append(times, t)
	}
	if len(times) == 0 {
		// 一天一档都不剩等于必然漏签，宁可退到默认那一档
		return defaultTimes(), dropped, true
	}
	return times, dropped, false
}

// minutesOption 解析一个可选的"分钟数"配置项，并在降级时给出可读原因。
// 为什么宽容：schedule 段是用户手写的配置，一个写错的数字不该让整次签到崩掉，
// 与 Python 版 _as_float 的取向一致。
func minutesOption(name string, p *float64, def float64) (value float64, warning string) {
	if p == nil {
		return def, ""
	}
	v := *p
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def, fmt.Sprintf("schedule.%s 不是有限数值，已按默认 %g 处理", name, def)
	}
	if v < 0 {
		// 负值等于"反向等待"，没有语义；按 0 处理（0 是合法且常用的显式配置）
		return 0, fmt.Sprintf("schedule.%s 为负值 %g，已按 0 处理", name, v)
	}
	return v, ""
}

// Parse 把配置规范化成 Settings；任何字段写错都退回默认值并记一条 Warning，绝不失败。
//
// 为什么集中在一处：默认值散落到各调用点，就会变成"配置少一个键 → 默认值不一致"。
func Parse(cfg model.ScheduleConfig) Settings {
	var warnings []string

	defaultStart := parseClockOr(defaultWindowStart, time.Date(2000, 1, 1, 21, 0, 0, 0, cn))
	defaultEnd := parseClockOr(defaultWindowEnd, time.Date(2000, 1, 1, 23, 59, 0, 0, cn))

	start := parseClockOr(cfg.WindowStart, defaultStart)
	end := parseClockOr(cfg.WindowEnd, defaultEnd)
	if timeOfDay(end) <= timeOfDay(start) {
		// 跨日窗口（如 23:00-01:00）会让"今天是哪一天"变得含混：22:30 和 00:30 到底算同一档
		// 还是两档？当前不支持。与其猜，不如整段回退默认窗口并明确告诉用户改哪里。
		warnings = append(warnings, fmt.Sprintf(
			"schedule 窗口配置无效：window_end %s 不晚于 window_start %s，"+
				"已回退默认窗口 %s-%s（暂不支持跨日窗口）",
			hhmm(end), hhmm(start), defaultWindowStart, defaultWindowEnd))
		start, end = defaultStart, defaultEnd
	}

	times, dropped, fellBack := parseTimes(cfg.Times)
	if len(dropped) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"schedule.times 有 %d 项不是 HH:MM[:SS]，已丢弃：%s",
			len(dropped), strings.Join(dropped, ", ")))
	}
	// 只有"用户写了 times 但一项都没解析出来"才值得警告。
	// times 整段缺失（最常见的安全默认配置）走兜底档是设计如此，报警只会变成噪音，
	// 让真正的降级提示被淹没。
	if fellBack && len(cfg.Times) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"schedule.times 无可解析档位，已回退默认档 %s", defaultTime))
	}

	jitter, w := minutesOption("time_jitter_minutes", cfg.TimeJitterMinutes, 0)
	if w != "" {
		warnings = append(warnings, w)
	}
	maxWait, w := minutesOption("max_wait_minutes", cfg.MaxWaitMinutes, defaultMaxWaitMin)
	if w != "" {
		warnings = append(warnings, w)
	}
	tolerance, w := minutesOption("tolerance_minutes", cfg.ToleranceMinutes, defaultToleranceMin)
	if w != "" {
		warnings = append(warnings, w)
	}

	return Settings{
		WindowStart:      start,
		WindowEnd:        end,
		Times:            times,
		JitterMinutes:    jitter,
		MaxWaitMinutes:   maxWait,
		ToleranceMinutes: tolerance,
		Warnings:         warnings,
	}
}

// ============ 窗口判定 ============

// InWindow 判断 t 的时分秒是否落在 [start, end] 内（含端点，end 整分钟按 :59.999 收尾）。
//
// 只比时刻不比日期：窗口是同日的，日期由 PlanSlots 统一负责。
// 零值视为"未配置"并直接返回 false——零值 time.Time 折到北京时区是 08:00，
// 若不拦，一个"没赋值的 now"会被判成在窗口内，从而在错误的时间触发签到。
func InWindow(t time.Time, start, end time.Time) bool {
	if t.IsZero() || start.IsZero() || end.IsZero() {
		return false
	}
	moment := timeOfDay(t)
	return timeOfDay(start) <= moment && moment <= effectiveEnd(end)
}

// VerifyInWindow 是执行前的二次校验：在窗口内返回 nil，越界返回 error 说明差在哪。
//
// 为什么留在本包：抖动与等待都可能把执行点推过边界，"是否允许签"必须只有一条判定路径，
// 否则迟早出现"规划说能签、执行说不能签"的两套结论。
func VerifyInWindow(t time.Time, start, end time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("打卡时间为空，无法校验签到窗口")
	}
	if !InWindow(t, start, end) {
		return fmt.Errorf("当前时间 %s 不在签到窗口 %s-%s 内",
			t.In(cn).Format("2006-01-02 15:04:05"), hhmmOrUnknown(start), hhmmOrUnknown(end))
	}
	return nil
}

// hhmmOrUnknown 只在错误文案里用：窗口端点没配置时给出 "?" 而不是 08:00。
func hhmmOrUnknown(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return hhmm(t)
}

// ============ 档位规划 ============

// Slot 是一个"配置时刻"折算出的最终执行点。
// Configured 与 Target 分开存，是为了在日志/推送里能说清"配的是 21:35、实际按 21:41 签"，
// 出问题时不必回头猜抖动抽到了多少。
type Slot struct {
	Configured time.Time // 配置原始时刻（今天的日期 + 配置时分）
	Target     time.Time // 抖动 + 钳制后的目标时刻
	Valid      bool      // 配置时刻本身是否在窗口内
	Jittered   bool      // 是否发生过时间抖动
	Clamped    bool      // 抖动越界后被贴到窗口边界
	Reason     string    // ok|clamped_low|clamped_high|out_of_window
}

// PlanSlots 算出当天所有配置时刻的最终执行点（抖动 → 重抽 → 贴边 → 夹紧）。
//
// 三级降级的原因：抖动是为了让签到看起来像手动点的，但绝不能因为抖动把人抖到窗口外
// 而漏签。所以越界先重抽；抽不回来就贴到窗口边界（再随机漂移几十秒，避免每天都卡同一秒）；
// 最后再夹一次，保证 Valid 档位的 Target 恒在窗口内。
//
// 返回按 Target 升序的切片，方便调用方直接取"最近一档"。
func PlanSlots(s Settings, now time.Time, src Source) []Slot {
	if src == nil {
		src = GlobalSource()
	}
	now = now.In(cn)
	start, end := s.WindowStart, s.WindowEnd
	jitter := s.JitterMinutes
	if jitter < 0 {
		jitter = 0
	}
	times := s.Times
	if len(times) == 0 {
		// 调用方直接构造 Settings（跳过 Parse）时也得有档位，否则当天一档都没有
		times = defaultTimes()
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cn)
	startEdge := midnight.Add(timeOfDay(start))
	endEdge := midnight.Add(effectiveEnd(end))

	slots := make([]Slot, 0, len(times))
	for _, configuredTime := range times {
		base := onDay(now, configuredTime)

		// 档位本身就在窗口外：不做任何抖动，交给 ResolveRun 走 no_slot_fallback。
		// 顺序不能反：先判窗口再抖动，否则会为一个注定不用的时刻白抽随机数，
		// 而随机序列一旦多消费一格，跨实现一致性就全盘错位。
		if !InWindow(base, start, end) {
			slots = append(slots, Slot{
				Configured: base,
				Target:     base,
				Valid:      false,
				Reason:     reasonOutOfWindow,
			})
			continue
		}

		if jitter <= 0 {
			slots = append(slots, Slot{Configured: base, Target: base, Valid: true, Reason: reasonOK})
			continue
		}

		var target time.Time
		found := false
		last := base
		for i := 0; i <= maxJitterRedraws; i++ {
			delta := uniform(src, -jitter, jitter)
			candidate := base.Add(minutesDuration(delta))
			if InWindow(candidate, start, end) {
				target = candidate
				found = true
				break
			}
			last = candidate
		}
		if found {
			slots = append(slots, Slot{
				Configured: base,
				Target:     target,
				Valid:      true,
				Jittered:   true,
				Reason:     reasonOK,
			})
			continue
		}

		// 重抽用完仍越界 → 贴边。用完整日期时间而不是时分秒判断偏了哪一侧，
		// 这样即使候选滚到次日凌晨（"看起来"比窗口起点早）也能判对方向。
		var reason string
		if last.Before(startEdge) {
			reason = reasonClampedLow
			target = startEdge.Add(time.Duration(uniform(src, 0, clampJitterSeconds) * float64(time.Second)))
		} else {
			reason = reasonClampedHigh
			target = endEdge.Add(-time.Duration(uniform(src, 0, clampJitterSeconds) * float64(time.Second)))
		}
		// 最后一道保险：窗口极窄（如 21:00-21:00）时边界 ±60s 的漂移仍可能顶出窗口，
		// 而"已钳制"的档位一旦越界就等于漏签。
		if target.Before(startEdge) {
			target = startEdge
		}
		if target.After(endEdge) {
			target = endEdge
		}
		slots = append(slots, Slot{
			Configured: base,
			Target:     target,
			Valid:      true,
			Jittered:   true,
			Clamped:    true,
			Reason:     reason,
		})
	}

	// 稳定排序：Target 相同的两档必须保持配置顺序，否则同一份配置在不同运行里
	// 可能挑出不同的"最近一档"，历史记录就不可比了。
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Target.Before(slots[j].Target) })
	return slots
}

// ============ 决策 ============

// Decision 是一次"签 / 等 / 跳过"的结论。上层只按这个结构分支，不再自己算时间。
type Decision struct {
	Mode        string    // run|wait|skip
	Slot        *Slot     // 命中的档位；无档命中（窗口外/兜底）时为 nil
	Target      time.Time // 执行时刻；skip/out_of_window 时为零值
	WaitSeconds float64   // mode==wait 时有效，>=0
	Reason      string    // on_target|wait_until_target|wait_too_long|late_fallback|no_slot_fallback|out_of_window|force:...
}

// touchedWindow 判断某档是否"刚到点、还在容忍范围内"。
// 用浮点秒而不是 Duration 比较：Python 侧就是 (now-target).total_seconds() <= tol，
// 折成纳秒会引入截断，让"刚好等于上界"的档位在两边结论不同。
func touchedWindow(slot Slot, now time.Time, toleranceSeconds float64) bool {
	if slot.Target.After(now) {
		return false
	}
	return now.Sub(slot.Target).Seconds() <= toleranceSeconds
}

// guardedTarget 兜最后一道：窗口外的目标时刻换成"现在就签"。
//
// 上游传入手工构造的 slots 时可能带窗口外 target（例如抖动后被改了窗口配置），
// 把越界时刻交给执行路径就是必然漏签；宁可立刻签，也不要交出一个不可能的等待点。
func guardedTarget(target, now, start, end time.Time) time.Time {
	if !target.IsZero() && InWindow(target, start, end) {
		return target
	}
	return now
}

// ResolveRun 决定"现在签 / 等一会儿 / 放弃"，六个分支的顺序即优先级，不可重排：
//
//  1. 窗口外什么都不做——登录、推送都留到窗口内，避免延迟触发刷出误报；
//  2. 刚过点且仍在 tolerance 内立刻签——否则"下一档还早但等不起"会把一次本来就
//     准点的机会挤成 skip（times=[21:35,21:50]、21:37 到达就是这种坑）；
//  3. 有未来档先看等不等得起（max_wait 是"这次唤醒值不值得"的上限）；
//  4. 等不起就明确 skip，别把一个不可能的等待塞给调度循环；
//  5. 没有未来档时全部过期 → 补签（宁可迟到也不漏签）；
//  6. 一档可用的都没有 → 窗口内兜底签一次。
//
// slots 传入外部已规划好的档位：等待后重判时必须传回来，否则 PlanSlots 会重新抽一次
// 抖动，第二次的目标可能比刚才等的那个晚出 max_wait，这一轮唤醒就白等了。
// slots 为 nil 时才现规划（并消费 src 的随机序列）。
func ResolveRun(s Settings, now time.Time, src Source, slots []Slot) Decision {
	now = now.In(cn)
	start, end := s.WindowStart, s.WindowEnd

	if !InWindow(now, start, end) {
		return Decision{Mode: modeSkip, Reason: reasonOutOfWindow}
	}
	if slots == nil {
		slots = PlanSlots(s, now, src)
	}

	// 用指针指向入参切片元素：Decision.Slot 与调用方手里的那一档是同一份数据，
	// 上层拿它写运行记录时不会看到两份互相矛盾的副本。
	usable := make([]*Slot, 0, len(slots))
	for i := range slots {
		if slots[i].Valid {
			usable = append(usable, &slots[i])
		}
	}
	if len(usable) == 0 {
		// 档位全配在窗口外或解析失败，而此刻就在窗口内：兜底签一次也比空手而归强
		return Decision{Mode: modeRun, Target: now, Reason: reasonNoSlotFallback}
	}

	maxWait := s.MaxWaitMinutes * 60.0
	tolerance := s.ToleranceMinutes * 60.0

	var touched *Slot
	for _, slot := range usable {
		if !touchedWindow(*slot, now, tolerance) {
			continue
		}
		// 取最近（最晚）那一档：两档都还在容忍范围内时，签字该照最接近现在的那个
		if touched == nil || slot.Target.After(touched.Target) {
			touched = slot
		}
	}
	if touched != nil {
		return Decision{
			Mode:   modeRun,
			Slot:   touched,
			Target: guardedTarget(touched.Target, now, start, end),
			Reason: reasonOnTarget,
		}
	}

	var next *Slot
	for _, slot := range usable {
		if slot.Target.Before(now) {
			continue
		}
		if next == nil || slot.Target.Before(next.Target) {
			next = slot
		}
	}
	if next != nil {
		waitSeconds := next.Target.Sub(now).Seconds()
		target := guardedTarget(next.Target, now, start, end)
		if waitSeconds <= maxWait {
			// wait_seconds 不做上限截断：是否等待完全由上面那条 max_wait 判定决定，
			// 截断会让上层误以为"只等 12 分钟"而实际是 40 分钟。
			return Decision{
				Mode:        modeWait,
				Slot:        next,
				Target:      target,
				WaitSeconds: waitSeconds,
				Reason:      reasonWaitUntilTarget,
			}
		}
		return Decision{Mode: modeSkip, Slot: next, Target: target, Reason: reasonWaitTooLong}
	}

	latest := usable[0]
	for _, slot := range usable {
		if slot.Target.After(latest.Target) {
			latest = slot
		}
	}
	return Decision{
		Mode:   modeRun,
		Slot:   latest,
		Target: guardedTarget(latest.Target, now, start, end),
		Reason: reasonLateFallback,
	}
}

// ============ 对外文案 ============

// reasonText 把 reason 枚举翻成给人看的一句话。
// 不直接回显枚举值：日志与推送是给人看的，out_of_window 这种串只有写代码的人认得。
func reasonText(reason string) string {
	// 手动补签的 reason 是 "force:<原始原因>"，前缀原样保留以便追溯
	if strings.HasPrefix(reason, "force:") {
		return "手动强制 · " + reasonText(strings.TrimPrefix(reason, "force:"))
	}
	switch reason {
	case reasonOnTarget:
		return "已到点"
	case reasonWaitUntilTarget:
		return "等到目标时刻"
	case reasonWaitTooLong:
		return "等待超过上限"
	case reasonLateFallback:
		return "已过末档，迟到补签"
	case reasonNoSlotFallback:
		return "无有效档位，窗口内兜底"
	case reasonOutOfWindow:
		return "不在签到窗口内"
	}
	return reason
}

// modeText 把 mode 枚举翻成动作。
func modeText(mode string) string {
	switch mode {
	case modeRun:
		return "签到"
	case modeWait:
		return "等待"
	case modeSkip:
		return "跳过"
	}
	return mode
}

// slotText 说明"配的是几点、为什么实际不是那一刻"。
func slotText(slot *Slot) string {
	if slot == nil {
		return ""
	}
	parts := []string{"配置 " + slot.Configured.In(cn).Format("15:04")}
	switch {
	case slot.Clamped:
		parts = append(parts, "抖动越界已贴边")
	case slot.Jittered:
		parts = append(parts, "时间抖动")
	}
	if !slot.Valid {
		parts = append(parts, "档位在窗口外")
	}
	return "（" + strings.Join(parts, " + ") + "）"
}

// DecisionText 把决策压成一行可读文案，供日志与推送复用。
// 形如：等待（等到目标时刻） 目标 21:35:12（配置 21:35 + 时间抖动），还有 30 秒。
func DecisionText(d Decision) string {
	var b strings.Builder
	b.WriteString(modeText(d.Mode))
	b.WriteString("（")
	b.WriteString(reasonText(d.Reason))
	b.WriteString("）")
	if !d.Target.IsZero() {
		b.WriteString(" 目标 ")
		b.WriteString(d.Target.In(cn).Format("15:04:05"))
		b.WriteString(slotText(d.Slot))
	}
	if d.WaitSeconds > 0 {
		// 向上取整：剩余 0.4 秒显示成"还有 0 秒"会让人以为已经到点却没动作
		fmt.Fprintf(&b, "，还有 %d 秒", int64(math.Ceil(d.WaitSeconds)))
	}
	return b.String()
}

// NextRunAt 返回仪表盘倒计时用的下一档时刻；没有未来档时返回零值 time.Time{}。
//
// 只认 Target > now（严格大于）：正好等于 now 的档位属于"已到点"，由 ResolveRun
// 走 on_target 处理，倒计时再把它算成"未来"会让前端显示一个永久 0 秒。
func NextRunAt(s Settings, slots []Slot, now time.Time) time.Time {
	now = now.In(cn)
	// PlanSlots 已按 Target 升序返回，因此"第一个晚于 now 的档"就是最近一档
	for _, slot := range slots {
		if slot.Target.After(now) {
			return slot.Target.In(cn)
		}
	}
	return time.Time{}
}
