package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

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
