package server

import (
	"errors"
	"fmt"
	"strings"

	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
)

// logFunc 是 flow 里那条"带级别的日志"回调（同时写内存环与 SSE）。
type logFunc func(level, format string, args ...any)

// mintToken 换一个新的晚点名免登 token。
//
// 优先级是 Cookie → 学号密码，理由不是"新写法更时髦"，而是：
//   - Cookie 复用浏览器里已经通过人机校验的会话，不碰密码；
//   - 学号密码要现算 AES 加密再 POST，学校侧一旦要求验证码就整条路走不通。
//
// 两者都配了就先用 Cookie，失败再退学号密码——多花一次请求，换"少一次漏签"。
// 这条顺序必须钉死（login_second_auth_test.go 的 TestMintTokenUsesCookieBeforePassword
// 有断言）：反过来的话，密码通道会先撞上风控验证码，而 Cookie 那条本来还能用。
//
// 关键约束：**两条通道的失败原因都必须留住**。只报最后一条会把两类完全不同的
// 处置混成一句"登录失败"——Cookie 那条要用户重新导出会话，账密那条在云端
// （校外 IP）多半是学校要求短信二次验证。用户看到的原因决定他下一步动哪里。
//
// 返回的 source 是给人看的中文来源，直接写进日志与推送。
func (s *Server) mintToken(cfg model.Config, addLog logFunc) (token, source string, err error) {
	if addLog == nil {
		addLog = func(string, string, ...any) {}
	}
	cookie := strings.TrimSpace(cfg.User.Cookie)
	account := hasAccountCredential(cfg)

	var cookieErr error
	if cookie != "" {
		addLog("info", "用浏览器 Cookie 换取晚点名 token（不需要密码）…")
		res, err := s.ssoCookieLogin(cookie)
		if err == nil {
			if token = strings.TrimSpace(res.Token); token != "" {
				return token, "浏览器 Cookie", nil
			}
			err = errors.New("Cookie 登录没有返回 token（登录页结构可能已变化）")
		}
		cookieErr = err
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
		merged := mergeLoginErrors(cookieErr, loginErr)
		if errors.Is(loginErr, fzu.ErrSecondAuth) {
			// 二次认证不是"坏了"：账号密码已经通过了，缺的是只有真人能做的一步。
			// 云端部署（校外 IP）上这就是常态，所以文案直接给可操作的下一步，
			// 而不是让用户去查配置、查网络、查页面结构——2026-11-18 那次就是
			// 因为这里只说了一句"没有可读原因"，用户完全不知道该干嘛。
			return "", "学号密码", fmt.Errorf("%w；%s", merged, secondAuthManualSteps)
		}
		return "", "学号密码", merged
	}
	token = strings.TrimSpace(fresh)
	if token == "" {
		return "", "学号密码", errors.New("SSO 登录返回了空 token（登录页结构可能已变化）")
	}
	return token, "学号密码", nil
}

// mergeLoginErrors 把"Cookie 换 token 失败"和"学号密码登录失败"两条原因都留住。
//
// %w 指向真正让这次登录断掉的那一条（学号密码），所以 errors.Is/As 判
// ErrSecondAuth 之类的哨兵不受影响；Cookie 那条以纯文本留在同一句话里，
// 用户才能看出"两条都不通"而不是"某一条失败了"。
func mergeLoginErrors(cookieErr, loginErr error) error {
	if cookieErr == nil {
		return loginErr
	}
	if loginErr == nil {
		return cookieErr
	}
	return fmt.Errorf("两条通道都不通——浏览器 Cookie：%s；学号密码：%w", cookieErr.Error(), loginErr)
}

// hasLoginCredential 判断除 token 之外还有没有办法登录（决定 init 失败后要不要重试一次）。
func hasLoginCredential(cfg model.Config) bool {
	return strings.TrimSpace(cfg.User.Cookie) != "" || hasAccountCredential(cfg)
}

func hasAccountCredential(cfg model.Config) bool {
	return strings.TrimSpace(cfg.User.Username) != "" && strings.TrimSpace(cfg.User.Password) != ""
}
