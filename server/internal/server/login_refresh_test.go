package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// refreshResult 是 /login/refresh 的响应体（只取断言用到的字段）。
type refreshResult struct {
	OK          bool   `json:"ok"`
	Mode        string `json:"mode"`
	Message     string `json:"message"`
	TokenHint   string `json:"token_hint"`
	Plan        string `json:"plan"`
	Saved       bool   `json:"saved"`
	CookieSaved bool   `json:"cookie_saved"`
	Version     int64  `json:"version"`
}

// TestLoginRefreshSavesTokenFromSavedCookie：已保存 Cookie、没有 token 时，
// 点一次「获取免登凭证」就应该把可用的 token 写进配置——用户不必再复制粘贴。
func TestLoginRefreshSavesTokenFromSavedCookie(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())
	before := env.store.ConfigVersion()

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/refresh", map[string]any{})
	env.expectStatus(res, http.StatusOK, "POST /login/refresh")

	var out refreshResult
	decodeBody(t, res.body, &out)
	if !out.OK || !out.Saved || out.Mode != "cookie" {
		t.Fatalf("响应不符: %s", truncateForLog(res.body))
	}
	if got := env.store.Config().User.Token; got != env.cookieToken {
		t.Fatalf("token 应被写入配置：期望 %q，实际 %q", env.cookieToken, got)
	}
	if out.Version <= before {
		t.Fatalf("配置版本应从 %d 递增，实际 %d", before, out.Version)
	}
	if out.CookieSaved {
		t.Fatal("这次没有新填 Cookie，不该报 cookie_saved")
	}
	if strings.Contains(string(res.body), env.cookieToken) {
		t.Fatalf("响应里不能出现 token 原文: %s", truncateForLog(res.body))
	}
}

// TestLoginRefreshPersistsTypedCookie：这次刚粘的 Cookie 要跟着一起存下来，
// 否则明天 token 过期时用户又得重新粘一遍。
func TestLoginRefreshPersistsTypedCookie(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	cfg.User.Token = ""
	cfg.User.Cookie = ""
	env.saveConfig(cfg)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/refresh",
		map[string]any{"cookie": "SESSION=typed-just-now"})
	env.expectStatus(res, http.StatusOK, "带 Cookie 换凭证")

	var out refreshResult
	decodeBody(t, res.body, &out)
	if !out.CookieSaved {
		t.Fatalf("这次填了 Cookie，应报 cookie_saved: %s", truncateForLog(res.body))
	}
	saved := env.store.Config()
	if saved.User.Cookie != "SESSION=typed-just-now" {
		t.Fatalf("Cookie 应被保存，实际 %q", saved.User.Cookie)
	}
	if saved.User.Token != env.cookieToken {
		t.Fatalf("token 应被保存，实际 %q", saved.User.Token)
	}
}

// TestLoginRefreshNeedsCredential：没有任何能自动换凭证的凭据时，
// 必须给出可照做的下一步，而不是一句"失败"。
func TestLoginRefreshNeedsCredential(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	env.saveConfig(cfg)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/refresh", map[string]any{})
	env.expectStatus(res, http.StatusBadRequest, "没有凭据")
	var env2 struct {
		Error struct {
			Message string   `json:"message"`
			Details []string `json:"details"`
		} `json:"error"`
	}
	decodeBody(t, res.body, &env2)
	if !strings.Contains(env2.Error.Message, "浏览器 Cookie") {
		t.Fatalf("message 应指明要填什么: %s", truncateForLog(res.body))
	}
	if len(env2.Error.Details) == 0 {
		t.Fatal("details 里应给出不走 Cookie 的替代做法")
	}
}

// TestLoginRefreshVerifiesBeforeSaving：换到的 token 学校端不认时，
// 绝不能写进配置——否则接下来每一轮都会拿它撞同一堵墙。
func TestLoginRefreshVerifiesBeforeSaving(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(cookieOnlyConfig())
	env.school.rejectToken(env.cookieToken)

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/refresh", map[string]any{})
	env.expectStatus(res, http.StatusBadGateway, "token 不可用")
	if code := errCode(t, res.body); code != codeLoginFailed {
		t.Fatalf("错误码 = %q", code)
	}
	if got := env.store.Config().User.Token; got != "" {
		t.Fatalf("不可用的 token 不该被写入配置，实际 %q", got)
	}
	if env.cookieCallCount() != 1 {
		t.Fatalf("应尝试过 Cookie 登录，实际 %d 次", env.cookieCallCount())
	}
}

// TestLoginRefreshFallsBackToPassword：Cookie 失效但配了账密时，
// 一键换凭证应当自动改走账密，而不是把失败甩给用户。
func TestLoginRefreshFallsBackToPassword(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Cookie = "SESSION=stale"
	cfg.User.Token = ""
	env.saveConfig(cfg)
	env.setCookieErr(errors.New("提交的 Cookie 在统一身份认证上不是登录状态"))

	res := env.do(env.authedClient(), http.MethodPost, apiPrefix+"/login/refresh", map[string]any{})
	env.expectStatus(res, http.StatusOK, "Cookie 失败回落账密")

	var out refreshResult
	decodeBody(t, res.body, &out)
	if out.Mode != "password" {
		t.Fatalf("mode = %q，期望 password", out.Mode)
	}
	if got := env.store.Config().User.Token; got != env.ssoToken {
		t.Fatalf("应写入账密换来的 token，实际 %q（期望 %q）", got, env.ssoToken)
	}
}
