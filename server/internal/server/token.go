package server

import (
	"errors"
	"strings"

	"fzucheckin/internal/model"
)

// logFunc 是 flow 里那条"带级别的日志"回调（同时写内存环与 SSE）。
type logFunc func(level, format string, args ...any)

// mintToken 换一个新的晚点名免登 token。
//
// 优先级是 Cookie → 学号密码，理由不是"新写法更时髦"，而是：
//   - Cookie 复用浏览器里已经通过人机校验的会话，不碰密码，也不会触发风控验证码；
//   - 学号密码要现算 AES 加密再 POST，学校侧一旦要求验证码就整条路走不通。
//
// 两者都配了就先用 Cookie，失败再退学号密码——多花一次请求，换"少一次漏签"。
// 返回的 source 是给人看的中文来源，直接写进日志与推送。
func (s *Server) mintToken(cfg model.Config, addLog logFunc) (token, source string, err error) {
	if addLog == nil {
		addLog = func(string, string, ...any) {}
	}
	cookie := strings.TrimSpace(cfg.User.Cookie)
	account := hasAccountCredential(cfg)

	if cookie != "" {
		addLog("info", "用浏览器 Cookie 换取晚点名 token（不需要密码）…")
		res, cookieErr := s.ssoCookieLogin(cookie)
		if cookieErr == nil {
			if token = strings.TrimSpace(res.Token); token != "" {
				return token, "浏览器 Cookie", nil
			}
			cookieErr = errors.New("Cookie 登录没有返回 token（登录页结构可能已变化）")
		}
		if !account {
			return "", "浏览器 Cookie", cookieErr
		}
		addLog("warn", "Cookie 登录失败，改用学号密码：%s", cookieErr.Error())
	}

	if !account {
		return "", "", errors.New(
			"没有可用的登录凭据：请在「登录凭据」里填浏览器 Cookie（sso.fzu.edu.cn 的 SESSION）、" +
				"或填学号与密码、或直接粘贴晚点名 token，三选一")
	}

	addLog("info", "使用学号密码登录统一身份认证…")
	fresh, loginErr := s.ssoLogin(cfg.User.Username, cfg.User.Password)
	if loginErr != nil {
		return "", "学号密码", loginErr
	}
	token = strings.TrimSpace(fresh)
	if token == "" {
		return "", "学号密码", errors.New("SSO 登录返回了空 token（登录页结构可能已变化）")
	}
	return token, "学号密码", nil
}

// hasLoginCredential 判断除 token 之外还有没有办法登录（决定 init 失败后要不要重试一次）。
func hasLoginCredential(cfg model.Config) bool {
	return strings.TrimSpace(cfg.User.Cookie) != "" || hasAccountCredential(cfg)
}

func hasAccountCredential(cfg model.Config) bool {
	return strings.TrimSpace(cfg.User.Username) != "" && strings.TrimSpace(cfg.User.Password) != ""
}
