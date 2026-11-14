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
type configResponse struct {
	Config       model.Config    `json:"config"`
	Version      int64           `json:"version"`
	Warnings     []string        `json:"warnings"`
	MaskedFields map[string]bool `json:"masked_fields"`
	Source       string          `json:"source"`
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Config()
	// warnings 里带上 schedule 段的降级提示：用户不该等到"保存一次"才知道
	// times 里某一档写坏了。
	warnings := schedule.Parse(cfg.Checkin.Schedule).Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, configResponse{
		Config:       config.Mask(cfg),
		Version:      s.store.ConfigVersion(),
		Warnings:     warnings,
		MaskedFields: config.MaskedFields(cfg),
		Source:       "json",
	})
}

// ---------- PUT /api/v1/config ----------

// handleConfigPut 合并脱敏哨兵 → 规范化 → 校验 → 落盘 → 让调度器立刻重规划。
//
// 不实现乐观锁（前端把 config.version 原样带回，但我们不据此拒绝）：
// 这是单用户服务，唯一的写者就是他自己；引入 409 版本冲突只会让"两个标签页
// 都开着"变成一次莫名其妙的保存失败。响应仍返回新 version 供前端显示。
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
	version, err := s.store.SaveConfig(normalized, warnings)
	if err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeSaveFailed, "保存配置失败："+err.Error()))
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	// 配置一变就必须立刻重新规划：否则用户把 times 从 21:35 改到 21:00，
	// 调度器可能还抱着"等到 21:35"的定时器，白等一档。
	s.wakeScheduler("配置已保存")
	s.logf("info", "", "配置已保存 config_version=%d 警告=%d 条", version, len(warnings))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  version,
		"warnings": warnings,
	})
}

// ---------- POST /api/v1/config/import-yaml ----------

// handleConfigImportYAML 只解析预览、不落盘（§9）。响应必须脱敏：
// 用户粘进来的 YAML 里就带着明文密码，回显回去等于把它又写进浏览器缓存。
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
		"config":        config.Mask(cfg),
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
