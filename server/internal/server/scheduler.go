package server

import (
	"context"
	"time"

	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
)

// 调度循环的时间参数（§10.3）。
const (
	// schedulerTick 是扫描周期。30s 足够：最坏情况是"目标时刻过了 30 秒才发现"，
	// 而 tolerance 默认 5 分钟，仍然落在"刚过点立刻签"的窗口里。
	schedulerTick = 30 * time.Second
	// clockRewindThreshold 是判定"时钟被回拨"的阈值（§10.3）。
	// 小幅 NTP 校正不值得清缓存，5 分钟以上说明发生了真正的回退（改系统时间、
	// 虚拟机快照恢复），此时任何基于"现在几点"的缓存都不可信。
	clockRewindThreshold = 5 * time.Minute
	// schedulerRunCooldown 是两次调度发起之间至少间隔多久（本地补充，见报告）。
	// 契约只写了"失败也要重试"，但失败时每个 tick 都重打一遍上游（init+SSO+check）
	// 会在半小时内刷出几十次请求，正是设计文档里最担心的"异常流量特征"。
	schedulerRunCooldown = 5 * time.Minute
)

// runScheduler 是调度循环本体：30s 扫描 + 到点定时器 + 单飞 + 补跑 + 时钟回拨保护。
func (s *Server) runScheduler(ctx context.Context) {
	if s.schedulerDisabled {
		s.setSchedulerStopped(true)
		s.logf("info", "", "调度器未启用（DisableScheduler）：只提供 API 与手动运行")
		return
	}
	s.setSchedulerStopped(false)
	defer s.setSchedulerStopped(true)
	s.logf("info", "", "调度器已启动：每 %s 扫描一次，到点由一次性定时器唤醒", schedulerTick)

	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()

	var (
		timer    *time.Timer
		waitC    <-chan time.Time
		cached   []schedule.Slot
		cacheDay string
		lastTick time.Time
		cooldown time.Time
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		waitC = nil
	}
	// 退出时必须停掉定时器，否则一个指向 22:30 的 timer 会在进程已经"关完"之后
	// 再触发一次（如果还有 goroutine 活着），那会是一次没人预期的打卡。
	defer stopTimer()

	// 启动补跑：进程可能在签到窗口内被重启（升级、OOM、蓝屏）。
	// 若"今天在窗口内 + 未签到 + 已过最后一档"，等 30 秒后的第一次 tick 也行，
	// 但那 30 秒里如果又崩一次就彻底漏签，所以启动即判。
	s.catchUp()

	for {
		firedByTimer := false
		select {
		case <-ctx.Done():
			s.logf("info", "", "调度器收到退出信号，停止扫描")
			return
		case <-ticker.C:
		case <-s.wake:
			// 配置变更/手动触发后的立即重规划。
		case <-waitC:
			// 到点了：本次循环复用进入等待时的那批 slots 重判（§10.3）。
			firedByTimer = true
			waitC = nil
		}

		now := s.now()

		// 时钟回拨：清掉 last_slot_key 重规划。不清的后果是"回拨后的时刻全部
		// 命中旧键"，整晚再也不会有任何动作。
		if !lastTick.IsZero() && now.Add(clockRewindThreshold).Before(lastTick) {
			s.logf("warn", "", "检测到系统时钟回拨（上一轮 %s，当前 %s），清空 last_slot_key 并重新规划",
				lastTick.Format(time.RFC3339), now.Format(time.RFC3339))
			s.updateState(func(st *model.State) { st.LastSlotKey = "" })
			cached, cacheDay = nil, ""
			stopTimer()
		}
		lastTick = now

		if s.running.Load() {
			// 单飞：一次打卡可能持续十几秒，期间任何 tick 都不该再发起第二次。
			continue
		}
		if now.Before(cooldown) {
			continue
		}

		cfg := s.store.Config()
		st := s.store.State()
		today := now.Format(dateLayout)

		// 幂等第二道闸：今天已经成功过就彻底不再规划。它同时省掉了"等到 21:35
		// 再发现已签到"的白等，以及上游那几次多余的 init。
		if st.LastSuccessDate == today {
			s.setLastDecision("already_checked_today")
			stopTimer()
			continue
		}

		sched := schedule.Parse(cfg.Checkin.Schedule)
		var slots []schedule.Slot
		if firedByTimer && cacheDay == today && len(cached) > 0 {
			// 复用同一批 slots：重新 PlanSlots 会重抽抖动，第二个目标可能比刚才
			// 等的那个晚出 max_wait，这一轮唤醒就白等了。
			slots = cached
		} else {
			slots = schedule.PlanSlots(sched, now, schedule.GlobalSource())
		}
		d := schedule.ResolveRun(sched, now, nil, slots)
		s.setLastDecision(d.Reason)
		s.logf("debug", "", "调度决策：%s", schedule.DecisionText(d))

		switch d.Mode {
		case "skip":
			stopTimer()

		case "wait":
			cached, cacheDay = slots, today
			stopTimer()
			delay := time.Duration(d.WaitSeconds * float64(time.Second))
			if delay < time.Second {
				// WaitSeconds 可能因浮点误差落到 0.99s，给一个最小值避免 "0 时长的定时器
				// 立刻触发 → 立刻再规划" 的忙等。
				delay = time.Second
			}
			timer = time.NewTimer(delay)
			waitC = timer.C
			s.logf("info", "", "等待到点再签：%s", schedule.DecisionText(d))

		case "run":
			stopTimer()
			slotLabel, targetLabel := "", ""
			if d.Slot != nil {
				slotLabel = d.Slot.Configured.Format("15:04")
			}
			if !d.Target.IsZero() {
				targetLabel = d.Target.Format("15:04:05")
			}

			// 幂等第一道闸：同一档位只触发一次。
			// 注意：只有成功的运行才会写 last_slot_key（见 finishRun），
			// 所以这层拦的是"成功之后同一档又被规划到"，而不是"失败不许重试"。
			key := slotKeyOf(today, slotLabel, targetLabel)
			if st.LastSlotKey == key {
				s.logf("debug", "", "档位已执行过（last_slot_key=%s），跳过", key)
				continue
			}
			if !s.tryBeginRun() {
				continue
			}
			cooldown = now.Add(schedulerRunCooldown)
			s.logf("info", "", "开始签到：%s", schedule.DecisionText(d))
			s.startRun(runRequest{
				Mode:     "scheduled",
				Decision: d.Reason,
				Slot:     slotLabel,
				Target:   targetLabel,
			})
		}
	}
}

// catchUp 是启动补跑判定（§10.3：断电/重启后仍不放过当晚）。
func (s *Server) catchUp() {
	now := s.now()
	cfg := s.store.Config()
	st := s.store.State()
	today := now.Format(dateLayout)
	if st.LastSuccessDate == today {
		return
	}

	sched := schedule.Parse(cfg.Checkin.Schedule)
	if !schedule.InWindow(now, sched.WindowStart, sched.WindowEnd) {
		return
	}
	slots := schedule.PlanSlots(sched, now, schedule.GlobalSource())
	latest := latestValidSlot(slots)
	if latest == nil {
		// 全档在窗口外：交给常规循环走 no_slot_fallback，别在这里抢跑。
		return
	}
	if !latest.Target.Before(now) {
		// 最后一档还没到点：正常等它，补跑没有意义。
		return
	}
	slotLabel := latest.Configured.Format("15:04")
	targetLabel := latest.Target.Format("15:04:05")
	if st.LastSlotKey == slotKeyOf(today, slotLabel, targetLabel) {
		return
	}
	if !s.tryBeginRun() {
		return
	}
	s.logf("info", "", "启动补跑：已过最后一档 %s 且今日未签到，按 late_fallback 立即执行一次",
		targetLabel)
	s.startRun(runRequest{
		Mode:     "scheduled",
		Decision: "late_fallback",
		Slot:     slotLabel,
		Target:   targetLabel,
	})
}

// latestValidSlot 取 Target 最晚的可用档（全无效时返回 nil）。
// PlanSlots 已按 Target 升序，所以从后往前找第一个 Valid 即可。
func latestValidSlot(slots []schedule.Slot) *schedule.Slot {
	for i := len(slots) - 1; i >= 0; i-- {
		if slots[i].Valid {
			return &slots[i]
		}
	}
	return nil
}
