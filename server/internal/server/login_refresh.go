package server

import (
	"errors"
	"net/http"
	"strings"

	"fzucheckin/internal/model"
)

// handleLoginRefresh 是「一键获取免登凭证」：用已保存（或这次填的）Cookie / 学号密码
// 走一遍学校登录链路，把换到的 token **验证后写进配置**，并立刻叫醒调度器重新规划。
//
// 与 /login/test 的分工：
//   - /login/test 只回答"这条凭据行不行"，一个字都不写；
//   - /login/refresh 直接把可用凭据落盘，用户不必再去复制 token 回来粘一遍。
//
// 落盘前必须先 init 验证通过：写进配置的 token 会成为接下来每一轮的凭证，
// 一个"看似换到但学校端不认"的值只会让后面每一次运行都撞同一堵墙。
func (s *Server) handleLoginRefresh(w http.ResponseWriter, r *http.Request) {
	var body loginTestBody
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}

	saved := s.store.Config()
	formCookie, hasCookie := givenCredential(body.Cookie)
	formUser, hasUser := givenCredential(body.Username)
	formPass, hasPass := givenCredential(body.Password)

	cookie := firstNonEmpty(formCookie, saved.User.Cookie)
	username := firstNonEmpty(formUser, saved.User.Username)
	password := firstNonEmpty(formPass, saved.User.Password)

	probe := saved
	probe.User.Cookie = cookie
	probe.User.Username = username
	probe.User.Password = password

	account := strings.TrimSpace(username) != "" && password != ""
	if strings.TrimSpace(cookie) == "" && !account {
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			"还没有能自动换凭证的登录凭据：先在浏览器登录 sso.fzu.edu.cn，把 SESSION 粘进上面的「浏览器 Cookie」（或填学号与密码），再点一次这个按钮",
			"不想碰 Cookie 也可以：浏览器里打开晚点名页面，把地址栏 token= 后面那一段直接粘进「免登凭证」并保存。"))
		return
	}
	_ = hasUser
	_ = hasPass

	var (
		token, mode, note string
		hops              []string
	)
	if c := strings.TrimSpace(cookie); c != "" {
		res, err := s.ssoCookieLogin(c)
		hops = res.Hops
		if err == nil && strings.TrimSpace(res.Token) != "" {
			token, mode, note = strings.TrimSpace(res.Token), "cookie", "已用浏览器 Cookie 换到新的免登凭证"
		} else {
			if err == nil {
				err = errors.New("Cookie 登录没有返回 token（页面结构可能已变化）")
			}
			if !account {
				msg := s.sanitizeError(err.Error(), probe)
				s.logf("warn", "", "换免登凭证失败 mode=cookie：%s", msg)
				writeError(w, newError(http.StatusBadGateway, codeLoginFailed, msg, hops...))
				return
			}
			s.logf("warn", "", "Cookie 换凭证失败（%s），改用学号密码", s.sanitizeError(err.Error(), probe))
			hops = append(hops, "Cookie 换凭证失败，改用学号密码")
		}
	}
	if token == "" {
		fresh, err := s.ssoLogin(username, password)
		if err != nil {
			msg := s.sanitizeError(err.Error(), probe)
			s.logf("warn", "", "换免登凭证失败 mode=password：%s", msg)
			writeError(w, newError(http.StatusBadGateway, codeLoginFailed, "学号密码登录失败："+msg, hops...))
			return
		}
		token, mode, note = strings.TrimSpace(fresh), "password", "已用学号密码换到新的免登凭证"
		if hops == nil {
			hops = []string{"POST 统一身份认证（学号密码）→ 跟随 CAS 回调 → 取到 token"}
		}
	}
	if token == "" {
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"登录流程走完了但没有取到晚点名 token（页面结构可能已变化）", hops...))
		return
	}

	plan, initErr := s.initCheck(token)
	if initErr != nil {
		msg := s.sanitizeError(initErr.Error(), probe)
		s.logf("warn", "", "换到凭证但 init 失败，未写入配置 mode=%s：%s", mode, msg)
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"换到了凭证但学校端 init 没成功，没有写入配置："+msg, hops...))
		return
	}

	// 用户这次亲手粘的 Cookie 一并落盘：否则明天 token 过期时又得重新粘一次。
	saveCookie := hasCookie
	version, saveErr := s.store.UpdateConfig(func(latest *model.Config) []string {
		latest.User.Token = token
		if saveCookie {
			latest.User.Cookie = formCookie
		}
		return nil
	})
	if saveErr != nil {
		writeError(w, newError(http.StatusInternalServerError, codeSaveFailed,
			"凭证换到了，但写入配置失败："+s.sanitizeError(saveErr.Error(), probe)))
		return
	}

	s.wakeScheduler("免登凭证已更新")
	s.logf("info", "", "已换新免登凭证 mode=%s 写入 config v%d cookie_saved=%t ip=%s",
		mode, version, saveCookie, s.clientIP(r))
	if hops == nil {
		hops = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"mode":         mode,
		"message":      note,
		"token_hint":   maskSecret(token),
		"plan":         plan,
		"saved":        true,
		"cookie_saved": saveCookie,
		"version":      version,
		"hops":         hops,
	})
}
