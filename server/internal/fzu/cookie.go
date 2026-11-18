package fzu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 这个文件是「用浏览器 Cookie 免密换 token」的唯一实现处。
//
// 为什么需要它：学校侧对账密登录可能弹验证码（风控），而 Cookie 复用的是一个
// 已经通过人机校验的浏览器会话——只要用户在浏览器里登录过统一身份认证，我们
// 就能拿着那个会话让 CAS 直接发一张 service ticket，全程不需要密码、不需要验证码。
//
// 链路（每一步都是真实抓包看到的行为）：
//
//	GET  sso.fzu.edu.cn/login?service=<晚点名回调>   带 Cookie: SESSION=…
//	  ├─ 会话有效 → 302 <回调>?ticket=ST-…  → 302 authorize.action?token=…  ✅
//	  └─ 会话无效 → 200 登录页（页面里有 #login-page-flowkey）            ❌
//
// 少数情况下 CAS 不直接 302，而是在登录页里把「已经登录」标在某个登录方式上
// （<div class="isRedirect">true</div>），这时再补一次
// GET sso.fzu.edu.cn/clientredirect?client_name=<code>&service=<回调> 就能拿到 ticket。

// maxCookieValueLen 是单个 Cookie 值的长度上限。
//
// 导出的 cookies.json 里常混着埋点/AB 测试的 Cookie（sensorsdata、growingio……），
// 它们的值动辄上千字符。把这些原样塞进请求头只会让 SESSION 挤在 4KB 之后，
// 而服务端根本不看它们。
const maxCookieValueLen = 2048

// maxCookieHeaderLen 是整条 Cookie 请求头的长度上限（Nginx 默认 8KB，留一半余量）。
const maxCookieHeaderLen = 4096

// sessionCookieNames 是各代统一身份认证用来表示"已登录"的 Cookie 名。
// 顺序即优先级：新版是 SESSION，老版金智 CAS 是 JSESSIONID + CASTGC。
var sessionCookieNames = []string{"SESSION", "JSESSIONID", "CASTGC", "TGC", "iPlanetDirectoryPro"}

var (
	// ruleRe 抓登录页里「登录方式 + 是否自动跳转」这一对值。
	// (?s) 让 .*? 跨行：服务端渲染出来的 <div> 之间常常换行。
	ruleRe = regexp.MustCompile(`(?s)<div class="code">\s*([^<]+?)\s*</div>.*?<div class="isRedirect">\s*([^<]+?)\s*</div>`)
	// loginPageRe 是"这是登录页"的判据。这两个 id 是登录表单的隐藏字段，
	// 只有未登录时才会渲染出来。
	loginPageRe = regexp.MustCompile(`id="(?:login-page-flowkey|login-croypto)"`)
	// userIDRe 取页面上服务端渲染出来的学号/用户标识：已登录时它非空。
	userIDRe = regexp.MustCompile(`id="user-id">\s*([^<]*?)\s*</p>`)
)

// CookieLoginResult 是一次 Cookie 换 token 的结果。
//
// 带 Hops 而不是只返回 error：面板上要能回答"到底断在哪一步"——是把 Cookie
// 发过去就被打回登录页，还是拿到 ticket 但回调没给 token。只回一句"登录失败"
// 会让用户和我都只能靠猜。
type CookieLoginResult struct {
	Token string
	Hops  []string
	Note  string
}

// cookiePair 是解析出来的一条 Cookie。
type cookiePair struct {
	Name   string
	Value  string
	Domain string
}

// ---------- Cookie 解析 ----------

// ParseCookieBundle 把用户粘进来的任意形态的 Cookie 归一成一条请求头。
//
// 支持四种真实会遇到的输入：
//  1. 浏览器「复制 Cookie」得到的请求头：`SESSION=xxx; other=y`
//  2. 导出插件（EditThisCookie / Cookie-Editor）的 cookies.json 数组
//  3. 抓包工具导出的 Netscape cookies.txt
//  4. 只复制了值本身（一串 UUID / 随机串）——按 SESSION=<值> 处理
//
// 返回值第二项是被采用的 Cookie 名列表，用于在面板上告诉用户"我拿到了哪几条"，
// 绝不回显值本身。
func ParseCookieBundle(raw string) (string, []string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", nil, errors.New("Cookie 是空的：请把浏览器里 sso.fzu.edu.cn 的 SESSION 值，或导出的 cookies.json 全文粘进来")
	}
	// 用户可能连 DevTools 里的 "cookie:" 一起复制进来。
	if len(text) > 7 && strings.EqualFold(text[:7], "cookie:") {
		text = strings.TrimSpace(text[7:])
	}

	var pairs []cookiePair
	switch {
	case strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{"):
		parsed, err := parseCookieJSON(text)
		if err != nil {
			return "", nil, err
		}
		pairs = parsed
	case looksNetscape(text):
		pairs = parseCookieNetscape(text)
	default:
		pairs = parseCookieHeader(text)
	}

	pairs = orderCookiePairs(pairs)
	if len(pairs) == 0 {
		return "", nil, errors.New("这段文本里没有解析出任何 Cookie：需要 name=value 形式，例如 SESSION=0f1e2d3c-…")
	}

	var (
		parts []string
		names []string
		total int
	)
	for _, p := range pairs {
		piece := p.Name + "=" + p.Value
		if total+len(piece)+2 > maxCookieHeaderLen && len(parts) > 0 {
			break
		}
		total += len(piece) + 2
		parts = append(parts, piece)
		names = append(names, p.Name)
	}
	return strings.Join(parts, "; "), names, nil
}

// parseCookieJSON 解析 cookies.json（数组或对象两种形态）。
func parseCookieJSON(text string) ([]cookiePair, error) {
	var items []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	}
	if strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), &items); err != nil {
			return nil, fmt.Errorf("cookies.json 不是合法 JSON：%w", err)
		}
		out := make([]cookiePair, 0, len(items))
		for _, it := range items {
			out = append(out, cookiePair{Name: it.Name, Value: it.Value, Domain: it.Domain})
		}
		return out, nil
	}
	// 对象形态：{"SESSION":"xxx"} 或 {"cookies":[{"name":...}]}
	var wrapped struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal([]byte(text), &wrapped); err == nil && len(wrapped.Cookies) > 0 {
		out := make([]cookiePair, 0, len(wrapped.Cookies))
		for _, it := range wrapped.Cookies {
			out = append(out, cookiePair{Name: it.Name, Value: it.Value, Domain: it.Domain})
		}
		return out, nil
	}
	var flat map[string]string
	if err := json.Unmarshal([]byte(text), &flat); err != nil {
		return nil, fmt.Errorf("这段 JSON 不是 cookies.json 的形状（需要 [{\"name\":…,\"value\":…}] 或 {\"名字\":\"值\"}）：%w", err)
	}
	names := make([]string, 0, len(flat))
	for name := range flat {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]cookiePair, 0, len(names))
	for _, name := range names {
		out = append(out, cookiePair{Name: name, Value: flat[name]})
	}
	return out, nil
}

// looksNetscape 判断是不是 Netscape cookie 文件：至少有一行能用 tab 切成 7 段。
func looksNetscape(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Count(line, "\t") >= 6 {
			return true
		}
	}
	return false
}

// parseCookieNetscape 解析 Netscape cookies.txt。
// 字段顺序：domain, flag, path, secure, expiry, name, value（第 6、7 段）。
// HttpOnly 的行以 "#HttpOnly_" 开头，它不是注释，要去掉前缀后照常解析。
func parseCookieNetscape(text string) []cookiePair {
	var out []cookiePair
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		out = append(out, cookiePair{
			Domain: strings.TrimSpace(fields[0]),
			Name:   strings.TrimSpace(fields[5]),
			Value:  strings.TrimSpace(fields[6]),
		})
	}
	return out
}

// parseCookieHeader 解析 `a=b; c=d` 形态，并容忍"只给了一个值"。
func parseCookieHeader(text string) []cookiePair {
	// 换行也可能是分隔符（多行粘贴），一并当成 ';'。
	text = strings.ReplaceAll(text, "\r", ";")
	text = strings.ReplaceAll(text, "\n", ";")

	var out []cookiePair
	for _, item := range strings.Split(text, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, value, found := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !found {
			// 整段只有一个裸值（最常见的是直接复制 SESSION 的值）：按 SESSION 处理，
			// 这比报"格式不对"有用得多——用户本来就是想让我们拿它当会话用。
			if len(out) == 0 && strings.ContainsAny(item, "-_.") && !strings.Contains(item, " ") {
				out = append(out, cookiePair{Name: "SESSION", Value: item})
			}
			continue
		}
		if name == "" || value == "" {
			continue
		}
		// 值里可能带引号（"xxx"），去掉成对的双引号。
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		out = append(out, cookiePair{Name: name, Value: value})
	}
	return out
}

// orderCookiePairs 过滤噪音并按"会话 Cookie 优先"排序。
//
// 过滤两条：跨站导出的 `名字_-_来源域名` 标记（浏览器对别的站点存的 Cookie，
// 对 sso 毫无意义），以及长度离谱的埋点值。
func orderCookiePairs(pairs []cookiePair) []cookiePair {
	seen := make(map[string]bool, len(pairs))
	out := make([]cookiePair, 0, len(pairs))
	for _, p := range pairs {
		p.Name = strings.TrimSpace(p.Name)
		p.Value = strings.TrimSpace(p.Value)
		if p.Name == "" || p.Value == "" || seen[p.Name] {
			continue
		}
		if strings.Contains(p.Name, "_-_") || len(p.Value) > maxCookieValueLen {
			continue
		}
		seen[p.Name] = true
		out = append(out, p)
	}

	rank := func(p cookiePair) int {
		for i, name := range sessionCookieNames {
			if strings.EqualFold(p.Name, name) {
				return i
			}
		}
		// 非会话 Cookie 排在后面，但属于 fzu 域的仍优先于第三方。
		if p.Domain == "" || strings.HasSuffix(strings.ToLower(p.Domain), "fzu.edu.cn") {
			return len(sessionCookieNames)
		}
		return len(sessionCookieNames) + 1
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// ---------- 用 Cookie 换 token ----------

// errCookieNotLoggedIn 是"Cookie 不是登录态"的哨兵，便于上层判断要不要提示重新导出。
var errCookieNotLoggedIn = errors.New("Cookie 不是统一身份认证的登录态")

// LoginWithCookie 用浏览器 Cookie 换晚点名免登 token（生产默认地址）。
func LoginWithCookie(cookie string) (CookieLoginResult, error) {
	return LoginWithCookieAt(SSOBase, ServiceURL, cookie)
}

// LoginWithCookieAt 是可注入 SSO 地址/回调地址的实现（测试与自建反代用它）。
func LoginWithCookieAt(ssoBase, service, cookie string) (CookieLoginResult, error) {
	loginURL, err := normalizeLoginURL(ssoBase)
	if err != nil {
		return CookieLoginResult{}, err
	}
	if strings.TrimSpace(service) == "" {
		service = ServiceURL
	}
	header, names, err := ParseCookieBundle(cookie)
	if err != nil {
		return CookieLoginResult{}, err
	}

	res := CookieLoginResult{
		Hops: []string{fmt.Sprintf("解析出 %d 个 Cookie：%s", len(names), strings.Join(names, "、"))},
	}
	attempted, err := attemptCookieLogin(loginURL, service, header, &res)
	res.Note = attempted
	if err == nil {
		return res, nil
	}
	// 打了登录页 = Cookie 没登录态。这是最可能的一种失败，单独翻译成可操作的提示：
	// 用户十有八九是把别的站点的 Cookie 复制过来了（Cookie 按域名隔离）。
	if errors.Is(err, errCookieNotLoggedIn) {
		return res, fmt.Errorf(
			"提交的 Cookie 在统一身份认证上不是登录状态（服务端返回的是登录页）。" +
				"Cookie 是按域名隔离的，要点：① 必须是 sso.fzu.edu.cn 的 SESSION，不是一站式服务大厅(oss.fzu.edu.cn)或其他子站的；" +
				"② 浏览器里退出过登录、或太久没动，SESSION 已过期，重新登录一次再复制。")
	}
	return res, err
}

// attemptCookieLogin 走一遍"带 Cookie 请求登录页 → 跟随 → 必要时 clientredirect"，
// 成功时把 token 写进 res 并返回一句人话结论。
func attemptCookieLogin(loginURL, service, cookieHeader string, res *CookieLoginResult) (string, error) {
	target := loginURL + "?" + url.Values{"service": {service}}.Encode()
	// 首跳不是重定向，不会被 CheckRedirect 记录，得自己写进诊断里——
	// 否则失败时用户只看到"解析出 N 个 Cookie"，看不到我们到底请求了什么。
	hops := &hopRecorder{hops: append(append([]string{}, res.Hops...), "GET "+target)}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: hops.check}

	page, status, finalURL, err := ssoGetWithCookie(client, target, cookieHeader)
	res.Hops = append(hops.hops, fmt.Sprintf("→ HTTP %d", status))
	if token := firstToken(res.Hops, finalURL); token != "" {
		res.Token = token
		return "会话有效，CAS 直接签发了 service ticket，已换到晚点名 token", nil
	}
	if err != nil && !errors.Is(err, errTooManyRedirects) {
		return "", err
	}

	// 校外/机房 IP 上，CAS 对"密码登录"和"已登录会话"一视同仁地先要短信验证：
	// 现场取证到的 200 页面里 sso-second=true + second-auth-tip，且没有 isRedirect。
	// 必须先判掉，否则下面会落到「Cookie 是登录态（用户 X），但服务端没有自动跳转」
	// ——那句会让人以为 Cookie 没问题、是流程问题，正好把真实原因藏起来。
	if sa, ok := secondAuthFrom(page); ok {
		return "", fmt.Errorf("%w（会话 Cookie 通道同样被要求短信验证）", sa)
	}

	if status == http.StatusOK {
		// 页面里若有 isRedirect=true 的登录方式，说明服务端认得这个会话，
		// 只是等前端跳一次 /clientredirect。
		if code, ok := autoRedirectCode(page); ok {
			origin := siblingOrigin(loginURL)
			redirectURL := origin + "/clientredirect?client_name=" + url.QueryEscape(code) +
				"&service=" + url.QueryEscape(service)
			hops.hops = append(hops.hops, "GET "+redirectURL)
			page2, status2, finalURL2, err2 := ssoGetWithCookie(client, redirectURL, cookieHeader)
			res.Hops = append(hops.hops, fmt.Sprintf("→ HTTP %d", status2))
			if token := firstToken(res.Hops, finalURL2); token != "" {
				res.Token = token
				return "会话有效（经 clientredirect " + code + " 换取 ticket），已换到晚点名 token", nil
			}
			if err2 != nil && !errors.Is(err2, errTooManyRedirects) {
				return "", err2
			}
			if status2 == http.StatusOK {
				if sa, ok := secondAuthFrom(page2); ok {
					return "", fmt.Errorf("%w（clientredirect 也停在二次认证页）", sa)
				}
				if loginPageRe.Match(page2) {
					return "", fmt.Errorf("%w（clientredirect 仍返回登录页）", errCookieNotLoggedIn)
				}
			}
			return "", fmt.Errorf("clientredirect(%s) 没有换到 token（HTTP %d），统一身份认证的跳转流程可能已变化", code, status2)
		}
		if loginPageRe.Match(page) {
			return "", errCookieNotLoggedIn
		}
		if uid := userIDValue(page); uid != "" {
			return "", fmt.Errorf("Cookie 是登录态（用户 %s），但服务端没有自动跳转，也未找到可用的登录方式标记；"+
				"请在浏览器里打开一次晚点名页面，然后把地址栏里的 token 直接填进「晚点名 token」", uid)
		}
		return "", fmt.Errorf("%w（登录页里既没有登录表单，也没有自动跳转标记，页面结构可能已变化）", errCookieNotLoggedIn)
	}
	if err != nil {
		if errors.Is(err, errTooManyRedirects) {
			return "", fmt.Errorf("重定向超过 %d 跳且链接里没有 token（回调地址或登录流程已变化）", ssoMaxRedirects)
		}
		return "", err
	}
	return "", fmt.Errorf("统一身份认证返回 HTTP %d，没有换到 token", status)
}

// hopRecorder 记录每一跳，供诊断展示。
type hopRecorder struct{ hops []string }

func (h *hopRecorder) check(req *http.Request, via []*http.Request) error {
	if req != nil && req.URL != nil {
		h.hops = append(h.hops, req.Method+" "+req.URL.String())
	}
	if len(via) >= ssoMaxRedirects {
		return errTooManyRedirects
	}
	return nil
}

// ssoGetWithCookie 带 Cookie 取一页，返回正文、状态码与最终 URL（不跟随重定向时
// 最终 URL 就是请求地址）。
func ssoGetWithCookie(client *http.Client, target, cookieHeader string) ([]byte, int, string, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, "", fmt.Errorf("统一身份认证请求构造失败: %w", err)
	}
	req.Header.Set("User-Agent", uaSSO)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("统一身份认证请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if readErr != nil {
		return body, resp.StatusCode, finalURL, fmt.Errorf("统一身份认证响应读取失败: %w", readErr)
	}
	return body, resp.StatusCode, finalURL, nil
}

// firstToken 在重定向链里找 token：token 只会出现在 URL 上，且常常出现在
// 又一次 302 的 Location 里（那一跳本身永远不返回 200）。
func firstToken(hops []string, finalURL string) string {
	for _, u := range append(append([]string{}, hops...), finalURL) {
		if token := tokenFromURL(u); token != "" {
			return token
		}
	}
	return ""
}

// autoRedirectCode 找登录页里 isRedirect=true 的登录方式代码。
func autoRedirectCode(page []byte) (string, bool) {
	for _, m := range ruleRe.FindAllSubmatch(page, -1) {
		if strings.EqualFold(strings.TrimSpace(string(m[2])), "true") {
			return strings.TrimSpace(string(m[1])), true
		}
	}
	return "", false
}

// userIDValue 取服务端渲染的用户标识（已登录时非空）。
func userIDValue(page []byte) string {
	m := userIDRe.FindSubmatch(page)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// siblingOrigin 返回与 loginURL 同源的根（`https://sso.fzu.edu.cn`）。
func siblingOrigin(loginURL string) string {
	u, err := url.Parse(loginURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://sso.fzu.edu.cn"
	}
	return u.Scheme + "://" + u.Host
}
