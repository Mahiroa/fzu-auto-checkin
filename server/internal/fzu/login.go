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

	// errTooManyRedirects 是 CheckRedirect 里用的哨兵：超跳后我们仍要回头
	// 扫一遍已经走过的 URL（token 可能就在里面）。
	errTooManyRedirects = errors.New("SSO 重定向超过上限")
)

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
	if status != http.StatusOK {
		return "", fmt.Errorf("SSO 登录失败: HTTP %d，%s", status, loginErrorHint(body))
	}
	return "", fmt.Errorf("SSO 登录失败: %s", loginErrorHint(body))
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
func parsePageField(page []byte, elemID string, re *regexp.Regexp) (string, error) {
	m := re.FindSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("登录页缺少元素 #%s，登录页结构可能已变化", elemID)
	}
	value := strings.TrimSpace(string(m[1]))
	if value == "" {
		return "", fmt.Errorf("登录页元素 #%s 是空的，登录页结构可能已变化", elemID)
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

// loginErrorHint 从失败页里取对端给的原因。
//
// 无 error-msg 时不能说"未知错误"就完事：这种情况几乎都是要求验证码（SSO 风控）
// 或页面结构变了，把这两种可能写进提示，用户才知道下一步该干什么。
func loginErrorHint(body []byte) string {
	if m := loginErrRe.FindSubmatch(body); m != nil {
		if text := strings.TrimSpace(string(m[1])); text != "" {
			return text
		}
	}
	return "未知错误（可能需要验证码或页面结构变化）"
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
