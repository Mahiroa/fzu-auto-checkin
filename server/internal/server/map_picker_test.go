package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestMapPickerFrontendContract 钉住「地图选点」的前端契约。
//
// 这个功能打破了两条旧约束，所以必须用测试把边界写死：
//  1. 页面开始依赖第三方**图片**（高德公开瓦片 / OSM 兜底）和**一个可选的接口**
//     （高德逆地理编码，用来拼"省市区路号靠近POI"的上报地点文字）——CSP 只能放宽
//     img-src 与 connect-src，脚本与样式策略一个字都不许动；
//  2. 前端多了一个按需加载的模块（map-picker.js），它不在首屏三文件的体积预算里，
//     所以这里给它自己定一个上限，避免它以"懒加载"为名慢慢长成第二个大文件。
//
// 同时锁住"不用付费服务"这个前提：不许出现高德 JS API 的 SDK 域名与安全密钥；
// 地名只走"用户自己填 key 的 Web 服务逆地理编码"，且 key 只存 localStorage、不进配置。
func TestMapPickerFrontendContract(t *testing.T) {
	env := newTestEnv(t)

	res := env.do(env.client(), http.MethodGet, "/map-picker.js", nil)
	env.expectStatus(res, http.StatusOK, "GET /map-picker.js")
	picker := string(res.body)
	if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("/map-picker.js 的 Content-Type 期望 text/javascript，实际 %q", ct)
	}
	const pickerBudget = 44 << 10
	if len(picker) > pickerBudget {
		t.Fatalf("map-picker.js 体积 %d 超过预算 %d（多 %.1f KiB）",
			len(picker), pickerBudget, float64(len(picker)-pickerBudget)/1024)
	}

	// 底图必须是"公开瓦片"，不是需要 key 的接口；地名服务走的是独立的 Web 服务 key。
	for _, want := range []string{
		"webrd0", "is.autonavi.com", "webst0", "tile.openstreetmap.org",
		"export function openPicker", "export function parsePoint",
		"restapi.amap.com", "geocode/regeo", // 可选逆地理编码：拼"省市区路号靠近POI"
		"CAMPUS_PREFIX", "localStorage", // 上报地点文字的格式前缀 + key/记忆的存放处
	} {
		if !strings.Contains(picker, want) {
			t.Fatalf("map-picker.js 缺少 %q", want)
		}
	}
	for _, bad := range []string{
		"webapi.amap.com", "securityJsCode", "window._AMapSecurityConfig", "http://",
	} {
		if strings.Contains(picker, bad) {
			t.Fatalf("map-picker.js 出现了 %q：底图应保持免 key、全程 https", bad)
		}
	}
	// key 绝不能落到配置里：模块不许自己调配置接口，key 只走 localStorage。
	if strings.Contains(picker, "/api/v1") {
		t.Fatalf("map-picker.js 不该直接调后端配置接口：key 与点位记忆只留在浏览器")
	}
	// 与 app.js 同一套约定：动态内容一律 createElement/textContent，不许拼 HTML。
	for _, bad := range []string{"innerHTML", "eval(", "new Function(", "document.write("} {
		if strings.Contains(picker, bad) {
			t.Fatalf("map-picker.js 使用了 %q（CSP 无 unsafe-eval/inline，且既有约定是 textContent）", bad)
		}
	}

	// index.html：按钮、弹窗骨架、地点文字框、位置池行内的「地图选点」都要在。
	index := staticFromEnv(t, env, "index.html")
	for _, want := range []string{
		`id="map-picker"`, `id="mp-map"`, `id="mp-tiles"`, `id="mp-pin"`,
		`id="mp-ok"`, `id="mp-paste"`, `id="btn-map-pick"`, `class="ghost r-pick"`,
		`id="mp-name"`, `id="mp-regeo"`, `id="mp-key"`,
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html 缺少 %q（地图选点的骨架）", want)
		}
	}

	// app.js：只做懒加载 + 回填，不许把选点逻辑抄进来（体积预算会爆）。
	appJS := staticFromEnv(t, env, "app.js")
	if !strings.Contains(appJS, "import('./map-picker.js')") {
		t.Fatalf("app.js 应该以动态 import 的方式懒加载 ./map-picker.js")
	}
	if strings.Contains(appJS, "is.autonavi.com") {
		t.Fatalf("瓦片地址只该出现在 map-picker.js 里，app.js 不碰地图细节")
	}
	// 地点文字必须跟坐标一起回填：确认后不能只把经纬度写进表单。
	if !strings.Contains(appJS, "name: actEl ? String(actEl.value || '').trim() : ''") {
		t.Fatalf("app.js 应该把现有的上报地点文字带进选点器")
	}

	// CSP：只放宽 img-src 与 connect-src；脚本/样式策略必须一个字符都没松。
	csp := env.do(env.client(), http.MethodGet, "/", nil).header.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'", "img-src 'self' data:",
		"https://*.autonavi.com", "https://tile.openstreetmap.org",
		"script-src 'self'", "style-src 'self'", "connect-src 'self' https://restapi.amap.com",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP 缺少 %q：%q", want, csp)
		}
	}
	for _, bad := range []string{"'unsafe-eval'", "'unsafe-inline'", "http://", "*;"} {
		if strings.Contains(csp, bad) {
			t.Fatalf("CSP 不该出现 %q：%q", bad, csp)
		}
	}
	// 瓦片是唯一的外部来源：脚本域不能跟着一起放开。
	if strings.Contains(csp, "script-src") &&
		(strings.Contains(csp, "script-src * 'self'") || strings.Contains(csp, "script-src 'self' https:")) {
		t.Fatalf("script-src 被放宽了：%q", csp)
	}

	t.Logf("地图选点：map-picker.js=%d B（预算 %d），CSP=%s", len(picker), pickerBudget, csp)
}
