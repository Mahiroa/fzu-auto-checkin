package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
)

// 档位状态的三个取值（前端 SLOT_TEXT 按这三个键取文案）。
const (
	slotStatePending = "pending"
	slotStateDone    = "done"
	slotStateMissed  = "missed"
)

// ---------- GET /api/v1/status（§9.1）----------

type statusResponse struct {
	Now           string           `json:"now"`
	Window        statusWindow     `json:"window"`
	Slots         []statusSlot     `json:"slots"`
	NextRunAt     *string          `json:"next_run_at"`
	Day           statusDay        `json:"day"`
	Vacation      *statusVacation  `json:"vacation"`
	Scheduler     statusScheduler  `json:"scheduler"`
	LastRun       *model.RunRecord `json:"last_run"`
	Version       string           `json:"version"`
	ConfigVersion int64            `json:"config_version"`
}

type statusWindow struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Inside bool   `json:"inside"`
}

type statusSlot struct {
	Configured string `json:"configured"` // HH:MM
	Target     string `json:"target"`     // HH:MM:SS
	Valid      bool   `json:"valid"`
	Jittered   bool   `json:"jittered"`
	Clamped    bool   `json:"clamped"`
	Reason     string `json:"reason"`
	State      string `json:"state"`
}

type statusDay struct {
	Date            string        `json:"date"`
	CheckedIn       bool          `json:"checked_in"`
	LastSuccessDate string        `json:"last_success_date"`
	Plan            statusDayPlan `json:"plan"`
}

type statusDayPlan struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	LateLine string `json:"late_line"`
	HasPlan  bool   `json:"has_plan"`
}

// statusVacation 命中假期时的形态。未命中时整个字段必须是 null：
// 前端用 `if (vac)` 判真假，给 {} 会让它显示"假期中：假期区间"。
type statusVacation struct {
	Name  string `json:"name"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type statusScheduler struct {
	Enabled      bool   `json:"enabled"`
	Stopped      bool   `json:"stopped"`
	RunningNow   bool   `json:"running_now"`
	LastDecision string `json:"last_decision"`
}

// handleStatus 组装仪表盘所需的全部信息。
//
// 这里每次都重新 PlanSlots：抖动是"每天每档一次"的行为，缓存住抖动结果没必要，
// 而重新规划能让仪表盘展示的 target 与调度器即将执行的那一刻尽量一致
// （差一次抽签，量级是分钟级抖动内，可接受）。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Config()
	st := s.store.State()
	now := s.now()
	today := now.Format(dateLayout)

	sched := schedule.Parse(cfg.Checkin.Schedule)
	slots := schedule.PlanSlots(sched, now, schedule.GlobalSource())

	resp := statusResponse{
		Now: now.Format(time.RFC3339),
		Window: statusWindow{
			Start:  sched.WindowStart.Format("15:04"),
			End:    sched.WindowEnd.Format("15:04"),
			Inside: schedule.InWindow(now, sched.WindowStart, sched.WindowEnd),
		},
		Version:       Version,
		ConfigVersion: s.store.ConfigVersion(),
	}

	// §9.1：今日已成功 → 全部 done；否则"目标已过但没成功"= missed，其余 pending。
	// 这里的"已成功"只看 state.last_success_date：真正的权威是学校端 FState，
	// 但那需要一次上游请求，仪表盘不该为了画一个绿点去打卡接口。
	doneToday := st.LastSuccessDate == today
	resp.Slots = make([]statusSlot, 0, len(slots))
	for _, sl := range slots {
		state := slotStatePending
		switch {
		case doneToday:
			state = slotStateDone
		case sl.Target.Before(now):
			state = slotStateMissed
		}
		resp.Slots = append(resp.Slots, statusSlot{
			Configured: sl.Configured.Format("15:04"),
			Target:     sl.Target.Format("15:04:05"),
			Valid:      sl.Valid,
			Jittered:   sl.Jittered,
			Clamped:    sl.Clamped,
			Reason:     sl.Reason,
			State:      state,
		})
	}

	// 零值 time.Time 会被编码成 "0001-01-01T00:00:00Z"，前端倒计时会显示 0001 年。
	// 没有未来档就给 null。
	if next := schedule.NextRunAt(sched, slots, now); !next.IsZero() {
		text := next.Format(time.RFC3339)
		resp.NextRunAt = &text
	}

	resp.Day = statusDay{
		Date:            today,
		CheckedIn:       doneToday,
		LastSuccessDate: st.LastSuccessDate,
	}
	if st.DayCache != nil {
		// DayCache 只代表"最近一次 init 看到的计划"：跨天之后它已经过期，
		// 继续展示会让仪表盘在假期/无计划日显示昨天的时段。
		if st.DayCache.Checked && st.DayCache.Date == today {
			resp.Day.CheckedIn = true
		}
		if st.DayCache.Date == today {
			resp.Day.Plan = statusDayPlan{
				Start:    st.DayCache.Start,
				End:      st.DayCache.End,
				LateLine: st.DayCache.LateLine,
				HasPlan:  st.DayCache.HasPlan,
			}
		}
	}

	resp.Vacation = s.vacationOf(cfg, now)
	resp.Scheduler = statusScheduler{
		Enabled:      s.schedulerEnabled(),
		Stopped:      s.schedulerStopped(),
		RunningNow:   s.running.Load(),
		LastDecision: s.lastDecisionValue(),
	}

	if runs, err := s.store.Runs(1); err == nil && len(runs) > 0 {
		rec := runs[0]
		resp.LastRun = &rec
	}
	writeJSON(w, http.StatusOK, resp)
}

// vacationOf 判断今天是否落在假期区间内，并把区间名与起止日期一起回给前端。
//
// 基础包的 fzu.MatchedVacation 只回名字：前端要显示"寒假（2027-01-23 ~ 2027-02-21）"，
// 而名字默认值本身又是由日期拼的，靠名字反查不可靠。所以这里用同一口径（含端点、
// 跳过起止颠倒的区间）再定位一次具体区间——判定结论仍以 fzu.MatchedVacation 为准，
// 本函数只补展示字段。
func (s *Server) vacationOf(cfg model.Config, today time.Time) *statusVacation {
	name, hit := fzu.MatchedVacation(cfg, today)
	if !hit {
		return nil
	}
	out := &statusVacation{Name: name}
	year, month, day := today.Date()
	target := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	for _, item := range cfg.Vacation.SkipRanges {
		start, okStart := parseDay(item.Start)
		end, okEnd := parseDay(item.End)
		if !okStart || !okEnd || start.After(end) {
			continue
		}
		if target.Before(start) || target.After(end) {
			continue
		}
		out.Start = start.Format(dateLayout)
		out.End = end.Format(dateLayout)
		break
	}
	return out
}

// parseDay 按"纯日历日期"解析，与 fzu.MatchedVacation 内部口径一致（UTC 零点）。
func parseDay(text string) (time.Time, bool) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(text), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ---------- GET /api/v1/runs ----------

// 分页参数：默认 20、上限 500（§9 的 limit 分页，前端用"加载更多"递增 limit）。
const (
	runsDefaultLimit = 20
	runsMaxLimit     = 500
)

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit := runsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > runsMaxLimit {
		limit = runsMaxLimit
	}
	runs, err := s.store.Runs(limit)
	if err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "读取运行历史失败："+err.Error()))
		return
	}
	if runs == nil {
		runs = []model.RunRecord{} // JSON 里必须是 []，前端直接 Array.isArray
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleRunByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, newError(http.StatusNotFound, codeRunNotFound, "运行记录 id 为空"))
		return
	}
	rec, ok := s.store.RunByID(id)
	if !ok {
		writeError(w, newError(http.StatusNotFound, codeRunNotFound,
			"没有找到这条运行记录（可能已被 runs.jsonl 轮转删除）"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": rec})
}
