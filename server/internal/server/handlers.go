package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
	"fzucheckin/internal/secure"
)

// ---------- GET /api/v1/health ----------

// handleHealth 是唯一的完全公开接口，用于存活探针与容量观察。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	uptime := s.now().Sub(s.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	clients, dropped := s.hub.stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    Version,
		"uptime_s":   int64(uptime / time.Second),
		"goroutines": runtime.NumGoroutine(),
		"heap_mb":    math.Round(float64(mem.HeapAlloc)/(1<<20)*10) / 10,
		// 两个 SSE 诊断量：连接数用于确认没有 fd 泄漏，丢弃数用于确认慢消费者
		// 只是丢帧而没有把广播路径堵住。
		"sse_clients": clients,
		"sse_dropped": dropped,
	})
}

// ---------- GET /api/v1/config ----------

// configResponse 是 GET /config 的形状（§9 + 前端硬假设 4）。
//
// Config 里的敏感字段是**哨兵**（config.Mask），不是明文：这个响应会进浏览器历史
// 缓存、DevTools、扩展的请求日志，也可能被用户在排障时整段贴进聊天窗口。
// 要显示真实值时走 POST /config/reveal（需要再输一次管理密码），一次只给一项。
// MaskedFields 仍然回答"服务端到底存过哪些敏感字段"，供前端渲染「已配置」徽标。
//
// 这里刻意没有 version 字段：配置不记录版本，界面上也没有「配置版本 vN」可显示。
// "配置变过"的信号只在服务端内部用（store.ConfigVersion），排障看 /status。
type configResponse struct {
	Config       model.Config    `json:"config"`
	Warnings     []string        `json:"warnings"`
	MaskedFields map[string]bool `json:"masked_fields"`
	Source       string          `json:"source"`
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Config()
	// warnings 里带上 schedule 段的降级提示：用户不该等到"保存一次"才知道
	// times 里某一档写坏了。
	warnings := schedule.Parse(cfg.Checkin.Schedule).Warnings
	// 凭据没配时，一打开配置页就该看到这句话：等到 21:35 才发现"今晚没签"已经晚了。
	// 放到 warnings 而不是另造字段，是因为前端已经在逐条渲染这个列表。
	if msg := config.CredentialWarning(cfg); msg != "" {
		warnings = append(warnings, msg)
	}
	if warnings == nil {
		warnings = []string{}
	}
	// 敏感字段一律换哨兵：读取路径不再是"明文出口"。（导出、日志、SSE、运行记录
	// 本来就走脱敏；现在 GET 也回归同一条线，只剩 reveal 这一个按需出口。）
	// 前端拿到哨兵原样回传 = "保持原值"，因此这里不会把用户填过的密码冲掉。
	writeJSON(w, http.StatusOK, configResponse{
		Config:       config.Mask(cfg),
		Warnings:     warnings,
		MaskedFields: config.MaskedFields(cfg),
		Source:       "json",
	})
}

// ---------- POST /api/v1/config/reveal ----------

// revealResponse 只带一项真实值。一次一项是刻意的：把整份明文一次性吐回去，
// 等于把刚收回来的风险又原样放出来。
type revealResponse struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// handleConfigReveal 按需显示一个敏感字段的真实值，要求**再输一次管理密码**。
//
// 为什么不是"登录态就能看"：会话是长期（7 天）、无感、可能被复制走的凭证；而
// 看一眼学校密码这类操作应当要求"人就在键盘前"。二次校验把这个动作降回"当场
// 有人"级别——共享屏幕、临时借电脑、Cookie 被盗这些场景都拦得住。
//
// 失败按 IP 限速（与登录同一个阈值、独立的计数器）：这张接口是密码校验点，
// 给它开放无限次尝试等于给管理密码开一个离线爆破口。
func (s *Server) handleConfigReveal(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if wait, blocked := s.revealLimits.blocked(ip); blocked {
		writeTooManyAttempts(w, wait)
		return
	}
	var body struct {
		Field    string `json:"field"`
		Password string `json:"password"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}
	field := strings.TrimSpace(body.Field)
	cfg := s.store.Config()
	value, ok := config.LookupSecret(&cfg, field)
	if !ok {
		// 不认识就用 400 直接说清楚可选值：这既是校验也是文档。
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("没有这个敏感字段：%q", field),
			"可选："+strings.Join(config.SecretPaths(), "、")))
		return
	}
	hash := s.store.PasswordHash()
	if hash == "" {
		writeError(w, newError(http.StatusForbidden, codeNotInitialized, "服务尚未初始化：请先设置管理密码"))
		return
	}
	if !secure.VerifyPassword(hash, body.Password) {
		count, wait := s.revealLimits.fail(ip)
		s.logf("warn", "", "显示凭据被拒（管理密码不对）field=%s ip=%s 第 %d 次", field, ip, count)
		if wait > 0 {
			writeTooManyAttempts(w, wait)
			return
		}
		writeError(w, newError(http.StatusUnauthorized, codeBadPassword, "管理密码不正确"))
		return
	}
	s.revealLimits.reset(ip)
	// 只记"谁看了哪一项"，绝不记值本身。
	s.logf("info", "", "凭据已显示 field=%s ip=%s", field, ip)
	writeJSON(w, http.StatusOK, revealResponse{Field: field, Value: value})
}

// ---------- PUT /api/v1/config ----------

// handleConfigPut 合并脱敏哨兵 → 规范化 → 校验 → 落盘 → 让调度器立刻重规划。
//
// 没有乐观锁：配置里不再有版本号，前端也就没有可带回的版本。这是单用户服务，
// 唯一的写者就是他自己，引入 409 版本冲突只会让"两个标签页都开着"变成一次
// 莫名其妙的保存失败。响应只回 ok + warnings，不带任何版本号。
//
// 请求体里若还带着旧前端回传的 version（或任何别处的未知字段），走的是
// encoding/json 的默认解析——未知字段忽略、不报错，旧页面不会因此存不下配置。
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config model.Config `json:"config"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}

	old := s.store.Config()
	merged := config.MergeMasked(old, body.Config)
	normalized, warnings := config.Normalize(merged)
	if err := config.Validate(normalized); err != nil {
		writeError(w, newError(http.StatusBadRequest, codeConfigInvalid,
			"配置校验未通过，请按下面每一条修正后重试", detailsOf(err)...))
		return
	}
	if _, err := s.store.SaveConfig(normalized, warnings); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeSaveFailed, "保存配置失败："+err.Error()))
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	// 配置一变就必须立刻重新规划：否则用户把 times 从 21:35 改到 21:00，
	// 调度器可能还抱着"等到 21:35"的定时器，白等一档。
	s.wakeScheduler("配置已保存")
	s.logf("info", "", "配置已保存 警告=%d 条", len(warnings))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"warnings": warnings,
	})
}

// ---------- POST /api/v1/config/import-yaml ----------

// handleConfigImportYAML 只解析预览、不落盘（§9）。
//
// 预览同样回显真实值：它和 GET /config 是同一个"给我看看现在的配置"语义，
// 配置页的输入框按明文显示；而且预览里的凭据会被「应用」原样 PUT 回去，
// 若换成哨兵，回写时 MergeMasked 会把它翻译成"保持旧值"——导入的新密码反而
// 存不进去。导出（handleConfigExport）保持脱敏，不受这里影响。
func (s *Server) handleConfigImportYAML(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML string `json:"yaml"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}
	if strings.TrimSpace(body.YAML) == "" {
		writeError(w, newError(http.StatusBadRequest, codeYAMLInvalid, "YAML 内容为空",
			"把旧版 config.yaml 的全文粘进来，至少要有 user / checkin 段。"))
		return
	}
	cfg, warnings, err := config.ImportYAML(body.YAML)
	if err != nil {
		writeError(w, newError(http.StatusBadRequest, codeYAMLInvalid,
			"YAML 解析失败："+err.Error(), detailsOf(err)...))
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config":        cfg,
		"warnings":      warnings,
		"masked_fields": config.MaskedFields(cfg),
	})
}

// ---------- GET /api/v1/config/export ----------

// handleConfigExport 导出脱敏配置（§9：yaml 导出必须是脱敏版）。
// 导出文件常被贴进群里/issue 里排查问题，带明文凭据的导出等于一次主动泄露。
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "yaml"
	}
	masked := config.Mask(s.store.Config())

	var (
		body        []byte
		contentType string
		filename    string
	)
	switch format {
	case "yaml", "yml":
		body = []byte(config.ToYAML(masked))
		contentType = "application/yaml; charset=utf-8"
		filename = "config.yaml"
	case "json":
		raw, err := json.MarshalIndent(masked, "", "  ")
		if err != nil {
			writeError(w, newError(http.StatusInternalServerError, codeInternal, "序列化配置失败："+err.Error()))
			return
		}
		body = append(raw, '\n')
		contentType = "application/json; charset=utf-8"
		filename = "config.json"
	default:
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("不支持的导出格式 %q（可选 yaml / json）", format)))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---------- GET /api/v1/logs ----------

// handleLogs 返回内存日志环快照（新的在前，前端自己按时间正序重排）。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}
	logs := s.store.Logs(limit)
	if logs == nil {
		logs = []model.LogEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// ---------- POST /api/v1/runs ----------

// handleRunStart 受理一次手动运行（§F）。
//
// 立刻 202 + run_id，真正的执行在后台 goroutine 里：一次签到要打 4 个上游接口，
// 最长可能十几秒，同步等待会让前端按钮一直转圈，还可能撞上反向代理的超时。
func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode   string `json:"mode"`
		DryRun bool   `json:"dry_run"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}

	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	switch mode {
	case "", "normal", "force":
	default:
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("未知的运行模式 %q（可选 force / normal）", body.Mode)))
		return
	}

	// 先抢单飞开关再建 run_id：反过来的话，被 409 拒绝的那次也会先消耗一个 id，
	// run_id 与 runs.jsonl 的对应关系就不再是一一对应了。
	if !s.tryBeginRun() {
		writeError(w, newError(http.StatusConflict, codeRunInProgress,
			"已有一次运行正在进行中，等它结束后再试"))
		return
	}

	startedAt := s.now()
	req := runRequest{ID: newRunID(startedAt), DryRun: body.DryRun}
	switch {
	case body.DryRun:
		// 预检不算一次真签到，单独一个 mode 便于在历史里一眼区分（§F）。
		req.Mode = "probe"
		req.Decision = "probe:manual"
	case mode == "force":
		req.Mode = "force"
		req.Decision = "force:manual"
	default:
		req.Mode = "manual"
		req.Decision = "manual"
	}
	s.startRun(req)
	// 叫醒调度器（§H 的"手动触发"唤醒源）。它这一轮只会看到 running=true 然后跳过，
	// 但手动运行结束后调度器能立刻重新规划，而不是干等下一个 30s tick。
	s.wakeScheduler("手动触发运行")
	s.logf("info", req.ID, "已受理手动运行 mode=%s dry_run=%t ip=%s",
		req.Mode, body.DryRun, s.clientIP(r))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id":   req.ID,
		"accepted": true,
	})
}

// ---------- POST /api/v1/control ----------

// controlResponse 是首页「一键暂停/恢复」的响应：开关的新值 + 调度器快照。
// 只回 paused 不够——前端还要据此刷新"调度器：运行中/等待下一档"那一行，
// 再发一次 GET /status 只会多一个来回，还可能读到比开关更早的状态。
type controlResponse struct {
	Paused    bool            `json:"paused"`
	Scheduler statusScheduler `json:"scheduler"`
}

// handleControl 是"暂停/恢复自动签到"的唯一入口。
//
// 三条边界：
//  1. paused 落在 **state** 而不是配置里：配置页是整份回写的，浏览器里一个旧快照
//     点一次保存就能把暂停覆盖掉（见 model.State.Paused）；
//  2. 手动运行（POST /runs）与测试登录**不**受它影响——用户按下暂停的意思是"别自动
//     打上游"，不是"别让我手动排查"；
//  3. 写完必须 wake：调度器此刻可能正睡在一个按旧状态算出来的截止时刻上，
//     恢复后要立刻重规划，而不是等下一个 tick 或下一次自然醒来。
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	// 用指针而不是 bool：缺字段（{} 或 {"paused":null}）与 false 必须能区分开，
	// 否则一次拼错的请求会把暂停状态悄悄改成"恢复"，而那正是最坏的一种误操作。
	var body struct {
		Paused *bool `json:"paused"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}
	if body.Paused == nil {
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			`请求体里少了 paused 字段：需要 {"paused": true} 或 {"paused": false}`,
			"暂停只会停掉自动签到调度，不影响手动运行与测试登录。"))
		return
	}

	want := *body.Paused
	if s.store.State().Paused != want {
		s.updateState(func(st *model.State) { st.Paused = want })
		if want {
			s.logf("info", "", "面板开关：自动签到已暂停（调度器不再发起自动签到）")
		} else {
			s.logf("info", "", "面板开关：自动签到已恢复（调度器立即重新规划）")
		}
	}

	// 无论有没有变化都叫醒一次：重复点同一个值不该有任何副作用，但也不能因为
	// "值没变"就把一条已经排好的旧规划留在调度循环里。
	s.wakeScheduler("面板开关已切换暂停状态")

	// 回读到的是**落盘后**的真值：updateState 写失败时它会与 want 不一致，
	// 前端据此显示的就是实际情况，而不是一句安慰。
	paused := s.store.State().Paused
	if paused != want {
		s.logf("warn", "", "暂停开关没有生效：state.json 写入失败，当前仍为 paused=%t", paused)
	}
	writeJSON(w, http.StatusOK, controlResponse{Paused: paused, Scheduler: s.schedulerSnapshot()})
}

// ---------- POST /api/v1/notify/test ----------

// handleNotifyTest 用当前配置发一条测试推送（§9）。
//
// 成功判据完全交给 fzu.Notify（不同渠道的判据不一样：serverchan 看 code==0，
// 企业微信看 errcode==0）。这里只负责把失败翻译成 502 + 上游原文，
// 因为"推送测试"失败时用户唯一需要的就是上游那句错误。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Config()
	channel := strings.ToLower(strings.TrimSpace(cfg.Notify.Type))
	if channel == "" || channel == "none" {
		writeError(w, newError(http.StatusBadRequest, codeNotifyNotConfigured,
			"还没有配置推送渠道：请在「配置 → 推送」里选一个渠道并填好凭据"))
		return
	}
	title := "fzu-auto-checkin · 测试推送"
	content := fmt.Sprintf("测试推送，发送时间 %s（北京时间）。\n收到这条说明当前渠道配置可用。",
		s.now().Format(time.RFC3339))
	if err := s.notifier(cfg, title, content); err != nil {
		msg := s.sanitizeError(err.Error(), cfg)
		s.logf("warn", "", "测试推送失败 channel=%s：%s", channel, msg)
		writeError(w, newError(http.StatusBadGateway, codeNotifyFailed,
			"推送渠道返回失败："+msg))
		return
	}
	s.logf("info", "", "测试推送已发送 channel=%s", channel)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": channel})
}

// ---------- 静态资源 ----------

// handleStatic 在根路径托管内嵌前端（§G1：后端不认识 HTML，只是顺带托管）。
//
// 静态资源不要求登录：登录页本身就在这里面，要求登录才能取 index.html
// 会让未登录用户看到一个空白页。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, newError(http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"静态资源只支持 GET / HEAD"))
		return
	}
	// path.Clean 加前导 "/" 再裁掉，能吃掉 ".." 与重复斜杠，剩下的相对路径
	// 交给 fs.ReadFile（embed FS 本身也拒绝 ..，这里是双保险）。
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		if name == "index.html" {
			writeError(w, newError(http.StatusInternalServerError, codeInternal,
				"内嵌前端资源缺失：二进制可能构建不完整"))
			return
		}
		writeError(w, newError(http.StatusNotFound, codeNotFound, "页面不存在："+name))
		return
	}
	w.Header().Set("Content-Type", staticContentType(name))
	// 静态资源随二进制发布，内容只会在重新构建后变化，所以 no-cache 就够：
	// 让浏览器每次校验，改完前端刷新即见（调试体验优先于省一次请求）。
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
