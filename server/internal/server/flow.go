package server

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
	"fzucheckin/internal/secure"
)

// flow（§11.3）的常量。
const (
	// maxRunLogLines 是 RunRecord.logs 的条数上限：一份能贴进 issue 的摘要，
	// 而不是整晚的调试流水（完整日志在内存环与 SSE 里）。
	maxRunLogLines = 20
	// maxErrorTextLen 是 RunRecord.error 的长度上限。上游有时回一整个 HTML
	// 错误页，原样落盘会让 runs.jsonl 迅速膨胀。
	maxErrorTextLen = 600
	// notifyErrorWindow 是"失败通知 30 分钟内不重复"的窗口（§12）。
	notifyErrorWindow = 30 * time.Minute
)

// runRequest 描述一次运行。调度器与手动触发都走同一条 flow，
// 差别只在 Mode/Decision/Slot/Target 这几个展示字段。
type runRequest struct {
	ID       string // 空则由 flow 自己生成
	Mode     string // scheduled|force|manual|probe
	DryRun   bool
	Decision string
	Slot     string
	Target   string
}

// newRunID 生成 20060102T150405-<4hex>（§F）。
// 用 4 位随机十六进制而不是自增序号：重启后序号会重复，而随机串在
// runs.jsonl + 日志 + 推送三处交叉引用时基本不会撞。
func newRunID(t time.Time) string {
	return t.Format("20060102T150405") + "-" + secure.RandomHex(2)
}

// startRun 在后台起一次运行并把生命周期记进 runWG。
//
// Add 必须在 goroutine 之外调用：放进 goroutine 里就会出现"Wait 已经开始、
// 计数还没加上"的窗口，WaitGroup 会误判为"没有在飞的运行"而提前退出。
func (s *Server) startRun(req runRequest) {
	s.runWG.Add(1)
	go func() {
		// defer 顺序：先 endRun 释放单飞开关，再 Done 通知等待方。
		// 反过来的话，"等到 WaitGroup 归零"时 running 可能还是 true，
		// 刚重启的调度器第一轮就会以为"有运行在进行"。
		defer s.runWG.Done()
		defer s.endRun()
		s.flowRun(req)
	}()
}

// flowRun 是一次签到的完整编排：假期 → token → init → 位置 → check → clockIn → 推送。
//
// 所有分支都靠 defer 收尾，保证"任何路径都会写好 RunRecord 并广播 event: run"
// ——历史上最难受的故障就是"日志里说签了，历史里查不到"，用户无法自证。
func (s *Server) flowRun(req runRequest) {
	startedAt := s.now()
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newRunID(startedAt)
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "manual"
	}
	if req.DryRun {
		// 预检不提交 clockIn，必须在 mode 上区分开，否则历史里两类记录分不清。
		mode = "probe"
	}
	today := startedAt.Format(dateLayout)

	rec := model.RunRecord{
		ID:        id,
		Mode:      mode,
		StartedAt: startedAt,
		Decision:  req.Decision,
		Slot:      req.Slot,
		Target:    req.Target,
	}
	var logs []string
	addLog := func(level, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if len(logs) < maxRunLogLines {
			logs = append(logs, msg)
		}
		// 用 %s 而不是把 msg 当格式串：msg 里可能带上游返回的 %s、%d，
		// 直接当格式串会打出 "!s(int=...)" 这种鬼东西。
		s.logf(level, id, "%s", msg)
	}

	cfg := s.store.Config()
	// cfg 会被 SSO 登录刷新（token 回写），defers 在返回时才读取变量，
	// 因此推送与脱敏拿到的都是最终形态的配置。
	defer func() {
		s.finishRun(&rec, logs, startedAt, today, cfg)
	}()

	addLog("info", "开始运行 id=%s mode=%s decision=%s", id, mode, orDash(req.Decision))

	// 1. 假期：命中就什么都不做。它连 token 都不该去换——假期里登录上游
	// 本身就是一种异常特征，而且会让"假期不推消息"的承诺失效。
	if name, hit := fzu.MatchedVacation(cfg, startedAt); hit {
		rec.Status = model.StatusVacation
		addLog("info", "今天落在假期区间「%s」内，本次跳过（不打卡、不推送）", name)
		return
	}

	// 2. token：没有就 SSO 换一个并回写配置（§11.2）。token 会过期，而账密
	// 不会，所以这是让服务能连续跑几个月不断签的关键一步。
	token := strings.TrimSpace(cfg.User.Token)
	if token == "" {
		addLog("info", "没有可用的 user.token，使用学号密码登录 SSO…")
		fresh, err := s.ssoLogin(cfg.User.Username, cfg.User.Password)
		if err != nil {
			rec.Status = model.StatusError
			rec.Error = s.sanitizeError(err.Error(), cfg)
			addLog("error", "SSO 登录失败：%s", rec.Error)
			return
		}
		token = strings.TrimSpace(fresh)
		if token == "" {
			rec.Status = model.StatusError
			rec.Error = "SSO 登录返回了空 token（登录页结构可能已变化）"
			addLog("error", "%s", rec.Error)
			return
		}
		cfg.User.Token = token
		// 回写配置会让 config.version +1：token 是配置的一部分（§7 不单独存 token）。
		// 必须走 UpdateConfig（写锁内重读最新配置再改），否则会覆盖掉这十几秒里
		// 用户刚通过 PUT /config 改的密码/坐标/推送凭据（lost update）。
		if _, err := s.store.UpdateConfig(func(latest *model.Config) []string {
			latest.User.Token = token
			return []string{"已通过 SSO 登录自动刷新 user.token"}
		}); err != nil {
			addLog("warn", "登录成功但回写 user.token 失败：%s", s.sanitizeError(err.Error(), cfg))
		} else {
			addLog("info", "SSO 登录成功，已回写 user.token=%s", maskSecret(token))
		}
	}

	// 3. init：拿今日计划与学校端状态。
	client := s.clientFactory(token)
	initData, err := client.Init()
	if err != nil {
		// §11.3：init 失败按 no_plan 处理。这类失败（网关 500、维护页）通常是
		// 一时一过，不该每 30 秒推一次"签到失败"把人叫醒。
		// 但必须留下上游原文：把"上游挂了"和"今天真的没有晚点名"混成同一句话，
		// 会让用户在看到「今日无计划」时误判"今晚不用签"。
		rec.Status = model.StatusNoPlan
		rec.Error = s.sanitizeError(err.Error(), cfg)
		addLog("warn", "init 未成功，按「今日无计划」处理：%s", rec.Error)
		return
	}

	// 4. 解析 + 写 DayCache。
	plan, err := fzu.ParseInit(initData)
	if err != nil {
		rec.Status = model.StatusError
		rec.Error = s.sanitizeError(err.Error(), cfg)
		addLog("error", "init 响应解析失败：%s", rec.Error)
		return
	}
	s.updateState(func(st *model.State) {
		// 每次都整份替换：DayPlan 里带 FetchedAt，指针复用会让 store 的
		// 克隆语义（浅拷贝指针）与"就地改"混在一起，出现读到半更新状态的窗口。
		st.DayCache = &model.DayPlan{
			Date:      today,
			Start:     plan.Start,
			End:       plan.End,
			LateLine:  plan.LateLine,
			HasPlan:   plan.HasPlan,
			Checked:   plan.Checked,
			FetchedAt: s.now(),
		}
	})
	addLog("info", "init 成功：FState=%s 计划 %s-%s 迟到线 %s 签到范围 %d 个",
		orDash(plan.FState), orDash(plan.Start), orDash(plan.End),
		orDash(plan.LateLine), len(plan.Polygons))

	// 5. 幂等第三道闸：学校端已签。
	if plan.Checked {
		rec.Status = model.StatusAlready
		addLog("info", "学校端状态为「%s」，今日已签到，不重复提交", orDash(plan.FState))
		return
	}
	// 6. 没有计划。
	if !plan.HasPlan {
		rec.Status = model.StatusNoPlan
		addLog("warn", "今日没有晚点名计划（schoolData 为空），本次跳过")
		return
	}

	// 执行前再校验一次窗口：force 就该能签（用户明确要覆盖），但必须留痕。
	// 不校验的后果是——时钟跑飞/配置写错导致的"半夜三点签到"在事后无从发现。
	s.verifyWindow(cfg, startedAt, addLog)

	// 位置抖动与位置池抽签共用一个随机源，注入点与 schedule 包一致。
	draw := schedule.GlobalSource().Float64

	if req.DryRun {
		s.runProbe(cfg, client, plan, draw, &rec, addLog)
		return
	}

	// 7. 真正的打卡。
	result, err := fzu.DoCheckin(client, cfg, initData, draw, func(format string, args ...any) {
		addLog("info", format, args...)
	})
	if err != nil {
		switch {
		case errors.Is(err, fzu.ErrOutOfRange):
			rec.Status = model.StatusOutOfRange
			rec.Error = s.sanitizeError(err.Error(), cfg)
			addLog("warn", "%s", rec.Error)
		case errors.Is(err, fzu.ErrNoPlan):
			rec.Status = model.StatusNoPlan
			addLog("warn", "打卡途中发现当天已无签到范围：%s", s.sanitizeError(err.Error(), cfg))
		default:
			rec.Status = model.StatusError
			rec.Error = s.sanitizeError(err.Error(), cfg)
			addLog("error", "签到失败：%s", rec.Error)
		}
		return
	}

	rec.Status = model.StatusSuccess
	rec.IsLate = result.IsLate
	place := result.Place
	rec.Location = &model.RunLocation{
		Lon:            place.Lon,
		Lat:            place.Lat,
		Label:          place.Label,
		Jittered:       result.Jittered,
		ActualLocation: place.ActualLocation,
	}
	rec.School = &model.RunSchool{Campus: result.Campus, Matched: result.Matched}
	addLog("info", "clockIn 成功：校区 %s 坐标 %.5f,%.5f（%s，抖动=%t）迟到=%t",
		orDash(result.Campus), place.Lon, place.Lat, orDash(place.Label), result.Jittered, result.IsLate)
}

// runProbe 是 dry_run 路径：只做 init + 位置解析 + check.action，绝不 clockIn（§F）。
//
// 它的价值是让用户在白天的任意时刻回答一个问题——"我这个坐标现在能不能签"——
// 而不产生一条真实的签到记录。
func (s *Server) runProbe(cfg model.Config, client *fzu.Client, plan fzu.Plan,
	draw fzu.RandFunc, rec *model.RunRecord, addLog func(level, format string, args ...any)) {

	place, err := fzu.ResolveLocation(cfg, draw)
	if err != nil {
		rec.Status = model.StatusError
		rec.Error = s.sanitizeError(err.Error(), cfg)
		addLog("error", "位置解析失败：%s", rec.Error)
		return
	}
	radius := fzu.JitterRadius(cfg)
	lon, lat := place.Lon, place.Lat
	jittered := false
	if radius > 0 {
		lon, lat = fzu.JitterLocation(place.Lon, place.Lat, radius, draw)
		jittered = true
	}

	matched, err := client.Check(lon, lat, plan.Polygons)
	if err != nil {
		rec.Status = model.StatusError
		rec.Error = s.sanitizeError(err.Error(), cfg)
		addLog("error", "check 请求失败：%s", rec.Error)
		return
	}
	if len(matched) == 0 && jittered {
		// 抖动点越界时回退基准点：预检要回答的是"我填的坐标能不能签"，
		// 不是"这次随机偏出去的那一点能不能签"。
		addLog("info", "抖动点不在任何签到范围内，回退到配置坐标重试")
		lon, lat, jittered = place.Lon, place.Lat, false
		matched, err = client.Check(lon, lat, plan.Polygons)
		if err != nil {
			rec.Status = model.StatusError
			rec.Error = s.sanitizeError(err.Error(), cfg)
			addLog("error", "check 请求失败：%s", rec.Error)
			return
		}
	}

	rec.Location = &model.RunLocation{
		Lon:            lon,
		Lat:            lat,
		Label:          place.Label,
		Jittered:       jittered,
		ActualLocation: place.ActualLocation,
	}
	if len(matched) == 0 {
		rec.Status = model.StatusOutOfRange
		rec.Error = fmt.Sprintf("定位校验未命中任何签到范围（经度 %.5f 纬度 %.5f，来源 %s）",
			lon, lat, place.Label)
		addLog("warn", "%s", rec.Error)
		return
	}

	rec.Status = model.StatusSuccess
	rec.School = &model.RunSchool{Campus: matched[0].ID, Matched: true}
	addLog("info", "定位预检通过：命中 %d 个签到范围（首个 %s %s），本次未提交 clockIn",
		len(matched), orDash(matched[0].ID), orDash(matched[0].Name))
}

// verifyWindow 只告警不拦截（§10.1：force 也校验窗口）。
func (s *Server) verifyWindow(cfg model.Config, now time.Time,
	addLog func(level, format string, args ...any)) {

	sched := schedule.Parse(cfg.Checkin.Schedule)
	if err := schedule.VerifyInWindow(now, sched.WindowStart, sched.WindowEnd); err != nil {
		addLog("warn", "窗口校验未通过，仍按配置继续执行（仅告警）：%s", err.Error())
	}
}

// finishRun 是 flow 的唯一出口：补齐时间/日志/推送，落盘 RunRecord，更新 state，
// 广播 event: run。
func (s *Server) finishRun(rec *model.RunRecord, logs []string, startedAt time.Time,
	today string, cfg model.Config) {

	// 兜底：任何未被归类就 return 的路径都记成 error，而不是留一个空 status
	// ——前端把空 status 显示成 "—"，等于一次无痕的失败。
	if rec.Status == "" {
		rec.Status = model.StatusError
		if rec.Error == "" {
			rec.Error = "运行未产生结论（内部错误）"
		}
	}
	rec.FinishedAt = s.now()
	rec.LatencyMS = rec.FinishedAt.Sub(startedAt).Milliseconds()
	if rec.LatencyMS < 0 {
		rec.LatencyMS = 0
	}
	rec.Logs = append([]string(nil), logs...)

	// 推送先于落盘：RunRecord.notify 要带上推送结果，落盘一次就够（§9.3）。
	// notifyRun 内部保证失败不改变 rec.Status。
	s.notifyRun(cfg, rec, today)

	if err := s.store.AppendRun(*rec); err != nil {
		s.logf("error", rec.ID, "写入运行历史失败：%v", err)
	}
	s.updateState(func(st *model.State) {
		st.LastAttemptAt = startedAt
		st.LastRunID = rec.ID
		if rec.Status == model.StatusSuccess && rec.Mode != "probe" {
			// 只有真正打卡成功才写 last_success_date 与 last_slot_key：
			//  - 失败必须允许下一轮重试，否则一次网络抖动就把当晚补签名额用掉了；
			//  - 预检（probe）只是回答"这个坐标能不能签"，把它记成已签会让调度器
			//    整晚判定 already_checked_today 从而彻底漏签——最危险的一种"成功"。
			st.LastSuccessDate = today
			st.LastSlotKey = slotKeyOf(today, rec.Slot, rec.Target)
		}
	})
	s.hub.broadcast(eventRun, map[string]any{
		"id":     rec.ID,
		"status": rec.Status,
		"slot":   rec.Slot,
		"target": rec.Target,
	})
	s.logf(statusLogLevel(rec.Status), rec.ID, "运行结束 status=%s latency=%dms decision=%s",
		rec.Status, rec.LatencyMS, orDash(rec.Decision))
}

// ---------- 通知（§12）----------

// notifyRun 按去重规则推送一条结论。
//
// 三条规则（顺序即优先级）：
//  1. 没配渠道 → 什么都不做（不记录 notify 字段，前端显示"未推送"）；
//  2. 状态本身是否值得打扰：success/out_of_range/error 必推，no_plan 每天一次，
//     already_checked/skipped 静默，vacation 只在 vacation.notify 时推；
//  3. 同一天同一状态只推一次；error 例外，但 30 分钟内不重复（重试要能看到进展，
//     又不能在每次 tick 重试时把人吵醒）。
func (s *Server) notifyRun(cfg model.Config, rec *model.RunRecord, today string) {
	// 预检不是签到：即使结论是"位置在范围内"，也不该推一条「签到成功」出去。
	if rec.Mode == "probe" {
		return
	}
	channel := strings.ToLower(strings.TrimSpace(cfg.Notify.Type))
	if channel == "" || channel == "none" {
		return
	}
	if !notifyWanted(cfg, rec.Status) {
		return
	}
	if rec.Status != model.StatusError && historyContains(s.store.State().Notified[today], rec.Status) {
		s.logf("debug", rec.ID, "今日已推送过 %s，跳过重复推送", rec.Status)
		return
	}
	if rec.Status == model.StatusError && s.errorNotifyRecent(rec.FinishedAt, rec.ID) {
		s.logf("debug", rec.ID, "30 分钟内已推送过失败通知，本次不重复")
		return
	}

	title, content := buildNotifyMessage(rec)
	err := s.notifier(cfg, title, content)
	rec.Notify = &model.RunNotify{Channel: channel, OK: err == nil}
	if err != nil {
		// 推送失败只记账，绝不把成功改成失败：签到结论是上游的，推送只是通知。
		s.logf("warn", rec.ID, "推送失败（签到结论不变 status=%s）：%s",
			rec.Status, s.sanitizeError(err.Error(), cfg))
		return
	}
	s.updateState(func(st *model.State) {
		st.Notified[today] = appendUnique(st.Notified[today], rec.Status)
	})
}

// notifyWanted 判断某状态是否值得推送（§12）。
func notifyWanted(cfg model.Config, status string) bool {
	switch status {
	case model.StatusSuccess, model.StatusOutOfRange, model.StatusError, model.StatusNoPlan:
		return true
	case model.StatusVacation:
		return cfg.Vacation.Notify
	}
	return false
}

func historyContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func appendUnique(items []string, value string) []string {
	if historyContains(items, value) {
		return items
	}
	return append(items, value)
}

// errorNotifyRecent 检查最近是否已经成功推送过失败通知。
//
// 判据取自已落盘的 RunRecord 而不是内存变量：进程重启后内存计数器会丢，
// 而"重启后立刻再推一条同样的失败"正是最烦人的那种重复通知。
func (s *Server) errorNotifyRecent(now time.Time, excludeID string) bool {
	runs, err := s.store.Runs(20)
	if err != nil {
		return false
	}
	for _, prev := range runs {
		if prev.ID == excludeID || prev.Status != model.StatusError {
			continue
		}
		if prev.Notify == nil || !prev.Notify.OK {
			continue
		}
		if now.Sub(prev.FinishedAt) < notifyErrorWindow {
			return true
		}
	}
	return false
}

// buildNotifyMessage 组装推送标题与正文。
func buildNotifyMessage(rec *model.RunRecord) (string, string) {
	title := "晚点名：" + statusWord(rec.Status)
	switch rec.Status {
	case model.StatusSuccess:
		title = "✅ 晚点名签到成功"
	case model.StatusAlready:
		title = "今日已签到"
	case model.StatusNoPlan:
		title = "今日无晚点名计划"
		if rec.Error != "" {
			// 上游不可达/返回维护页与"今天真的没有晚点名"是两件事：
			// 用同一句文案会让用户以为今晚不用签（真漏签风险）。
			title = "⚠️ 无法确认今日计划（上游异常）"
		}
	case model.StatusOutOfRange:
		title = "⚠️ 定位不在签到范围"
	case model.StatusVacation:
		title = "假期跳过"
	case model.StatusError:
		title = "❌ 晚点名执行失败"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "时间：%s（北京时间）\n", rec.FinishedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "结论：%s\n", statusWord(rec.Status))
	if rec.Slot != "" || rec.Target != "" {
		fmt.Fprintf(&b, "档位：%s → %s\n", orDash(rec.Slot), orDash(rec.Target))
	}
	if rec.Decision != "" {
		fmt.Fprintf(&b, "决策：%s\n", rec.Decision)
	}
	if rec.Location != nil {
		fmt.Fprintf(&b, "位置：%.5f, %.5f（%s，抖动=%t）\n",
			rec.Location.Lon, rec.Location.Lat, orDash(rec.Location.Label), rec.Location.Jittered)
		if rec.Location.ActualLocation != "" {
			fmt.Fprintf(&b, "上报文案：%s\n", rec.Location.ActualLocation)
		}
	}
	if rec.School != nil && rec.School.Campus != "" {
		fmt.Fprintf(&b, "校区：%s\n", rec.School.Campus)
	}
	if rec.IsLate {
		b.WriteString("迟到：是（已按 isLate=1 上报）\n")
	}
	fmt.Fprintf(&b, "耗时：%d ms\n", rec.LatencyMS)
	if rec.Error != "" {
		fmt.Fprintf(&b, "错误：%s\n", rec.Error)
	}
	fmt.Fprintf(&b, "run_id：%s", rec.ID)
	return title, b.String()
}

// statusWord 把状态枚举翻成给人看的词（推送与日志共用）。
func statusWord(status string) string {
	switch status {
	case model.StatusSuccess:
		return "签到成功"
	case model.StatusAlready:
		return "今日已签到"
	case model.StatusNoPlan:
		return "今日无计划"
	case model.StatusOutOfRange:
		return "定位越界"
	case model.StatusVacation:
		return "假期跳过"
	case model.StatusSkipped:
		return "已跳过"
	case model.StatusError:
		return "执行失败"
	}
	return status
}

func statusLogLevel(status string) string {
	switch status {
	case model.StatusSuccess, model.StatusAlready:
		return "info"
	case model.StatusError:
		return "error"
	}
	return "warn"
}

// ---------- 脱敏 ----------

// sanitizeError 在错误原文落进 RunRecord / 日志 / SSE / 推送之前抹掉凭据。
//
// 为什么必须做：上游错误里经常带完整请求上下文（token、webhook、带 key 的 URL），
// 而这些字符串会被写进 runs.jsonl、推到手机、显示在浏览器里——任何一处泄漏都
// 等于把账号交出去。
func (s *Server) sanitizeError(msg string, cfg model.Config) string {
	msg = strings.TrimSpace(msg)
	for _, secret := range []string{
		cfg.User.Token,
		cfg.User.Password,
		cfg.Notify.ServerChanKey,
		cfg.Notify.PushPlusToken,
		cfg.Notify.BarkURL,
		cfg.Notify.WecomWebhook,
	} {
		secret = strings.TrimSpace(secret)
		// 短到 5 个字符以内的"凭据"多半是测试值或纯数字，参与替换会把错误原文
		// 里的状态码/坐标/ID 一起打碎，反而看不出原本的失败原因。真实的 token、
		// 推送 key、webhook 都远超这个长度。
		if utf8.RuneCountInString(secret) < 6 {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, maskSecret(secret))
	}
	if len(msg) > maxErrorTextLen {
		msg = truncateText(msg, maxErrorTextLen)
	}
	return msg
}

// maskSecret 只保留前两个字符 + 长度，形如 ab***（len=64）。
// 完全不保留字符会让用户无法判断"是哪一份 token 失效了"；保留太多就等于没脱敏。
func maskSecret(secret string) string {
	runes := []rune(secret)
	if len(runes) <= 2 {
		return "••••"
	}
	return string(runes[:2]) + "***（len=" + strconv.Itoa(len(runes)) + "）"
}

// truncateText 按字节截断但保证落在字符边界上（否则 JSON 里会出现半个汉字）。
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…（已截断）"
}

// slotKeyOf 是"同一档不重复触发"的键（§10.3 幂等第一层）。
func slotKeyOf(date, slot, target string) string {
	return date + "|" + slot + "|" + target
}
