package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
)

// cookieOnlyConfig 是"只有浏览器 Cookie，没有学号密码、没有 token"的配置。
func cookieOnlyConfig() model.Config {
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	cfg.User.Token = ""
	cfg.User.Cookie = "SESSION=good-session"
	return cfg
}

// TestFlowUsesCookieLogin 是这次改造的主线：没有 token、没有账密，只有 Cookie
// 也必须能签成，并且把换到的 token 回写进配置（下次直接复用，不再打 SSO）。
func TestFlowUsesCookieLogin(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if env.cookieCallCount() != 1 {
		t.Fatalf("Cookie 登录调用次数 = %d，期望 1", env.cookieCallCount())
	}
	if env.ssoCallCount() != 0 {
		t.Fatalf("有 Cookie 时不该再走学号密码登录（%d 次）", env.ssoCallCount())
	}
	if got := env.store.Config().User.Token; got != env.cookieToken {
		t.Fatalf("换到的 token 应回写进配置：期望 %q，实际 %q", env.cookieToken, got)
	}
	// 学校端收到的必须是 Cookie 换来的那个 token，而不是空值或旧值。
	f := env.school
	f.mu.Lock()
	last := f.lastToken
	f.mu.Unlock()
	if last != env.cookieToken {
		t.Fatalf("上游收到的 token = %q，期望 %q", last, env.cookieToken)
	}
	if !strings.Contains(strings.Join(rec.Logs, "\n"), "浏览器 Cookie") {
		t.Fatalf("日志里应写明用的是浏览器 Cookie：%v", rec.Logs)
	}
}

// TestFlowFallsBackToPasswordWhenCookieFails：Cookie 过期但配了账密时，
// 必须接着试账密——多一次请求换"少一次漏签"。
func TestFlowFallsBackToPasswordWhenCookieFails(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig() // 带学号密码
	cfg.User.Cookie = "SESSION=stale"
	env.saveConfig(cfg)
	env.setCookieErr(errors.New("提交的 Cookie 在统一身份认证上不是登录状态"))
	env.setNow(fixedDay(21, 40))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("Cookie 失败后应回落到学号密码并签成，实际 %s", describe(rec))
	}
	if env.cookieCallCount() != 1 || env.ssoCallCount() != 1 {
		t.Fatalf("期望 Cookie 1 次、账密 1 次，实际 Cookie=%d 账密=%d",
			env.cookieCallCount(), env.ssoCallCount())
	}
	if got := env.store.Config().User.Token; got != env.ssoToken {
		t.Fatalf("回写的应是账密换来的 token，实际 %q", got)
	}
}

// TestFlowRepairsExpiredTokenWithCookie：配置里带着一个已过期的 token 时，
// init 失败要用 Cookie 换新 token 并重试一次，而不是判成「今日无计划」放过去。
func TestFlowRepairsExpiredTokenWithCookie(t *testing.T) {
	env := newTestEnv(t)
	cfg := cookieOnlyConfig()
	cfg.User.Token = "stale-token-0123456789"
	env.saveConfig(cfg)
	env.school.rejectToken("stale-token-0123456789")
	env.setNow(fixedDay(21, 40))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("token 过期后应自动换新并签成，实际 %s", describe(rec))
	}
	if env.cookieCallCount() != 1 {
		t.Fatalf("应调用一次 Cookie 登录，实际 %d", env.cookieCallCount())
	}
	if got := env.store.Config().User.Token; got != env.cookieToken {
		t.Fatalf("配置里的 token 应被换成新的，实际 %q", got)
	}
}

// ---------- POST /api/v1/login/test ----------

func TestLoginTestEndpointCookieOK(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())
	c := env.authedClient()

	res := env.do(c, http.MethodPost, apiPrefix+"/login/test",
		map[string]any{"cookie": "SESSION=good-session"})
	env.expectStatus(res, http.StatusOK, "POST /login/test")

	var out struct {
		OK        bool     `json:"ok"`
		Mode      string   `json:"mode"`
		Message   string   `json:"message"`
		TokenHint string   `json:"token_hint"`
		SchoolOK  bool     `json:"school_ok"`
		Hops      []string `json:"hops"`
		Plan      string   `json:"plan"`
	}
	decodeBody(t, res.body, &out)
	if !out.OK || !out.SchoolOK || out.Mode != "cookie" {
		t.Fatalf("响应不符: %s", truncateForLog(res.body))
	}
	if out.Message == "" || out.Hops == nil {
		t.Fatalf("成功响应也要带结论与逐跳诊断: %s", truncateForLog(res.body))
	}
	if !strings.Contains(out.Plan, "21:00-23:59") {
		t.Fatalf("应带今日计划摘要，实际 %q", out.Plan)
	}
	// 安全底线：token 只回掩码，绝不能回原文。
	if strings.Contains(string(res.body), env.cookieToken) {
		t.Fatalf("响应里出现了 token 原文: %s", truncateForLog(res.body))
	}
	if !strings.HasPrefix(out.TokenHint, "co***") {
		t.Fatalf("token 掩码形状不符: %q", out.TokenHint)
	}
}

func TestLoginTestEndpointCookieInvalid(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())
	env.setCookieErr(errors.New("提交的 Cookie 在统一身份认证上不是登录状态（服务端返回的是登录页）"))

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test",
		map[string]any{"cookie": "SESSION=stale"})
	env.expectStatus(res, http.StatusBadGateway, "POST /login/test")
	if code := errCode(t, res.body); code != codeLoginFailed {
		t.Fatalf("错误码 = %q，期望 %q", code, codeLoginFailed)
	}
	var env2 struct {
		Error struct {
			Message string   `json:"message"`
			Details []string `json:"details"`
		} `json:"error"`
	}
	decodeBody(t, res.body, &env2)
	if !strings.Contains(env2.Error.Message, "不是登录状态") {
		t.Fatalf("message 应带上游原因: %s", truncateForLog(res.body))
	}
	// details 必须把逐跳诊断带出来，否则用户不知道断在哪一步。
	if len(env2.Error.Details) == 0 {
		t.Fatalf("details 应包含逐跳诊断: %s", truncateForLog(res.body))
	}
}

func TestLoginTestEndpointNoCredentials(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	env.saveConfig(cfg)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test", map[string]any{})
	env.expectStatus(res, http.StatusBadRequest, "三种凭据都没填")
	if code := errCode(t, res.body); code != codeBadRequest {
		t.Fatalf("错误码 = %q", code)
	}
}

// TestLoginTestEndpointUsesSavedCredentials：请求体里不带值时用已保存的配置，
// 并且脱敏哨兵也按"没改"处理——否则用户不重新输入就永远测不了。
func TestLoginTestEndpointUsesSavedCredentials(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())

	for _, payload := range []map[string]any{
		{},
		{"cookie": config.MaskSentinel},
		{"cookie": "   "},
	} {
		res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test", payload)
		env.expectStatus(res, http.StatusOK, "回落已保存凭据")
		var out struct {
			Mode string `json:"mode"`
		}
		decodeBody(t, res.body, &out)
		if out.Mode != "cookie" {
			t.Fatalf("payload=%v 应回落到已保存的 Cookie，实际 mode=%q", payload, out.Mode)
		}
	}
}

// TestLoginTestEndpointTokenMode：只填 token 时也要能测（哪怕是过期的），
// 这样用户能从报错里看到"学校端不认这个 token"而不是"没填凭据"。
func TestLoginTestEndpointTokenMode(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test",
		map[string]any{"token": "manual-token-0123456789"})
	env.expectStatus(res, http.StatusOK, "POST /login/test token 模式")

	var out struct {
		OK   bool   `json:"ok"`
		Mode string `json:"mode"`
	}
	decodeBody(t, res.body, &out)
	if !out.OK || out.Mode != "token" {
		t.Fatalf("响应不符: %s", truncateForLog(res.body))
	}
	// 测 token 不等于登录：不能因此多打一次 SSO。
	if env.cookieCallCount() != 0 || env.ssoCallCount() != 0 {
		t.Fatalf("token 模式不该触发登录：cookie=%d 账密=%d", env.cookieCallCount(), env.ssoCallCount())
	}
}

// TestConfigCookieIsMasked：Cookie 是凭据，必须和其他凭据一样脱敏。
func TestConfigCookieIsMasked(t *testing.T) {
	cfg := cookieOnlyConfig()
	masked := config.Mask(cfg)
	if masked.User.Cookie != config.MaskSentinel {
		t.Fatalf("Cookie 应被脱敏，实际 %q", masked.User.Cookie)
	}
	fields := config.MaskedFields(cfg)
	if !fields["user.cookie"] {
		t.Fatalf("masked_fields 应标记 user.cookie 已配置: %v", fields)
	}
	// 原样回传哨兵 = 保持旧值（前端不重新输入时就是这样）。
	merged := config.MergeMasked(cfg, model.Config{User: model.UserConfig{Cookie: config.MaskSentinel}})
	if merged.User.Cookie != cfg.User.Cookie {
		t.Fatalf("回传哨兵应保持旧 Cookie，实际 %q", merged.User.Cookie)
	}
	// 空串 = 清空。
	merged = config.MergeMasked(cfg, model.Config{User: model.UserConfig{Cookie: ""}})
	if merged.User.Cookie != "" {
		t.Fatalf("回传空串应清空 Cookie，实际 %q", merged.User.Cookie)
	}
}

// TestLoginTestPrefersTypedCookieOverSavedToken 覆盖一个真实会踩的坑：
// 配置里已经存着 token（flow 每次换 token 都会回写，所以这是常态），用户粘上
// 新 Cookie 点「测试登录」——必须测 Cookie，而不是拿已保存的 token 去交差。
func TestLoginTestPrefersTypedCookieOverSavedToken(t *testing.T) {
	env := newTestEnv(t)
	cfg := cookieOnlyConfig()
	cfg.User.Token = "goodtoken0123456789" // 已保存且是好的
	env.saveConfig(cfg)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test",
		map[string]any{"cookie": "SESSION=typed-just-now"})
	env.expectStatus(res, http.StatusOK, "填了新 Cookie 就该测 Cookie")

	var out struct {
		Mode string `json:"mode"`
	}
	decodeBody(t, res.body, &out)
	if out.Mode != "cookie" {
		t.Fatalf("mode = %q，期望 cookie（配置里有 token 也不能抢走这次测试）", out.Mode)
	}
	if env.cookieCallCount() != 1 {
		t.Fatalf("Cookie 登录应被调用 1 次，实际 %d", env.cookieCallCount())
	}
}

// TestLoginTestSavedTokenOK：什么都不填、已保存的 token 可用 → 直接报 token 可用，
// 并且不触发任何登录（省一次上游请求，也避免把登录流量当常态）。
func TestLoginTestSavedTokenOK(t *testing.T) {
	env := newTestEnv(t)
	cfg := cookieOnlyConfig()
	cfg.User.Token = "goodtoken0123456789"
	env.saveConfig(cfg)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test", map[string]any{})
	env.expectStatus(res, http.StatusOK, "已保存 token 可用")

	var out struct {
		Mode string `json:"mode"`
	}
	decodeBody(t, res.body, &out)
	if out.Mode != "token" {
		t.Fatalf("mode = %q，期望 token", out.Mode)
	}
	if env.cookieCallCount() != 0 || env.ssoCallCount() != 0 {
		t.Fatalf("token 可用时不该登录：cookie=%d 账密=%d", env.cookieCallCount(), env.ssoCallCount())
	}
}

// TestLoginTestSavedChainRefreshesDeadToken：已保存的 token 已死、但存着可用的
// Cookie —— 这是"今晚会不会签上"的正确答案：会，因为 flow 会自动换新。
func TestLoginTestSavedChainRefreshesDeadToken(t *testing.T) {
	env := newTestEnv(t)
	cfg := cookieOnlyConfig()
	cfg.User.Token = "stale-token-0123456789"
	env.saveConfig(cfg)
	env.school.rejectToken("stale-token-0123456789")

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test", map[string]any{})
	env.expectStatus(res, http.StatusOK, "token 已死但 Cookie 可用，应报成功")

	var out struct {
		Mode    string `json:"mode"`
		Message string `json:"message"`
	}
	decodeBody(t, res.body, &out)
	if out.Mode != "cookie" {
		t.Fatalf("mode = %q，期望 cookie", out.Mode)
	}
	if !strings.Contains(out.Message, "已保存的 token 不可用") {
		t.Fatalf("message 应说明旧 token 已失效: %q", out.Message)
	}
	if env.cookieCallCount() != 1 {
		t.Fatalf("应调用一次 Cookie 登录，实际 %d", env.cookieCallCount())
	}
}

// TestLoginTestSavedDeadTokenWithoutCredential：token 死了又没有别的凭据 →
// 必须说清楚"没法自己换新"，而不是笼统的失败。
func TestLoginTestSavedDeadTokenWithoutCredential(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	cfg.User.Cookie = ""
	cfg.User.Token = "stale-token-0123456789"
	env.saveConfig(cfg)
	env.school.rejectToken("stale-token-0123456789")

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/test", map[string]any{})
	env.expectStatus(res, http.StatusBadGateway, "无凭据换新")
	var env2 struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, res.body, &env2)
	if !strings.Contains(env2.Error.Message, "没法自己换新") {
		t.Fatalf("message 应指出无法换新: %s", truncateForLog(res.body))
	}
}
