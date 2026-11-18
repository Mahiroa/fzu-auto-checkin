package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"fzucheckin/internal/config"
	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
)

// secondAuthManualSteps 是「校外 IP 要短信验证」时唯一可行的下一步。
//
// 为什么单独抽出来：同一个结论要出现在三个地方——日志（排查用）、运行记录的
// error（历史与推送用）、409 的 message（面板用）。三处各写一遍迟早漂移。
const secondAuthManualSteps = "在面板里填手机收到的那 6 位数字（验证码会自动发到你手机，填满就自动提交）；也可以在自己浏览器里登录后把地址栏 token= 后面那串粘进「晚点名 token」，或在校园网/VPN 里再试一次"

// secondAuthResponse 是二次认证的专用响应体（HTTP 409）。
//
// 为什么不按标准错误信封回：前端要把 phone/tip 直接渲染成「已向 138****1234
// 发送验证码」，塞进 details 就只能当一段要正则解析的文字。reason 是稳定机器码
// （second_auth），前端按它分支而不是匹配中文。
type secondAuthResponse struct {
	OK         bool              `json:"ok"`
	Reason     string            `json:"reason"`
	Message    string            `json:"message"`
	SecondAuth secondAuthPayload `json:"second_auth"`
	// SMSAvailable 告诉面板"409 之后还有一条路"：这个进程支持面板内的短信二次认证。
	//
	// 它是个**能力位**而不是"现在就能发码"的承诺：配置里没填学号密码时只能把原因
	// 写在 sms_error 里，前端在那条分支上本来就要引导用户去填凭据。
	SMSAvailable bool `json:"sms_available"`

	// 以下五个字段是「不用再点发送」的全部凭据（2026-11-18 深夜）：
	// 登录被判二次认证的这一刻，服务端已经用配置里的学号密码**当场**发了验证码。
	//
	//   - Challenge 非空即表示码已经在用户手机上（复用上一条时同样非空）；发码失败/只有
	//     Cookie 时是空串，前端据此决定是否直接弹出输入框；
	//   - Sent 区分"这一轮真的发了"与"复用了还在有效期内的上一条"；
	//   - ResendAfter / ExpiresIn 是给倒计时用的秒数（0 表示暂时没有可用的倒计时）；
	//   - SMSError 非空表示这次没能自动发码，原因写在里面（人话）。
	//
	// 409 本身一个字节都不改：状态码、reason 都没有变——发码失败也只是把结论写进
	// SMSError，绝不会把 409 变成 502 或 200。
	Challenge   string `json:"challenge"`
	Sent        bool   `json:"sent"`
	ResendAfter int    `json:"resend_after"`
	ExpiresIn   int    `json:"expires_in"`
	// TokenHint 只在"这次根本没触发二次认证、token 已经拿到"时有值。
	TokenHint string `json:"token_hint,omitempty"`
	SMSError  string `json:"sms_error,omitempty"`
}

// secondAuthPayload 只带"够用户认出来"的信息：手机号已打码，tip 是学校原话。
// 学号之类的标识一律不带出去——它已经在校验成功的账号里，回显没有必要。
type secondAuthPayload struct {
	Phone string `json:"phone"`
	Tip   string `json:"tip"`
}

// onlyCookieSMSError 是"409 但没法自动发码"里最常见的那一句：凭据不全。
const onlyCookieSMSError = "只有 Cookie 无法自动发码，需要学号密码：「浏览器 Cookie」换不出 token 时，" +
	"学校要求的那条短信只能靠学号密码这一步去触发；请在「账号」里补上学号与密码后重试" +
	"（也可以直接在浏览器里打开晚点名，把地址栏 token= 后面那串粘进「晚点名 token」）"

// writeSecondAuth 按契约回 409 + second_auth，并**当场**把验证码发出去；
// err 与二次认证无关时返回 false，调用方继续走原来的 502/400 分支。
//
// 为什么优先级最高：二次认证不是链路故障，它是"缺一步人工操作"。把它混进
// login_failed 里，用户会去改密码、改 Cookie、重装服务，而真正该做的只是
// 在校园网跑一次、或粘一份 token。
//
// 为什么在这里就发码（2026-11-18 深夜）：早先 409 只把"你可以点发送"告诉前端，
// 用户必须再点一次按钮才走 POST /login/sms/start —— 明明是个可以替用户做的事。
// 现在 409 自己带上 challenge：前端拿到就能直接弹验证码输入框。
//
// 三条纪律：
//   - 凭据只从配置取（s.store.Config()）：这条路上没有表单凭据可用，而把用户刚填的
//     密码塞进日志/响应更没必要；
//   - 密码为空（只有 Cookie）时不发码，只把原因写进 sms_error；
//   - 发码失败不改 409，只把原因写进 sms_error（409 是"要人做一步"的语义，与发码
//     成不成功无关；前端据 reason 分支，不会被一次上游故障带偏）。
func (s *Server) writeSecondAuth(w http.ResponseWriter, err error, username, password string) bool {
	sa, ok := asSecondAuth(err)
	if !ok {
		return false
	}
	// 发码必须用「这次真正尝试的那组凭据」——用户可能在表单里临时填了另一个账号再点测试，
	// 用配置里那份发码，验证码会跑到另一个人的手机上（实测踩过这个语义坑）。
	cfg := s.store.Config()
	username = firstNonEmpty(strings.TrimSpace(username), strings.TrimSpace(cfg.User.Username))
	password = firstNonEmpty(password, cfg.User.Password)

	resp := secondAuthResponse{
		OK:           false,
		Reason:       "second_auth",
		SecondAuth:   secondAuthPayload{Phone: sa.Phone, Tip: sa.Tip},
		SMSAvailable: true,
	}

	var (
		begin  *smsBegin
		apiErr *apiError
	)
	if password == "" {
		// 只有 Cookie：学校那条短信必须由"学号密码"这一步触发，硬发也不会成功。
		resp.SMSError = onlyCookieSMSError
		s.logf("info", "", "面板登录：二次认证，但没有可用的学号密码，未自动发码")
	} else {
		begin, apiErr = s.beginSecondAuth(username, password)
	}

	if begin != nil {
		resp.Challenge = begin.liveChallenge()
		resp.Sent = begin.Sent
		resp.ResendAfter = begin.ResendAfter
		resp.ExpiresIn = begin.ExpiresIn
		resp.TokenHint = begin.TokenHint
		// 学校页面上的手机号/原话是这条 409 的原始信息；哨兵错误没带结构时才用会话里的补上。
		if resp.SecondAuth.Phone == "" {
			resp.SecondAuth.Phone = begin.Phone
		}
		if resp.SecondAuth.Tip == "" {
			resp.SecondAuth.Tip = begin.Tip
		}
	}
	if apiErr != nil {
		resp.SMSError = smsErrorText(begin, apiErr)
		// 409 这条路上没有把句柄交给前端：留着一条"码根本没发出去"的会话，只会让下一次
		// 登录被判二次认证时被复用分支认领，白白承诺一条永远不会到的短信。
		// （/login/sms/start 那条路不动：它会把 challenge 一起回给前端，句柄还有用。）
		if begin != nil && begin.Challenge != "" && !begin.Sent && !begin.Reused {
			smsChallenges.drop(s, begin.Challenge)
		}
		s.logf("warn", "", "面板登录：二次认证但自动发码没成功 reason=%s：%s", apiErr.Code, resp.SMSError)
	} else if begin != nil && begin.Sent {
		s.logf("info", "", "面板登录：二次认证，已自动发码 phone=%s challenge=%s",
			begin.Phone, shortChallenge(begin.Challenge))
	} else if begin != nil && begin.Reused {
		s.logf("info", "", "面板登录：二次认证，复用还在有效期内的验证码 challenge=%s",
			shortChallenge(begin.Challenge))
	}

	resp.Message = secondAuthMessage(sa) + secondAuthAutoNote(begin, apiErr, resp.SMSError)
	writeJSON(w, http.StatusConflict, resp)
	return true
}

// smsErrorText 把"为什么没自动发出码"翻成一句人话（409 的 sms_error 直接取它）。
//
// 每一条都要给出用户能做的动作：只写"发码失败"等于把人扔在原地。
func smsErrorText(begin *smsBegin, e *apiError) string {
	if e == nil {
		return ""
	}
	secs := 0
	if begin != nil {
		secs = begin.ResendAfter
	}
	switch e.Code {
	case "no_credentials":
		return "配置里没有可用的学号与密码，没法自动发码：先在「账号」里填好学号密码再点一次"
	case "too_frequent":
		if secs > 0 {
			return fmt.Sprintf("%s（还要等 %d 秒）", e.Message, secs)
		}
		return e.Message
	case "phone_unbound":
		return e.Message
	case "challenge_expired":
		return "这次二次认证的会话已经失效，没法发码：请重新点一次「测试登录」或「获取凭证」"
	default:
		return e.Message
	}
}

// secondAuthAutoNote 给 409 的 message 追加一句"这次自动发码的结论"。
//
// 老前端只读 message 也能看懂发生了什么；新前端以 challenge / sent / sms_error 为准。
// 只追加、不改写：secondAuthMessage 里那三条可行路径必须原样留着。
func secondAuthAutoNote(begin *smsBegin, apiErr *apiError, smsErr string) string {
	switch {
	case apiErr != nil:
		// 自动发码失败时也要说清楚"为什么"和"还能怎么办"，否则 409 的其余文案会让人
		// 以为码已经在路上了（用户会一直盯着手机等一条永远不会来的短信）。
		if strings.TrimSpace(smsErr) == "" {
			return "（这次没能自动发出验证码，可以点「发送验证码」重试，或改用浏览器取 token）"
		}
		return "（这次没能自动发出验证码：" + smsErr + "）"
	case begin == nil:
		return ""
	case begin.Mode == "password":
		return "（这次用学号密码登录没有触发二次验证，晚点名 token 已经拿到并保存，可以跳过短信这一步）"
	case begin.Sent:
		return fmt.Sprintf("（验证码已经自动发到 %s，直接填手机上那 6 位数字即可，不用再点「发送验证码」）",
			begin.Phone)
	case begin.Reused:
		return fmt.Sprintf("（上一条验证码还在有效期内，直接填手机上最近收到的那条；%d 秒后才允许重发）",
			begin.ResendAfter)
	}
	return ""
}

// asSecondAuth 取出二次认证的结构化信息。
//
// 命中哨兵但没带结构时返回空结构而不是 nil：调用方可以无条件取 .Phone/.Tip，
// 不必再判一次空指针（日志里出现过包装成裸 ErrSecondAuth 的情形）。
func asSecondAuth(err error) (*fzu.SecondAuthError, bool) {
	if err == nil {
		return nil, false
	}
	var sa *fzu.SecondAuthError
	if errors.As(err, &sa) && sa != nil {
		return sa, true
	}
	if errors.Is(err, fzu.ErrSecondAuth) {
		return &fzu.SecondAuthError{}, true
	}
	return nil, false
}

// secondAuthMessage 拼 409 里那句人话：说清"这不是故障"，再给能立刻做的事。
//
// 2026-11-18 晚加了口内的短信通道（POST /login/sms/start）之后，这段文案的第一条
// 从"人工去浏览器"变成"面板里点发送验证码"——否则面板明明能发码，提示却让用户
// 去开 DevTools，等于自己打自己的脸。2026-11-18 深夜起这一条连点都不必点了：
// 409 自己就把码发了（结论追加在后面的 secondAuthAutoNote 里）。
func secondAuthMessage(sa *fzu.SecondAuthError) string {
	var b strings.Builder
	b.WriteString("学校要求短信二次验证：账号密码已经通过了，只差手机上那 6 位数字")
	if sa != nil && sa.Phone != "" {
		fmt.Fprintf(&b, "（验证码发往 %s）", sa.Phone)
	}
	if sa != nil && sa.Tip != "" {
		fmt.Fprintf(&b, "。学校原话：%s", sa.Tip)
	}
	b.WriteString("。三条可行路径：")
	b.WriteString("① 在面板里填这 6 位数字——验证码会自动发到你手机，填满就自动提交（最省事）；")
	b.WriteString("② 在校园网/VPN 里再跑一次——校内 IP 不触发二次验证，会直接成功；")
	b.WriteString("③ 在浏览器里打开晚点名页面，把地址栏 token= 后面那串粘进「晚点名 token」。")
	return b.String()
}

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
		// 校外 IP 上 Cookie 通道同样会被要求短信验证（现场取证过），这时该回 409
		// 让人去校内网或改用 token，而不是回一句"Cookie 不是登录态"。
		// Cookie 通道没有"这次用的学号密码"，交给 writeSecondAuth 回落配置里的那份。
		if s.writeSecondAuth(w, err, "", "") {
			return
		}
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
		if s.writeSecondAuth(w, err, username, password) {
			return
		}
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed,
			"学号密码登录失败："+msg,
			"如果提示需要验证码，说明学校侧对这台服务器的 IP 触发了风控登录；校外 IP 上改用 Cookie 通常也绕不过去（同样会被要求短信验证），"+
				"可行的做法是在校园网/VPN 里跑一次，或在浏览器里打开晚点名页面把地址栏的 token 粘进来。"))
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
			if s.writeSecondAuth(w, err, "", "") {
				return
			}
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
		// 这一条是云端部署最常见的场景：已保存的 token 过期 + 换新被二次认证挡住。
		// 它既不是配置错也不是网络坏，唯一的出路是人工粘一份新 token，所以回 409。
		if s.writeSecondAuth(w, err, saved.User.Username, saved.User.Password) {
			return
		}
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
