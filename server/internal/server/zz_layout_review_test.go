package server

// 独立验收（第二轮，v1.1）：左右布局 / 浏览器定位 / 全自绘控件的前端契约。
//
// 定位：zz_review_test.go 管的是"能否上线"的既有契约（CSP、体积、外链、MIME），
// 本文件只钉 v1.1 这一轮新加、且上一轮没人断言过的东西：
//   - `#shell` 左侧 <aside class="sidebar"> + 右侧 `#main` 的 DOM 结构与顺序；
//   - "无内联样式 / 无内联脚本 / 无外部资源"三条 CSP 硬线的**字面**证据；
//   - 自绘控件依赖的具体选择器（appearance / data URI 图标 / 原生指示器覆盖 / 自绘滚动条 / 焦点环）；
//   - 浏览器定位的 WGS-84 → GCJ-02 转换与 getCurrentPosition 调用点；
//   - §13 的分文件体积预算（index ≤40KB / style ≤45KB / app.js ≤80KB / 合计 ≤160KB）。
//
// 全部离线、无服务、无网络：读的是 web.FS —— go:embed 打进去的那一份，
// 也就是二进制里真正会被浏览器拿到的那一份（不是磁盘上可能忘记 rebuild 的副本）。
// 文件缺失 / 断言不符一律 t.Fatalf，不做"跳过"。

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"fzucheckin/web"
)

// §13 v1.1 的体积预算（未压缩字节数）。
const (
	layoutIndexBudget = 40 << 10 // index.html ≤ 40 KiB
	layoutStyleBudget = 45 << 10 // style.css  ≤ 45 KiB
	layoutAppBudget   = 80 << 10 // app.js     ≤ 80 KiB
	layoutTotalBudget = 160 << 10
)

// layoutStatic 从内嵌 FS 里读一个静态文件。读不到就失败——静态资源缺失是线上事故，
// 不是"环境没准备好"。
func layoutStatic(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(web.FS, web.StaticDir+"/"+name)
	if err != nil {
		t.Fatalf("内嵌静态资源里没有 %s/%s：%v（go:embed 没带上？）", web.StaticDir, name, err)
	}
	if len(raw) == 0 {
		t.Fatalf("内嵌静态资源 %s 是空文件", name)
	}
	return string(raw)
}

// layoutBetween 返回 s 中 from 之后、to 之前的那一段；顺序不对或缺失时返回 ok=false。
func layoutBetween(s, from, to string) (string, bool) {
	i := strings.Index(s, from)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// ============ 1. 左右布局的 DOM 结构（§13 布局 v1.1） ============

func TestReviewLayoutSidebarStructure(t *testing.T) {
	index := layoutStatic(t, "index.html")
	lower := strings.ToLower(index)

	// 左侧边栏本体：<aside class="sidebar"> + 品牌 + #nav + 底部退出/版本。
	if !strings.Contains(index, `class="sidebar"`) {
		t.Fatalf("index.html 缺少 class=\"sidebar\"：左侧栏没建起来")
	}
	if !strings.Contains(lower, "<aside") {
		t.Fatalf("index.html 里没有 <aside> 元素（侧栏必须是语义化的 aside，不是 div）")
	}
	for _, id := range []string{`id="nav"`, `id="main"`, `id="btn-geo"`, `id="geo-status"`} {
		if !strings.Contains(index, id) {
			t.Fatalf("index.html 缺少 %s", id)
		}
	}

	// 顺序：aside → (brand / #nav) → #main。导航必须在主内容之前，
	// 否则 Tab 顺序会先跳内容再跳导航，屏幕阅读器把导航读成"页尾"。
	navAt := strings.Index(index, `id="nav"`)
	mainAt := strings.Index(index, `id="main"`)
	asideAt := strings.Index(lower, "<aside")
	if asideAt < 0 || navAt < 0 || mainAt < 0 {
		t.Fatalf("定位失败：aside=%d nav=%d main=%d", asideAt, navAt, mainAt)
	}
	if !(asideAt < navAt && navAt < mainAt) {
		t.Fatalf("#nav 必须出现在 #main 之前（实际 aside=%d nav=%d main=%d）", asideAt, navAt, mainAt)
	}
	if !strings.Contains(index[:asideAt], `id="shell"`) {
		// #shell 在 aside 之前才说明 aside 是 #shell 的子节点（左右分栏的左右两格）。
		t.Fatalf("#shell 必须包住 aside：找不到 #shell 开在 aside 之前（aside 被挪到外壳外面了）")
	}

	// 底部区（退出登录 + 版本号）必须在 aside 内部，不能漂到 #main 里。
	asideBody, ok := layoutBetween(index, `<aside class="sidebar">`, "</aside>")
	if !ok {
		t.Fatalf("<aside class=\"sidebar\"> 没有对应的 </aside>")
	}
	if !strings.Contains(asideBody, `id="btn-logout"`) {
		t.Fatalf("侧栏底部缺少「退出登录」按钮（id=btn-logout）")
	}
	if !strings.Contains(asideBody, "side-version") {
		t.Fatalf("侧栏底部缺少版本号（class=side-version）")
	}

	// 四个导航项，且 data-route 与 hash 路由逐字一致（JS 靠 dataset.route 设 aria-current）。
	routes := []string{"#/dashboard", "#/config", "#/logs", "#/runs"}
	for _, route := range routes {
		attr := `data-route="` + route + `"`
		if got := strings.Count(index, attr); got != 1 {
			t.Fatalf("导航项 %s 期望恰好 1 个，实际 %d 个", attr, got)
		}
	}
	if got := strings.Count(index, `class="nav-btn"`); got != len(routes) {
		t.Fatalf("侧栏导航按钮期望 %d 个（.nav-btn），实际 %d 个", len(routes), got)
	}
	// 导航必须是 #nav 的子节点：取 #nav 到 </nav> 之间再数一遍。
	navBody, ok := layoutBetween(index, `id="nav"`, "</nav>")
	if !ok {
		t.Fatalf("#nav 没有对应的 </nav>")
	}
	if got := strings.Count(navBody, `class="nav-btn"`); got != len(routes) {
		t.Fatalf("#nav 里期望 %d 个 .nav-btn，实际 %d 个（有按钮漏在导航外面）", len(routes), got)
	}
}

// ============ 2. 无内联样式 / 无内联脚本（§13 CSP 三条硬线之一） ============

func TestReviewLayoutNoInlineStyleOrScript(t *testing.T) {
	index := layoutStatic(t, "index.html")
	lower := strings.ToLower(index)

	// style="…" 内联属性：CSP 是 style-src 'self'（无 'unsafe-inline'），
	// 只要出现一处，那一条声明就会被浏览器丢掉——宁可现在红，也不要线上静默失效。
	if got := strings.Count(index, ` style="`); got != 0 {
		t.Fatalf("index.html 出现 %d 处内联 style=\"…\"（CSP style-src 'self' 会整条丢弃）", got)
	}
	if got := strings.Count(lower, "<style"); got != 0 {
		t.Fatalf("index.html 出现 %d 处内联 <style>（CSP style-src 'self' 会整块丢弃）", got)
	}

	// <script>：只允许外链 app.js 这一处。
	if got := strings.Count(lower, "<script"); got != 1 {
		t.Fatalf("index.html 的 <script 期望恰好 1 处（外链 app.js），实际 %d 处", got)
	}
	if !strings.Contains(index, `<script type="module" src="./app.js">`) {
		t.Fatalf("index.html 的那唯一一处脚本必须逐字是 <script type=\"module\" src=\"./app.js\">")
	}
	// 内联事件处理器（on*=）和 javascript: 伪协议同样会被 CSP 挡掉。
	for _, attr := range []string{" onclick=", " onchange=", " onsubmit=", " onload=", " oninput=", " onerror="} {
		if strings.Contains(lower, attr) {
			t.Fatalf("index.html 含内联事件处理器 %q（CSP 会拒绝执行）", strings.TrimSpace(attr))
		}
	}
	if strings.Contains(lower, "javascript:") {
		t.Fatalf("index.html 含 javascript: 伪协议（CSP 下失效）")
	}
	// 静态模板里也不许塞 style（template 内容会被 clone 进文档，同样受 CSP 约束）。
	if strings.Contains(lower, `<template`) {
		tpl := lower[strings.Index(lower, "<template"):]
		if strings.Contains(tpl, ` style="`) || strings.Contains(tpl, "<style") {
			t.Fatalf("<template> 片段里出现了内联 style（clone 进文档后同样被 CSP 丢弃）")
		}
	}
}

// ============ 3. 无外部资源（离线可用 + CSP 'self'） ============

// svgNS 只匹配 SVG 命名空间声明本身。它不是网络请求，是 XML 命名空间标识符；
// 两种引号写法都必须豁免——style.css 的 data URI 里用的是单引号（见 --ico-*）。
var svgNS = regexp.MustCompile(`xmlns\s*=\s*['"]http://www\.w3\.org/2000/svg['"]`)

// loopbackHint 允许 app.js 在**用户提示文案**里出现回环地址（"改用 http://127.0.0.1"）。
// 它不是资源引用：没有 src/href/url()/fetch，浏览器不会为此发起任何请求。
var loopbackHint = regexp.MustCompile(`^https?://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?`)

func TestReviewLayoutNoExternalResources(t *testing.T) {
	files := map[string]string{
		"index.html": layoutStatic(t, "index.html"),
		"style.css":  layoutStatic(t, "style.css"),
		"app.js":     layoutStatic(t, "app.js"),
	}

	for name, body := range files {
		// 先摘掉 SVG 命名空间，再看还有没有 http(s) 字样。
		stripped := svgNS.ReplaceAllString(body, "")

		for _, marker := range []string{"//cdn", "unpkg", "jsdelivr", "googleapis"} {
			if strings.Contains(strings.ToLower(stripped), marker) {
				t.Fatalf("%s 引用了外部资源标记 %q（离线可用 + CSP 'self' 都会破）", name, marker)
			}
		}

		// 真正的资源引用形态：src=/href= 指向 //host、url(//…)、@import。
		for _, bad := range []string{`src="//`, `src='//`, `src="http`, `src='http`,
			`href="//`, `href='//`, `href="http`, `href='http`,
			"url(//", "url('//", `url("//`, "url(http", "url('http", `url("http`,
			"@import"} {
			if strings.Contains(strings.ToLower(stripped), bad) {
				t.Fatalf("%s 含外部资源引用 %q", name, bad)
			}
		}

		// 逐行核对残留的 http(s):// —— 只允许"回环地址提示文案"这一种。
		for _, line := range strings.Split(stripped, "\n") {
			idx := 0
			for {
				i := strings.Index(strings.ToLower(line[idx:]), "http")
				if i < 0 {
					break
				}
				at := idx + i
				idx = at + 4
				rest := line[at:]
				if !strings.HasPrefix(rest, "http://") && !strings.HasPrefix(rest, "https://") {
					continue
				}
				if loopbackHint.MatchString(rest) {
					continue // 允许：提示用户改用回环地址
				}
				trimmed := strings.TrimSpace(rest)
				if len(trimmed) > 80 {
					trimmed = trimmed[:80] + "…"
				}
				t.Fatalf("%s 出现了非回环的 http(s) 地址：%q", name, trimmed)
			}
		}
	}

	// index.html 与 style.css 里应当一个 http(s) 都不剩（SVG 命名空间已在上面摘掉）。
	for _, name := range []string{"index.html", "style.css"} {
		stripped := svgNS.ReplaceAllString(files[name], "")
		if i := strings.Index(strings.ToLower(stripped), "http://"); i >= 0 {
			t.Fatalf("%s 残留 http://（偏移 %d）：%q", name, i, stripped[i:min(i+60, len(stripped))])
		}
		if i := strings.Index(strings.ToLower(stripped), "https://"); i >= 0 {
			t.Fatalf("%s 残留 https://（偏移 %d）：%q", name, i, stripped[i:min(i+60, len(stripped))])
		}
	}

	// 现状记录：app.js 的定位失败提示里有一处回环地址文案（不是资源引用）。
	if n := strings.Count(files["app.js"], "http://127.0.0.1"); n > 0 {
		t.Logf("口径说明：app.js 有 %d 处 http://127.0.0.1 出现在用户提示文案里（非资源引用），已按语义放行", n)
	}
}

// ============ 4. 全自绘控件：CSS 侧证据（§13 控件 v1.1） ============

// layoutCSSRuleBody 找到"选择器列表里含 token"的第一条规则，返回**整条规则**
// （选择器列表 + 声明体）。返回整条而不是只返回声明体，是因为很多断言要同时看
// "这条规则覆盖了哪些选择器"和"它声明了什么"（例：共用输入规则必须列出 text/number/time…）。
// 只做静态文本扫描：token 之后、下一个 '{' 之前不能出现 '}' 或 ';'，否则说明这个 token
// 落在声明体/注释里，跳过继续找。
func layoutCSSRuleBody(css, token string) (string, bool) {
	idx := 0
	for {
		i := strings.Index(css[idx:], token)
		if i < 0 {
			return "", false
		}
		start := idx + i
		rest := css[start:]
		open := strings.Index(rest, "{")
		if open < 0 {
			return "", false
		}
		head := rest[:open]
		if strings.ContainsAny(head, "};") {
			idx = start + len(token)
			continue
		}
		absEnd := start + open + 1
		if end := strings.Index(css[absEnd:], "}"); end >= 0 {
			absEnd += end + 1
		} else {
			absEnd = len(css)
		}
		// 回溯到本条规则的起点：上一个 '}' 之后、且跳过紧邻的注释。
		begin := 0
		if j := strings.LastIndex(css[:start], "}"); j >= 0 {
			begin = j + 1
		}
		if k := strings.LastIndex(css[:start], "*/"); k >= 0 && k+2 > begin {
			begin = k + 2
		}
		return strings.TrimLeft(css[begin:absEnd], " \t\r\n"), true
	}
}

// layoutCSSRuleExact 找"token 独占选择器"的那条规则（token 与 '{' 之间只有空白，
// 且 token 前面不是 ',' / '>' / '+' / '~' —— 否则它只是选择器列表里的一员）。
func layoutCSSRuleExact(css, token string) (string, bool) {
	idx := 0
	for {
		i := strings.Index(css[idx:], token)
		if i < 0 {
			return "", false
		}
		start := idx + i
		rest := css[start+len(token):]
		open := strings.Index(rest, "{")
		if open < 0 {
			return "", false
		}
		if strings.TrimSpace(rest[:open]) != "" {
			idx = start + len(token)
			continue
		}
		// 往前跳过空白，看前一个非空白字符是不是选择器列表分隔符。
		prev := byte(0)
		for j := start - 1; j >= 0; j-- {
			c := css[j]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			prev = c
			break
		}
		if prev == ',' || prev == '>' || prev == '+' || prev == '~' {
			idx = start + len(token)
			continue
		}
		body := rest[open+1:]
		if end := strings.Index(body, "}"); end >= 0 {
			return body[:end], true
		}
		return body, true
	}
}

func TestReviewLayoutHandDrawnControlsCSS(t *testing.T) {
	css := layoutStatic(t, "style.css")

	// 4.1 appearance: none 的覆盖面。
	// 文本/密码/数字/time/date/select/textarea 共用一条规则，checkbox 单独一条。
	if got := strings.Count(css, "appearance: none"); got < 3 {
		t.Fatalf("style.css 里 appearance: none 只有 %d 处（至少要覆盖共用输入规则、checkbox、按钮）", got)
	}
	shared, ok := layoutCSSRuleBody(css, "input[type=text]")
	if !ok {
		t.Fatalf("找不到 input[type=text] 所在的规则")
	}
	if !strings.Contains(shared, "appearance: none") {
		t.Fatalf("共用输入规则（input[type=text]…）没有 appearance: none")
	}
	for _, sel := range []string{
		"input[type=text]", "input[type=password]", "input[type=number]",
		"input[type=time]", "input[type=date]", "select", "textarea",
	} {
		if !strings.Contains(shared, sel) {
			t.Fatalf("共用输入规则的选择器列表里缺少 %s（这个控件就还是原生外观）", sel)
		}
	}

	// checkbox 自绘：appearance:none + 18px 方框 + ::after 勾。
	checkbox, ok := layoutCSSRuleExact(css, "input[type=checkbox]")
	if !ok {
		t.Fatalf("找不到 input[type=checkbox] 的自绘规则")
	}
	if !strings.Contains(checkbox, "appearance: none") {
		t.Fatalf("checkbox 规则没有 appearance: none：%q", checkbox)
	}
	if !strings.Contains(css, "input[type=checkbox]::after") {
		t.Fatalf("checkbox 缺少 ::after 自绘勾")
	}

	// number：浏览器原生外观被显式覆盖成 textfield，并把 spin button 彻底隐藏。
	// （§13 正文写的是"一律 appearance:none"，实现选了 textfield + 隐藏 spin 按钮，
	//  两者效果等价：都是"看不到原生控件、只看到自绘外框"。这里按实现口径钉住，
	//  同时要求 spin button 必须被关掉，否则用户仍会看到原生小箭头。）
	number, ok := layoutCSSRuleExact(css, "input[type=number]")
	if !ok {
		t.Fatalf("找不到 input[type=number] 的规则")
	}
	if !strings.Contains(number, "appearance: textfield") && !strings.Contains(number, "appearance: none") {
		t.Fatalf("number 的 appearance 没有被覆盖（期望 textfield 或 none）：%q", number)
	}
	// 两个伪元素共用同一条规则，所以按"规则体"取，不按独占选择器取。
	for _, sel := range []string{"input[type=number]::-webkit-inner-spin-button", "input[type=number]::-webkit-outer-spin-button"} {
		if !strings.Contains(css, sel) {
			t.Fatalf("缺少 %s 的隐藏规则（原生上下箭头会露出来）", sel)
		}
	}
	spinBody, ok := layoutCSSRuleBody(css, "::-webkit-inner-spin-button")
	if !ok {
		t.Fatalf("找不到 ::-webkit-inner-spin-button 的规则体")
	}
	if !strings.Contains(spinBody, "appearance: none") {
		t.Fatalf("spin button 没有被关闭（期望 appearance: none）：%q", spinBody)
	}

	// 4.2 data URI SVG 图标（无外部图片、无 icon font）。
	if got := strings.Count(css, "data:image/svg+xml"); got < 3 {
		t.Fatalf("style.css 里 data:image/svg+xml 只有 %d 处（期望 chevron/clock/calendar 各一套，含浅色主题）", got)
	}
	if !strings.Contains(css, "%23") && strings.Contains(css, "data:image/svg+xml") {
		t.Logf("提示：data URI 里若出现 '#' 必须转义成百分号 23；当前未见该转义（图标里可能确实没有 # 颜色）")
	}

	// 4.3 time/date 原生指示器：透明覆盖在自绘图标上方，点击仍能弹原生选择器。
	if !strings.Contains(css, "::-webkit-calendar-picker-indicator") {
		t.Fatalf("style.css 缺少 ::-webkit-calendar-picker-indicator 覆盖（原生日历/时钟图标会叠在自绘图标上）")
	}
	indicator, ok := layoutCSSRuleExact(css, "input[type=time]::-webkit-calendar-picker-indicator,")
	if !ok {
		// 选择器是"两条一组"，精确匹配失败时退化成查询它所在规则的声明体。
		body, ok2 := layoutCSSRuleBody(css, "::-webkit-calendar-picker-indicator")
		if !ok2 {
			t.Fatalf("找不到 ::-webkit-calendar-picker-indicator 的规则体")
		}
		indicator = body
	}
	if !strings.Contains(indicator, "opacity: 0") {
		t.Fatalf("原生指示器没有被透明化（期望 opacity: 0）：%q", indicator)
	}
	for _, sel := range []string{"input[type=time] { background-image: var(--ico-clock)", "input[type=date] { background-image: var(--ico-calendar)"} {
		if !strings.Contains(css, sel) {
			t.Fatalf("style.css 缺少自绘图标：%q", sel)
		}
	}

	// 4.4 自绘滚动条 + 自定义焦点环。
	if !strings.Contains(css, "::-webkit-scrollbar") {
		t.Fatalf("style.css 缺少自绘滚动条（::-webkit-scrollbar）")
	}
	if !strings.Contains(css, ":focus-visible") {
		t.Fatalf("style.css 缺少 :focus-visible 自定义焦点环")
	}
	// 焦点环不能靠 outline: none 直接抹掉——抹掉且不补 box-shadow 等于不可见焦点。
	if strings.Contains(css, "outline: none") && !strings.Contains(css, "box-shadow") {
		t.Fatalf("出现了 outline: none 但没有任何 box-shadow 补位（键盘焦点会不可见）")
	}

	// 4.5 左右布局与响应式：Grid 两列 + ≤860px 折成顶部横条。
	if !strings.Contains(css, "grid-template-columns: var(--side-w) minmax(0, 1fr)") {
		t.Fatalf("#shell 不是 248px + 1fr 的 Grid 两列（找不到 grid-template-columns: var(--side-w) minmax(0, 1fr)）")
	}
	if !strings.Contains(css, "--side-w: 248px") {
		t.Fatalf("缺少 --side-w: 248px（§13 规定左栏 248px）")
	}
	aside, ok := layoutCSSRuleExact(css, "aside.sidebar")
	if !ok {
		t.Fatalf("找不到 aside.sidebar 的规则")
	}
	if !strings.Contains(aside, "position: sticky") && !strings.Contains(aside, "position: fixed") {
		t.Fatalf("aside.sidebar 不是 sticky/fixed（长页面滚动时导航会跟着跑掉）：%q", aside)
	}
	media, ok := layoutBetween(css, "@media (max-width: 860px)", "}\n}\n")
	if !ok {
		t.Fatalf("找不到 @media (max-width: 860px) 的整块规则")
	}
	if !strings.Contains(media, "position: sticky") {
		t.Fatalf("≤860px 的顶部横条没有 sticky（手机上滚下去导航就消失了）")
	}
	if !strings.Contains(media, "grid-template-columns: minmax(0, 1fr)") {
		t.Fatalf("≤860px 没有把 #shell 折成单列")
	}
}

// ============ 5. 浏览器定位：WGS-84 → GCJ-02（§13 定位 v1.1） ============

func TestReviewLayoutGeolocationConversion(t *testing.T) {
	js := layoutStatic(t, "app.js")

	// 5.1 真的调了浏览器定位 API，且带的是高精度/超时语义的参数。
	if !strings.Contains(js, "navigator.geolocation.getCurrentPosition") {
		t.Fatalf("app.js 没有调用 navigator.geolocation.getCurrentPosition")
	}
	for _, want := range []string{"enableHighAccuracy: true", "timeout: 15000", "maximumAge: 0"} {
		if !strings.Contains(js, want) {
			t.Fatalf("getCurrentPosition 的 options 里缺少 %q", want)
		}
	}

	// 5.2 坐标转换必须存在，而且是标准 GCJ-02 公式（克拉索夫斯基椭球参数）。
	for _, want := range []string{
		"function wgs84ToGcj02(", "const GCJ_A = 6378245.0", "const GCJ_EE = 0.00669342162296594323",
		"function transformLat(", "function transformLng(", "function outOfChina(",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少坐标转换要素 %q（浏览器给的是 WGS-84，不转换在福州会偏 ~590 米）", want)
		}
	}
	// 定义 + 至少一处调用。
	if got := strings.Count(js, "wgs84ToGcj02("); got < 2 {
		t.Fatalf("wgs84ToGcj02 只有定义没有调用（出现 %d 次）", got)
	}
	// 落地到输入框：6 位小数，且是 GCJ 结果。
	if got := strings.Count(js, "toFixed(6)"); got < 2 {
		t.Fatalf("经纬度回填没有按 6 位小数格式化（toFixed(6) 出现 %d 次）", got)
	}
	for _, id := range []string{"'cfg-lon'", "'cfg-lat'"} {
		if !strings.Contains(js, id) {
			t.Fatalf("app.js 没有回填 %s", id)
		}
	}
	if !strings.Contains(js, "dispatchEvent(new Event('input', { bubbles: true }))") {
		t.Fatalf("回填坐标后没有冒泡 input 事件（配置表单的徽标/提示不会同步）")
	}

	// 5.3 只回填、不自动保存：onGeoOk 的体内不许出现 apiFetch / saveConfig。
	body, ok := layoutBetween(js, "function onGeoOk(pos) {", "\n}\n")
	if !ok {
		t.Fatalf("找不到 onGeoOk 的函数体")
	}
	for _, banned := range []string{"apiFetch", "saveConfig", "PUT"} {
		if strings.Contains(body, banned) {
			t.Fatalf("onGeoOk 里出现了 %q：定位结果不允许自动保存，必须让用户确认后手动点保存", banned)
		}
	}

	// 5.4 安全上下文与错误分档：非 https/回环时不能"点了没反应"。
	if !strings.Contains(js, "isSecureContext") {
		t.Fatalf("app.js 没有判断 window.isSecureContext（http + 局域网 IP 下定位会被浏览器直接拒绝）")
	}
	for _, want := range []string{"code === 1", "code === 2", "code === 3", "精度较低"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 的定位错误/精度提示缺少分支 %q", want)
		}
	}
	if !strings.Contains(js, "'GCJ-02'") && !strings.Contains(js, "GCJ-02") {
		t.Fatalf("状态行没有提到 GCJ-02（用户无法确认回填的是哪种坐标系）")
	}

	// 5.5 CSP 无 'unsafe-eval'：不许 eval / new Function；动态文本一律 textContent。
	if regexp.MustCompile(`(^|[^\w$.])eval\s*\(`).MatchString(js) {
		t.Fatalf("app.js 使用了 eval(（CSP 无 'unsafe-eval'，会直接抛错）")
	}
	if strings.Contains(js, "new Function(") {
		t.Fatalf("app.js 使用了 new Function(（CSP 无 'unsafe-eval'）")
	}
	if regexp.MustCompile(`\.innerHTML\s*=`).MatchString(js) {
		t.Fatalf("app.js 给 innerHTML 赋过值（XSS 面；约定是 textContent）")
	}

	// 现状记录（P2，不在本文件断言里判红）：renderRunDetail 里有一处 CSSOM 行内样式，
	// 会生成 style 属性，与"无内联 style"的口径冲突；仅在错误详情渲染时才触发。
	// 渲染路径里的错误详情卡曾用 CSSOM 写 box.style.marginTop（P2，已改为 .err-box-gap）。
	// 现在「无内联样式」是硬门禁：整份 app.js 不允许出现 .style. 赋值。
	if i := strings.Index(js, ".style."); i >= 0 {
		line := 1 + strings.Count(js[:i], "\n")
		t.Fatalf("app.js:%d 用 CSSOM 写了内联样式（.style.*）——「文档里不出现 [style]」是 CSP 门禁，"+
			"间距/颜色请改用 class（参考 .err-box-gap）", line)
	}
}

// ============ 6. 体积预算（§13 v1.1） ============

func TestReviewLayoutSizeBudgets(t *testing.T) {
	index := layoutStatic(t, "index.html")
	css := layoutStatic(t, "style.css")
	js := layoutStatic(t, "app.js")

	cases := []struct {
		name   string
		size   int
		budget int
	}{
		{"index.html", len(index), layoutIndexBudget},
		{"style.css", len(css), layoutStyleBudget},
		{"app.js", len(js), layoutAppBudget},
	}
	for _, c := range cases {
		if c.size > c.budget {
			t.Fatalf("%s 体积 %d 超过预算 %d（多 %.1f KiB）",
				c.name, c.size, c.budget, float64(c.size-c.budget)/1024)
		}
		t.Logf("%-11s %6d B（%5.1f KiB / 预算 %d B）", c.name, c.size, float64(c.size)/1024, c.budget)
	}
	total := len(index) + len(css) + len(js)
	if total > layoutTotalBudget {
		t.Fatalf("前端三文件合计 %d 超过预算 %d（多 %.1f KiB）",
			total, layoutTotalBudget, float64(total-layoutTotalBudget)/1024)
	}
	t.Logf("合计        %6d B（%5.1f KiB / 预算 %d B）", total, float64(total)/1024, layoutTotalBudget)
}

// ============ 7. 守卫测试：断言依赖的两个 CSS 扫描助手本身要先被测 ============

// 这一段不碰生产资源，只验证 layoutCSSRuleBody / layoutCSSRuleExact / layoutBetween 的语义。
// 没有它的话，助手一旦退化成"永远返回 ok=true"，前面那些 Contains 断言就会变成假绿
// （拿到的规则体其实是空的，或者根本是别的规则）。
func TestReviewLayoutHelpersSelfCheck(t *testing.T) {
	css := ".a, .b { color: red; }\n.c { appearance: none; }\n.d::after, .e::after { opacity: 0; }\n"

	// 共享选择器列表：按其中任一 token 都能拿到整条规则（含选择器列表）。
	rule, ok := layoutCSSRuleBody(css, ".b")
	if !ok || !strings.Contains(rule, ".a") || !strings.Contains(rule, "color: red") {
		t.Fatalf("layoutCSSRuleBody(.b) 期望拿到整条共享规则，实际 ok=%v rule=%q", ok, rule)
	}
	// 独占选择器：.c 命中且只返回声明体；.a（列表里的一员）不算独占。
	if body, ok := layoutCSSRuleExact(css, ".c"); !ok || !strings.Contains(body, "appearance: none") {
		t.Fatalf("layoutCSSRuleExact(.c) 期望命中，实际 ok=%v body=%q", ok, body)
	}
	if _, ok := layoutCSSRuleExact(css, ".a"); ok {
		t.Fatalf("layoutCSSRuleExact(.a) 不该命中：.a 是共享规则里的一员，不是独占选择器")
	}
	// 共用伪元素：.e 精确匹配必须失败（它后面还有 .d::after），但按规则体能拿到。
	if _, ok := layoutCSSRuleExact(css, ".e::after"); ok {
		t.Fatalf("layoutCSSRuleExact(.e::after) 不该命中（它与 .d::after 共用一条规则）")
	}
	if rule, ok := layoutCSSRuleBody(css, ".e::after"); !ok || !strings.Contains(rule, "opacity: 0") {
		t.Fatalf("layoutCSSRuleBody(.e::after) 期望命中共用规则，实际 ok=%v rule=%q", ok, rule)
	}
	// 不存在的 token：必须报"找不到"，不能拿最后一条规则糊弄。
	if rule, ok := layoutCSSRuleBody(css, ".does-not-exist"); ok {
		t.Fatalf("不存在的 token 不该命中，实际返回 %q", rule)
	}
	if body, ok := layoutCSSRuleExact(css, ".does-not-exist"); ok {
		t.Fatalf("不存在的 token 不该精确命中，实际返回 %q", body)
	}
	// token 出现在声明体里时也要跳过（否则 ".c" 会命中 ".a{...}" 的体内文本）。
	if rule, ok := layoutCSSRuleBody("x { content: '.c'; }\n.c { color: blue; }", ".c"); !ok ||
		!strings.Contains(rule, "color: blue") {
		t.Fatalf("token 落在声明体里时应继续向后找，实际 ok=%v rule=%q", ok, rule)
	}

	// layoutBetween：正序命中、逆序必须失败。
	if got, ok := layoutBetween("a b c", "a", "c"); !ok || got != " b " {
		t.Fatalf("layoutBetween 正序期望 %q，实际 ok=%v got=%q", " b ", ok, got)
	}
	if _, ok := layoutBetween("a b c", "c", "a"); ok {
		t.Fatalf("layoutBetween 逆序不该命中")
	}
	if _, ok := layoutBetween("a b c", "zzz", "c"); ok {
		t.Fatalf("layoutBetween 缺少起点不该命中")
	}
}
