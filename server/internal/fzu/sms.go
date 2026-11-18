package fzu

// 这个文件是「校外 IP 短信二次验证」的唯一实现处：把统一身份认证在浏览器里
// 做的那几步（密码 → 发验证码 → 校验 → 提交换票）用 HTTP 复刻出来。
//
// 为什么值得复刻而不是让用户自己去浏览器里点：服务跑在机房 IP 上时，密码登录
// 一定会被拦到短信这一步（2026-11-18 现场取证，见 docs/go-service-design.md §21.3），
// 而晚点名 token 又只有这一条路能换。把「发码 + 校验」搬进面板，用户就只剩把手机上
// 的 6 位数字填进来——比开 DevTools 找 SESSION、或从地址栏抄 token 都省事。
//
// 链路（每一步都是照着实测响应写的，不是猜的）：
//
//	GET  /login?service=…                       → 登录页（#login-croypto / #login-page-flowkey）
//	POST /login  type=UsernamePassword …        → 校外 IP：二次认证页
//	                                              （#sso-second=true / #phone-number / #second-auth-user-id）
//	POST /api/protected/sms/publicNoToken/sendSmsCode  {phone, businessNo:"0008"}
//	                                              → {"code":200,"data":{"result":true,"sendInterval":120,…}}
//	POST /api/protected/sms/checkTokenResult     {phone, token, delete:false}
//	                                              → 200 = 验证码对；412 + errorCode 1320009 = 错/过期
//	POST /login  type=smsLogin&username=<手机号>&password=<验证码>&execution=…
//	                                              → CAS 发票 → callback → authorize → token
//
// 三处容易踩空的细节：
//  1. `*_protected*` 接口要求前端那套双提交头（Csrf-Key / Csrf-Value），少了直接
//     401 "Invalid request"——看着像验证码错，其实是没带证件。
//  2. 验证码的字段名是 `password`（前端把密码输入框复用到验证码框），`smsCode`
//     只是模板引用名；填 smsCode=… 会被当成空验证码。
//  3. 二次认证页的 execution（flowkey）每次渲染都换一把，且和这条 SESSION 绑定：
//     发码与提交必须用同一个 cookie jar、同一份 flowkey。

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"fzucheckin/internal/secure"
)

const (
	// smsBusinessNo 是前端对「普通短信验证码」写死的业务号；换别的值服务端回
	// 1320012（短信发送服务存在问题）。
	smsBusinessNo = "0008"
	// defaultSendInterval 是服务端没给 sendInterval 时的重发冷却兜底（前端实测 120s）。
	defaultSendInterval = 120 * time.Second
	// defaultCodeExpire 是验证码有效期兜底（实测 expireTime=300）。
	defaultCodeExpire = 300 * time.Second
)

var (
	// ErrSMSCodeRejected 表示验证码不对或已过期（服务端 errorCode 1320009）。
	ErrSMSCodeRejected = errors.New("短信验证码不对或已过期")
	// ErrSMSRateLimited 表示刚发过，还在冷却里（412 + errorCode 8000068）。
	ErrSMSRateLimited = errors.New("验证码发送太频繁")
	// ErrSMSPhoneUnbound 表示这个账号没绑手机（errorCode 1320010），短信这条路走不通。
	ErrSMSPhoneUnbound = errors.New("该账号没有绑定手机号，短信验证走不通")
	// ErrSMSSessionExpired 表示这条二次认证会话已经没用（flowkey 过期 / 服务端已清）。
	ErrSMSSessionExpired = errors.New("二次认证会话已过期，请重新发起一次")
)

var csrfAlphabet = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

// SendCodeResult 是一次「发验证码」的结果，数值全部来自服务端响应。
type SendCodeResult struct {
	// Interval 是下一次允许重发的冷却时长（sendInterval）。
	Interval time.Duration
	// Expire 是这条验证码的有效期（expireTime）。
	Expire time.Duration
}

// PendingSecondAuth 是一条「密码已经过了、只等短信验证码」的登录会话。
//
// 它必须自己拿着 cookie jar 与 flowkey：CAS 把「密码已验证」记在服务端会话里，
// 换一个 jar 再提交验证码等于换了个陌生人，服务端只会回登录页。所以它是**有状态**的，
// 只能被同一个进程使用（不要跨实例迁移，也不要落盘：里面是活会话）。
type PendingSecondAuth struct {
	mu sync.Mutex

	loginURL string // 带 service 的登录地址
	service  string
	jar      http.CookieJar
	client   *http.Client

	flowKey string // 二次认证页上的 execution
	phone   string // 真实手机号（内部用；对外只给打码版）
	userID  string // 页面渲染的学号，主表单失败时的兜底 username
	tip     string // 学校原话

	created  time.Time
	lastSend time.Time
	interval time.Duration
	expire   time.Duration
}

// PhoneMasked 返回打码手机号（保前 3 后 4），给前端显示。
func (p *PendingSecondAuth) PhoneMasked() string { return maskPhone(p.phone) }

// Tip 返回学校在页面上给出的原话。
func (p *PendingSecondAuth) Tip() string { return p.tip }

// CreatedAt 返回会话建立时间，供上层算 TTL。
func (p *PendingSecondAuth) CreatedAt() time.Time { return p.created }

// SendInterval 返回服务端给的重发冷却。
func (p *PendingSecondAuth) SendInterval() time.Duration {
	if p.interval > 0 {
		return p.interval
	}
	return defaultSendInterval
}

// CodeExpire 返回验证码有效期。
func (p *PendingSecondAuth) CodeExpire() time.Duration {
	if p.expire > 0 {
		return p.expire
	}
	return defaultCodeExpire
}

// ResendAfter 返回还要等多久才能重发（已过冷却则 0）。
func (p *PendingSecondAuth) ResendAfter(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resendAfterLocked(now)
}

func (p *PendingSecondAuth) resendAfterLocked(now time.Time) time.Duration {
	if p.lastSend.IsZero() {
		return 0
	}
	if d := p.SendInterval() - now.Sub(p.lastSend); d > 0 {
		return d
	}
	return 0
}

// StartSecondAuth 用学号密码把会话推到「等短信」这一步。
//
// 返回三种情况，调用方都要处理：
//   - token 非空、pending 为 nil：这次登录根本不需要短信（校内 IP），直接成了；
//   - pending 非空：密码已过，等验证码；
//   - err 非 nil：密码错、要图片验证码、网络问题等，按原文报。
func StartSecondAuth(username, password string) (*PendingSecondAuth, string, error) {
	return StartSecondAuthAt(SSOBase, ServiceURL, username, password)
}

// StartSecondAuthAt 是可注入 SSO 地址/回调地址的实现（测试与自建反代用它）。
func StartSecondAuthAt(ssoBase, service, username, password string) (*PendingSecondAuth, string, error) {
	loginURL, err := normalizeLoginURL(ssoBase)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(service) == "" {
		service = ServiceURL
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, "", errors.New("短信验证登录需要学号与密码（user.username / user.password）")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, "", fmt.Errorf("初始化 Cookie 容器失败: %w", err)
	}
	p := &PendingSecondAuth{
		loginURL: loginURL,
		service:  service,
		jar:      jar,
		created:  time.Now(),
	}
	query := url.Values{"service": {service}}.Encode()
	target := loginURL + "?" + query

	// 1) 登录页：先取 croypto/flowkey；如果连表单都没有、直接就是二次认证页
	//    （复用了上一个会话时的少数情况），按二次认证处理。
	page, status, err := ssoGet(&http.Client{Timeout: 25 * time.Second, Jar: jar}, target)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("SSO 登录页 HTTP %d: %s", status, truncate(page))
	}
	if sa, ok := secondAuthFrom(page); ok {
		p.adoptSecondAuthPage(page, sa)
		p.client = &http.Client{Timeout: 25 * time.Second, Jar: jar}
		return p, "", nil
	}
	croypto, err := parsePageField(page, "login-croypto", croyptoRe)
	if err != nil {
		return nil, "", err
	}
	execution, err := parsePageField(page, "login-page-flowkey", flowKeyRe)
	if err != nil {
		return nil, "", err
	}
	key, err := base64.StdEncoding.DecodeString(croypto)
	if err != nil {
		return nil, "", fmt.Errorf("登录页下发的 croypto 不是合法 Base64（页面结构可能已变化）: %w", err)
	}
	encrypted, err := secure.EncryptECBBlock(key, []byte(password))
	if err != nil {
		return nil, "", err
	}

	// 2) 提交密码（字段顺序照抄前端表单）。
	form := strings.Join([]string{
		"type=" + url.QueryEscape("UsernamePassword"),
		"_eventId=" + url.QueryEscape("submit"),
		"geolocation=" + url.QueryEscape(""),
		"execution=" + url.QueryEscape(execution),
		"croypto=" + url.QueryEscape(croypto),
		"username=" + url.QueryEscape(username),
		"password=" + url.QueryEscape(base64.StdEncoding.EncodeToString(encrypted)),
	}, "&")

	var hops []string
	client := &http.Client{
		Timeout: 25 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hops = append(hops, req.URL.String())
			if len(via) >= ssoMaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	p.client = client

	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form))
	if err != nil {
		return nil, "", fmt.Errorf("SSO 登录请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", uaSSO)
	req.Header.Set("Origin", siblingOrigin(loginURL))
	req.Header.Set("Referer", target)

	resp, doErr := client.Do(req)
	var body []byte
	if resp != nil {
		status = resp.StatusCode
		if resp.Request != nil && resp.Request.URL != nil {
			hops = append(hops, resp.Request.URL.String())
		}
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
		}
	}
	// token 出现在任何一跳里 = 不需要二次认证，直接成功。
	for _, u := range hops {
		if token := tokenFromURL(u); token != "" {
			return nil, token, nil
		}
	}
	if doErr != nil {
		return nil, "", fmt.Errorf("SSO 登录提交失败: %w", doErr)
	}

	sa, ok := secondAuthFrom(body)
	if !ok {
		hint := loginErrorHint(status, body)
		if isWrongCredential(hint) {
			return nil, "", fmt.Errorf("学号或密码不对（统一身份认证原话：%s）", hint)
		}
		return nil, "", fmt.Errorf("SSO 登录失败：%s", hint)
	}
	p.adoptSecondAuthPage(body, sa)
	if p.flowKey == "" {
		return nil, "", errors.New("二次认证页缺少 #login-page-flowkey，没法提交验证码（页面结构可能已变化）")
	}
	return p, "", nil
}

// adoptSecondAuthPage 把二次认证页上的会话要素收进 pending。
//
// 页面会重新渲染一把 flowkey（和第一步那把不同），提交验证码必须用它；
// 手机号取**未打码**的原值——发码接口要的是真号码，打码版只给前端显示。
func (p *PendingSecondAuth) adoptSecondAuthPage(page []byte, sa *SecondAuthError) {
	p.phone = pageFieldValue(page, secondPhoneRe)
	p.userID = sa.UserID
	p.tip = sa.Tip
	p.flowKey = pageFieldValue(page, flowKeyRe)
	if p.phone == "" {
		p.tip = JoinTip(p.tip, "学校页面没有渲染手机号")
	}
}

// JoinTip 拼一段人话提示（空串自动跳过）。
func JoinTip(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "；")
}

// SendCode 请求学校给这个手机号发一条验证码。
//
// 冷却与有效期都以服务端返回的数为准（sendInterval / expireTime）：自己拍一个
// 60 秒会和学校对不上，用户点重发被拒时反而以为是我们的 bug。
func (p *PendingSecondAuth) SendCode() (SendCodeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.phone == "" {
		return SendCodeResult{}, ErrSMSPhoneUnbound
	}
	payload, _ := json.Marshal(map[string]string{"phone": p.phone, "businessNo": smsBusinessNo})
	req, err := http.NewRequest(http.MethodPost,
		siblingOrigin(p.loginURL)+"/api/protected/sms/publicNoToken/sendSmsCode", bytes.NewReader(payload))
	if err != nil {
		return SendCodeResult{}, fmt.Errorf("发码请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", uaSSO)
	req.Header.Set("Referer", p.loginURL)
	csrfHeaders(req)

	raw, err := p.doAPI(req, "发验证码")
	if err != nil {
		return SendCodeResult{}, err
	}
	var env struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"message"`
		Data struct {
			Result       bool `json:"result"`
			SendInterval int  `json:"sendInterval"`
			ExpireTime   int  `json:"expireTime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return SendCodeResult{}, fmt.Errorf("发码响应不是 JSON: %s", truncate(raw))
	}
	switch {
	case rawCodeIs(env.Code, 200) && env.Data.Result:
		if env.Data.SendInterval > 0 {
			p.interval = time.Duration(env.Data.SendInterval) * time.Second
		}
		if env.Data.ExpireTime > 0 {
			p.expire = time.Duration(env.Data.ExpireTime) * time.Second
		}
		p.lastSend = time.Now()
		return SendCodeResult{Interval: p.SendInterval(), Expire: p.CodeExpire()}, nil
	case envelopeErrorCode(raw) == 8000068:
		// 412 + 8000068：刚发过，errorMessage 里带 {"remaining":N} 秒。
		if remaining := remainingSeconds(raw); remaining > 0 {
			// 把上次发送时间回拨到"还差 remaining 秒"的那一刻，重发冷却就与学校一致了。
			p.lastSend = time.Now().Add(time.Duration(remaining)*time.Second - p.SendInterval())
			return SendCodeResult{}, fmt.Errorf("%w：还要等 %d 秒才能重发", ErrSMSRateLimited, remaining)
		}
		return SendCodeResult{}, ErrSMSRateLimited
	case envelopeErrorCode(raw) == 1320010:
		return SendCodeResult{}, ErrSMSPhoneUnbound
	case envelopeErrorCode(raw) == 1320012:
		return SendCodeResult{}, fmt.Errorf("学校的短信发送服务暂时有问题（1320012），稍后再试")
	case isSendQuotaCode(envelopeErrorCode(raw)) || (rawCodeIs(env.Code, 412) && envelopeErrorCode(raw) != 0):
		// 实测两种形状：{code:412,message:"13001016"}（码在 message 里）与
		// {code:412,data:{errorCode:60008}}。现象一样：码发不出去，换手机号/换会话都不行
		// ——是当天发送次数到顶或同一号码太频繁。按"冷却/限额"报，比"未知失败"有用得多。
		return SendCodeResult{}, fmt.Errorf("%w：学校的短信通道拒绝了这次发送（错误码 %d，通常是当天发送次数到顶"+
			"或同一号码太频繁）。过一会儿或明天再试；急着用就在浏览器里打开晚点名，把地址栏 token= 后面那串粘进来",
			ErrSMSRateLimited, envelopeErrorCode(raw))
	}
	// 剩下的 412 也不把"code=412：13001016"这种半截原文丢给用户：学校那边拒发的原因
	// 只有码能说清，而用户能做的动作永远是"等一会儿 / 换条路"。
	if rawCodeIs(env.Code, 412) {
		return SendCodeResult{}, fmt.Errorf("学校拒绝了这次发码（错误码 %s）：多半是同一号码或同一账号"+
			"今天发得太多，过一会儿或明天再试；急着用就在浏览器里打开晚点名，把地址栏 token= 后面那串粘进来",
			strings.TrimSpace(env.Msg))
	}
	return SendCodeResult{}, fmt.Errorf("发验证码失败（code=%s）：%s",
		strings.TrimSpace(string(env.Code)), strings.TrimSpace(env.Msg))
}

// VerifyCode 提交验证码，成功时返回晚点名免登 token。
//
// 顺序照抄前端：先 checkTokenResult 校验（这一步给得出「验证码错」这种可读原因），
// 通过后再换票。换票两种收尾都试：
//  1. 校验通过后学校可能已把会话标成"已认证"，此时 GET /login 会直接 302 出发票；
//  2. 否则提交 smsLogin 表单（username 先用手机号，再退学号）。
//
// 之所以两条都写：我没有一个真实可用的验证码能把收尾路径验证到只剩一条，宁可贵一次
// 请求，也不能让用户填对了码却卡在"提交没反应"。
func (p *PendingSecondAuth) VerifyCode(code string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	code = strings.TrimSpace(code)
	if code == "" {
		return "", errors.New("验证码是空的")
	}

	payload, _ := json.Marshal(map[string]any{"phone": p.phone, "token": code, "delete": false})
	req, err := http.NewRequest(http.MethodPost,
		siblingOrigin(p.loginURL)+"/api/protected/sms/checkTokenResult", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("校验请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", uaSSO)
	req.Header.Set("Referer", p.loginURL)
	csrfHeaders(req)

	raw, err := p.doAPI(req, "校验验证码")
	if err != nil {
		return "", err
	}
	var env struct {
		Code json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(raw, &env)
	switch envelopeErrorCode(raw) {
	case 1320009:
		return "", ErrSMSCodeRejected
	case 1320010:
		return "", ErrSMSPhoneUnbound
	}
	if !rawCodeIs(env.Code, 200) {
		return "", fmt.Errorf("校验验证码失败（code=%s）：%s", strings.TrimSpace(string(env.Code)), truncate(raw))
	}

	// 收尾一：已经认证的会话直接换票；顺手把页面上的 flowkey 刷新一遍——
	// 服务端每次渲染二次认证页都会换一把 execution，校验通过后旧那把可能已经作废，
	// 而收尾二（表单提交）要用的正是它。刷新失败不影响后面的尝试。
	if token, page := p.ticketViaLoginPage(); token != "" {
		return token, nil
	} else if sa, ok := secondAuthFrom(page); ok {
		p.adoptSecondAuthPage(page, sa)
	}

	// 收尾二：提交 smsLogin 表单。username 先用手机号（前端就是这么填的），
	// 再退到学号——有的部署在二次认证页上认的是 user-id。
	// 顺序与前端一致：先校验（拿到"验证码错"这种可读原因），再提表单。
	var diag []string
	for _, uname := range dedupe([]string{p.phone, p.userID}) {
		token, status, stillSecond := p.submitSMSForm(uname, code)
		if token != "" {
			return token, nil
		}
		diag = append(diag, fmt.Sprintf("username=%s 的表单提交 HTTP %d，仍返回二次认证页=%v", maskPhone(uname), status, stillSecond))
	}
	return "", fmt.Errorf("%w（校验已通过，但换票没成功：%s）", ErrSMSSessionExpired, strings.Join(diag, "；"))
}

// ticketViaLoginPage 带着这条会话再取一次登录页：已认证的话 CAS 会直接 302 出发票。
// 返回正文是给调用方刷新 flowkey 用的（不是发票时，这一页就是新的二次认证页）。
func (p *PendingSecondAuth) ticketViaLoginPage() (string, []byte) {
	var hops []string
	client := &http.Client{
		Timeout: 25 * time.Second,
		Jar:     p.jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hops = append(hops, req.URL.String())
			if len(via) >= ssoMaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, p.loginURL+"?"+url.Values{"service": {p.service}}.Encode(), nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("User-Agent", uaSSO)
	resp, err := client.Do(req)
	var body []byte
	if resp != nil {
		if resp.Request != nil && resp.Request.URL != nil {
			hops = append(hops, resp.Request.URL.String())
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
	}
	if token := firstToken(hops, ""); token != "" {
		return token, body
	}
	if err != nil && len(hops) == 0 {
		return "", nil
	}
	return "", body
}

// submitSMSForm 提交服务端渲染的 #smsLoginForm 换票。
// 返回值里的 status / stillSecond 只用于诊断：第一次真机跑的时候，"码是对的但没票"
// 到底卡在 HTTP 码还是卡在"页面还是二次认证页"，决定了下一步该改哪里。
func (p *PendingSecondAuth) submitSMSForm(username, code string) (token string, status int, stillSecond bool) {
	if p.flowKey == "" {
		return "", 0, true
	}
	form := strings.Join([]string{
		"type=" + url.QueryEscape("smsLogin"),
		"_eventId=" + url.QueryEscape("submit"),
		"geolocation=" + url.QueryEscape(""),
		"execution=" + url.QueryEscape(p.flowKey),
		"username=" + url.QueryEscape(username),
		"password=" + url.QueryEscape(code),
	}, "&")
	target := p.loginURL + "?" + url.Values{"service": {p.service}}.Encode()

	var hops []string
	client := &http.Client{
		Timeout: 25 * time.Second,
		Jar:     p.jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hops = append(hops, req.URL.String())
			if len(via) >= ssoMaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form))
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", uaSSO)
	req.Header.Set("Origin", siblingOrigin(p.loginURL))
	req.Header.Set("Referer", target)
	resp, err := client.Do(req)
	if resp != nil {
		status = resp.StatusCode
		if resp.Request != nil && resp.Request.URL != nil {
			hops = append(hops, resp.Request.URL.String())
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		// 表单被拒时服务端会把二次认证页重新渲染一遍（401/200 都见过）：顺手更新 tip。
		if sa, ok := secondAuthFrom(body); ok {
			stillSecond = true
			if sa.Tip != "" {
				p.tip = sa.Tip
			}
		}
	}
	if token := firstToken(hops, ""); token != "" {
		return token, status, stillSecond
	}
	_ = err
	return "", status, stillSecond
}

// doAPI 发一个「protected」接口请求，统一处理未通过接口鉴权与网络错误。
func (p *PendingSecondAuth) doAPI(req *http.Request, op string) ([]byte, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s请求失败: %w", op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("%s响应读取失败: %w", op, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || strings.Contains(string(raw), `"message":"Unauthorized"`) {
		return nil, fmt.Errorf("%s被拒（HTTP %d，没通过接口鉴权）：%s", op, resp.StatusCode, truncate(raw))
	}
	return raw, nil
}

// csrfHeaders 复刻前端拦截器给 *protected* 接口加的双提交头。
//
// 前端算法（照抄，不是猜的）：
//
//	key   = 32 位随机字母数字
//	b64   = base64(key)
//	value = b64前半 + b64 + b64后半
//	Csrf-Key: key        Csrf-Value: md5(value) 的小写十六进制
//
// 少了这两个头，服务端一律回 {"code":401,"message":"Unauthorized","data":{"errorMessage":"Invalid request"}}
// ——看起来像"验证码错"，其实是没带证件。这一条是 2026-11-18 实测踩出来的。
func csrfHeaders(req *http.Request) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return
	}
	for i := range buf {
		buf[i] = csrfAlphabet[int(buf[i])%len(csrfAlphabet)]
	}
	key := string(buf)
	b64 := base64.StdEncoding.EncodeToString([]byte(key))
	half := len(b64) / 2
	sum := md5.Sum([]byte(b64[:half] + b64 + b64[half:]))
	req.Header.Set("Csrf-Key", key)
	req.Header.Set("Csrf-Value", hex.EncodeToString(sum[:]))
}

// rawCodeIs 判断接口响应里的 code 是否等于某个值（容忍 "200" 与 200 两种写法）。
func rawCodeIs(raw json.RawMessage, want int) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	return strings.Trim(s, `"`) == fmt.Sprint(want)
}

// envelopeErrorCode 从接口响应里取错误码。
//
// 两种真实形状都要认（2026-11-18 实测）：
//
//	{"code":412,"data":{"errorCode":60008,"errorMessage":"60008"}}   ← 码在 data.errorCode
//	{"code":412,"message":"13001016"}                                 ← 码在 message
//
// 只认前一种的后果很具体：13001016 那种会被当成"未知失败"，用户看到的是一串数字，
// 而不是"今天发得太多，先用浏览器取 token"。
func envelopeErrorCode(raw []byte) int {
	var env struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    struct {
			ErrorCode int `json:"errorCode"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return 0
	}
	if env.Data.ErrorCode != 0 {
		return env.Data.ErrorCode
	}
	if n, err := strconv.Atoi(strings.TrimSpace(env.Message)); err == nil {
		return n
	}
	return 0
}

// isSendQuotaCode 判断这个码是不是"学校不肯再发短信"。
//
// 13001016 与 60008 都不是文档里的名字，但两次实测都伴随 412、都跟手机号与会话无关、
// 隔一段时间会自己恢复——按限额处理是唯一不误导用户的解释。
func isSendQuotaCode(code int) bool {
	switch code {
	case 13001016, 60008, 10000001:
		return true
	}
	return false
}

// remainingSeconds 从 412 的 errorMessage（形如 {"remaining":93}）里取剩余秒数。
func remainingSeconds(raw []byte) int {
	var env struct {
		Data struct {
			ErrorMessage string `json:"errorMessage"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.ErrorMessage == "" {
		return 0
	}
	var inner struct {
		Remaining int `json:"remaining"`
	}
	if json.Unmarshal([]byte(env.Data.ErrorMessage), &inner) != nil {
		return 0
	}
	return inner.Remaining
}

// dedupe 去掉空串与重复项，保持顺序。
func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}
