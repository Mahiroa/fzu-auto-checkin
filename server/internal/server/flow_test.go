package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// runsJSONL 返回 runs.jsonl 的非空行数。
func runsJSONL(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取 runs.jsonl 失败: %v", err)
	}
	out := make([]string, 0, 4)
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// acceptRun 发一次 POST /runs 并返回 run_id。
func (e *testEnv) acceptRun(c *http.Client, payload map[string]any) string {
	e.t.Helper()
	res := e.do(c, http.MethodPost, apiPrefix+"/runs", payload)
	e.expectStatus(res, http.StatusAccepted, "POST /runs")
	var out struct {
		RunID    string `json:"run_id"`
		Accepted bool   `json:"accepted"`
	}
	decodeBody(e.t, res.body, &out)
	if !out.Accepted || out.RunID == "" {
		e.t.Fatalf("受理响应不符: %s", truncateForLog(res.body))
	}
	return out.RunID
}

// TestForceRunEndToEnd 覆盖验收清单第 6 条：假上游全链路 → success + runs.jsonl + state。
func TestForceRunEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	// 只配学号密码、不配 token：这样会真的走一遍 SSO → 回写 token（§11.2）。
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "force", "dry_run": false})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if rec.Mode != "force" {
		t.Fatalf("mode 期望 force，实际 %q", rec.Mode)
	}
	if rec.LatencyMS < 0 || rec.FinishedAt.IsZero() {
		t.Fatalf("运行记录缺少结束时间/耗时: %s", describe(rec))
	}
	if rec.Location == nil || rec.School == nil {
		t.Fatalf("成功记录应带 location/school: %s", describe(rec))
	}
	if rec.Location.Label != "single" {
		t.Fatalf("单点配置期望 label=single，实际 %q", rec.Location.Label)
	}
	if rec.School.Campus != "campus-1" {
		t.Fatalf("校区期望 campus-1，实际 %q", rec.School.Campus)
	}

	// runs.jsonl 恰好一行。
	lines := runsJSONL(t, env.dir)
	if len(lines) != 1 {
		t.Fatalf("runs.jsonl 期望 1 行，实际 %d 行", len(lines))
	}

	// state 里的 last_success_date 是"今天"。
	st := env.store.State()
	if st.LastSuccessDate != env.today() {
		t.Fatalf("last_success_date 期望 %s，实际 %q", env.today(), st.LastSuccessDate)
	}
	if st.LastRunID != id {
		t.Fatalf("last_run_id 期望 %s，实际 %q", id, st.LastRunID)
	}
	if st.LastSlotKey != "" && !strings.Contains(st.LastSlotKey, env.today()) {
		t.Fatalf("last_slot_key 格式不符: %q", st.LastSlotKey)
	}
	// DayCache 应被刷新（仪表盘重启后不必再打上游）。
	if st.DayCache == nil || st.DayCache.Date != env.today() || !st.DayCache.HasPlan {
		t.Fatalf("DayCache 未被正确写入: %+v", st.DayCache)
	}

	// SSO 只登录一次，token 已回写。
	if n := env.ssoCallCount(); n != 1 {
		t.Fatalf("SSO 登录期望 1 次，实际 %d", n)
	}
	if got := env.store.Config().User.Token; got != env.ssoToken {
		t.Fatalf("user.token 未回写，实际 %q", got)
	}

	// 假上游确实收到了 clockIn。
	initCalls, checkCalls, clockInCalls := env.school.counts()
	if clockInCalls != 1 {
		t.Fatalf("clockIn 期望 1 次，实际 %d（init=%d check=%d）", clockInCalls, initCalls, checkCalls)
	}

	// GET /runs 能看到它。
	res := env.do(c, http.MethodGet, apiPrefix+"/runs?limit=5", nil)
	env.expectStatus(res, http.StatusOK, "GET /runs")
	var list struct {
		Runs []model.RunRecord `json:"runs"`
	}
	decodeBody(t, res.body, &list)
	if len(list.Runs) != 1 || list.Runs[0].ID != id {
		t.Fatalf("GET /runs 未返回刚写入的记录: %+v", list.Runs)
	}
}

// TestRunWithTokenSkipsSSO 验证"有 token 就不该再去登录"。
func TestRunWithTokenSkipsSSO(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "preset-token-0123456789"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if n := env.ssoCallCount(); n != 0 {
		t.Fatalf("已有 token 时不该登录 SSO，实际调用 %d 次", n)
	}
}

// TestDryRunDoesNotClockIn 覆盖验收清单第 7 条。
func TestDryRunDoesNotClockIn(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "normal", "dry_run": true})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("预检期望 success（在范围内），实际 %s", describe(rec))
	}
	if rec.Mode != "probe" {
		t.Fatalf("dry_run 的 mode 期望 probe，实际 %q", rec.Mode)
	}
	_, checkCalls, clockInCalls := env.school.counts()
	if clockInCalls != 0 {
		t.Fatalf("dry_run 绝不能调用 clockIn，实际 %d 次", clockInCalls)
	}
	if checkCalls == 0 {
		t.Fatalf("dry_run 必须调用 check.action")
	}
	if rec.Location == nil || rec.School == nil {
		t.Fatalf("预检记录应带 location/school: %s", describe(rec))
	}
}

// TestDryRunOutOfRange 验证预检也能给出 out_of_range。
func TestDryRunOutOfRange(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	env.school.setNoCheckHit(true)
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "normal", "dry_run": true}), 5*time.Second)
	if rec.Status != model.StatusOutOfRange {
		t.Fatalf("期望 out_of_range，实际 %s", describe(rec))
	}
	_, _, clockInCalls := env.school.counts()
	if clockInCalls != 0 {
		t.Fatalf("越界预检不该 clockIn，实际 %d 次", clockInCalls)
	}
}

// TestRunConflictWhileRunning 覆盖验收清单第 8 条：运行中再发一次 → 409。
func TestRunConflictWhileRunning(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	gate := newSchoolGate()
	env.school.setGate(gate)

	id := env.acceptRun(c, map[string]any{"mode": "force"})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待上游 init 进入超时")
	}

	res := env.do(c, http.MethodPost, apiPrefix+"/runs", map[string]any{"mode": "force"})
	env.expectStatus(res, http.StatusConflict, "运行中再次 POST /runs")
	if code := errCode(t, res.body); code != codeRunInProgress {
		t.Fatalf("错误码期望 %s，实际 %s", codeRunInProgress, code)
	}

	// /status 期间应显示 running_now=true。
	status := env.statusOf(env.newSession())
	if !status.Scheduler.RunningNow {
		t.Fatalf("运行中 scheduler.running_now 应为 true")
	}

	close(gate.release)
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("放行后期望 success，实际 %s", describe(rec))
	}
	env.waitIdle(2 * time.Second)

	// 运行结束、单飞释放后可以再次受理。
	res = env.do(c, http.MethodPost, apiPrefix+"/runs", map[string]any{"mode": "force"})
	env.expectStatus(res, http.StatusAccepted, "结束后再次 POST /runs")
	var out struct {
		RunID string `json:"run_id"`
	}
	decodeBody(t, res.body, &out)
	env.waitRun(out.RunID, 5*time.Second)
}

// TestRunRejectsUnknownMode 验证非法 mode 直接 400（且不会占用单飞开关）。
func TestRunRejectsUnknownMode(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	res := env.do(c, http.MethodPost, apiPrefix+"/runs", map[string]any{"mode": "turbo"})
	env.expectStatus(res, http.StatusBadRequest, "非法 mode")
	if env.srv.running.Load() {
		t.Fatalf("被拒绝的请求不该占用单飞开关")
	}
}

// TestAlreadyCheckedStaysSilent 验证第二层幂等（FState=SUCCESS）与静默。
func TestAlreadyCheckedStaysSilent(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Notify.Type = "wecom"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fake"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	env.school.setChecked(true)
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusAlready {
		t.Fatalf("期望 already_checked，实际 %s", describe(rec))
	}
	_, _, clockInCalls := env.school.counts()
	if clockInCalls != 0 {
		t.Fatalf("学校端已签时绝不能 clockIn，实际 %d 次", clockInCalls)
	}
	if titles := env.notifyTitles(); len(titles) != 0 {
		t.Fatalf("already_checked 应静默，实际推送了 %d 条: %v", len(titles), titles)
	}
}

// TestNoPlanPaths 验证 init 失败与 schoolData 为空都归到 no_plan（且每日只推一次）。
func TestNoPlanPaths(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Notify.Type = "serverchan"
	cfg.Notify.ServerChanKey = "SCTfakekey"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	// schoolData 为空。
	env.school.setNoPlan(true)
	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusNoPlan {
		t.Fatalf("期望 no_plan，实际 %s", describe(rec))
	}
	if titles := env.notifyTitles(); len(titles) != 1 {
		t.Fatalf("no_plan 当日首推一次，实际 %d 条: %v", len(titles), titles)
	}

	// 再跑一次：同一天同状态不再推。
	rec = env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusNoPlan {
		t.Fatalf("期望 no_plan，实际 %s", describe(rec))
	}
	if titles := env.notifyTitles(); len(titles) != 1 {
		t.Fatalf("no_plan 当日只该推一次，实际 %d 条: %v", len(titles), titles)
	}

	// init 失败也按 no_plan 处理，不轰炸推送。
	env.school.setNoPlan(false)
	env.school.setInitFail(true)
	rec = env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusNoPlan {
		t.Fatalf("init 失败时期望 no_plan，实际 %s", describe(rec))
	}
	if titles := env.notifyTitles(); len(titles) != 1 {
		t.Fatalf("init 失败不该追加推送，实际 %d 条: %v", len(titles), titles)
	}
}

// TestClockInFailureIsError 验证 clockIn 失败 → error，且错误原文已脱敏。
func TestClockInFailureIsError(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "secret-token-should-not-leak"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	env.school.mu.Lock()
	env.school.clockInFail = true
	env.school.mu.Unlock()
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusError {
		t.Fatalf("期望 error，实际 %s", describe(rec))
	}
	if rec.Error == "" {
		t.Fatalf("error 状态必须带原文")
	}
	if strings.Contains(rec.Error, "secret-token-should-not-leak") {
		t.Fatalf("运行记录泄漏了 token: %q", rec.Error)
	}
}

// TestSSOErrorIsSanitizedAndErrors 验证 SSO 失败 → error，且密码不出现在记录/日志里。
func TestSSOErrorIsSanitizedAndErrors(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig() // 无 token → 必然走 SSO
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	env.setSSOErr(errors.New("SSO 登录失败：token 换取失败，password=" + testSchoolPwd + " 无效"))
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusError {
		t.Fatalf("期望 error，实际 %s", describe(rec))
	}
	if strings.Contains(rec.Error, testSchoolPwd) {
		t.Fatalf("错误原文泄漏了密码: %q", rec.Error)
	}
	if !strings.Contains(rec.Error, "••••") && !strings.Contains(rec.Error, "***") {
		t.Fatalf("错误原文应保留脱敏痕迹: %q", rec.Error)
	}
	_, _, clockInCalls := env.school.counts()
	if clockInCalls != 0 {
		t.Fatalf("登录失败后不该 clockIn")
	}
}

// TestVacationSkipsUpstreamEntirely 验证假期连 init 都不打（少一次上游特征）。
func TestVacationSkipsUpstreamEntirely(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Vacation.SkipRanges = []model.VacationRange{{Name: "寒假", Start: "2026-11-01", End: "2026-11-30"}}
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusVacation {
		t.Fatalf("期望 vacation，实际 %s", describe(rec))
	}
	if initCalls, _, _ := env.school.counts(); initCalls != 0 {
		t.Fatalf("假期内不该访问上游，实际 init %d 次", initCalls)
	}
	if n := env.ssoCallCount(); n != 0 {
		t.Fatalf("假期内不该登录 SSO，实际 %d 次", n)
	}
}

// TestNotifyDedupAndFailure 覆盖 §12 的去重与"推送失败不改结论"。
func TestNotifyDedupAndFailure(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "preset-token-0123456789"
	cfg.Notify.Type = "serverchan"
	cfg.Notify.ServerChanKey = "SCTfakekey"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	// 第一次成功 → 推送一次。
	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if rec.Notify == nil || !rec.Notify.OK || rec.Notify.Channel != "serverchan" {
		t.Fatalf("成功记录应带 notify.ok=true: %+v", rec.Notify)
	}
	if n := len(env.notifyTitles()); n != 1 {
		t.Fatalf("期望推送 1 次，实际 %d", n)
	}

	// 第二次同状态 → 去重，不再推。
	rec = env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if n := len(env.notifyTitles()); n != 1 {
		t.Fatalf("同一天同一状态只推一次，实际 %d 次", n)
	}

	// 推送失败：不影响 status，只落 notify.ok=false。
	// 用 error 状态来测（它不在今日已推列表里，会真的走到渠道调用）。
	env.setNotifierErr(errors.New("push gateway exploded"))
	env.school.mu.Lock()
	env.school.clockInFail = true
	env.school.mu.Unlock()
	rec = env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusError {
		t.Fatalf("clockIn 失败时期望 error，实际 %s", describe(rec))
	}
	if rec.Notify == nil || rec.Notify.OK {
		t.Fatalf("推送失败时 notify.ok 应为 false: %+v", rec.Notify)
	}
	if runs, _ := env.store.Runs(1); len(runs) == 1 && runs[0].Status != model.StatusError {
		t.Fatalf("推送失败不该改变落盘的状态: %s", describe(runs[0]))
	}
}

// TestOutOfRangeIsNotified 验证 out_of_range 必推（§12）。
func TestOutOfRangeNotified(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "preset-token-0123456789"
	cfg.Notify.Type = "bark"
	cfg.Notify.BarkURL = "https://api.day.app/fakekey"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	env.school.setNoCheckHit(true)
	c := env.authedClient()

	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusOutOfRange {
		t.Fatalf("期望 out_of_range，实际 %s", describe(rec))
	}
	if n := len(env.notifyTitles()); n != 1 {
		t.Fatalf("out_of_range 必推一次，实际 %d 次", n)
	}
	if titles := env.notifyTitles(); len(titles) == 1 && !strings.Contains(titles[0], "定位") {
		t.Fatalf("推送标题不符: %q", titles[0])
	}
}

// TestNotifyTestEndpoint 覆盖 POST /notify/test 的三种结果。
func TestNotifyTestEndpoint(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig()) // type=none
	c := env.authedClient()

	res := env.do(c, http.MethodPost, apiPrefix+"/notify/test", nil)
	env.expectStatus(res, http.StatusBadRequest, "未配置推送时测试")
	if code := errCode(t, res.body); code != codeNotifyNotConfigured {
		t.Fatalf("错误码期望 %s，实际 %s", codeNotifyNotConfigured, code)
	}

	cfg := env.store.Config()
	cfg.Notify.Type = "wecom"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fake"
	env.saveConfig(cfg)

	res = env.do(c, http.MethodPost, apiPrefix+"/notify/test", nil)
	env.expectStatus(res, http.StatusOK, "测试推送成功")
	var out struct {
		OK      bool   `json:"ok"`
		Channel string `json:"channel"`
	}
	decodeBody(t, res.body, &out)
	if !out.OK || out.Channel != "wecom" {
		t.Fatalf("测试推送响应不符: %s", truncateForLog(res.body))
	}

	env.setNotifierErr(errors.New("wecom 返回 errcode=93000"))
	res = env.do(c, http.MethodPost, apiPrefix+"/notify/test", nil)
	env.expectStatus(res, http.StatusBadGateway, "测试推送失败")
	if code := errCode(t, res.body); code != codeNotifyFailed {
		t.Fatalf("错误码期望 %s，实际 %s", codeNotifyFailed, code)
	}
	if !strings.Contains(string(res.body), "errcode=93000") {
		t.Fatalf("502 应带上游原文: %s", truncateForLog(res.body))
	}
}
