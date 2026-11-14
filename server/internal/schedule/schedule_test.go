package schedule

// 表驱动单元测试：覆盖窗口端点、六条决策分支、抖动重抽与贴边、
// 空/非法 times 回退、跨日窗口回退、end_eff 收尾、NextRunAt 无未来档。
// 跨实现一致性另见 parity_test.go。

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// at 构造"今天"（2026-11-14）的某个北京时刻，让表驱动用例读起来像配置。
func at(h, mi, s int) time.Time {
	return time.Date(2026, 11, 14, h, mi, s, 0, cn)
}

// atNano 同上但带纳秒，专用于验证 end_eff 的 59.999999999 收尾。
func atNano(h, mi, s, nanos int) time.Time {
	return time.Date(2026, 11, 14, h, mi, s, nanos, cn)
}

// nextDay 是窗口判定里的关键边界：次日 00:00:00。
func nextDay(h, mi, s int) time.Time {
	return time.Date(2026, 11, 15, h, mi, s, 0, cn)
}

// clock 只取时分秒，便于断言时刻而不被日期干扰。
func clock(t time.Time) string { return t.In(cn).Format("15:04:05") }

// fp 给 *float64 配置项取值。
func fp(v float64) *float64 { return &v }

// slotAt 造一个无抖动档位；valid=false 表示"档位本身在窗口外"。
func slotAt(h, mi, s int, valid bool) Slot {
	t := at(h, mi, s)
	return Slot{Configured: t, Target: t, Valid: valid, Reason: reasonOK}
}

// ============ Parse ============

func TestParseDefaults(t *testing.T) {
	got := Parse(model.ScheduleConfig{})
	if w := clock(got.WindowStart); w != "21:00:00" {
		t.Errorf("默认窗口起点: got=%q want=%q", w, "21:00:00")
	}
	if w := clock(got.WindowEnd); w != "23:59:00" {
		t.Errorf("默认窗口终点: got=%q want=%q", w, "23:59:00")
	}
	if len(got.Times) != 1 || clock(got.Times[0]) != "21:35:00" {
		t.Errorf("默认档位: len=%d clocks=%v", len(got.Times), got.Times)
	}
	if got.JitterMinutes != 0 {
		t.Errorf("默认抖动: got=%v want=0", got.JitterMinutes)
	}
	if got.MaxWaitMinutes != 12 {
		t.Errorf("默认最大等待: got=%v want=12", got.MaxWaitMinutes)
	}
	if got.ToleranceMinutes != 5 {
		t.Errorf("默认容忍: got=%v want=5", got.ToleranceMinutes)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("默认配置不该产生警告: %v", got.Warnings)
	}
}

func TestParseTimes(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		want     []string
		minWarns int
	}{
		{"标准", []string{"21:35"}, []string{"21:35:00"}, 0},
		{"带秒", []string{"21:35:59"}, []string{"21:35:59"}, 0},
		{"首尾空白", []string{" 21:35 "}, []string{"21:35:00"}, 0},
		{"中文全角冒号", []string{"21：35"}, []string{"21:35:00"}, 0},
		{"多档", []string{"21:35", "22:10"}, []string{"21:35:00", "22:10:00"}, 0},
		{"半角空白分隔", []string{"21 : 35"}, []string{"21:35:00"}, 0},
		{"坏值丢弃", []string{"abc", "21:50"}, []string{"21:50:00"}, 1},
		{"越界小时丢弃", []string{"24:00", "21:50"}, []string{"21:50:00"}, 1},
		{"越界分钟丢弃", []string{"21:60", "21:50"}, []string{"21:50:00"}, 1},
		{"越界秒丢弃", []string{"21:50:60", "21:50"}, []string{"21:50:00"}, 1},
		{"空串丢弃", []string{"", "21:50"}, []string{"21:50:00"}, 1},
		{"段数过多丢弃", []string{"21:50:00:00", "21:50"}, []string{"21:50:00"}, 1},
		{"全坏回退默认", []string{"abc", "99:99"}, []string{"21:35:00"}, 2},
		{"空列表回退默认（不算降级）", []string{}, []string{"21:35:00"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(model.ScheduleConfig{Times: c.in})
			if len(got.Times) != len(c.want) {
				t.Fatalf("档位数量: got=%d want=%d (%v)", len(got.Times), len(c.want), got.Times)
			}
			for i, want := range c.want {
				if got := clock(got.Times[i]); got != want {
					t.Errorf("times[%d]: got=%q want=%q", i, got, want)
				}
			}
			if len(got.Warnings) < c.minWarns {
				t.Errorf("警告条数: got=%d want>=%d (%v)", len(got.Warnings), c.minWarns, got.Warnings)
			}
			if c.minWarns == 0 && len(got.Warnings) != 0 {
				t.Errorf("不该有警告: %v", got.Warnings)
			}
		})
	}
}

func TestParseWindow(t *testing.T) {
	cases := []struct {
		name               string
		start, end         string
		wantStart, wantEnd string
		warnSubstr         string
	}{
		{"默认", "", "", "21:00:00", "23:59:00", ""},
		{"自定义", "19:00", "20:00", "19:00:00", "20:00:00", ""},
		{"端点带秒", "21:00:30", "21:00:59", "21:00:30", "21:00:59", ""},
		{"起点坏值", "abc", "22:00", "21:00:00", "22:00:00", ""},
		{"终点坏值", "20:00", "abc", "20:00:00", "23:59:00", ""},
		{"跨日回退", "23:00", "01:00", "21:00:00", "23:59:00", "跨日"},
		{"相等回退", "21:00", "21:00", "21:00:00", "23:59:00", "不晚于"},
		{"终点早于起点回退", "22:00", "21:00", "21:00:00", "23:59:00", "不晚于"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(model.ScheduleConfig{WindowStart: c.start, WindowEnd: c.end})
			if g := clock(got.WindowStart); g != c.wantStart {
				t.Errorf("window_start: got=%q want=%q", g, c.wantStart)
			}
			if g := clock(got.WindowEnd); g != c.wantEnd {
				t.Errorf("window_end: got=%q want=%q", g, c.wantEnd)
			}
			if c.warnSubstr == "" {
				if len(got.Warnings) != 0 {
					t.Errorf("不该有警告: %v", got.Warnings)
				}
				return
			}
			if len(got.Warnings) == 0 {
				t.Fatalf("缺少降级警告，期望包含 %q", c.warnSubstr)
			}
			if !strings.Contains(strings.Join(got.Warnings, " | "), c.warnSubstr) {
				t.Errorf("警告未提到 %q: %v", c.warnSubstr, got.Warnings)
			}
		})
	}
}

func TestParseMinutes(t *testing.T) {
	cases := []struct {
		name       string
		cfg        model.ScheduleConfig
		wantJitter float64
		wantMax    float64
		wantTol    float64
		wantWarns  int
	}{
		{"全部缺省", model.ScheduleConfig{}, 0, 12, 5, 0},
		{"正常值", model.ScheduleConfig{
			TimeJitterMinutes: fp(4.5), MaxWaitMinutes: fp(30), ToleranceMinutes: fp(2.5),
		}, 4.5, 30, 2.5, 0},
		{"显式零等待合法", model.ScheduleConfig{MaxWaitMinutes: fp(0)}, 0, 0, 5, 0},
		{"负抖动按 0", model.ScheduleConfig{TimeJitterMinutes: fp(-5)}, 0, 12, 5, 1},
		{"NaN 抖动按默认", model.ScheduleConfig{TimeJitterMinutes: fp(math.NaN())}, 0, 12, 5, 1},
		{"+Inf 容差按默认", model.ScheduleConfig{ToleranceMinutes: fp(math.Inf(1))}, 0, 12, 5, 1},
		{"负等待按 0", model.ScheduleConfig{MaxWaitMinutes: fp(-1)}, 0, 0, 5, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.cfg)
			if got.JitterMinutes != c.wantJitter {
				t.Errorf("jitter: got=%v want=%v", got.JitterMinutes, c.wantJitter)
			}
			if got.MaxWaitMinutes != c.wantMax {
				t.Errorf("max_wait: got=%v want=%v", got.MaxWaitMinutes, c.wantMax)
			}
			if got.ToleranceMinutes != c.wantTol {
				t.Errorf("tolerance: got=%v want=%v", got.ToleranceMinutes, c.wantTol)
			}
			if len(got.Warnings) != c.wantWarns {
				t.Errorf("警告条数: got=%d want=%d (%v)", len(got.Warnings), c.wantWarns, got.Warnings)
			}
		})
	}
}

// ============ InWindow / VerifyInWindow ============

func TestInWindow(t *testing.T) {
	s := Parse(model.ScheduleConfig{})
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"窗口前一秒", at(20, 59, 59), false},
		{"窗口起点", at(21, 0, 0), true},
		{"窗口中段", at(21, 59, 59), true},
		{"终点整分钟", at(23, 59, 0), true},
		{"终点最后一秒", at(23, 59, 59), true},
		{"终点纳秒收尾", atNano(23, 59, 59, 999999999), true},
		{"次日零点失效", nextDay(0, 0, 0), false},
		{"午后", at(12, 0, 0), false},
		{"日期不影响判定", time.Date(2027, 3, 5, 21, 30, 0, 0, cn), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := InWindow(c.at, s.WindowStart, s.WindowEnd); got != c.want {
				t.Errorf("InWindow(%s): got=%v want=%v", clock(c.at), got, c.want)
			}
		})
	}
}

func TestInWindowEndWithSeconds(t *testing.T) {
	// 终点秒位非 0 时不做整分钟延伸：22:30:30 就是硬边界
	s := Parse(model.ScheduleConfig{WindowStart: "21:00", WindowEnd: "22:30:30"})
	if !InWindow(at(22, 30, 30), s.WindowStart, s.WindowEnd) {
		t.Error("22:30:30 应仍在窗口内（含端点）")
	}
	if InWindow(at(22, 30, 31), s.WindowStart, s.WindowEnd) {
		t.Error("22:30:31 已越界")
	}
}

func TestInWindowZeroValues(t *testing.T) {
	// 零值表示"未配置"：绝不能因为零值折算成北京 08:00 就被判进窗口
	s := Parse(model.ScheduleConfig{})
	if InWindow(time.Time{}, s.WindowStart, s.WindowEnd) {
		t.Error("零值时刻不该判为在窗口内")
	}
	if InWindow(at(21, 30, 0), time.Time{}, s.WindowEnd) {
		t.Error("窗口起点未配置时不该判为在窗口内")
	}
	if InWindow(at(21, 30, 0), s.WindowStart, time.Time{}) {
		t.Error("窗口终点未配置时不该判为在窗口内")
	}
}

func TestVerifyInWindow(t *testing.T) {
	s := Parse(model.ScheduleConfig{})
	if err := VerifyInWindow(at(21, 30, 0), s.WindowStart, s.WindowEnd); err != nil {
		t.Errorf("窗口内不该报错: %v", err)
	}
	err := VerifyInWindow(at(20, 0, 0), s.WindowStart, s.WindowEnd)
	if err == nil {
		t.Fatal("窗口外应报错")
	}
	if msg := err.Error(); !strings.Contains(msg, "不在签到窗口") || !strings.Contains(msg, "21:00-23:59") {
		t.Errorf("错误信息不可定位: %q", msg)
	}
	if msg := VerifyInWindow(time.Time{}, s.WindowStart, s.WindowEnd).Error(); !strings.Contains(msg, "打卡时间为空") {
		t.Errorf("空时刻的错误信息不对: %q", msg)
	}
	// 窗口端点本身未配置时，文案里用 ? 表示，不能伪造成 08:00
	if msg := VerifyInWindow(at(21, 30, 0), at(21, 0, 0), time.Time{}).Error(); !strings.Contains(msg, "?") {
		t.Errorf("未配置的终点应显示为 ?: %q", msg)
	}
}

// ============ PlanSlots ============

func TestPlanSlotsNoJitter(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35", "21:50"}})
	slots := PlanSlots(s, at(21, 0, 0), NewScriptSource([]float64{0.5}))
	if len(slots) != 2 {
		t.Fatalf("档位数量: got=%d want=2", len(slots))
	}
	if !slots[0].Target.Before(slots[1].Target) {
		t.Errorf("必须按 Target 升序返回: %v %v", slots[0].Target, slots[1].Target)
	}
	for i, slot := range slots {
		if !slot.Valid {
			t.Errorf("slots[%d] 应在窗口内", i)
		}
		if slot.Jittered || slot.Clamped {
			t.Errorf("slots[%d] 不该抖动/钳制: %+v", i, slot)
		}
		if slot.Reason != reasonOK {
			t.Errorf("slots[%d].reason: got=%q want=%q", i, slot.Reason, reasonOK)
		}
		if !slot.Target.Equal(slot.Configured) {
			t.Errorf("slots[%d]: 无抖动时 target 应等于 configured", i)
		}
	}
	if got := clock(slots[0].Target); got != "21:35:00" {
		t.Errorf("第一档: got=%q want=%q", got, "21:35:00")
	}
}

func TestPlanSlotsOutOfWindow(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"20:30"}})
	src := &countingSource{inner: NewScriptSource([]float64{0.5})}
	slots := PlanSlots(s, at(21, 0, 0), src)
	// 先判窗口再抖动：窗口外的档位一格随机数都不该消费，
	// 否则随机序列错位，后面每一档的时刻都会跟 Python 版对不上
	if src.n != 0 {
		t.Errorf("窗口外档位不该消费随机数: got=%d want=0", src.n)
	}
	if len(slots) != 1 {
		t.Fatalf("档位数量: got=%d want=1", len(slots))
	}
	slot := slots[0]
	if slot.Valid || slot.Jittered || slot.Clamped {
		t.Errorf("窗口外档位不该有效/抖动/钳制: %+v", slot)
	}
	if slot.Reason != reasonOutOfWindow {
		t.Errorf("reason: got=%q want=%q", slot.Reason, reasonOutOfWindow)
	}
	if !slot.Target.Equal(slot.Configured) {
		t.Error("窗口外档位的 target 应保持配置时刻")
	}
}

func TestPlanSlotsJitterWithinWindow(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}, TimeJitterMinutes: fp(5)})
	src := &countingSource{inner: NewScriptSource([]float64{0.5})}
	slots := PlanSlots(s, at(21, 0, 0), src)
	// u=0.5 → delta = -5 + 10*0.5 = 0，目标正好落在配置时刻
	if src.n != 1 {
		t.Errorf("随机数消费: got=%d want=1", src.n)
	}
	if len(slots) != 1 {
		t.Fatalf("档位数量: got=%d want=1", len(slots))
	}
	slot := slots[0]
	if !slot.Valid || !slot.Jittered || slot.Clamped {
		t.Errorf("应为有效且仅抖动: %+v", slot)
	}
	if slot.Reason != reasonOK {
		t.Errorf("reason: got=%q want=%q", slot.Reason, reasonOK)
	}
	if !slot.Target.Equal(slot.Configured) {
		t.Errorf("delta=0 时 target 应等于 configured: %v vs %v", slot.Target, slot.Configured)
	}
}

func TestPlanSlotsJitterRedraw(t *testing.T) {
	// 窗口 21:30-21:40、档位 21:35、抖动 ±30 分钟：
	// 第一格 u=0 → delta=-30 分钟 → 21:05 越界，必须重抽；第二格 u=0.5 → 回到 21:35
	s := Parse(model.ScheduleConfig{
		WindowStart: "21:30", WindowEnd: "21:40",
		Times: []string{"21:35"}, TimeJitterMinutes: fp(30),
	})
	src := &countingSource{inner: NewScriptSource([]float64{0, 0.5})}
	slots := PlanSlots(s, at(21, 0, 0), src)
	if src.n != 2 {
		t.Errorf("随机数消费（1 次越界 + 1 次命中）: got=%d want=2", src.n)
	}
	slot := slots[0]
	if !slot.Jittered || slot.Clamped {
		t.Errorf("重抽命中不该算钳制: %+v", slot)
	}
	if slot.Reason != reasonOK {
		t.Errorf("reason: got=%q want=%q", slot.Reason, reasonOK)
	}
	if got := clock(slot.Target); got != "21:35:00" {
		t.Errorf("target: got=%q want=%q", got, "21:35:00")
	}
}

func TestPlanSlotsClampLow(t *testing.T) {
	// 档位贴在窗口起点、抖动远大于窗口余量：9 次抽取全部越界 → 贴到起点
	s := Parse(model.ScheduleConfig{Times: []string{"21:00"}, TimeJitterMinutes: fp(30)})
	src := &countingSource{inner: NewScriptSource([]float64{0})} // u=0 → delta 恒为 -30 分钟
	slots := PlanSlots(s, at(21, 0, 0), src)
	// maxJitterRedraws+1 次重抽 + 1 次贴边漂移
	if want := maxJitterRedraws + 2; src.n != want {
		t.Errorf("随机数消费: got=%d want=%d", src.n, want)
	}
	slot := slots[0]
	if !slot.Valid || !slot.Jittered || !slot.Clamped {
		t.Errorf("应有效 + 抖动 + 钳制: %+v", slot)
	}
	if slot.Reason != reasonClampedLow {
		t.Errorf("reason: got=%q want=%q", slot.Reason, reasonClampedLow)
	}
	// u=0 → 漂移 0 秒 → 恰好落在窗口起点
	if !slot.Target.Equal(at(21, 0, 0)) {
		t.Errorf("target: got=%s want=%s", clock(slot.Target), "21:00:00")
	}
	if !InWindow(slot.Target, s.WindowStart, s.WindowEnd) {
		t.Error("钳制后必须仍在窗口内")
	}
}

func TestPlanSlotsClampLowDrift(t *testing.T) {
	// 贴边后仍要随机漂移几十秒：每天都精确卡在 21:00:00 比不抖动更假
	s := Parse(model.ScheduleConfig{Times: []string{"21:00"}, TimeJitterMinutes: fp(30)})
	us := append(make([]float64, maxJitterRedraws+1), 0.5) // 前 9 格全越界，第 10 格漂移 30 秒
	src := &countingSource{inner: NewScriptSource(us)}
	slots := PlanSlots(s, at(21, 0, 0), src)
	slot := slots[0]
	if slot.Reason != reasonClampedLow {
		t.Fatalf("reason: got=%q want=%q", slot.Reason, reasonClampedLow)
	}
	if got := clock(slot.Target); got != "21:00:30" {
		t.Errorf("target: got=%q want=%q", got, "21:00:30")
	}
}

func TestPlanSlotsClampHigh(t *testing.T) {
	// 档位贴在窗口终点、抖动 ±30 分钟：正向抽取全部滚到次日凌晨 → 贴到终点内侧
	s := Parse(model.ScheduleConfig{Times: []string{"23:59"}, TimeJitterMinutes: fp(30)})
	src := &countingSource{inner: NewScriptSource([]float64{0.99})} // delta 恒约 +29.4 分钟
	slots := PlanSlots(s, at(21, 0, 0), src)
	if want := maxJitterRedraws + 2; src.n != want {
		t.Errorf("随机数消费: got=%d want=%d", src.n, want)
	}
	slot := slots[0]
	if !slot.Valid || !slot.Jittered || !slot.Clamped {
		t.Errorf("应有效 + 抖动 + 钳制: %+v", slot)
	}
	if slot.Reason != reasonClampedHigh {
		t.Errorf("reason: got=%q want=%q", slot.Reason, reasonClampedHigh)
	}
	if slot.Target.After(atNano(23, 59, 59, 999999999)) {
		t.Errorf("target 越过了 end_eff: %v", slot.Target)
	}
	if !InWindow(slot.Target, s.WindowStart, s.WindowEnd) {
		t.Error("钳制后必须仍在窗口内")
	}
	// u=0.99 → 漂移 59.4 秒 → 23:59:00.6 左右
	if got := clock(slot.Target); got != "23:59:00" {
		t.Errorf("target: got=%q want=%q", got, "23:59:00")
	}
}

func TestPlanSlotsNarrowWindowClampStaysInWindow(t *testing.T) {
	// 窗口只有 30 秒：贴边漂移最多 60 秒，会把目标顶到窗口起点之前，
	// 最后那道夹紧必须拉回来——"已钳制"的档位反而越界就等于漏签
	s := Settings{
		WindowStart:      at(21, 0, 0),
		WindowEnd:        at(21, 0, 30),
		Times:            []time.Time{at(21, 0, 0)},
		JitterMinutes:    30,
		MaxWaitMinutes:   12,
		ToleranceMinutes: 5,
	}
	src := &countingSource{inner: NewScriptSource([]float64{0.99})}
	slots := PlanSlots(s, at(21, 0, 0), src)
	slot := slots[0]
	if slot.Reason != reasonClampedHigh {
		t.Fatalf("reason: got=%q want=%q", slot.Reason, reasonClampedHigh)
	}
	if !slot.Target.Equal(at(21, 0, 0)) {
		t.Errorf("应被夹回窗口起点: got=%s", clock(slot.Target))
	}
}

func TestPlanSlotsSorted(t *testing.T) {
	// 配置顺序不决定执行顺序
	s := Parse(model.ScheduleConfig{Times: []string{"22:10", "21:35"}})
	slots := PlanSlots(s, at(21, 0, 0), nil)
	if len(slots) != 2 {
		t.Fatalf("档位数量: got=%d want=2", len(slots))
	}
	if !slots[0].Target.Before(slots[1].Target) {
		t.Fatalf("必须升序: %s %s", clock(slots[0].Target), clock(slots[1].Target))
	}
	if got := clock(slots[0].Target); got != "21:35:00" {
		t.Errorf("slots[0]: got=%q want=%q", got, "21:35:00")
	}
	if got := clock(slots[1].Target); got != "22:10:00" {
		t.Errorf("slots[1]: got=%q want=%q", got, "22:10:00")
	}
}

func TestPlanSlotsEmptyTimesUsesDefault(t *testing.T) {
	// 调用方直接构造 Settings（跳过 Parse）时也得有档位，否则当天一档都没有
	s := Settings{
		WindowStart:      at(21, 0, 0),
		WindowEnd:        at(23, 59, 0),
		MaxWaitMinutes:   12,
		ToleranceMinutes: 5,
	}
	slots := PlanSlots(s, at(21, 0, 0), nil)
	if len(slots) != 1 {
		t.Fatalf("档位数量: got=%d want=1", len(slots))
	}
	if got := clock(slots[0].Target); got != "21:35:00" {
		t.Errorf("兜底档位: got=%q want=%q", got, "21:35:00")
	}
}

func TestPlanSlotsNormalizesTimezone(t *testing.T) {
	// 传 UTC 也按北京日期落档：UTC 13:00 == 北京 21:00，同日
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	utc := time.Date(2026, 11, 14, 13, 0, 0, 0, time.UTC)
	slots := PlanSlots(s, utc, nil)
	if len(slots) != 1 {
		t.Fatalf("档位数量: got=%d want=1", len(slots))
	}
	if got := slots[0].Target.In(cn).Format("2006-01-02 15:04:05"); got != "2026-11-14 21:35:00" {
		t.Errorf("按北京时间落档: got=%q", got)
	}
}

// ============ ResolveRun ============

func TestResolveRunOutOfWindow(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	for _, now := range []time.Time{at(20, 59, 59), nextDay(0, 0, 0)} {
		d := ResolveRun(s, now, nil, nil)
		if d.Mode != modeSkip || d.Reason != reasonOutOfWindow {
			t.Errorf("%s: got=(%s,%s) want=(%s,%s)", clock(now), d.Mode, d.Reason, modeSkip, reasonOutOfWindow)
		}
		if !d.Target.IsZero() {
			t.Errorf("%s: 窗口外不该给目标时刻: %v", clock(now), d.Target)
		}
		if d.Slot != nil {
			t.Errorf("%s: 窗口外不该带档位", clock(now))
		}
		if d.WaitSeconds != 0 {
			t.Errorf("%s: 窗口外不该带等待秒数: %v", clock(now), d.WaitSeconds)
		}
	}
}

func TestResolveRunOnTarget(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	cases := []struct {
		name string
		now  time.Time
	}{
		{"正好到点", at(21, 35, 0)},
		{"迟到 4 分 59 秒仍在容忍内", at(21, 39, 59)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ResolveRun(s, c.now, nil, []Slot{slotAt(21, 35, 0, true)})
			if d.Mode != modeRun || d.Reason != reasonOnTarget {
				t.Fatalf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeRun, reasonOnTarget)
			}
			if got := clock(d.Target); got != "21:35:00" {
				t.Errorf("target: got=%q want=%q", got, "21:35:00")
			}
			if d.WaitSeconds != 0 {
				t.Errorf("run 分支不该带等待秒数: %v", d.WaitSeconds)
			}
			if d.Slot == nil || clock(d.Slot.Configured) != "21:35:00" {
				t.Errorf("应带命中档位: %+v", d.Slot)
			}
		})
	}
}

func TestResolveRunJustMissedTolerance(t *testing.T) {
	// 刚超出容忍、且没有未来档 → 迟到补签（宁可迟到也不漏签）
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	d := ResolveRun(s, at(21, 40, 1), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Mode != modeRun || d.Reason != reasonLateFallback {
		t.Errorf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeRun, reasonLateFallback)
	}
}

func TestResolveRunOnTargetPicksNearest(t *testing.T) {
	// 两档都还在容忍范围内，取最近（最晚）那一档，并且与入参切片同一份内存
	s := Parse(model.ScheduleConfig{Times: []string{"21:35", "21:37"}})
	slots := []Slot{slotAt(21, 35, 0, true), slotAt(21, 37, 0, true)}
	d := ResolveRun(s, at(21, 38, 0), nil, slots)
	if d.Reason != reasonOnTarget {
		t.Fatalf("reason: got=%q want=%q", d.Reason, reasonOnTarget)
	}
	if got := clock(d.Target); got != "21:37:00" {
		t.Errorf("target: got=%q want=%q", got, "21:37:00")
	}
	if d.Slot != &slots[1] {
		t.Errorf("命中档位必须指向入参切片元素，便于上层写运行记录")
	}
}

func TestResolveRunWait(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	d := ResolveRun(s, at(21, 34, 0), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Mode != modeWait || d.Reason != reasonWaitUntilTarget {
		t.Fatalf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeWait, reasonWaitUntilTarget)
	}
	if d.WaitSeconds != 60 {
		t.Errorf("wait_seconds: got=%v want=60", d.WaitSeconds)
	}
	if got := clock(d.Target); got != "21:35:00" {
		t.Errorf("target: got=%q want=%q", got, "21:35:00")
	}
}

func TestResolveRunWaitBoundaryInclusive(t *testing.T) {
	// 恰好等于 max_wait 上界：仍然可以等（判定是 <=，不是 <）
	s := Parse(model.ScheduleConfig{Times: []string{"21:47"}, MaxWaitMinutes: fp(12)})
	d := ResolveRun(s, at(21, 35, 0), nil, []Slot{slotAt(21, 47, 0, true)})
	if d.Mode != modeWait {
		t.Errorf("mode: got=%q want=%q (wait_seconds=%v)", d.Mode, modeWait, d.WaitSeconds)
	}
	if d.WaitSeconds != 720 {
		t.Errorf("wait_seconds: got=%v want=720", d.WaitSeconds)
	}
}

func TestResolveRunWaitTooLong(t *testing.T) {
	cases := []struct {
		name string
		cfg  model.ScheduleConfig
		now  time.Time
		slot Slot
	}{
		{"默认上限 12 分钟等不起", model.ScheduleConfig{Times: []string{"21:35"}}, at(21, 0, 0), slotAt(21, 35, 0, true)},
		{"显式 0 等待", model.ScheduleConfig{Times: []string{"21:50"}, MaxWaitMinutes: fp(0)}, at(21, 49, 0), slotAt(21, 50, 0, true)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Parse(c.cfg)
			d := ResolveRun(s, c.now, nil, []Slot{c.slot})
			if d.Mode != modeSkip || d.Reason != reasonWaitTooLong {
				t.Fatalf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeSkip, reasonWaitTooLong)
			}
			if d.WaitSeconds != 0 {
				t.Errorf("skip 分支不该带等待秒数: %v", d.WaitSeconds)
			}
			if !d.Target.Equal(c.slot.Target) {
				t.Errorf("仍要给出计划目标时刻: got=%s want=%s", clock(d.Target), clock(c.slot.Target))
			}
			if d.Slot == nil {
				t.Error("skip 也应带上是哪一档等不起")
			}
		})
	}
}

func TestResolveRunLateFallback(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	d := ResolveRun(s, at(22, 0, 0), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Mode != modeRun || d.Reason != reasonLateFallback {
		t.Fatalf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeRun, reasonLateFallback)
	}
	if got := clock(d.Target); got != "21:35:00" {
		t.Errorf("target: got=%q want=%q", got, "21:35:00")
	}

	// 多档全部过期：取最晚那一档，避免用更早的时间去补签
	s2 := Parse(model.ScheduleConfig{Times: []string{"21:35", "21:50"}})
	slots := []Slot{slotAt(21, 35, 0, true), slotAt(21, 50, 0, true)}
	d = ResolveRun(s2, at(23, 0, 0), nil, slots)
	if got := clock(d.Target); got != "21:50:00" {
		t.Errorf("多档补签取最晚: got=%q want=%q", got, "21:50:00")
	}
	if d.Slot != &slots[1] {
		t.Error("应命中最后一档")
	}
}

func TestResolveRunNoSlotFallback(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"20:30"}})
	now := at(21, 30, 0)
	invalid := []Slot{slotAt(20, 30, 0, false)}

	// 全无效档
	d := ResolveRun(s, now, nil, invalid)
	if d.Mode != modeRun || d.Reason != reasonNoSlotFallback {
		t.Fatalf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeRun, reasonNoSlotFallback)
	}
	if !d.Target.Equal(now) {
		t.Errorf("兜底应立刻签: got=%s want=%s", clock(d.Target), clock(now))
	}
	if d.Slot != nil {
		t.Error("兜底不该带档位")
	}

	// 空切片（非 nil）同样兜底：与 Python 的 slots=[] 一致
	d = ResolveRun(s, now, nil, []Slot{})
	if d.Reason != reasonNoSlotFallback {
		t.Errorf("空切片应走兜底: got=%q", d.Reason)
	}

	// slots 为 nil 时内部现规划，规划结果全在窗口外 → 仍是兜底
	d = ResolveRun(s, now, NewScriptSource([]float64{0.5}), nil)
	if d.Reason != reasonNoSlotFallback {
		t.Errorf("内部规划的无效档也应兜底: got=%q", d.Reason)
	}
}

func TestResolveRunPlansWhenSlotsNil(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	d := ResolveRun(s, at(21, 34, 0), NewScriptSource([]float64{0.5}), nil)
	if d.Mode != modeWait || d.WaitSeconds != 60 {
		t.Errorf("got=(%s,%v) want=(%s,60)", d.Mode, d.WaitSeconds, modeWait)
	}
}

func TestResolveRunGuardedTarget(t *testing.T) {
	// 手工构造的档位 target 落在窗口外：宁可立刻签，也不把越界时刻交给执行路径
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	now := at(21, 30, 0)
	bad := Slot{Configured: at(20, 0, 0), Target: at(20, 0, 0), Valid: true, Reason: reasonOK}
	d := ResolveRun(s, now, nil, []Slot{bad})
	if d.Reason != reasonLateFallback {
		t.Fatalf("reason: got=%q want=%q", d.Reason, reasonLateFallback)
	}
	if !d.Target.Equal(now) {
		t.Errorf("窗口外的 target 应换成 now: got=%s want=%s", clock(d.Target), clock(now))
	}
}

func TestResolveRunToleranceZero(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}, ToleranceMinutes: fp(0)})
	// 差值 0 秒仍然算到点
	d := ResolveRun(s, at(21, 35, 0), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Reason != reasonOnTarget {
		t.Errorf("到点当刻: got=%q want=%q", d.Reason, reasonOnTarget)
	}
	// 迟到 1 秒就超出容忍，且没有未来档 → 迟到补签
	d = ResolveRun(s, at(21, 35, 1), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Reason != reasonLateFallback {
		t.Errorf("迟到 1 秒: got=%q want=%q", d.Reason, reasonLateFallback)
	}
}

func TestResolveRunNormalizesTimezone(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35"}})
	// UTC 13:35 == 北京 21:35：时区归一后才是"到点"
	d := ResolveRun(s, time.Date(2026, 11, 14, 13, 35, 0, 0, time.UTC), nil, []Slot{slotAt(21, 35, 0, true)})
	if d.Reason != reasonOnTarget {
		t.Errorf("UTC 时刻必须按北京时间判定: got=%q", d.Reason)
	}
	if got := clock(d.Target); got != "21:35:00" {
		t.Errorf("target: got=%q want=%q", got, "21:35:00")
	}
	// UTC 12:00 == 北京 20:00 → 窗口外
	d = ResolveRun(s, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC), nil, nil)
	if d.Mode != modeSkip || d.Reason != reasonOutOfWindow {
		t.Errorf("got=(%s,%s) want=(%s,%s)", d.Mode, d.Reason, modeSkip, reasonOutOfWindow)
	}
}

// ============ NextRunAt ============

func TestNextRunAt(t *testing.T) {
	s := Parse(model.ScheduleConfig{Times: []string{"21:35", "21:50"}})
	slots := PlanSlots(s, at(21, 0, 0), nil)
	cases := []struct {
		name string
		now  time.Time
		in   []Slot
		want string // 空 = 期望零值
	}{
		{"第一档之前", at(21, 0, 0), slots, "21:35:00"},
		{"恰好等于第一档（严格大于）", at(21, 35, 0), slots, "21:50:00"},
		{"两档之间", at(21, 40, 0), slots, "21:50:00"},
		{"全部过期", at(22, 0, 0), slots, ""},
		{"空切片", at(21, 0, 0), nil, ""},
		{"UTC 时刻按北京时间算", time.Date(2026, 11, 14, 13, 40, 0, 0, time.UTC), slots, "21:50:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextRunAt(s, c.in, c.now)
			if c.want == "" {
				if !got.IsZero() {
					t.Errorf("应返回零值: got=%v", got)
				}
				return
			}
			if gotClock := clock(got); gotClock != c.want {
				t.Errorf("got=%q want=%q", gotClock, c.want)
			}
		})
	}
}

// ============ DecisionText ============

func TestDecisionText(t *testing.T) {
	jittered := &Slot{Configured: at(21, 35, 0), Target: at(21, 35, 12), Valid: true, Jittered: true, Reason: reasonOK}
	plain := func(h, mi, s int) *Slot {
		return &Slot{Configured: at(h, mi, s), Target: at(h, mi, s), Valid: true, Reason: reasonOK}
	}
	cases := []struct {
		name string
		d    Decision
		want string
	}{
		{
			name: "等待含抖动档与剩余秒数",
			d:    Decision{Mode: modeWait, Slot: jittered, Target: at(21, 35, 12), WaitSeconds: 30, Reason: reasonWaitUntilTarget},
			want: "等待（等到目标时刻） 目标 21:35:12（配置 21:35 + 时间抖动），还有 30 秒",
		},
		{
			name: "窗口外跳过不带目标时刻",
			d:    Decision{Mode: modeSkip, Reason: reasonOutOfWindow},
			want: "跳过（不在签到窗口内）",
		},
		{
			name: "到点且钳制",
			d: Decision{Mode: modeRun, Target: at(23, 59, 30), Reason: reasonOnTarget,
				Slot: &Slot{Configured: at(23, 59, 0), Target: at(23, 59, 30), Valid: true, Jittered: true, Clamped: true, Reason: reasonClampedHigh}},
			want: "签到（已到点） 目标 23:59:30（配置 23:59 + 抖动越界已贴边）",
		},
		{
			name: "兜底不带档位",
			d:    Decision{Mode: modeRun, Target: at(22, 0, 0), Reason: reasonNoSlotFallback},
			want: "签到（无有效档位，窗口内兜底） 目标 22:00:00",
		},
		{
			name: "等待超上限",
			d:    Decision{Mode: modeSkip, Slot: plain(21, 35, 0), Target: at(21, 35, 0), Reason: reasonWaitTooLong},
			want: "跳过（等待超过上限） 目标 21:35:00（配置 21:35）",
		},
		{
			name: "迟到补签",
			d:    Decision{Mode: modeRun, Slot: plain(21, 35, 0), Target: at(21, 35, 0), Reason: reasonLateFallback},
			want: "签到（已过末档，迟到补签） 目标 21:35:00（配置 21:35）",
		},
		{
			name: "档位在窗口外",
			d: Decision{Mode: modeRun, Target: at(21, 0, 0), Reason: reasonNoSlotFallback,
				Slot: &Slot{Configured: at(20, 30, 0), Target: at(20, 30, 0), Reason: reasonOutOfWindow}},
			want: "签到（无有效档位，窗口内兜底） 目标 21:00:00（配置 20:30 + 档位在窗口外）",
		},
		{
			name: "force 前缀原样追溯",
			d:    Decision{Mode: modeRun, Slot: plain(21, 35, 0), Target: at(21, 35, 0), Reason: "force:" + reasonOnTarget},
			want: "签到（手动强制 · 已到点） 目标 21:35:00（配置 21:35）",
		},
		{
			name: "未知枚举原样透出",
			d:    Decision{Mode: "probe", Target: at(21, 35, 0), Reason: "weird"},
			want: "probe（weird） 目标 21:35:00",
		},
		{
			name: "不足一秒向上取整",
			d:    Decision{Mode: modeWait, Slot: plain(21, 35, 0), Target: at(21, 35, 0), WaitSeconds: 0.4, Reason: reasonWaitUntilTarget},
			want: "等待（等到目标时刻） 目标 21:35:00（配置 21:35），还有 1 秒",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecisionText(c.d); got != c.want {
				t.Errorf("got=%q\nwant=%q", got, c.want)
			}
		})
	}
}

// ============ 随机源与时钟 ============

func TestScriptSourceCycles(t *testing.T) {
	src := NewScriptSource([]float64{0.25, 0.75})
	want := []float64{0.25, 0.75, 0.25, 0.75, 0.25}
	for i, w := range want {
		if got := src.Float64(); got != w {
			t.Fatalf("第 %d 格: got=%v want=%v", i, got, w)
		}
	}
}

func TestScriptSourceCopiesInput(t *testing.T) {
	us := []float64{0.1}
	src := NewScriptSource(us)
	us[0] = 0.9 // 调用方改自己的切片不该影响已注入的序列
	if got := src.Float64(); got != 0.1 {
		t.Errorf("序列被外部改动污染: got=%v want=0.1", got)
	}
}

func TestScriptSourceEmpty(t *testing.T) {
	// 空序列取中点：0 会把抖动固定拉到下界，0.5 最接近"没有抖动"
	if got := NewScriptSource(nil).Float64(); got != 0.5 {
		t.Errorf("nil 序列: got=%v want=0.5", got)
	}
	if got := NewScriptSource([]float64{}).Float64(); got != 0.5 {
		t.Errorf("空序列: got=%v want=0.5", got)
	}
}

func TestGlobalSourceRange(t *testing.T) {
	src := GlobalSource()
	for i := 0; i < 128; i++ {
		got := src.Float64()
		if got < 0 || got >= 1 {
			t.Fatalf("u 必须落在 [0,1): got=%v", got)
		}
	}
}

func TestNowCN(t *testing.T) {
	now := NowCN()
	if _, offset := now.Zone(); offset != 8*3600 {
		t.Errorf("必须是 +08:00: got offset=%d", offset)
	}
	if name, _ := now.Zone(); name != "CST" {
		t.Errorf("时区名: got=%q want=%q", name, "CST")
	}
	if delta := time.Since(now); delta > time.Minute || delta < -time.Minute {
		t.Errorf("NowCN 偏离本机时钟太多: %v", delta)
	}
}

func TestUniformUsesSource(t *testing.T) {
	// uniform 必须严格是 low + (high-low)*u：跨实现一致性全靠这个式子
	cases := []struct{ u, low, high, want float64 }{
		{0, -5, 5, -5},
		{0.5, -5, 5, 0},
		{1, -5, 5, 5},
		{0.25, 0, 60, 15},
	}
	for _, c := range cases {
		if got := uniform(NewScriptSource([]float64{c.u}), c.low, c.high); got != c.want {
			t.Errorf("u=%v [%v,%v]: got=%v want=%v", c.u, c.low, c.high, got, c.want)
		}
	}
}

func TestConcurrentPlanAndResolve(t *testing.T) {
	// 调度循环在多 goroutine 下复用同一个 Source 是常态（SSE 的 status 接口也会顺手算一轮），
	// 这里共享脚本随机源并发跑，配合 -race 用于证明 scriptSource 的游标是安全的：
	// 结果不要求确定性（谁先取哪一格取决于调度），只要求不 panic、不出现零值档位。
	s := Parse(model.ScheduleConfig{Times: []string{"21:00", "21:35", "21:50"}, TimeJitterMinutes: fp(30)})
	src := NewScriptSource([]float64{0.1, 0.9, 0.5, 0.3, 0.7})
	now := at(21, 10, 0)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots := PlanSlots(s, now, src)
			if len(slots) != 3 {
				t.Errorf("档位数量: got=%d want=3", len(slots))
				return
			}
			for _, slot := range slots {
				if slot.Valid && !InWindow(slot.Target, s.WindowStart, s.WindowEnd) {
					t.Errorf("有效档位必须落在窗口内: %v", slot.Target)
				}
			}
			d := ResolveRun(s, now, src, slots)
			if d.Mode != modeRun && d.Mode != modeWait && d.Mode != modeSkip {
				t.Errorf("未知 mode: %q", d.Mode)
			}
			_ = NextRunAt(s, slots, now)
		}()
	}
	wg.Wait()
}
