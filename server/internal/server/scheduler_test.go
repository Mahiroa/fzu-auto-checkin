package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
)

// withFastTick 把窗口内的复查周期临时压到 d，返回复原函数。
//
// 为什么需要：30s 的常量在单测里等不起，而"窗口外/已签之后到底还在不在轮询"
// 必须有一个可观测的差异才能守住（旧实现是每 tick 一条 DEBUG，tick 压实后
// 300ms 内就能堆出十几条）。生产值恒为 30s，测试跑完立刻复原。
func withFastTick(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := schedulerTick
	schedulerTick = d
	return func() { schedulerTick = old }
}

// logMessages 返回内存日志环里包含 substr 的全部消息（新 → 旧），带级别前缀。
func logMessages(env *testEnv, substr string) []string {
	var out []string
	for _, ev := range env.store.Logs(0) {
		if strings.Contains(ev.Msg, substr) {
			out = append(out, ev.Level+" "+ev.Msg)
		}
	}
	return out
}

// logTail 只用于失败信息：把最近的日志按时间顺序拼起来。
func logTail(env *testEnv) string {
	events := env.store.Logs(12)
	lines := make([]string, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		lines = append(lines, events[i].Level+" "+events[i].Msg)
	}
	return strings.Join(lines, "\n")
}

// runSchedulerAsync 起一个调度循环，返回取消函数与"已退出"通道。
func (e *testEnv) runSchedulerAsync() (cancel func(), done <-chan struct{}) {
	e.t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		e.srv.runScheduler(ctx)
	}()
	e.t.Cleanup(cancelFn)
	return cancelFn, doneCh
}

// TestCatchUpRunsLateFallback 验证启动补跑：已过末档且今日未签 → 立刻按 late_fallback 跑一次。
func TestCatchUpRunsLateFallback(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig()) // times=["21:35"]，窗口 21:00-23:59
	env.setNow(fixedDay(23, 30))

	env.srv.catchUp()

	var rec model.RunRecord
	waitValue(t, 5*time.Second, "补跑产生记录", func() bool {
		runs, err := env.store.Runs(1)
		if err != nil || len(runs) == 0 {
			return false
		}
		if runs[0].FinishedAt.IsZero() {
			return false
		}
		rec = runs[0]
		return true
	})
	if rec.Mode != "scheduled" {
		t.Fatalf("补跑的 mode 期望 scheduled，实际 %q", rec.Mode)
	}
	if rec.Decision != "late_fallback" {
		t.Fatalf("补跑的 decision 期望 late_fallback，实际 %q", rec.Decision)
	}
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if st := env.store.State(); st.LastSuccessDate != env.today() {
		t.Fatalf("补跑成功后 last_success_date 期望 %s，实际 %q", env.today(), st.LastSuccessDate)
	}
	if st := env.store.State(); st.LastSlotKey != env.today()+"|21:35|21:35:00" {
		t.Fatalf("last_slot_key 不符: %q", st.LastSlotKey)
	}

	// 再判一次：今日已签 → 不再补跑。
	env.waitIdle(2 * time.Second)
	env.srv.catchUp()
	time.Sleep(150 * time.Millisecond)
	runs, err := env.store.Runs(10)
	if err != nil {
		t.Fatalf("读取运行记录失败: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("今日已签后不该再补跑，实际 %d 条记录", len(runs))
	}
}

// TestCatchUpSkipsOutsideWindow 验证窗口外不补跑（登录/推送都留到窗口内）。
func TestCatchUpSkipsOutsideWindow(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(20, 0)) // 窗口起点之前

	env.srv.catchUp()
	time.Sleep(150 * time.Millisecond)

	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("窗口外不该补跑，实际 %d 条", len(runs))
	}
	if env.srv.running.Load() {
		t.Fatalf("窗口外不该占用单飞开关")
	}
}

// TestCatchUpSkipsWhenBeforeLastSlot 验证"最后一档还没到"时不补跑。
func TestCatchUpSkipsWhenBeforeLastSlot(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 0)) // 窗口内，但 21:35 还没到

	env.srv.catchUp()
	time.Sleep(150 * time.Millisecond)

	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("末档还没到不该补跑，实际 %d 条", len(runs))
	}
}

// TestCatchUpHonoursLastSlotKey 验证幂等第一道闸。
func TestCatchUpHonoursLastSlotKey(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(23, 30))
	env.srv.updateState(func(st *model.State) {
		// 失败重试后不会写这个键，这里手工构造"该档已执行过"的状态。
		st.LastSlotKey = env.today() + "|21:35|21:35:00"
	})

	env.srv.catchUp()
	time.Sleep(150 * time.Millisecond)

	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("last_slot_key 命中时不该再跑，实际 %d 条", len(runs))
	}
}

// TestSchedulerClockRewind 验证时钟回拨会清空 last_slot_key 并重规划。
func TestSchedulerClockRewind(t *testing.T) {
	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())

	base := fixedDay(20, 0) // 窗口外：每轮都是 skip，不会触发运行
	env.setNow(base)
	env.srv.updateState(func(st *model.State) {
		st.LastSlotKey = "2026-11-14|21:35|21:35:00"
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.srv.runScheduler(ctx)
	}()
	waitSchedulerStarted(t, env)

	// 第一轮：正常时刻（不清键）。
	env.srv.wakeScheduler("")
	time.Sleep(80 * time.Millisecond)
	if st := env.store.State(); st.LastSlotKey == "" {
		t.Fatalf("正常时刻不该清空 last_slot_key")
	}

	// 第二轮：把时钟往回拨 10 分钟 → 判定回拨并清键。
	env.setNow(base.Add(-10 * time.Minute))
	env.srv.wakeScheduler("")
	waitValue(t, 2*time.Second, "时钟回拨后清空 last_slot_key", func() bool {
		return env.store.State().LastSlotKey == ""
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("调度器退出超时")
	}
}

// TestSchedulerDecisionLoopSkipsOutsideWindow 验证 tick 循环在窗口外保持 skip。
func TestSchedulerDecisionLoopSkipsOutsideWindow(t *testing.T) {
	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(20, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.srv.runScheduler(ctx)
	}()
	waitSchedulerStarted(t, env)

	env.srv.wakeScheduler("")
	waitValue(t, 2*time.Second, "记录 out_of_window 决策", func() bool {
		return env.srv.lastDecisionValue() == "out_of_window"
	})
	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("窗口外不该产生运行记录，实际 %d 条", len(runs))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("调度器退出超时")
	}
	if !env.srv.schedulerStopped() {
		t.Fatalf("退出后 scheduler.stopped 应为 true")
	}
}

// TestRunGracefulShutdown 覆盖验收清单第 10 条：ctx 取消后 5s 内返回。
func TestRunGracefulShutdown(t *testing.T) {
	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(20, 0))
	env.srv.SetListen("127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- env.srv.Run(ctx) }()

	// 等 HTTP 真正开始服务：Run 在 Serve 之前会写一条"HTTP 服务已就绪"日志。
	// 用内存日志环判就绪比探测端口稳（Run 用的是 :0，测试拿不到最终端口）。
	waitValue(t, 3*time.Second, "HTTP 服务就绪", func() bool {
		for _, event := range env.store.Logs(50) {
			if strings.Contains(event.Msg, "HTTP 服务已就绪") {
				return true
			}
		}
		return false
	})
	waitSchedulerStarted(t, env)

	start := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("优雅退出返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ctx 取消后 5 秒内没有返回（已耗时 %s）", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("优雅退出耗时 %s，超过 5s", elapsed)
	}
	if !env.srv.schedulerStopped() {
		t.Fatalf("退出后调度器应标记 stopped")
	}
}

// ============ 事件驱动调度：休眠到窗口起点（不再全天候轮询） ============

// TestNextWindowStart 钉住"下一个窗口开始时刻"的算法：
// 今天还没到起点就用今天，已经过了起点（含已过终点）就用明天，且必须严格晚于 now。
func TestNextWindowStart(t *testing.T) {
	sched := schedule.Parse(baseConfig().Checkin.Schedule) // 窗口 21:00-23:59

	cases := []struct {
		name string
		at   time.Time
		want string // 期望的休眠目标（北京时间）
	}{
		{"凌晨", fixedDay(0, 30), "11-14 21:00"},
		{"白天", fixedDay(13, 0), "11-14 21:00"},
		{"窗口起点前一分钟", fixedDay(20, 59), "11-14 21:00"},
		{"正好是窗口起点", fixedDay(21, 0), "11-15 21:00"},
		{"窗口内", fixedDay(22, 30), "11-15 21:00"},
		{"窗口最后一分钟", fixedDay(23, 59), "11-15 21:00"},
	}
	for _, c := range cases {
		got := nextWindowStart(sched, c.at)
		if text := got.Format("01-02 15:04"); text != c.want {
			t.Fatalf("%s：nextWindowStart(%s) 期望 %s，实际 %s",
				c.name, c.at.Format("01-02 15:04"), c.want, text)
		}
		if !got.After(c.at) {
			t.Fatalf("%s：休眠目标必须严格晚于 now，实际 %s <= %s",
				c.name, got.Format(time.RFC3339), c.at.Format(time.RFC3339))
		}
	}

	// 已签分支睡的是"明天"的窗口起点：现在还在今天的窗口内也一样。
	got := windowStartOn(sched, fixedDay(21, 40).AddDate(0, 0, 1))
	if text := got.Format("01-02 15:04"); text != "11-15 21:00" {
		t.Fatalf("次日窗口起点期望 11-15 21:00，实际 %s", text)
	}
}

// TestSchedulerSleepsOutsideWindow 覆盖"窗口外只休眠、不轮询"：
//
// 旧实现每 30s 一条 DEBUG「调度决策：跳过（不在签到窗口内）」，一天能刷掉内存日志环。
// 这里把复查周期压到 20ms（等价于把旧实现的轮询放大 1500 倍）：如果窗口外还在轮询，
// 300ms 内必然多出十几条日志。断言是"一条都不许再多"。
func TestSchedulerSleepsOutsideWindow(t *testing.T) {
	restoreTick := withFastTick(t, 20*time.Millisecond)
	defer restoreTick()

	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig()) // times=["21:35"]，窗口 21:00-23:59
	env.setNow(fixedDay(20, 0))  // 窗口起点之前：天然处于窗口外

	cancel, done := env.runSchedulerAsync()
	waitSchedulerStarted(t, env)

	// 1) 恰好一条"窗口外休眠"，且目标是**今天**的 21:00（不是明天）。
	// 判据用「下一次签到窗口」而不是「窗口外休眠」：后者也出现在启动横幅里。
	waitValue(t, 3*time.Second, "窗口外休眠日志", func() bool {
		return len(logMessages(env, "下一次签到窗口")) == 1
	})
	sleeps := logMessages(env, "下一次签到窗口")
	if !strings.Contains(sleeps[0], "11-14 21:00") {
		t.Fatalf("窗口外应休眠到今天 21:00，实际日志：%s", sleeps[0])
	}
	if !strings.HasPrefix(sleeps[0], "info ") {
		t.Fatalf("进入休眠应打 INFO（默认级别下用户要看得见），实际：%s", sleeps[0])
	}

	// 2) /status 的 last_decision 仍是 out_of_window，且没有产生任何运行。
	if got := env.srv.lastDecisionValue(); got != "out_of_window" {
		t.Fatalf("窗口外 last_decision 期望 out_of_window，实际 %q", got)
	}
	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("窗口外不该产生运行记录，实际 %d 条", len(runs))
	}

	// 3) 300ms（≈ 旧实现 15 个 tick）：日志环一条都不许增长。
	before := len(env.store.Logs(0))
	time.Sleep(300 * time.Millisecond)
	if after := len(env.store.Logs(0)); after != before {
		t.Fatalf("窗口外出现了周期性日志：%d → %d 条\n最近日志：\n%s",
			before, after, logTail(env))
	}
	if n := len(logMessages(env, "调度决策")); n != 0 {
		t.Fatalf("窗口外不该打调度决策 DEBUG，实际 %d 条：%v", n, logMessages(env, "调度决策"))
	}

	// 4) wake 能立刻穿透睡眠重新评估，但同一个休眠目标不重复打日志。
	env.srv.wakeScheduler("")
	time.Sleep(150 * time.Millisecond)
	if n := len(logMessages(env, "下一次签到窗口")); n != 1 {
		t.Fatalf("wake 后同一休眠目标不该重复打日志，实际 %d 条：%v",
			n, logMessages(env, "下一次签到窗口"))
	}

	// 5) 把窗口改成"现在就在窗口内"再 wake：必须立刻进窗口，
	//    证明睡眠不是死睡（等到 21:00 才醒）而是随时可被配置变更打断。
	cfg := baseConfig()
	cfg.Checkin.Schedule.WindowStart = "19:00"
	cfg.Checkin.Schedule.WindowEnd = "22:00"
	env.saveConfig(cfg)
	env.srv.wakeScheduler("")
	waitValue(t, 3*time.Second, "wake 穿透休眠并进入窗口", func() bool {
		return len(logMessages(env, "进入签到窗口")) == 1
	})
	if got := env.srv.lastDecisionValue(); got == "out_of_window" {
		t.Fatalf("进入窗口后 last_decision 不该还是 out_of_window")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("调度器退出超时")
	}
	if !env.srv.schedulerStopped() {
		t.Fatalf("退出后 scheduler.stopped 应为 true")
	}
}

// TestSchedulerSleepsWhenAlreadyChecked 覆盖"今日已签到后不再周期性唤醒"：
// 一条 INFO 说明休眠到次日窗口之后，整个循环静止——不复查、不打日志、不碰上游。
func TestSchedulerSleepsWhenAlreadyChecked(t *testing.T) {
	restoreTick := withFastTick(t, 20*time.Millisecond)
	defer restoreTick()

	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40)) // 窗口内、档位已过，但今天已经签成功过
	env.srv.updateState(func(st *model.State) {
		st.LastSuccessDate = env.today()
		st.LastSlotKey = env.today() + "|21:35|21:35:00"
	})

	cancel, done := env.runSchedulerAsync()
	waitSchedulerStarted(t, env)

	// 1) 恰好一条"今日已签到"，并且睡到**次日**的 21:00（11-15），不是今天剩下的时间。
	waitValue(t, 3*time.Second, "已签休眠日志", func() bool {
		return len(logMessages(env, "今日已签到")) == 1
	})
	sleeps := logMessages(env, "今日已签到")
	if !strings.Contains(sleeps[0], "11-15 21:00") {
		t.Fatalf("已签后应休眠到次日 11-15 21:00，实际日志：%s", sleeps[0])
	}
	if !strings.HasPrefix(sleeps[0], "info ") {
		t.Fatalf("已签休眠应打 INFO，实际：%s", sleeps[0])
	}

	// 2) 决策原因码保持不变（前端/状态接口依赖它），且绝不重复发起运行。
	if got := env.srv.lastDecisionValue(); got != "already_checked_today" {
		t.Fatalf("last_decision 期望 already_checked_today，实际 %q", got)
	}
	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("今日已签后不该产生运行记录，实际 %d 条", len(runs))
	}

	// 3) 300ms（≈ 旧实现 15 个 tick 的空转）：一条新日志都不许有。
	before := len(env.store.Logs(0))
	time.Sleep(300 * time.Millisecond)
	if after := len(env.store.Logs(0)); after != before {
		t.Fatalf("已签后仍在周期性唤醒：%d → %d 条\n最近日志：\n%s", before, after, logTail(env))
	}

	// 4) wake 打断睡眠后依旧只解释一次（去重），不会变成"每次唤醒打一条"。
	env.srv.wakeScheduler("")
	time.Sleep(150 * time.Millisecond)
	if n := len(logMessages(env, "今日已签到")); n != 1 {
		t.Fatalf("重复 wake 不该重复打「今日已签到」，实际 %d 条：%v", n, logMessages(env, "今日已签到"))
	}
	if got := env.srv.lastDecisionValue(); got != "already_checked_today" {
		t.Fatalf("wake 后 last_decision 期望 already_checked_today，实际 %q", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("调度器退出超时")
	}
	if !env.srv.schedulerStopped() {
		t.Fatalf("退出后 scheduler.stopped 应为 true")
	}
}
