package server

import (
	"net/http"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// fixedDay 是状态用例统一使用的北京时间"今天"。
func fixedDay(hour, minute int) time.Time {
	return time.Date(2026, 11, 14, hour, minute, 0, 0, cstTest)
}

// statusOf 取一次 /status 并解码成契约结构。
func (e *testEnv) statusOf(c *httpClient) statusResponse {
	e.t.Helper()
	res := e.do(c.client, http.MethodGet, apiPrefix+"/status", nil)
	e.expectStatus(res, http.StatusOK, "GET /status")
	var out statusResponse
	decodeBody(e.t, res.body, &out)
	return out
}

// httpClient 只是为了让 statusOf 的签名短一点。
type httpClient struct{ client *http.Client }

func (e *testEnv) newSession() *httpClient {
	e.t.Helper()
	return &httpClient{client: e.authedClient()}
}

// TestStatusPendingSlotAndPlan 覆盖第 5 条：窗口内、pending、next_run_at 有值、
// vacation 为 null、day.plan 来自 DayCache。
func TestStatusPendingSlotAndPlan(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 10))
	today := env.today()

	// 预置 DayCache：模拟"今晚已经打过一次 init"，但还没有签到。
	env.srv.updateState(func(st *model.State) {
		st.DayCache = &model.DayPlan{
			Date:      today,
			Start:     "21:30",
			End:       "23:59",
			LateLine:  "22:30",
			HasPlan:   true,
			FetchedAt: fixedDay(21, 5),
		}
	})

	status := env.statusOf(env.newSession())

	if status.Now != "2026-11-14T21:10:00+08:00" {
		t.Fatalf("now 期望 RFC3339 北京时间，实际 %q", status.Now)
	}
	if !status.Window.Inside || status.Window.Start != "21:00" || status.Window.End != "23:59" {
		t.Fatalf("window 不符: %+v", status.Window)
	}
	if len(status.Slots) != 1 {
		t.Fatalf("slots 期望 1 档，实际 %d", len(status.Slots))
	}
	slot := status.Slots[0]
	if slot.Configured != "21:35" || slot.Target != "21:35:00" {
		t.Fatalf("档位时刻不符: %+v", slot)
	}
	if !slot.Valid || slot.Jittered || slot.Clamped || slot.Reason != "ok" {
		t.Fatalf("档位属性不符: %+v", slot)
	}
	if slot.State != slotStatePending {
		t.Fatalf("未到点时 state 期望 pending，实际 %q", slot.State)
	}
	if status.NextRunAt == nil {
		t.Fatalf("还有未来档时 next_run_at 不能是 null")
	}
	if *status.NextRunAt != "2026-11-14T21:35:00+08:00" {
		t.Fatalf("next_run_at 不符: %q", *status.NextRunAt)
	}
	if status.Vacation != nil {
		t.Fatalf("无假期时 vacation 必须是 null，实际 %+v", status.Vacation)
	}
	if status.Day.Date != today || status.Day.CheckedIn {
		t.Fatalf("day 不符: %+v", status.Day)
	}
	if !status.Day.Plan.HasPlan || status.Day.Plan.Start != "21:30" ||
		status.Day.Plan.End != "23:59" || status.Day.Plan.LateLine != "22:30" {
		t.Fatalf("day.plan 应来自 DayCache: %+v", status.Day.Plan)
	}
	if status.Version != Version {
		t.Fatalf("version 期望 %s，实际 %q", Version, status.Version)
	}
	if status.ConfigVersion != env.store.ConfigVersion() {
		t.Fatalf("config_version 不符: %d != %d", status.ConfigVersion, env.store.ConfigVersion())
	}
	if status.Scheduler.Enabled {
		t.Fatalf("DisableScheduler 的测试环境下 scheduler.enabled 应为 false: %+v", status.Scheduler)
	}
	if !status.Scheduler.Stopped || status.Scheduler.RunningNow {
		t.Fatalf("scheduler 状态不符: %+v", status.Scheduler)
	}
	if status.LastRun != nil {
		t.Fatalf("还没有运行记录时 last_run 应为 null")
	}
}

// TestStatusSchedulerEnabled 验证启用调度器时 enabled=true（前端据此显示"等待下一档"）。
func TestStatusSchedulerEnabled(t *testing.T) {
	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 10))

	status := env.statusOf(env.newSession())
	if !status.Scheduler.Enabled {
		t.Fatalf("启用调度器时 scheduler.enabled 应为 true: %+v", status.Scheduler)
	}
	if status.Scheduler.RunningNow {
		t.Fatalf("空闲时 running_now 应为 false")
	}
}

// TestStatusMissedAndNoNextRun 覆盖第 5 条：过了档位 → missed，且 next_run_at 为 null。
func TestStatusMissedAndNoNextRun(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(23, 30)) // 已过 21:35，但仍在窗口内

	status := env.statusOf(env.newSession())
	if !status.Window.Inside {
		t.Fatalf("23:30 应仍在 21:00-23:59 窗口内")
	}
	if status.Slots[0].State != slotStateMissed {
		t.Fatalf("过了档位后期望 missed，实际 %q", status.Slots[0].State)
	}
	if status.NextRunAt != nil {
		t.Fatalf("没有未来档时 next_run_at 必须是 null，实际 %q", *status.NextRunAt)
	}
}

// TestStatusOutsideWindow 覆盖窗口外判定。
func TestStatusOutsideWindow(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(20, 0))

	status := env.statusOf(env.newSession())
	if status.Window.Inside {
		t.Fatalf("20:00 应在窗口外")
	}
	if status.Slots[0].State != slotStatePending {
		t.Fatalf("还没到点的档位仍是 pending，实际 %q", status.Slots[0].State)
	}
}

// TestStatusDoneAfterSuccess 覆盖"今日已签 → 全 done"。
func TestStatusDoneAfterSuccess(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	today := env.today()

	env.srv.updateState(func(st *model.State) {
		st.LastSuccessDate = today
		st.DayCache = &model.DayPlan{Date: today, Start: "21:00", End: "23:59",
			LateLine: "22:30", HasPlan: true, Checked: true}
	})

	status := env.statusOf(env.newSession())
	if !status.Day.CheckedIn {
		t.Fatalf("已签到后 day.checked_in 应为 true")
	}
	if status.Day.LastSuccessDate != today {
		t.Fatalf("last_success_date 不符: %q", status.Day.LastSuccessDate)
	}
	if status.Slots[0].State != slotStateDone {
		t.Fatalf("已签到后档位应为 done，实际 %q", status.Slots[0].State)
	}
}

// TestStatusDayCacheFromOtherDay 覆盖"DayCache 跨天即作废"。
func TestStatusDayCacheFromOtherDay(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 10))

	env.srv.updateState(func(st *model.State) {
		st.DayCache = &model.DayPlan{Date: "2026-11-13", Start: "21:30", End: "23:59",
			LateLine: "22:30", HasPlan: true}
	})

	status := env.statusOf(env.newSession())
	if status.Day.Plan.HasPlan {
		t.Fatalf("昨天的 DayCache 不该被当成今天的计划: %+v", status.Day.Plan)
	}
	if status.Day.Plan.Start != "" || status.Day.Plan.End != "" || status.Day.Plan.LateLine != "" {
		t.Fatalf("跨天的 DayCache 各字段应为空串: %+v", status.Day.Plan)
	}
}

// TestStatusVacation 覆盖假期命中：返回 name/start/end，而不是 {}。
func TestStatusVacation(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Vacation.SkipRanges = []model.VacationRange{
		{Name: "寒假", Start: "2026-11-01", End: "2026-11-30"},
	}
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 10))

	status := env.statusOf(env.newSession())
	if status.Vacation == nil {
		t.Fatalf("假期区间内 vacation 不能是 null")
	}
	if status.Vacation.Name != "寒假" || status.Vacation.Start != "2026-11-01" || status.Vacation.End != "2026-11-30" {
		t.Fatalf("vacation 字段不符: %+v", status.Vacation)
	}

	// 区间外 → null。
	env.setNow(fixedDay(21, 10).AddDate(0, 2, 0))
	status = env.statusOf(env.newSession())
	if status.Vacation != nil {
		t.Fatalf("区间外 vacation 必须是 null，实际 %+v", status.Vacation)
	}
}

// TestStatusSlotsWithJitter 覆盖"抖动/钳制"的可视化字段。
func TestStatusSlotsWithJitter(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Checkin.Schedule.TimeJitterMinutes = float64Ptr(5)
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 10))

	status := env.statusOf(env.newSession())
	if len(status.Slots) != 1 {
		t.Fatalf("期望 1 档，实际 %d", len(status.Slots))
	}
	slot := status.Slots[0]
	if !slot.Jittered {
		t.Fatalf("开启时间抖动后期望 jittered=true: %+v", slot)
	}
	if slot.Reason != "ok" {
		t.Fatalf("抖动落在窗口内时 reason 应为 ok，实际 %q", slot.Reason)
	}
	if len(slot.Target) != len("21:35:00") {
		t.Fatalf("target 应是 HH:MM:SS，实际 %q", slot.Target)
	}
}

// TestRunsListAndDetail 覆盖 /runs 与 /runs/{id}。
func TestRunsListAndDetail(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	base := fixedDay(21, 0)
	for i := 0; i < 3; i++ {
		rec := model.RunRecord{
			ID:        "20261114T21000" + string(rune('0'+i)) + "-abcd",
			Mode:      "scheduled",
			StartedAt: base.Add(time.Duration(i) * time.Minute),
			Status:    model.StatusSuccess,
		}
		rec.FinishedAt = rec.StartedAt.Add(2 * time.Second)
		if err := env.store.AppendRun(rec); err != nil {
			t.Fatalf("写入运行记录失败: %v", err)
		}
	}

	res := env.do(c, http.MethodGet, apiPrefix+"/runs", nil)
	env.expectStatus(res, http.StatusOK, "GET /runs")
	var list struct {
		Runs []model.RunRecord `json:"runs"`
	}
	decodeBody(t, res.body, &list)
	if len(list.Runs) != 3 {
		t.Fatalf("期望 3 条运行记录，实际 %d", len(list.Runs))
	}
	// 新的在前。
	if !list.Runs[0].StartedAt.After(list.Runs[2].StartedAt) {
		t.Fatalf("runs 应按时间倒序: %+v", list.Runs)
	}

	res = env.do(c, http.MethodGet, apiPrefix+"/runs?limit=2", nil)
	decodeBody(t, res.body, &list)
	if len(list.Runs) != 2 {
		t.Fatalf("limit=2 期望 2 条，实际 %d", len(list.Runs))
	}

	id := list.Runs[0].ID
	res = env.do(c, http.MethodGet, apiPrefix+"/runs/"+id, nil)
	env.expectStatus(res, http.StatusOK, "GET /runs/{id}")
	var single struct {
		Run model.RunRecord `json:"run"`
	}
	decodeBody(t, res.body, &single)
	if single.Run.ID != id {
		t.Fatalf("详情返回了错误的记录: %q != %q", single.Run.ID, id)
	}

	res = env.do(c, http.MethodGet, apiPrefix+"/runs/nope", nil)
	env.expectStatus(res, http.StatusNotFound, "GET /runs/nope")
	if code := errCode(t, res.body); code != codeRunNotFound {
		t.Fatalf("错误码期望 %s，实际 %s", codeRunNotFound, code)
	}
}
