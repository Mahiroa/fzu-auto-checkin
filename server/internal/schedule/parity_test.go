package schedule

// 跨实现一致性测试：同一个 (schedule 配置, now, u 序列) 下，
// Go 版必须与 Python 原实现（src/schedule.py）得出完全相同的
// mode / reason / target / wait_seconds / slots[]。
//
// 语料由 scripts/gen_parity.py 生成，随机源是"循环消费的固定 u 序列"，
// 所以这里用 NewScriptSource 把同一串 u 喂进去，就能逐条对答案。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// parityISO 是语料里的时间格式：Python 侧固定 6 位微秒 + 固定 +08:00。
const parityISO = "2006-01-02T15:04:05.000000-07:00"

// parityTimeTolerance 是时刻比对容差：Python timedelta 只有微秒精度、
// Go Duration 是纳秒，浮点分钟折算的末位必然差着几百纳秒；5ms 足够宽，
// 又远小于"差一秒"这种真错误。
const parityTimeTolerance = 5 * time.Millisecond

// parityWaitTolerance 是 wait_seconds 的容差（秒）。
const parityWaitTolerance = 0.01

type paritySlot struct {
	Configured string `json:"configured"`
	Target     string `json:"target"`
	Valid      bool   `json:"valid"`
	Jittered   bool   `json:"jittered"`
	Clamped    bool   `json:"clamped"`
	Reason     string `json:"reason"`
}

type parityExpect struct {
	Mode        string       `json:"mode"`
	Reason      string       `json:"reason"`
	Target      *string      `json:"target"` // null = 决策不带目标时刻（窗口外跳过）
	WaitSeconds float64      `json:"wait_seconds"`
	Slots       []paritySlot `json:"slots"`
}

type parityCase struct {
	Name     string               `json:"name"`
	Schedule model.ScheduleConfig `json:"schedule"`
	Now      string               `json:"now"`
	Uses     int                  `json:"uses"`
	Expect   parityExpect         `json:"expect"`
}

type parityCorpus struct {
	GeneratedBy string       `json:"generated_by"`
	U           []float64    `json:"u"`
	Cases       []parityCase `json:"cases"`
}

// countingSource 记录被消费了多少个 u。
// 语料里的 uses 也是对答案的一部分：多消费或少消费一格，说明抖动重抽次数与 Python
// 不同，此后所有时刻都会错位——这条断言比时间比对更早、更准地定位问题。
type countingSource struct {
	inner Source
	n     int
}

func (c *countingSource) Float64() float64 {
	c.n++
	return c.inner.Float64()
}

func fmtClock(t time.Time) string {
	if t.IsZero() {
		return "<零值>"
	}
	return t.In(cn).Format(parityISO)
}

func TestScheduleParity(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "schedule_parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取一致性语料失败 %s: %v", path, err)
	}
	var corpus parityCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析一致性语料失败: %v", err)
	}
	if len(corpus.U) == 0 {
		t.Fatal("语料缺少 u 序列：无法重放随机抖动")
	}
	// 契约要求 ≥200 例；语料被误删/截断时必须立刻失败，而不是"测试变绿但没测到"
	if len(corpus.Cases) < 200 {
		t.Fatalf("语料只有 %d 例，少于契约要求的 200 例", len(corpus.Cases))
	}

	for _, c := range corpus.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			now, err := time.Parse(parityISO, c.Now)
			if err != nil {
				t.Fatalf("now 解析失败 %q: %v", c.Now, err)
			}

			settings := Parse(c.Schedule)
			src := &countingSource{inner: NewScriptSource(corpus.U)}
			slots := PlanSlots(settings, now, src)
			if src.n != c.Uses {
				t.Errorf("随机序列消费格数: got=%d want=%d", src.n, c.Uses)
			}

			// 决策复用同一批 slots：与 main.py "一次规划、全程复用" 的调用方式一致，
			// 也让这条用例只考察决策分支本身，不掺入第二次抖动。
			d := ResolveRun(settings, now, nil, slots)

			if d.Mode != c.Expect.Mode {
				t.Errorf("mode: got=%q want=%q", d.Mode, c.Expect.Mode)
			}
			if d.Reason != c.Expect.Reason {
				t.Errorf("reason: got=%q want=%q", d.Reason, c.Expect.Reason)
			}

			switch {
			case c.Expect.Target == nil:
				if !d.Target.IsZero() {
					t.Errorf("target: got=%s want=<零值>", fmtClock(d.Target))
				}
				if d.Slot != nil {
					t.Errorf("target 为空时不该带档位: got=%+v", *d.Slot)
				}
			default:
				want, err := time.Parse(parityISO, *c.Expect.Target)
				if err != nil {
					t.Fatalf("expect.target 解析失败 %q: %v", *c.Expect.Target, err)
				}
				assertTimeNear(t, "target", d.Target, want)
				if d.Reason == reasonNoSlotFallback && d.Slot != nil {
					t.Errorf("no_slot_fallback 不该带档位: got=%+v", *d.Slot)
				}
				if d.Reason != reasonNoSlotFallback && d.Slot == nil {
					t.Errorf("reason=%q 应带命中档位，实际为 nil", d.Reason)
				}
			}

			if diff := d.WaitSeconds - c.Expect.WaitSeconds; diff > parityWaitTolerance || diff < -parityWaitTolerance {
				t.Errorf("wait_seconds: got=%v want=%v", d.WaitSeconds, c.Expect.WaitSeconds)
			}

			if len(slots) != len(c.Expect.Slots) {
				t.Fatalf("slots 数量: got=%d want=%d", len(slots), len(c.Expect.Slots))
			}
			for i, want := range c.Expect.Slots {
				got := slots[i]
				prefix := fmt.Sprintf("slots[%d]", i)
				if gotClock := got.Configured.In(cn).Format("15:04:05"); gotClock != want.Configured {
					t.Errorf("%s.configured: got=%q want=%q", prefix, gotClock, want.Configured)
				}
				// Configured 的日期必须是 now 的北京日期：档位永远落在"今天"，
				// 日期错位会让当晚所有档位都失效。
				if day := got.Configured.In(cn).Format("2006-01-02"); day != now.In(cn).Format("2006-01-02") {
					t.Errorf("%s.configured 日期: got=%q want=%q", prefix, day, now.In(cn).Format("2006-01-02"))
				}
				wantTarget, err := time.Parse(parityISO, want.Target)
				if err != nil {
					t.Fatalf("%s.target 解析失败 %q: %v", prefix, want.Target, err)
				}
				assertTimeNear(t, prefix+".target", got.Target, wantTarget)
				if got.Valid != want.Valid {
					t.Errorf("%s.valid: got=%v want=%v", prefix, got.Valid, want.Valid)
				}
				if got.Jittered != want.Jittered {
					t.Errorf("%s.jittered: got=%v want=%v", prefix, got.Jittered, want.Jittered)
				}
				if got.Clamped != want.Clamped {
					t.Errorf("%s.clamped: got=%v want=%v", prefix, got.Clamped, want.Clamped)
				}
				if got.Reason != want.Reason {
					t.Errorf("%s.reason: got=%q want=%q", prefix, got.Reason, want.Reason)
				}
			}
		})
	}
}

// assertTimeNear 比对两个时刻，容忍 ±5ms；失败信息给出两边完整时刻与差值。
func assertTimeNear(t *testing.T, field string, got, want time.Time) {
	t.Helper()
	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}
	if delta > parityTimeTolerance {
		t.Errorf("%s: got=%s want=%s (Δ=%s)", field, fmtClock(got), fmtClock(want), delta)
	}
}
