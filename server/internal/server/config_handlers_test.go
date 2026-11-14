package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
)

// configWithSecrets 是一份"三种敏感字段都填了"的配置，用来断言脱敏。
func configWithSecrets() model.Config {
	cfg := baseConfig()
	cfg.User.Token = "token-abcdefghijklmnopqrstuvwxyz"
	cfg.Notify.Type = "serverchan"
	cfg.Notify.ServerChanKey = "SCT123456secretkey"
	return cfg
}

// TestConfigGetMasksSecrets 覆盖验收清单第 4 条的前半：GET 脱敏且不泄漏明文。
func TestConfigGetMasksSecrets(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(configWithSecrets())
	c := env.authedClient()

	res := env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusOK, "GET /config")

	text := string(res.body)
	for _, secret := range []string{testSchoolPwd, "token-abcdefghijklmnopqrstuvwxyz", "SCT123456secretkey"} {
		if strings.Contains(text, secret) {
			t.Fatalf("GET /config 泄漏了明文凭据 %q: %s", secret, truncateForLog(res.body))
		}
	}
	if !strings.Contains(text, config.MaskSentinel) {
		t.Fatalf("GET /config 缺少脱敏哨兵 %q: %s", config.MaskSentinel, truncateForLog(res.body))
	}

	var payload struct {
		Config       model.Config    `json:"config"`
		Version      int64           `json:"version"`
		Warnings     []string        `json:"warnings"`
		MaskedFields map[string]bool `json:"masked_fields"`
		Source       string          `json:"source"`
	}
	decodeBody(t, res.body, &payload)
	if payload.Config.User.Password != config.MaskSentinel {
		t.Fatalf("user.password 期望哨兵，实际 %q", payload.Config.User.Password)
	}
	if payload.Config.User.Token != config.MaskSentinel {
		t.Fatalf("user.token 期望哨兵，实际 %q", payload.Config.User.Token)
	}
	if !payload.MaskedFields["user.password"] || !payload.MaskedFields["notify.serverchan_key"] {
		t.Fatalf("masked_fields 应该标出已配置的敏感字段: %v", payload.MaskedFields)
	}
	if payload.MaskedFields["notify.bark_url"] {
		t.Fatalf("未配置的 bark_url 不该是 true: %v", payload.MaskedFields)
	}
	if payload.Source != "json" {
		t.Fatalf("source 期望 json，实际 %q", payload.Source)
	}
	if payload.Version != env.store.ConfigVersion() {
		t.Fatalf("version 与 store 不一致: %d != %d", payload.Version, env.store.ConfigVersion())
	}
	if payload.Warnings == nil {
		t.Fatalf("warnings 必须是数组而不是 null")
	}
}

// TestConfigPutValidation 覆盖第 4 条后半：缺坐标 400 + details 非空。
func TestConfigPutValidation(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	bad := baseConfig()
	bad.Checkin.Longitude = ""
	bad.Checkin.Latitude = ""
	res := env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": bad})
	env.expectStatus(res, http.StatusBadRequest, "PUT /config 缺坐标")
	if code := errCode(t, res.body); code != codeConfigInvalid {
		t.Fatalf("错误码期望 %s，实际 %s", codeConfigInvalid, code)
	}
	var payload struct {
		Error struct {
			Details []string `json:"details"`
		} `json:"error"`
	}
	decodeBody(t, res.body, &payload)
	if len(payload.Error.Details) == 0 {
		t.Fatalf("config_invalid 必须给出逐条 details: %s", truncateForLog(res.body))
	}

	// 没凭据也应 400 且提示两种写法。
	noCred := baseConfig()
	noCred.User.Username = ""
	noCred.User.Password = ""
	noCred.User.Token = ""
	res = env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": noCred})
	env.expectStatus(res, http.StatusBadRequest, "PUT /config 无凭据")
	if !strings.Contains(string(res.body), "user.token") {
		t.Fatalf("错误详情应给出可照抄的写法: %s", truncateForLog(res.body))
	}
}

// TestConfigPutMaskedRoundTrip 验证"回传哨兵 = 保持原值"与"空串 = 清空"两条协议。
func TestConfigPutMaskedRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(configWithSecrets())
	c := env.authedClient()

	// 前端行为：GET → 原样回传（哨兵不动）→ 只改 times。
	var got struct {
		Config model.Config `json:"config"`
	}
	res := env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusOK, "GET /config")
	decodeBody(t, res.body, &got)

	before := env.store.ConfigVersion()
	incoming := got.Config
	incoming.Checkin.Schedule.Times = []string{"21:40", "22:10"}
	res = env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": incoming})
	env.expectStatus(res, http.StatusOK, "PUT /config 回传哨兵")
	var saved struct {
		OK       bool     `json:"ok"`
		Version  int64    `json:"version"`
		Warnings []string `json:"warnings"`
	}
	decodeBody(t, res.body, &saved)
	if !saved.OK || saved.Version != before+1 {
		t.Fatalf("保存响应期望 ok=true version=%d，实际 %+v", before+1, saved)
	}

	stored := env.store.Config()
	if stored.User.Password != testSchoolPwd {
		t.Fatalf("回传哨兵应保持原密码，实际 %q", stored.User.Password)
	}
	if stored.User.Token != "token-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("回传哨兵应保持原 token，实际 %q", stored.User.Token)
	}
	if stored.Notify.ServerChanKey != "SCT123456secretkey" {
		t.Fatalf("回传哨兵应保持原推送 key，实际 %q", stored.Notify.ServerChanKey)
	}
	if len(stored.Checkin.Schedule.Times) != 2 || stored.Checkin.Schedule.Times[0] != "21:40" {
		t.Fatalf("times 没被保存: %v", stored.Checkin.Schedule.Times)
	}

	// 空串 = 清空（用户显式删掉推送 key）。
	clear := env.store.Config()
	clear.Notify.ServerChanKey = ""
	res = env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": clear})
	env.expectStatus(res, http.StatusOK, "PUT /config 清空 key")
	if env.store.Config().Notify.ServerChanKey != "" {
		t.Fatalf("空串应清空 serverchan_key，实际 %q", env.store.Config().Notify.ServerChanKey)
	}
	if env.store.Config().User.Password != testSchoolPwd {
		t.Fatalf("清空推送 key 不该动到密码")
	}
}

// TestConfigPutWakesScheduler 验证保存后立刻唤醒调度器（不必等下一个 tick）。
func TestConfigPutWakesScheduler(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	// 先把 wake 通道抽干，确保观察到的是本次保存发出的信号。
	select {
	case <-env.srv.wake:
	default:
	}

	res := env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": env.store.Config()})
	env.expectStatus(res, http.StatusOK, "PUT /config")

	select {
	case <-env.srv.wake:
	case <-time.After(2 * time.Second):
		t.Fatalf("保存配置后没有唤醒调度器")
	}
}

// TestConfigImportYAML 覆盖第 4 条：导入成功/失败，且回显必须脱敏。
func TestConfigImportYAML(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	yamlText := strings.Join([]string{
		"user:",
		"  username: \"" + testUser + "\"",
		"  password: \"" + testSchoolPwd + "\"",
		"checkin:",
		"  longitude: \"119.20463\"",
		"  latitude: \"26.04920\"",
		"  schedule:",
		"    window_start: \"21:00\"",
		"    window_end: \"23:59\"",
		"    times: [\"21:35\", \"22:05\"]",
		"notify:",
		"  type: \"none\"",
	}, "\n")

	res := env.do(c, http.MethodPost, apiPrefix+"/config/import-yaml", map[string]string{"yaml": yamlText})
	env.expectStatus(res, http.StatusOK, "POST /config/import-yaml")
	text := string(res.body)
	if strings.Contains(text, testSchoolPwd) {
		t.Fatalf("导入预览回显了明文密码: %s", truncateForLog(res.body))
	}
	if !strings.Contains(text, config.MaskSentinel) {
		t.Fatalf("导入预览应脱敏成哨兵: %s", truncateForLog(res.body))
	}
	var preview struct {
		Config   model.Config `json:"config"`
		Warnings []string     `json:"warnings"`
	}
	decodeBody(t, res.body, &preview)
	if len(preview.Config.Checkin.Schedule.Times) != 2 {
		t.Fatalf("导入的 times 不对: %v", preview.Config.Checkin.Schedule.Times)
	}
	if preview.Warnings == nil {
		t.Fatalf("warnings 必须是数组")
	}

	// 解析失败 → 400 yaml_invalid，且详情指出行号。
	res = env.do(c, http.MethodPost, apiPrefix+"/config/import-yaml", map[string]string{"yaml": "user: [\n"})
	env.expectStatus(res, http.StatusBadRequest, "坏 YAML")
	if code := errCode(t, res.body); code != codeYAMLInvalid {
		t.Fatalf("坏 YAML 错误码期望 %s，实际 %s", codeYAMLInvalid, code)
	}
	if !strings.Contains(string(res.body), "第") {
		t.Fatalf("坏 YAML 应指出第几行: %s", truncateForLog(res.body))
	}

	// 空 YAML → 400。
	res = env.do(c, http.MethodPost, apiPrefix+"/config/import-yaml", map[string]string{"yaml": "   "})
	env.expectStatus(res, http.StatusBadRequest, "空 YAML")
}

// TestConfigExport 覆盖第 4 条：两种格式的附件导出，yaml 必须是脱敏版。
func TestConfigExport(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(configWithSecrets())
	c := env.authedClient()

	res := env.do(c, http.MethodGet, apiPrefix+"/config/export?format=yaml", nil)
	env.expectStatus(res, http.StatusOK, "导出 yaml")
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("yaml 导出 Content-Type 期望 application/yaml，实际 %q", ct)
	}
	if cd := res.header.Get("Content-Disposition"); !strings.Contains(cd, "filename=config.yaml") {
		t.Fatalf("yaml 导出 Content-Disposition 期望附件 config.yaml，实际 %q", cd)
	}
	yamlText := string(res.body)
	if strings.Contains(yamlText, testSchoolPwd) || strings.Contains(yamlText, "SCT123456secretkey") {
		t.Fatalf("yaml 导出泄漏明文凭据: %s", truncateForLog(res.body))
	}
	if !strings.Contains(yamlText, config.MaskSentinel) {
		t.Fatalf("yaml 导出应带脱敏哨兵: %s", truncateForLog(res.body))
	}

	res = env.do(c, http.MethodGet, apiPrefix+"/config/export?format=json", nil)
	env.expectStatus(res, http.StatusOK, "导出 json")
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("json 导出 Content-Type 期望 application/json，实际 %q", ct)
	}
	if cd := res.header.Get("Content-Disposition"); !strings.Contains(cd, "filename=config.json") {
		t.Fatalf("json 导出 Content-Disposition 期望附件 config.json，实际 %q", cd)
	}
	var exported model.Config
	if err := json.Unmarshal(res.body, &exported); err != nil {
		t.Fatalf("json 导出不是合法配置 JSON: %v", err)
	}
	if exported.User.Password != config.MaskSentinel {
		t.Fatalf("json 导出应脱敏，实际 %q", exported.User.Password)
	}

	// 未知格式 → 400。
	res = env.do(c, http.MethodGet, apiPrefix+"/config/export?format=xml", nil)
	env.expectStatus(res, http.StatusBadRequest, "导出 xml")
}

// TestConfigPutRequiresCSRF 顺带验证写接口的 CSRF 不能被跳过（缺头 → 403）。
func TestConfigPutRequiresCSRF(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()
	res := env.doRaw(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": env.store.Config()}, false)
	env.expectStatus(res, http.StatusForbidden, "PUT /config 缺 CSRF")
}
