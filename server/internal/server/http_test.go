package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// TestHealthIsPublic 验证健康探针不需要登录（运维/容器探针的唯一入口）。
func TestHealthIsPublic(t *testing.T) {
	env := newTestEnv(t)
	res := env.do(env.client(), http.MethodGet, apiPrefix+"/health", nil)
	env.expectStatus(res, http.StatusOK, "GET /health")

	body := jsonMap(t, res.body)
	if body["status"] != "ok" {
		t.Fatalf("health.status 期望 ok，实际 %v", body["status"])
	}
	if body["version"] != Version {
		t.Fatalf("health.version 期望 %s，实际 %v", Version, body["version"])
	}
	if _, ok := body["goroutines"]; !ok {
		t.Fatalf("health 缺少 goroutines 字段: %s", truncateForLog(res.body))
	}
}

// TestUninitializedFlow 覆盖验收清单第 1 条：
// /health 200、/config 401、setup 200、再次 setup 409。
func TestUninitializedFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.client()

	res := env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusUnauthorized, "未初始化 GET /config")
	if code := errCode(t, res.body); code != codeNotInitialized {
		t.Fatalf("未初始化时错误码期望 %s，实际 %s", codeNotInitialized, code)
	}

	// 未初始化时登录给 403 not_initialized：前端据此跳"设置密码"。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": "whatever-1"})
	env.expectStatus(res, http.StatusForbidden, "未初始化 POST /auth/login")
	if code := errCode(t, res.body); code != codeNotInitialized {
		t.Fatalf("未初始化登录错误码期望 %s，实际 %s", codeNotInitialized, code)
	}

	// 过短的密码被明确拒绝。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": "short"})
	env.expectStatus(res, http.StatusBadRequest, "短密码 setup")
	if code := errCode(t, res.body); code != codeWeakPassword {
		t.Fatalf("短密码错误码期望 %s，实际 %s", codeWeakPassword, code)
	}

	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": testPassword})
	env.expectStatus(res, http.StatusOK, "POST /auth/setup")
	if !strings.Contains(string(res.body), `"ok":true`) {
		t.Fatalf("setup 响应缺少 ok:true: %s", truncateForLog(res.body))
	}

	// 已初始化 → 409，而不是覆盖密码。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": "another-pass-1"})
	env.expectStatus(res, http.StatusConflict, "重复 setup")
	if code := errCode(t, res.body); code != codeAlreadyInitialized {
		t.Fatalf("重复 setup 错误码期望 %s，实际 %s", codeAlreadyInitialized, code)
	}

	// setup 下发 Cookie，所以同一个客户端已经是登录状态。
	res = env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusOK, "setup 后 GET /config")
}

// TestSessionAndLogin 覆盖 session 三态与密码校验。
func TestSessionAndLogin(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())

	anonymous := env.client()
	res := env.do(anonymous, http.MethodGet, apiPrefix+"/auth/session", nil)
	env.expectStatus(res, http.StatusOK, "GET /auth/session")
	session := jsonMap(t, res.body)
	if session["initialized"] != false || session["authed"] != false {
		t.Fatalf("未初始化时 session 期望 initialized=false authed=false，实际 %v", session)
	}
	if session["env"] != envName {
		t.Fatalf("session.env 期望 %s，实际 %v", envName, session["env"])
	}

	// 设密码后：同一个匿名客户端仍是未登录。
	_ = env.authedClient()
	res = env.do(anonymous, http.MethodGet, apiPrefix+"/auth/session", nil)
	session = jsonMap(t, res.body)
	if session["initialized"] != true || session["authed"] != false {
		t.Fatalf("已初始化未登录时 session 期望 true/false，实际 %v", session)
	}

	// 未登录访问受保护接口 → 401 unauthorized。
	res = env.do(anonymous, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusUnauthorized, "未登录 GET /config")
	if code := errCode(t, res.body); code != codeUnauthorized {
		t.Fatalf("未登录错误码期望 %s，实际 %s", codeUnauthorized, code)
	}

	// 错密码 → 401 bad_password。
	res = env.do(anonymous, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": "wrong-password"})
	env.expectStatus(res, http.StatusUnauthorized, "错误密码登录")
	if code := errCode(t, res.body); code != codeBadPassword {
		t.Fatalf("错误密码错误码期望 %s，实际 %s", codeBadPassword, code)
	}

	// 正确密码 → 200 + Cookie，之后可以访问受保护接口。
	res = env.do(anonymous, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": testPassword})
	env.expectStatus(res, http.StatusOK, "正确密码登录")
	res = env.do(anonymous, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusOK, "登录后 GET /config")
}

// TestCSRFRequired 覆盖验收清单第 2 条：带 Cookie 但缺 X-Requested-With → 403。
func TestCSRFRequired(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	res := env.doRaw(c, http.MethodPost, apiPrefix+"/auth/logout", nil, false)
	env.expectStatus(res, http.StatusForbidden, "缺 CSRF 头的 POST")
	if code := errCode(t, res.body); code != codeCSRF {
		t.Fatalf("CSRF 错误码期望 %s，实际 %s", codeCSRF, code)
	}

	// 带头的同一个请求成功（顺带验证 logout 之后 Cookie 失效）。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/logout", nil)
	env.expectStatus(res, http.StatusOK, "带 CSRF 头的 logout")
	res = env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(res, http.StatusUnauthorized, "logout 后 GET /config")

	// GET 不需要 CSRF 头。
	other := env.authedClient()
	res = env.doRaw(other, http.MethodGet, apiPrefix+"/status", nil, false)
	env.expectStatus(res, http.StatusOK, "无 CSRF 头的 GET /status")
}

// TestLoginRateLimit 覆盖验收清单第 3 条：15 分钟内失败 5 次 → 429 + Retry-After。
func TestLoginRateLimit(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	_ = env.authedClient() // 先把密码设好，后续请求都是"已初始化"
	c := env.client()

	wrong := map[string]string{"password": "not-the-password"}
	for i := 1; i <= 4; i++ {
		res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错密期望 401，实际 %d", i, res.status)
		}
	}
	// 第 5 次：条件（5 次失败）已经满足，直接 429 并把 Retry-After 给出来。
	res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
	env.expectStatus(res, http.StatusTooManyRequests, "第 5 次错密")
	retryAfter := strings.TrimSpace(res.header.Get("Retry-After"))
	if retryAfter == "" {
		t.Fatalf("429 缺少 Retry-After 头")
	}
	if code := errCode(t, res.body); code != codeTooManyAttempts {
		t.Fatalf("限速错误码期望 %s，实际 %s", codeTooManyAttempts, code)
	}

	// 第 6 次（即便密码正确）仍然 429。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": testPassword})
	env.expectStatus(res, http.StatusTooManyRequests, "第 6 次登录")

	// 同一个 TCP 来源伪造 X-Forwarded-For：默认不采信，因此仍然限速。
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+apiPrefix+"/auth/login",
		strings.NewReader(`{"password":"wrong-but-from-other-ip"}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, csrfHeaderValue)
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	// X-Forwarded-For 是客户端能随手写的头，默认（FZU_TRUST_PROXY 未开）**不得**
	// 被当成限速身份：否则爆破者每换一个头就绕过了「同 IP 15 分钟 5 次」的限制。
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("伪造 X-Forwarded-For 后仍应 429（默认不采信 XFF），实际 %d", resp.StatusCode)
	}
}

// TestStaticAssets 覆盖验收清单第 11 条。
func TestStaticAssets(t *testing.T) {
	env := newTestEnv(t)

	res := env.do(env.client(), http.MethodGet, "/", nil)
	env.expectStatus(res, http.StatusOK, "GET /")
	if !strings.Contains(strings.ToLower(string(res.body)), "<!doctype html>") {
		t.Fatalf("index.html 缺少 <!doctype html>: %s", truncateForLog(res.body))
	}
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / 的 Content-Type 期望 text/html，实际 %q", ct)
	}

	res = env.do(env.client(), http.MethodGet, "/app.js", nil)
	env.expectStatus(res, http.StatusOK, "GET /app.js")
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("GET /app.js 的 Content-Type 期望 JS，实际 %q", ct)
	}

	res = env.do(env.client(), http.MethodGet, "/style.css", nil)
	env.expectStatus(res, http.StatusOK, "GET /style.css")
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Fatalf("GET /style.css 的 Content-Type 期望 CSS，实际 %q", ct)
	}

	// 未知路径给 JSON 404（而不是 ServeMux 的纯文本）。
	res = env.do(env.client(), http.MethodGet, "/nope.js", nil)
	env.expectStatus(res, http.StatusNotFound, "GET /nope.js")
	if code := errCode(t, res.body); code != codeNotFound {
		t.Fatalf("未知静态资源错误码期望 %s，实际 %s", codeNotFound, code)
	}

	// HEAD 允许但不带 body。
	res = env.do(env.client(), http.MethodHead, "/app.js", nil)
	env.expectStatus(res, http.StatusOK, "HEAD /app.js")
	if len(res.body) != 0 {
		t.Fatalf("HEAD 响应不该有 body，实际 %d 字节", len(res.body))
	}
}

// TestSecurityHeaders 验证 §B 的四组安全头。
func TestSecurityHeaders(t *testing.T) {
	env := newTestEnv(t)
	// 基础安全头对 API 与页面都生效。
	for _, path := range []string{apiPrefix + "/health", "/"} {
		res := env.do(env.client(), http.MethodGet, path, nil)
		for key, value := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := res.header.Get(key); got != value {
				t.Fatalf("%s 的 %s 期望 %q，实际 %q", path, key, value, got)
			}
		}
	}

	// CSP 只加在自己托管的页面上：给 API 也塞 connect-src 'self'，会与
	// 「前端放 nginx、后端只跑 API」的跨源部署承诺冲突。
	csp := env.do(env.client(), http.MethodGet, "/", nil).header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP 不符合契约: %q", csp)
	}
	if apiCSP := env.do(env.client(), http.MethodGet, apiPrefix+"/health", nil).header.Get("Content-Security-Policy"); apiCSP != "" {
		t.Fatalf("API 响应不该带 CSP（跨源部署会被自己的策略打死），实际 %q", apiCSP)
	}
	// 配置里有学号与精确坐标，任何中间缓存都不该留存。

	if cc := env.do(env.client(), http.MethodGet, apiPrefix+"/health", nil).header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("API 响应应带 Cache-Control: no-store，实际 %q", cc)
	}
}

// TestUnknownAPIIs404JSON 验证未知接口也回错误信封。
func TestUnknownAPIIs404JSON(t *testing.T) {
	env := newTestEnv(t)
	c := env.authedClient()
	res := env.do(c, http.MethodGet, apiPrefix+"/nope", nil)
	env.expectStatus(res, http.StatusNotFound, "GET /api/v1/nope")
	if code := errCode(t, res.body); code != codeNotFound {
		t.Fatalf("未知接口错误码期望 %s，实际 %s", codeNotFound, code)
	}
}

// TestCORSWhitelist 验证白名单回显、预检 204、Vary: Origin。
func TestCORSWhitelist(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Server.AllowOrigins = []string{"https://ui.example.com"}
	env.saveConfig(cfg)
	c := env.authedClient()

	// 预检：不带 Cookie、不带 CSRF，直接 204。
	req, err := http.NewRequest(http.MethodOptions, env.ts.URL+apiPrefix+"/status", nil)
	if err != nil {
		t.Fatalf("构造预检请求失败: %v", err)
	}
	req.Header.Set("Origin", "https://ui.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("预检期望 204，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://ui.example.com" {
		t.Fatalf("预检 ACAO 期望白名单源，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("预检 ACAC 期望 true，实际 %q", got)
	}

	// 实请求：同样回显 + Vary: Origin。
	req, _ = http.NewRequest(http.MethodGet, env.ts.URL+apiPrefix+"/status", nil)
	req.Header.Set("Origin", "https://ui.example.com")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatalf("跨源实请求失败: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://ui.example.com" {
		t.Fatalf("实请求 ACAO 期望白名单源，实际 %q", got)
	}
	if vary := resp.Header.Values("Vary"); len(vary) == 0 || !strings.Contains(strings.Join(vary, ","), "Origin") {
		t.Fatalf("缺少 Vary: Origin，实际 %v", vary)
	}

	// 不在白名单的来源：不回显任何 CORS 头。
	req, _ = http.NewRequest(http.MethodGet, env.ts.URL+apiPrefix+"/status", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatalf("跨源实请求失败: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("非白名单来源不该回显 ACAO，实际 %q", got)
	}
}

// TestLogsSnapshot 验证内存环快照接口。
func TestLogsSnapshot(t *testing.T) {
	env := newTestEnv(t)
	c := env.authedClient()
	for i := 0; i < 5; i++ {
		env.store.AppendLog(model.LogEvent{Level: "info", Msg: "测试日志"})
	}
	res := env.do(c, http.MethodGet, apiPrefix+"/logs?limit=3", nil)
	env.expectStatus(res, http.StatusOK, "GET /logs")
	var payload struct {
		Logs []model.LogEvent `json:"logs"`
	}
	decodeBody(t, res.body, &payload)
	if len(payload.Logs) != 3 {
		t.Fatalf("limit=3 期望返回 3 条，实际 %d", len(payload.Logs))
	}
	// 新的在前。
	if payload.Logs[0].Msg != "测试日志" {
		t.Fatalf("日志内容不符: %+v", payload.Logs[0])
	}
}

// TestSetLogLevelToleratesGarbage 验证错的 FZU_LOG_LEVEL 不会关掉日志。
func TestSetLogLevelToleratesGarbage(t *testing.T) {
	env := newTestEnv(t)
	env.srv.SetLogLevel("nonsense")
	env.srv.SetLogLevel("debug")
	env.srv.SetLogLevel("")
	res := env.do(env.client(), http.MethodGet, apiPrefix+"/health", nil)
	env.expectStatus(res, http.StatusOK, "health")
}

// TestSessionCookieExpiry 验证过期的签名 Cookie 被拒。
func TestSessionCookieExpiry(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	_ = env.authedClient()

	now := time.Date(2026, 11, 14, 21, 10, 0, 0, cstTest)
	env.setNow(now.Add(8 * 24 * time.Hour)) // 超过 7 天有效期
	c := env.client()
	// 手工构造一个"签名正确但已过期"的 Cookie。
	payload := []byte(`{"sub":"admin","iat":1,"exp":2}`)
	token := encodeSessionForTest(env.srv, payload)

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+apiPrefix+"/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("过期 Cookie 期望 401，实际 %d", resp.StatusCode)
	}
}
