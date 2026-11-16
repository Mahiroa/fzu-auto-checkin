package server

import (
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// TestAlreadyCheckedStopsPollingForToday：学校端已经显示"今日已签到"时，
// 调度器必须把今天彻底当成完成态——否则窗口内会每 30 秒打一次 init 去确认
// 同一件事（用户明确要求过：签完到下一个签到时间开始前不要再轮询）。
func TestAlreadyCheckedStopsPollingForToday(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.school.setChecked(true) // 学校端：已签到
	env.setNow(fixedDay(22, 10))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusAlready {
		t.Fatalf("期望 already_checked，实际 %s", describe(rec))
	}

	st := env.store.State()
	if st.LastSuccessDate != env.today() {
		t.Fatalf("already_checked 也要写 last_success_date（今天=%s），实际 %q",
			env.today(), st.LastSuccessDate)
	}
	// 但 last_slot_key 记录的是"我们触发过哪一档"，这次没有触发任何档位，不该写。
	if st.LastSlotKey != "" {
		t.Fatalf("already_checked 不该写 last_slot_key，实际 %q", st.LastSlotKey)
	}
}

// TestProbeDoesNotMarkChecked：预检（probe）永远不能把今天标成已完成，
// 否则"点一次预检"就等于放弃当晚的自动签到——最危险的一种成功。
func TestProbeDoesNotMarkChecked(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(22, 10))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force", "dry_run": true})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("预检应成功，实际 %s", describe(rec))
	}
	if got := env.store.State().LastSuccessDate; got != "" {
		t.Fatalf("预检不该写 last_success_date，实际 %q", got)
	}
}
