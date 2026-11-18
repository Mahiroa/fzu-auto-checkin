package fzu

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"fzucheckin/internal/secure"
)

const (
	// SSOBase 统一身份认证的登录端点（注意带 /login：GET/POST 都打这个地址）。
	SSOBase = "https://sso.fzu.edu.cn/login"
	// ServiceURL 是晚点名 H5 的 CAS 回调地址。SSO 登录成功后 302 链会经
	// callback.action?ticket=ST-... 换到 authorize.action?token=...
	ServiceURL = "https://yzsxg.fzu.edu.cn/livecloud/project/fzu/attn/oauth2/callback.action"

	// uaSSO 用桌面 Chrome 的 UA：sso 这套 Angular 前端对移动端 UA 会走
	// "clientredirect" 多端适配分支，登录页字段可能不同。
	uaSSO = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// ssoMaxRedirects CAS 换 ticket 的正常链路是 2~3 跳；给到 5 跳仍不出现 token
	// 说明协议变了，继续跟只会打到别的系统。
	ssoMaxRedirects = 5
	// minTokenLen token 至少 20 位：太短的匹配多半是某个无关参数（如 token=abc）。
	minTokenLen = 20
)

var (
	// 登录页把挑战值放在 <p id="login-croypto">…</p> / <p id="login-page-flowkey">…</p>。
	// (?s) 让 .*? 跨行：Angular 渲染出来的标签之间常有换行与缩进。
	croyptoRe  = regexp.MustCompile(`(?s)id="login-croypto"\s*>(.*?)</p>`)
	flowKeyRe  = regexp.MustCompile(`(?s)id="login-page-flowkey"\s*>(.*?)</p>`)
	loginErrRe = regexp.MustCompile(`id="login-error-msg"[^>]*>([^<]+)`)
	// tokenRe 从重定向链的 URL 里捞 token：只认字母数字，长度下限见 minTokenLen。
	tokenRe = regexp.MustCompile(`[?&]token=([A-Za-z0-9]{20,})`)

	// captchaIDRe 取页面上的验证码挑战 id（有值 = 这次登录要验证码）。
	captchaIDRe = regexp.MustCompile(`id="captchaId">\s*([^<]*?)\s*</p>`)
	// netEaseCaptchaIDRe 是另一套验证码（网易易盾）的挑战 id，判据与 captchaId 相同。
	netEaseCaptchaIDRe = regexp.MustCompile(`(?s)id="netEaseCaptchaId"\s*>\s*([^<]*?)\s*<`)
	// loginErrCodeRe 取 #login-error-code：新版前端把可读文案交给 JS，但错误码
	// 仍由服务端渲染。只有码没有文案时，把码带上总比说"什么都不知道"有用。
	loginErrCodeRe = regexp.MustCompile(`(?s)id="login-error-code"\s*>\s*([^<]*?)\s*<`)

	// 二次认证页（校外 IP 登录时服务端直接渲染的「请短信验证」页）上的字段。
	// 这几个 id 是 2026-11-18 现场取证到的真值形状：
	//	<p id="sso-second">true</p>
	//	<p id="second-auth-tip">您正在使用非校内ip登录，请进行手机验证…</p>
	//	<p id="phone-number">138****1234</p>
	//	<p id="second-auth-user-id">20210001</p>
	secondFlagRe  = regexp.MustCompile(`(?s)id="sso-second"\s*>\s*([^<]*?)\s*<`)
	secondTipRe   = regexp.MustCompile(`(?s)id="second-auth-tip"\s*>\s*([^<]*?)\s*<`)
	secondPhoneRe = regexp.MustCompile(`(?s)id="phone-number"\s*>\s*([^<]*?)\s*<`)
	secondUserRe  = regexp.MustCompile(`(?s)id="second-auth-user-id"\s*>\s*([^<]*?)\s*<`)

	// secretInTextRe 抹掉正文里可能夹带的凭据（token/password/会话 Cookie）：
	// 这段正文会被原样贴进错误信息、日志、运行记录和手机推送。
	secretInTextRe = regexp.MustCompile(`(?i)\b(token|password|passwd|SESSION|JSESSIONID|CASTGC|TGC)=([^\s"'&;,<]{1,})`)

	// captchaHint 是"这次要验证码"的统一说法。不写"改用 Cookie 就能绕过"了：
	// 校外/机房 IP 上密码与 Cookie 都会被要求短信二次验证，那句承诺兑现不了。
	captchaHint = "学校这次要求输入验证码（同一来源连续登录失败会被风控）；" +
		"直接粘一份有效的晚点名 token 通常能跳过它"

	// knownLoginErrors 是统一身份认证会在页面上出现的几种可读原因，按具体程度排序。
	// 整页文本里最长出现的两句是「用户名或密码错误」与「验证码错误」。
	knownLoginErrors = []string{
		"用户名或密码错误",
		"账号或密码错误",
		"密码错误",
		"用户不存在",
		"账号不存在",
		"账号已被锁定",
		"账号已锁定",
		"验证码错误",
		"账号未激活",
	}

	// wrongCredentialPhrases 是 knownLoginErrors 里"学号/密码不对"的那一部分：
	// 只有它们值得给出"换成 App 现在用的那组密码"这种专门引导。
	// 锁定/未激活/验证码错误都不属于这一类，混进来会让用户去改密码。
	wrongCredentialPhrases = []string{
		"用户名或密码错误",
		"账号或密码错误",
		"密码错误",
		"用户不存在",
		"账号不存在",
	}

	// errTooManyRedirects 是 CheckRedirect 里用的哨兵：超跳后我们仍要回头
	// 扫一遍已经走过的 URL（token 可能就在里面）。
	errTooManyRedirects = errors.New("SSO 重定向超过上限")
)

// ErrSecondAuth 表示学校把这次登录判成「校外 IP，需要短信二次验证」。
//
// 它不是故障：账号密码已经通过了（二次认证页上渲染的 #second-auth-user-id 就是学号），
// 缺的只是那一步只有真人能做的验证码。2026-11-18 线上事故之所以被报成 bug，是因为
// 这张页面既没有 token、也没有 #login-error-msg，于是所有失败都退化成一句
// 「登录页结构可能已变化」——把最关键的「去人工验一次」吞掉了。有了这个哨兵，
// 上层就能把「需要人做一步」与「链路坏了」分开报，也能据此回 409。
var ErrSecondAuth = errors.New("统一身份认证要求短信二次验证")

// SecondAuthError 是 ErrSecondAuth 的带信息形态：把页面上的原话与（已打码的）
// 手机号一起带上去。上层既要 errors.Is(err, ErrSecondAuth) 判类型，又要把
// tip/phone 原样回给前端（前端用它们显示「已向 138****1234 发送验证码」）。
type SecondAuthError struct {
	// Tip 是页面原话，例如「您正在使用非校内ip登录，请进行手机验证以保障您的账号安全」。
	Tip string
	// Phone 已打码（138****1234）。页面没渲染手机号时为空串。
	Phone string
	// UserID 是页面渲染的学号/用户标识；非空恰好证明「账号密码是对的」。
	UserID string
}

// Error 拼成一句能直接进日志/推送/运行记录的人话。
func (e *SecondAuthError) Error() string {
	if e == nil {
		return ErrSecondAuth.Error()
	}
	msg := ErrSecondAuth.Error()
	if e.Tip != "" {
		msg += "：" + e.Tip
	}
	if e.Phone != "" {
		msg += "（手机 " + e.Phone + "）"
	}
	return msg
}

// Unwrap 让 errors.Is/As 能穿过 fmt.Errorf("%w", …) 的包装继续命中 ErrSecondAuth。
func (e *SecondAuthError) Unwrap() error { return ErrSecondAuth }

// Login 用学号密码登录 SSO，返回晚点名免登 token。
func Login(username, password string) (string, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return "", errors.New("SSO 登录需要学号与密码（user.username / user.password 未配置）")
	}
	return LoginAt(SSOBase, ServiceURL, username, password)
}

// LoginAt 是可注入 SSO 地址/回调地址的登录实现（测试与自建反代都用它）。
//
// 每一步都对应抓包看到的一次真实请求：
//  1. GET  登录页 → 取 croypto（一次性 AES 密钥）与 execution（flowkey）
//  2. POST 同一地址，password = Base64(AES-128-ECB(PKCS7(明文), key=Base64Decode(croypto)))
//  3. 跟随 302 链，从每跳 URL 与最终 URL 里取 token
func LoginAt(ssoBase, service, username, password string) (string, error) {
	loginURL, err := normalizeLoginURL(ssoBase)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(service) == "" {
		service = ServiceURL
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return "", errors.New("SSO 登录需要学号与密码（user.username / user.password 未配置）")
	}

	// CAS 靠 Cookie 串起 GET → POST → 302 链（JSESSIONID / TGC），
	// 少了 jar，第二步会被当成一次全新的匿名会话。
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("初始化 Cookie 容器失败: %w", err)
	}
	var hops []string
	client := &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hops = append(hops, req.URL.String())
			if len(via) >= ssoMaxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
	}

	query := url.Values{"service": {service}}.Encode()

	// 1) 拿登录页
	page, status, err := ssoGet(client, loginURL+"?"+query)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("SSO 登录页 HTTP %d: %s", status, truncate(page))
	}
	// 连登录表单都没出现、直接就是二次认证页：先判掉，否则下面的"缺少
	// #login-croypto"会把真正的原因（要人工过短信）盖成"页面结构变了"。
	if sa, ok := secondAuthFrom(page); ok {
		return "", sa
	}

	croypto, err := parsePageField(page, "login-croypto", croyptoRe)
	if err != nil {
		return "", err
	}
	execution, err := parsePageField(page, "login-page-flowkey", flowKeyRe)
	if err != nil {
		return "", err
	}

	// 2) 加密密码。croypto 是页面下发的一次性 16 字节密钥（Base64 编码），
	// 前端拿它做 AES-ECB 加密明文密码，服务端解出来与库里的密码比对。
	key, err := base64.StdEncoding.DecodeString(croypto)
	if err != nil {
		return "", fmt.Errorf("登录页下发的 croypto 不是合法 Base64（页面结构可能已变化）: %w", err)
	}
	encrypted, err := secure.EncryptECBBlock(key, []byte(password))
	if err != nil {
		return "", err
	}
	encPassword := base64.StdEncoding.EncodeToString(encrypted)

	// 表单按抓包顺序手工拼：对端是否在意字段顺序未知，但按原样发是零成本的；
	// url.Values.Encode() 会按 key 排序，顺序就和真机不一样了。
	form := strings.Join([]string{
		"type=" + url.QueryEscape("UsernamePassword"),
		"_eventId=" + url.QueryEscape("submit"),
		"geolocation=" + url.QueryEscape(""),
		"execution=" + url.QueryEscape(execution),
		"croypto=" + url.QueryEscape(croypto),
		"username=" + url.QueryEscape(username),
		"password=" + url.QueryEscape(encPassword),
	}, "&")

	req, err := http.NewRequest(http.MethodPost, loginURL+"?"+query, strings.NewReader(form))
	if err != nil {
		return "", fmt.Errorf("SSO 登录请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", uaSSO)
	// Origin/Referer 补齐成同源登录页：POST 是浏览器从这张页面上发出的，真实
	// 浏览器一定带这两个头。现场实测带与不带都能成功（不构成判据），但带着更像
	// 浏览器、也不给风控留"缺头"这个额外特征，成本为零。
	req.Header.Set("Origin", siblingOrigin(loginURL))
	req.Header.Set("Referer", loginURL+"?"+query)

	resp, doErr := client.Do(req)
	finalURL := ""
	status = 0
	var body []byte
	if resp != nil {
		status = resp.StatusCode
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
		}
	}

	// 3) 先扫链接：token 出现即成功，与 HTTP 状态无关——CAS 会把 token 塞在
	// 302 的 Location 里，而那一跳本身永远不返回 200。
	for _, u := range append(append([]string{}, hops...), finalURL) {
		if token := tokenFromURL(u); token != "" {
			return token, nil
		}
	}

	if doErr != nil {
		if errors.Is(doErr, errTooManyRedirects) {
			return "", fmt.Errorf("SSO 登录重定向超过 %d 跳且链接里没有 token（回调地址或登录流程已变化）",
				ssoMaxRedirects)
		}
		return "", fmt.Errorf("SSO 登录提交失败: %w", doErr)
	}

	// 4) 二次认证页必须在 loginErrorHint 之前判：它既没有 token、也没有
	// #login-error-msg，落到 hint 里只能被猜成"认不出的页面"。现场 200 与 401
	// 两种状态都出现过（401 是短信提交被我试错时回的），所以不看状态码，只看页面。
	if sa, ok := secondAuthFrom(body); ok {
		return "", sa
	}

	hint := loginErrorHint(status, body)
	if isWrongCredential(hint) {
		// 这类失败最常见，而且用户最容易改错方向（去改 Cookie、改 token）。
		// 直接点名"是学号密码不对"，并提醒填的是 App 现在用的那组。
		return "", fmt.Errorf("学号或密码不对（统一身份认证原话：%s）；填「现在能登录智汇福大 App」的那组，不是初始密码", hint)
	}
	// hint 自带状态码与页面事实（字节数/标题/正文开头），这里不再重复拼一遍 HTTP 码。
	return "", fmt.Errorf("SSO 登录失败：%s", hint)
}

// ssoGet 取一页 HTML 并返回状态码与正文。
func ssoGet(client *http.Client, target string) (body []byte, status int, err error) {
	req, reqErr := http.NewRequest(http.MethodGet, target, nil)
	if reqErr != nil {
		return nil, 0, fmt.Errorf("SSO 登录页请求构造失败: %w", reqErr)
	}
	req.Header.Set("User-Agent", uaSSO)
	resp, doErr := client.Do(req)
	if doErr != nil {
		return nil, 0, fmt.Errorf("SSO 登录页请求失败: %w", doErr)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("SSO 登录页读取失败: %w", readErr)
	}
	return body, resp.StatusCode, nil
}

// parsePageField 用正则取页面里的隐藏字段值（去空白）。
//
// 取不到就直接点名缺了哪个元素并提示"页面结构可能已变化"：CAS 前端改版后
// 最典型的表现就是这里先炸，报清楚比回一句"登录失败"有用得多。
//
// 但"缺元素"还有一个更容易被误认的来源：二次认证页本来就没有 croypto/flowkey。
// 那种情况由 secondAuthFrom 抢先判掉（见 LoginAt），这句补充只是给读日志的人留个
// 岔路口，免得又照着"结构变化"去查。
func parsePageField(page []byte, elemID string, re *regexp.Regexp) (string, error) {
	m := re.FindSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("登录页缺少元素 #%s，登录页结构可能已变化"+
			"（如果你看到的是「请进行手机验证」，那是校外 IP 的二次认证，不是结构变化）", elemID)
	}
	value := strings.TrimSpace(string(m[1]))
	if value == "" {
		return "", fmt.Errorf("登录页元素 #%s 是空的，登录页结构可能已变化"+
			"（如果你看到的是「请进行手机验证」，那是校外 IP 的二次认证，不是结构变化）", elemID)
	}
	return value, nil
}

// tokenFromURL 从 URL 里提取 token（长度 ≥ minTokenLen）。
func tokenFromURL(rawURL string) string {
	m := tokenRe.FindStringSubmatch(rawURL)
	if m == nil {
		return ""
	}
	token := m[1]
	if len(token) < minTokenLen {
		return ""
	}
	return token
}

// loginErrorHint 从失败页里取对端给的原因；取不到就按**已知事实**分档描述这一页。
//
// 新版前端把错误文案交给 JS 渲染，`#login-error-msg` 经常是空的——但服务端仍会在
// 页面里留下几个可读信号：`#login-error-code`、`#captchaId`/`#netEaseCaptchaId`
// （非空 = 这次要验证码）、以及整页文本里的中文错误句（实测「用户名或密码错误」
// 就在 401 页面正文里）。
//
// 兜底绝不再写"登录页结构可能已变化"：2026-11-18 的事故里真实原因是"校外 IP 要求
// 短信二次验证"，那句猜测把用户引去查了两天页面结构。现在兜底只陈述能验证的事实
// ——状态码、字节数、标题、正文开头——让人自己判断这是登录页、维护页还是别的系统。
// （二次认证页本身由 secondAuthFrom 在调用前判掉，走不到这里的兜底。）
func loginErrorHint(status int, body []byte) string {
	code := pageFieldValue(body, loginErrCodeRe)
	withCode := func(text string) string {
		if code == "" {
			return text
		}
		return text + "（页面错误码 " + code + "）"
	}

	if m := loginErrRe.FindSubmatch(body); m != nil {
		if text := strings.TrimSpace(string(m[1])); text != "" {
			return withCode(text)
		}
	}
	if pageFieldValue(body, captchaIDRe) != "" || pageFieldValue(body, netEaseCaptchaIDRe) != "" {
		return withCode(captchaHint)
	}
	text := string(body)
	for _, phrase := range knownLoginErrors {
		if strings.Contains(text, phrase) {
			return withCode(phrase)
		}
	}
	if code != "" {
		return "统一身份认证只给了一个错误码 " + code + "，没有可读的提示文字"
	}
	if status == http.StatusUnauthorized {
		// 401 且页面一个字都没说：最常见的是密码不对，或这一串"同源 IP 连续登录失败"
		// 被风控直接掐掉，对端连原因都不渲染。两种处置不同，所以只陈述事实。
		return "密码被拒或被风控拦截（HTTP 401，页面无提示）"
	}
	if status != http.StatusOK {
		return fmt.Sprintf("统一身份认证返回 HTTP %d，页面里没有可读的原因", status)
	}
	// 又回了一张登录表单：密码没被接受，但页面没渲染任何原因。这一页我们认得，
	// 就不该说"认不出"——它和"被网关换成维护页/别的系统"处置完全不同。
	if loginPageRe.Match(body) {
		return fmt.Sprintf("统一身份认证又返回了一次登录页（HTTP 200，%d 字节）：账号密码没有被接受，"+
			"页面没有给出原因%s", len(body), snippetPart(body))
	}
	return fmt.Sprintf("统一身份认证返回了一个认不出的页面（HTTP 200，%d 字节，%s）%s",
		len(body), titlePart(body), snippetPart(body))
}

// isWrongCredential 判断这句话是不是"学号或密码不对"（要给出专门的引导）。
//
// 用前缀匹配而不是全等：hint 现在可能带"（页面错误码 xxx）"这样的后缀，
// 全等会把一句明确的「用户名或密码错误」判成"不是凭据问题"。
func isWrongCredential(hint string) bool {
	for _, phrase := range wrongCredentialPhrases {
		if strings.HasPrefix(hint, phrase) {
			return true
		}
	}
	return false
}

// secondAuthFrom 判断这一页是不是「校外 IP，需要短信二次验证」。
//
// 判据只用两个服务端渲染的信号：`#sso-second` 为 true，或 `#second-auth-tip` 非空。
// 不按"页面里出现『手机验证』几个字"来认：普通登录页的帮助文案里也有这几个字，
// 按文本匹配会把"密码错"报成"要短信"，等于换一种误导。
func secondAuthFrom(page []byte) (*SecondAuthError, bool) {
	tip := pageFieldValue(page, secondTipRe)
	flag := strings.ToLower(pageFieldValue(page, secondFlagRe))
	if flag != "true" && tip == "" {
		return nil, false
	}
	return &SecondAuthError{
		Tip:    tip,
		Phone:  maskPhone(pageFieldValue(page, secondPhoneRe)),
		UserID: pageFieldValue(page, secondUserRe),
	}, true
}

// pageFieldValue 取一个隐藏字段的值；元素不存在或值为空都返回空串。
func pageFieldValue(page []byte, re *regexp.Regexp) string {
	m := re.FindSubmatch(page)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// maskPhone 只留前 3 位与后 4 位（13800001234 → 138****1234）。
//
// 手机号必须出现在错误文案里（用户要认出是哪台手机在等验证码），但这一串会进
// 日志、运行记录、SSE、手机推送和浏览器，所以只留"够认出来"的那几位。长度不足
// 7 位时整串打掉：短号里"前 3 后 4"几乎等于原文。
func maskPhone(phone string) string {
	runes := []rune(strings.TrimSpace(phone))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) < 7 {
		return "****"
	}
	return string(runes[:3]) + "****" + string(runes[len(runes)-4:])
}

// titlePart 把 <title> 拼成"标题「…」"；没有标题也明说，不留空。
func titlePart(body []byte) string {
	if title := pageTitle(body); title != "" {
		return "标题「" + title + "」"
	}
	return "页面没有标题"
}

// pageTitle 取 <title>…</title> 的文本并压成一行（跨行的标题会撑破日志格式）。
func pageTitle(body []byte) string {
	lower := strings.ToLower(string(body))
	start := strings.Index(lower, "<title")
	if start < 0 {
		return ""
	}
	rest := string(body)[start:]
	open := strings.Index(rest, ">")
	if open < 0 {
		return ""
	}
	rest = rest[open+1:]
	end := strings.Index(strings.ToLower(rest), "</title>")
	if end < 0 {
		return ""
	}
	return strings.Join(strings.Fields(rest[:end]), " ")
}

// snippetPart 附上正文开头 300 字节，让人自己判断这到底是什么页面。
func snippetPart(body []byte) string {
	snippet := strings.Join(strings.Fields(string(headBytes(body, bodySnippetLen))), " ")
	if snippet == "" {
		return ""
	}
	return "，正文开头：" + redactSecrets(snippet)
}

// headBytes 取前 n 字节并退到字符边界（否则会切出半个汉字，JSON 里就变成乱码）。
func headBytes(body []byte, n int) []byte {
	if len(body) <= n {
		return body
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
}

// redactSecrets 抹掉正文里可能夹带的凭据。
//
// 这段正文会进错误信息、日志、运行记录与手机推送，任何一处出现完整的 token /
// 密码 / 会话 Cookie 都等于把账号交出去。
func redactSecrets(text string) string {
	return secretInTextRe.ReplaceAllString(text, "$1=***")
}

// normalizeLoginURL 容错地规整登录地址：
//   - 空 → 用 SSOBase；
//   - 只给站点根（http://host 或 http://host/）→ 补 /login；
//   - 末尾多余斜杠去掉。
func normalizeLoginURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = SSOBase
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("SSO 地址非法: %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("SSO 地址非法（需要含协议与主机名）: %q", base)
	}
	if strings.TrimRight(u.Path, "/") == "" {
		u.Path = "/login"
	} else {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	// 清掉调用方可能带上的 query/fragment：service 由本函数自己拼，
	// 留着旧 query 会拼出两个 '?'，对端直接 400。
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
