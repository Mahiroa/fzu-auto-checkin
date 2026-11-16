package server

import (
	"context"
	"time"

	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
)

// 调度循环的时间参数（§10.3）。
const (
	// clockRewindThreshold 是判定"时钟被回拨"的阈值（§10.3）。
	// 小幅 NTP 校正不值得清缓存，5 分钟以上说明发生了真正的回退（改系统时间、
	// 虚拟机快照恢复），此时任何基于"现在几点"的缓存都不可信。
	clockRewindThreshold = 5 * time.Minute
	// schedulerRunCooldown 是两次调度发起之间至少间隔多久（本地补充，见报告）。
	// 契约只写了"失败也要重试"，但失败时每个 tick 都重打一遍上游（init+SSO+check）
	// 会在半小时内刷出几十次请求，正是设计文档里最担心的"异常流量特征"。
	schedulerRunCooldown = 5 * time.Minute
)

// schedulerTick 是**窗口内**的复查周期。30s 足够：最坏情况是"目标时刻过了 30 秒才发现"，
// 而 tolerance 默认 5 分钟，仍然落在"刚过点立刻签"的窗口里。
//
// 它只管窗口内——失败重试、5 分钟冷却、单飞结束后的复查。窗口外与"今日已签"两个状态
// 完全不用它：那两个状态睡的是"下一个窗口开始时刻"，一天只醒一次。
//
// 写成 var 而非常量只是为了让测试把它压到几十毫秒：30s 的常量在单测里等不起，而
// "窗口外到底还在不在轮询"必须有一个可观测的差异才能守住。生产值恒为 30s。
var schedulerTick = 30 * time.Second

// sleepOutcome 说明一次休眠是被什么结束的。
type sleepOutcome int

const (
	// sleepElapsed 表示定时器到点（到点签到，或窗口内的 30s 复查）。
	sleepElapsed sleepOutcome = iota
	// sleepWoken 表示被 s.wake 打断：配置变更或手动触发要求立刻重规划。
	sleepWoken
	// sleepCanceled 表示 ctx 已取消，调用方必须立刻退出循环。
	sleepCanceled
)

// pauseUntil 睡到 deadline（用可注入的 s.now() 换算时长），只响应三件事：
// ctx 取消、s.wake、定时器到点。
//
// 这是整个调度器**唯一**的阻塞点，所以：
//  1. 窗口外的长眠、窗口内的 30s 复查、到点签到前的等待全都走这里，
//     不存在第二条会漏掉 ctx.Done() 的阻塞路径（漏掉就是关不掉进程）；
//  2. 阻塞在 channel 上的 goroutine 不消耗 CPU——没有任何 while 循环空转，
//     单飞期间（s.running）也一样，只是等一个 30s 的定时器；
//  3. 配置变更/手动触发能立刻穿透睡眠：wake 通道容量 1 + 非阻塞发送，
//     发送方从不因没人在听而卡住。
//
// 计时器在这里创建也在这里停：退出路径不会留下一个"进程已经关完了还会触发"的定时器
// ——那会是一次没人预期的打卡。
func (s *Server) pauseUntil(ctx context.Context, deadline time.Time) sleepOutcome {
	delay := deadline.Sub(s.now())
	if delay < time.Millisecond {
		// 目标时刻已到，或时钟被前跳。给下限而不是 0：0 时长的 Timer 立刻触发，
		// 下一轮重算又是 0，那就是一个 CPU 空转。1ms 足够让下一轮算出正数时长。
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return sleepCanceled
	case <-s.wake:
		return sleepWoken
	case <-timer.C:
		return sleepElapsed
	}
}

// windowStartOn 把 schedule 的窗口起点落到 day 所在的那一天。
//
// 时区取自 WindowStart 自身（schedule.Parse 用 +08:00 造它），而不是 now 的时区：
// 窗口判定（InWindow）用的是同一个时区，两者必须同一把尺子，否则"今天 21:00"
// 在 UTC 宿主上会算成北京时间次日 05:00。
func windowStartOn(sched schedule.Settings, day time.Time) time.Time {
	loc := sched.WindowStart.Location()
	d := day.In(loc)
	start := sched.WindowStart.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(),
		start.Hour(), start.Minute(), start.Second(), 0, loc)
}

// nextWindowStart 返回**严格晚于** now 的下一个窗口开始时刻：
// 今天还没到起点就用今天，已经过了起点（自然也已经过了终点）就用明天。
//
// 这就是"窗口外不做任何轮询"的全部依据——休眠时长必须精确算到这里，
// 否则要么睡过头漏掉整晚，要么早醒变成轮询。
func nextWindowStart(sched schedule.Settings, now time.Time) time.Time {
	if today := windowStartOn(sched, now); today.After(now) {
		return today
	}
	return windowStartOn(sched, now.AddDate(0, 0, 1))
}

// runScheduler 是调度循环本体：窗口外休眠 + 窗口内 30s 复查 + 到点定时器 + 单飞 +
// 补跑 + 时钟回拨保护。
//
// 循环是一个显式状态机：每一轮只做一件事——判断当前在哪个状态，然后把"下一次再来看"
// 落成一个 time.Timer（全部经由 pauseUntil）。分支与睡眠条件：
//
//	每一轮开头
//	  └─ 时钟回拨检测：now+5min < 上一轮？→ 清 last_slot_key、作废已规划档位、重打阶段日志
//	       ↓
//	① state.LastSuccessDate == 今天（幂等第二道闸）
//	     日志：INFO「今日已签到，休眠至次日窗口 <mm-dd hh:mm>」（每次进入只一条）
//	     睡眠：windowStartOn(明天)          ← 严格跳过今天剩下的时间，不再空转
//	       ↓ 否则
//	② 不在签到窗口内（schedule.InWindow == false）
//	     决策：仍跑一次 ResolveRun 只为把 /status 的 last_decision 记成 out_of_window
//	     日志：INFO「窗口外休眠：下一次签到窗口 <mm-dd hh:mm> 开始」（每次进入只一条，
//	           不再是每 30s 一条「调度决策：跳过（不在签到窗口内）」）
//	     睡眠：nextWindowStart(now)         ← 今天未到起点→今天，已过终点→明天
//	       ↓ 否则（窗口内）
//	③ 进入签到窗口：阶段切换时 INFO 一条
//	     s.running（单飞中）  → 睡 schedulerTick，醒来再看；不忙等
//	     now < cooldown（5min）→ 睡 schedulerTick，醒来再看
//	     否则 → PlanSlots（到点唤醒时复用上一轮的档位）→ ResolveRun：
//	        run  → 幂等闸(last_slot_key) → 单飞闸(tryBeginRun) → startRun；睡 schedulerTick
//	        wait → 睡到该档 Target（复用这批档位，不重抽抖动）
//	        skip → 睡 schedulerTick（失败重试/冷却靠它复查）
//
// 唯一的日志"刷屏"风险被两处去重挡住：阶段（stage）与决策文案（decText）都只在
// **变化时**打一条。窗口外与已签状态下循环整天静止，日志环里不会再有周期性噪音。
func (s *Server) runScheduler(ctx context.Context) {
	if s.schedulerDisabled {
		s.setSchedulerStopped(true)
		s.logf("info", "", "调度器未启用（DisableScheduler）：只提供 API 与手动运行")
		return
	}
	s.setSchedulerStopped(false)
	defer s.setSchedulerStopped(true)
	s.logf("info", "", "调度器已启动：窗口外休眠到下一个窗口起点（不轮询），窗口内每 %s 复查一次",
		schedulerTick)

	var (
		// cached/cacheDay 是"这一轮等待所依据的那批档位"。
		// 到点唤醒时复用它们：重新 PlanSlots 会重抽抖动，第二个目标可能比刚才等的那个
		// 晚出 max_wait，这一轮唤醒就白等了（§10.3）。
		cached   []schedule.Slot
		cacheDay string
		// slotFire 表示"上一轮的休眠落在某一档的到点时刻上"，复用 cached 的前提。
		slotFire bool
		lastTick time.Time
		cooldown time.Time
		// stage 是当前状态机的阶段键：只在**变化**时打日志。
		stage string
		// decText 是上一次打出的决策文案：窗口内 30s 复查到同一个结论不再重复打。
		decText string
	)

	// logStage 只在阶段真正切换时写一条日志。
	// 顺带清空 decText：换了阶段之后第一条决策文案一定要打出来。
	logStage := func(key, level, format string, args ...any) {
		if stage == key {
			return
		}
		stage = key
		decText = ""
		s.logf(level, "", format, args...)
	}

	// waitUntil 是本循环唯一的"等"：睡到 deadline，返回 false 表示该退出了。
	// 被 wake 打断时作废已规划的档位——配置可能刚被改过，按旧抖动继续等就是等一个
	// 已经不存在的时刻。
	waitUntil := func(deadline time.Time) bool {
		switch s.pauseUntil(ctx, deadline) {
		case sleepCanceled:
			s.logf("info", "", "调度器收到退出信号，停止扫描")
			return false
		case sleepWoken:
			cached, cacheDay, slotFire = nil, "", false
		}
		return true
	}

	// 启动补跑：进程可能在签到窗口内被重启（升级、OOM、蓝屏）。
	// 若"今天在窗口内 + 未签到 + 已过最后一档"，等 30 秒后的第一次复查也行，
	// 但那 30 秒里如果又崩一次就彻底漏签，所以启动即判。
	s.catchUp()

	for {
		now := s.now()

		// 时钟回拨：清掉 last_slot_key 重规划。不清的后果是"回拨后的时刻全部
		// 命中旧键"，整晚再也不会有任何动作。
		if !lastTick.IsZero() && now.Add(clockRewindThreshold).Before(lastTick) {
			s.logf("warn", "", "检测到系统时钟回拨（上一轮 %s，当前 %s），清空 last_slot_key 并重新规划",
				lastTick.Format(time.RFC3339), now.Format(time.RFC3339))
			s.updateState(func(st *model.State) { st.LastSlotKey = "" })
			cached, cacheDay, slotFire = nil, "", false
			// 回拨可能跨过午夜，"今天"和休眠目标都变了，阶段键一并作废。
			stage = ""
		}
		lastTick = now

		cfg := s.store.Config()
		st := s.store.State()
		today := now.Format(dateLayout)
		sched := schedule.Parse(cfg.Checkin.Schedule)

		// ---- 状态 ①：今日已签到 → 睡到次日窗口开始 ----
		//
		// 幂等第二道闸：今天已经成功过就彻底不再规划。它同时省掉了"等到 21:35
		// 再发现已签到"的白等、上游那几次多余的 init，以及每 30 秒一次的无效唤醒。
		if st.LastSuccessDate == today {
			s.setLastDecision("already_checked_today")
			cached, cacheDay, slotFire = nil, "", false
			target := windowStartOn(sched, now.AddDate(0, 0, 1))
			logStage("sleep:checked:"+target.Format(time.RFC3339), "info",
				"今日已签到，休眠至次日窗口 %s（期间不唤醒、不再扫上游）",
				target.Format("01-02 15:04"))
			if !waitUntil(target) {
				return
			}
			continue
		}

		// ---- 状态 ②：窗口外 → 睡到下一个窗口开始 ----
		//
		// 仍然要走一次决策：/status 的 scheduler.last_decision 是仪表盘的一部分，
		// 窗口外它必须记成 out_of_window。区别只在于这一步不再每 30 秒重复一次。
		// 传 nil 档位：ResolveRun 在窗口外第一个分支就返回，不会白抽抖动。
		if !schedule.InWindow(now, sched.WindowStart, sched.WindowEnd) {
			d := schedule.ResolveRun(sched, now, nil, nil)
			s.setLastDecision(d.Reason)
			cached, cacheDay, slotFire = nil, "", false
			target := nextWindowStart(sched, now)
			logStage("sleep:out_of_window:"+target.Format(time.RFC3339), "info",
				"窗口外休眠：下一次签到窗口 %s 开始", target.Format("01-02 15:04"))
			if !waitUntil(target) {
				return
			}
			continue
		}

		// ---- 状态 ③：窗口内 ----
		logStage("active", "info", "进入签到窗口 %s-%s，按档位规划开始复查",
			sched.WindowStart.Format("15:04"), sched.WindowEnd.Format("15:04"))

		// 本轮结束后默认 30s 再来看；下面每个分支可以把它改小/改大。
		recheck := now.Add(schedulerTick)
		// 上一轮的休眠是不是"某一档到点"：读一次就清掉，别让旧标记影响后面的轮次。
		reuseSlots := slotFire
		slotFire = false

		switch {
		case s.running.Load():
			// 单飞：一次打卡可能持续十几秒，期间任何一次复查都不该再发起第二次。
			// 这里只睡 30s 等它结束——绝不 `for s.running.Load() {}` 空转，
			// 那会把一个 goroutine 变成 100% CPU 且完全不听 ctx。
		case now.Before(cooldown):
			// 失败冷却：5 分钟内不再打上游（init+SSO+check 连着打就是异常流量）。
		default:
			var slots []schedule.Slot
			if reuseSlots && cacheDay == today && len(cached) > 0 {
				slots = cached
			} else {
				slots = schedule.PlanSlots(sched, now, schedule.GlobalSource())
			}
			d := schedule.ResolveRun(sched, now, nil, slots)
			s.setLastDecision(d.Reason)

			// 决策文案只在变化时打：窗口内 30s 复查到同一个"等待/跳过"不是新信息，
			// 而它正是旧实现把用户日志刷满的那类条目。
			text := schedule.DecisionText(d)
			decided := text != decText
			if decided {
				decText = text
				s.logf("debug", "", "调度决策：%s", text)
			}

			switch d.Mode {
			case "skip":
				// 窗口内没有可做的事（等不起的档位/全部过期）：30s 后再看一次。

			case "wait":
				cached, cacheDay = slots, today
				delay := time.Duration(d.WaitSeconds * float64(time.Second))
				if delay < time.Second {
					// WaitSeconds 可能因浮点误差落到 0.99s，给一个最小值避免 "0 时长的定时器
					// 立刻触发 → 立刻再规划" 的忙等。
					delay = time.Second
				}
				if decided {
					s.logf("info", "", "等待到点再签：%s", text)
				}
				recheck = now.Add(delay)
				// 本轮休眠的终点就是这一档的到点时刻：醒来时复用这批档位重判。
				slotFire = true

			case "run":
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
					break
				}
				if !s.tryBeginRun() {
					break
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

		if !waitUntil(recheck) {
			return
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
