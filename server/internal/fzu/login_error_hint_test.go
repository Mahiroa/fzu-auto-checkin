package fzu

import (
	"strings"
	"testing"
)

// ssoWrongPasswordPage 是真实 401 响应的精简版（新版统一身份认证）：
// #login-error-msg 是空的，错误文案只出现在正文里，#captchaId 为空表示不要验证码。
const ssoWrongPasswordPage = `<!doctype html><html lang="zh"><head><title>统一身份认证平台</title></head>
<body>
<div style="display:none">
  <p id="current-login-type">UsernamePassword</p>
  <p id="login-error-code"></p>
  <p id="login-error-msg"></p>
  <p id="captchaId"></p>
  <p id="captcha-url"></p>
  <p id="recaptcha-invisible"></p>
</div>
<div class="error-tip">用户名或密码错误</div>
</body></html>`

// ssoCaptchaPage 是"要求验证码"的形态：captchaId 有值。
const ssoCaptchaPage = `<html><body><p id="login-error-msg"></p>
<p id="captchaId">0c7f5c1e9d2b4a8e</p></body></html>`

func TestLoginErrorHintFindsChineseReason(t *testing.T) {
	if got := loginErrorHint([]byte(ssoWrongPasswordPage)); got != "用户名或密码错误" {
		t.Fatalf("应从整页文本里认出原因，实际 %q", got)
	}
	if !isWrongCredential("用户名或密码错误") {
		t.Fatal("「用户名或密码错误」应被识别成凭据错误")
	}
}

func TestLoginErrorHintCaptcha(t *testing.T) {
	got := loginErrorHint([]byte(ssoCaptchaPage))
	if !strings.Contains(got, "验证码") {
		t.Fatalf("captchaId 非空时应明确说要求验证码，实际 %q", got)
	}
	if isWrongCredential(got) {
		t.Fatal("要求验证码不能被说成「密码不对」")
	}
}

// TestLoginErrorHintPrefersExplicitMessage：老前端会把原因写在 #login-error-msg 里，
// 这条路径必须继续优先（比整页文本更精确）。
func TestLoginErrorHintPrefersExplicitMessage(t *testing.T) {
	page := `<html><body><div id="login-error-msg">账号已被锁定，请联系管理员</div>
<div>用户名或密码错误</div></body></html>`
	if got := loginErrorHint([]byte(page)); got != "账号已被锁定，请联系管理员" {
		t.Fatalf("应优先取 login-error-msg，实际 %q", got)
	}
}

func TestLoginErrorHintFallback(t *testing.T) {
	got := loginErrorHint([]byte("<html><body>结构完全变了</body></html>"))
	if !strings.Contains(got, "没有给出可读原因") {
		t.Fatalf("兜底文案不对: %q", got)
	}
	if isWrongCredential(got) {
		t.Fatal("兜底文案不该被当成凭据错误")
	}
}
