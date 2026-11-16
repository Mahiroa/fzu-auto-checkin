package server

import (
	"fmt"
	"net/http"
	"strings"

	"fzucheckin/internal/config"
	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
)

// loginTestBody 是「测试登录」的请求体。
//
// 四个字段用指针而**不是**字符串：必须能区分「这次没填」和「填了空」。
// 早先用 string 时踩过一个坑——配置里存着 token，用户粘上新 Cookie 点测试，
// 请求里 token 为空于是回落到已保存的 token，结果测试走的是 token 模式，
// Cookie 一次都没发出去，用户看到的却是「token 被拒绝」，正好被引向错误结论。
//
// 现在的口径：请求里明确给了哪个字段就测哪个；四个字段都没给，才按真实运行
// 顺序（token → 用凭据换新）走一遍，回答"今晚到底会不会签上"。
type loginTestBody struct {
	Cookie   *string `json:"cookie"`
	Username *string `json:"username"`
	Password *string `json:"password"`
	Token    *string `json:"token"`
}

// handleLoginTest 现场用一遍登录凭据，把"能不能换到 token、token 能不能用"直接答出来。
//
// 为什么值得单独做一个接口：登录失败的三种原因（Cookie 不是 sso 的、密码要验证码、
// token 过期）在 UI 上长得一模一样，用户只能反复试。这里把每一跳都返回去，
// 一眼就能看出断在哪。
//
// 语义约定（前端依赖）：
//   - 200 + ok:true  → 拿到 token 且学校端 init 正常，可以保存；
//   - 502 + login_failed → 登录链路失败，message 是上游/程序给的原因，
//     details 是逐跳诊断；
//   - 400 → 三种凭据一个都没填。
func (s *Server) handleLoginTest(w http.ResponseWriter, r *http.Request) {
	var body loginTestBody
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}

	saved := s.store.Config()
	formCookie, hasCookie := givenCredential(body.Cookie)
	formUser, hasUser := givenCredential(body.Username)
	formPass, hasPass := givenCredential(body.Password)
	formToken, hasToken := givenCredential(body.Token)

	// 脱敏用：把这次可能参与请求的凭据都装进一份配置，避免上游错误里带回的
	// Cookie/密码被原样写进响应与日志。
	probe := saved
	probe.User.Cookie = firstNonEmpty(formCookie, saved.User.Cookie)
	probe.User.Password = firstNonEmpty(formPass, saved.User.Password)
	probe.User.Token = firstNonEmpty(formToken, saved.User.Token)

	switch {
	case hasCookie:
		s.loginTestByCookie(w, r, probe, formCookie)
	case hasToken:
		s.loginTestByToken(w, r, probe, formToken)
	case hasUser || hasPass:
		username := firstNonEmpty(formUser, saved.User.Username)
		password := firstNonEmpty(formPass, saved.User.Password)
		s.loginTestByPassword(w, r, probe, username, password)
	default:
		s.loginTestSavedChain(w, r, probe, saved)
	}
}

// loginTestByCookie 测"刚填的这段 Cookie"。
func (s *Server) loginTestByCookie(w http.ResponseWriter, r *http.Request, probe model.Config, cookie string) {
	res, err := s.ssoCookieLogin(cookie)
	if err != nil {
		msg := s.sanitizeError(err.Error(), probe)
		s.logf("warn", "", "测试登录失败 mode=cookie：%s", msg)
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed, msg, res.Hops...))
		return
	}
	token := strings.TrimSpace(res.Token)
	if token == "" {
		s.logf("warn", "", "测试登录没有换到 token mode=cookie")
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"Cookie 登录流程走完了但没有取到晚点名 token（页面结构可能已变化）", res.Hops...))
		return
	}
	s.finishLoginTest(w, r, probe, "cookie", token, res.Note, res.Hops)
}

// loginTestByToken 只验证"这份 token 学校端认不认"，不涉及任何登录。
func (s *Server) loginTestByToken(w http.ResponseWriter, r *http.Request, probe model.Config, token string) {
	s.finishLoginTest(w, r, probe, "token", token,
		"使用填写的晚点名 token（未走登录）",
		[]string{"跳过统一身份认证，直接用填写的 token 请求学校端 init.action"})
}

// loginTestByPassword 测学号密码登录。
func (s *Server) loginTestByPassword(w http.ResponseWriter, r *http.Request, probe model.Config,
	username, password string) {

	if strings.TrimSpace(username) == "" || password == "" {
		writeError(w, newError(http.StatusBadRequest, codeBadRequest,
			"学号密码登录需要两项都填：只填了一项的话，请把另一半也补上"))
		return
	}
	fresh, err := s.ssoLogin(username, password)
	if err != nil {
		msg := s.sanitizeError(err.Error(), probe)
		s.logf("warn", "", "测试登录失败 mode=password：%s", msg)
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"学号密码登录失败："+msg,
			"如果提示需要验证码，说明学校侧对这台服务器的 IP 触发了风控登录；改用浏览器 Cookie 方式可绕过验证码。"))
		return
	}
	token := strings.TrimSpace(fresh)
	if token == "" {
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"学号密码登录走完了但没有取到晚点名 token（登录页结构可能已变化）"))
		return
	}
	s.finishLoginTest(w, r, probe, "password", token, "学号密码登录成功，已换到晚点名 token",
		[]string{"POST 统一身份认证（学号密码）→ 跟随 CAS 回调 → 取到 token"})
}

// loginTestSavedChain 在"什么都没填"时回答真正重要的问题：今晚会不会签上。
//
// 顺序与 flowRun 完全一致：先用已保存的 token；它不可用就按 Cookie → 学号密码
// 的顺序换一个新 token 再试。这样"token 死了但 Cookie 还能用"这种最常见的情况
// 会报成功，而不是报一句让人以为整晚都完蛋的失败。
func (s *Server) loginTestSavedChain(w http.ResponseWriter, r *http.Request, probe, saved model.Config) {
	token := strings.TrimSpace(saved.User.Token)
	if token != "" {
		if plan, err := s.initCheck(token); err == nil {
			s.writeLoginTestOK(w, r, "token", token, "已保存的 token 可用",
				[]string{"用已保存的 token 直接请求学校端 init.action"}, plan)
			return
		} else {
			msg := s.sanitizeError(err.Error(), probe)
			s.logf("info", "", "测试登录：已保存的 token 不可用（%s），尝试用凭据换新", msg)
		}
	}

	// token 缺失或不可用：用已保存的凭据换新（Cookie 优先，与 flowRun 一致）。
	cookie := strings.TrimSpace(saved.User.Cookie)
	account := hasAccountCredential(saved)
	if cookie == "" && !account {
		if token == "" {
			writeError(w, newError(http.StatusBadRequest, codeBadRequest,
				"三种凭据都还没填：可以填浏览器 Cookie、或填学号与密码、或直接粘贴晚点名 token，任选一种"))
			return
		}
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"配置里的 token 已经不可用，而且没有填浏览器 Cookie 或学号密码，服务没法自己换新 token；"+
				"请在「浏览器 Cookie」里填 sso.fzu.edu.cn 的 SESSION（或填学号密码）后再点测试"))
		return
	}

	var hops []string
	if cookie != "" {
		res, err := s.ssoCookieLogin(cookie)
		hops = res.Hops
		if err == nil && strings.TrimSpace(res.Token) != "" {
			note := "已保存的 token 不可用，已用已保存的 Cookie 换到新 token"
			s.finishLoginTest(w, r, probe, "cookie", strings.TrimSpace(res.Token), note+appendNote(res.Note), hops)
			return
		}
		if err == nil {
			err = fmt.Errorf("Cookie 登录没有返回 token（页面结构可能已变化）")
		}
		if !account {
			msg := s.sanitizeError(err.Error(), probe)
			s.logf("warn", "", "测试登录失败 mode=cookie：%s", msg)
			writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
				"已保存的 token 不可用，用 Cookie 换新也失败："+msg, hops...))
			return
		}
		s.logf("warn", "", "测试登录：Cookie 换新失败（%s），改用学号密码", s.sanitizeError(err.Error(), probe))
		hops = append(hops, "Cookie 换新失败，改用学号密码")
	}

	fresh, err := s.ssoLogin(saved.User.Username, saved.User.Password)
	if err != nil {
		msg := s.sanitizeError(err.Error(), probe)
		s.logf("warn", "", "测试登录失败 mode=password：%s", msg)
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"已保存的 token 不可用，用学号密码换新也失败："+msg, hops...))
		return
	}
	s.finishLoginTest(w, r, probe, "password", strings.TrimSpace(fresh),
		"已保存的 token 不可用，已用学号密码换到新 token",
		append(hops, "POST 统一身份认证（学号密码）→ 跟随 CAS 回调 → 取到 token"))
}

// finishLoginTest 校验 token 是否被学校端接受，然后输出 200/502。
func (s *Server) finishLoginTest(w http.ResponseWriter, r *http.Request, probe model.Config,
	mode, token, note string, hops []string) {

	if strings.TrimSpace(token) == "" {
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"登录流程走完了但没有取到晚点名 token（页面结构可能已变化）", hops...))
		return
	}
	plan, err := s.initCheck(token)
	if err != nil {
		msg := s.sanitizeError(err.Error(), probe)
		s.logf("warn", "", "测试登录拿到 token 但 init 失败 mode=%s：%s", mode, msg)
		// 措辞按模式区分：token 模式并没有"换"这个动作，说"换到了 token"会让人以为
		// 服务做了什么额外的事。
		message := "换到了 token，但学校端 init 没成功（token 可能已失效或对端维护中）：" + msg
		if mode == "token" {
			message = "用这份 token 请求学校端 init 未成功：" + msg
		}
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed, message, hops...))
		return
	}
	s.writeLoginTestOK(w, r, mode, token, note, hops, plan)
}

func (s *Server) writeLoginTestOK(w http.ResponseWriter, r *http.Request,
	mode, token, note string, hops []string, planLine string) {

	if hops == nil {
		hops = []string{}
	}
	s.logf("info", "", "测试登录成功 mode=%s ip=%s", mode, s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"mode":       mode,
		"message":    note,
		"token_hint": maskSecret(token),
		"school_ok":  true,
		"hops":       hops,
		"plan":       planLine,
	})
}

// initCheck 用 token 打一次 init，返回计划摘要。
func (s *Server) initCheck(token string) (string, error) {
	initData, err := s.clientFactory(token).Init()
	if err != nil {
		return "", err
	}
	return describeInit(initData), nil
}

// givenCredential 判断这次请求是否"明确给了这个字段"。
// nil / 空串 / 脱敏哨兵都表示没给（前端把已保存的值显示成哨兵，用户不重填就原样回传）。
func givenCredential(v *string) (string, bool) {
	if v == nil {
		return "", false
	}
	trimmed := strings.TrimSpace(*v)
	if trimmed == "" || trimmed == config.MaskSentinel {
		return "", false
	}
	return trimmed, true
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// appendNote 把可选说明拼到结论后面（空则不加）。
func appendNote(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return "（" + note + "）"
}

// describeInit 把 init 的结论压缩成一句人话，便于直接显示在测试结果里。
func describeInit(initData map[string]any) string {
	plan, err := fzu.ParseInit(initData)
	if err != nil {
		return ""
	}
	var b strings.Builder
	if !plan.HasPlan {
		b.WriteString("学校端 init 正常，但今日没有晚点名计划")
		return b.String()
	}
	fmt.Fprintf(&b, "学校端 init 正常，今日计划 %s-%s", orDash(plan.Start), orDash(plan.End))
	if plan.Checked {
		b.WriteString("，学校端显示今日已签到")
	} else {
		b.WriteString("，尚未签到")
	}
	return b.String()
}
