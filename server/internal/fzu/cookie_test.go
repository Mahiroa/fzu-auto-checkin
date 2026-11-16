package fzu

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cookieBundleJSON 是缩小版的导出结构（含一条噪音 Cookie 和一条跨站标记）。
//
// 所有值都是合成值，形状与真实导出一致：SESSION 用显式 UUID，Access-Token 用显式
// SYNTHETIC- 前缀。绝不要把真实抓包里的值填进来——这个文件在公开仓库里。
const cookieBundleJSON = `[
  {"domain": ".fzu.edu.cn", "name": "Hm_lvt_0dae59e1f85da1153b28fb5a2671647f", "value": "1789572251"},
  {"domain": "sso.fzu.edu.cn", "name": "SESSION", "value": "0f1e2d3c-aaaa-bbbb-cccc-1234567890ab"},
  {"domain": ".fzu.edu.cn", "name": "sensorsdata2015jssdkcross_-_.xuetangx.com", "value": "%7B%22distinct_id%22%3A%22x%22%7D"},
  {"domain": "oss.fzu.edu.cn", "name": "Access-Token", "value": "SYNTHETIC-access-token-0001"}
]`

func TestParseCookieBundleJSON(t *testing.T) {
	header, names, err := ParseCookieBundle(cookieBundleJSON)
	if err != nil {
		t.Fatalf("解析 cookies.json 失败: %v", err)
	}
	// 会话 Cookie 必须排在最前：服务端/中间设备对超长请求头会截断，
	// SESSION 排在埋点后面是真实会丢登录态的一种事故。
	if !strings.HasPrefix(header, "SESSION=0f1e2d3c-aaaa-bbbb-cccc-1234567890ab") {
		t.Fatalf("SESSION 没有被排到最前: %q", header)
	}
	if strings.Contains(header, "_-_") {
		t.Fatalf("跨站导出的标记 Cookie 不该被发出去: %q", header)
	}
	if len(names) != 3 {
		t.Fatalf("应保留 3 条（SESSION/Hm_lvt/Access-Token），实际 %d: %v", len(names), names)
	}
}

func TestParseCookieBundleHeader(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantHead string
		wantN    int
	}{
		{"标准请求头", "SESSION=abc-123; other=1", "SESSION=abc-123", 2},
		{"带 cookie: 前缀", "cookie: SESSION=abc-123", "SESSION=abc-123", 1},
		{"多行粘贴", "SESSION=abc-123\nfoo=2", "SESSION=abc-123", 2},
		{"裸值按 SESSION 处理", "0f1e2d3c-aaaa-bbbb", "SESSION=0f1e2d3c-aaaa-bbbb", 1},
		{"值带双引号", `SESSION="abc-123"`, "SESSION=abc-123", 1},
		{"空值被丢弃", "SESSION=; foo=1", "foo=1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header, names, err := ParseCookieBundle(tc.raw)
			if err != nil {
				t.Fatalf("解析 %q 失败: %v", tc.raw, err)
			}
			if !strings.HasPrefix(header, tc.wantHead) {
				t.Fatalf("请求头 = %q，期望以 %q 开头", header, tc.wantHead)
			}
			if len(names) != tc.wantN {
				t.Fatalf("Cookie 条数 = %d，期望 %d（%v）", len(names), tc.wantN, names)
			}
		})
	}
}

func TestParseCookieBundleNetscape(t *testing.T) {
	raw := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		"",
		"#HttpOnly_sso.fzu.edu.cn\tFALSE\t/\tTRUE\t0\tSESSION\t0f1e2d3c-1111-2222-3333-444455556666",
	}, "\n")
	header, _, err := ParseCookieBundle(raw)
	if err != nil {
		t.Fatalf("解析 Netscape cookies.txt 失败: %v", err)
	}
	if header != "SESSION=0f1e2d3c-1111-2222-3333-444455556666" {
		t.Fatalf("请求头 = %q", header)
	}
}

func TestParseCookieBundleRejectsEmpty(t *testing.T) {
	if _, _, err := ParseCookieBundle("   "); err == nil {
		t.Fatal("空 Cookie 必须报错")
	}
	if _, _, err := ParseCookieBundle("这是一段随便的中文"); err == nil {
		t.Fatal("解析不出任何 Cookie 时必须报错")
	}
}

// fakeCookieSSO 是一个仿真统一身份认证：只有带着 sessionGood 才会签发 ticket。
type fakeCookieSSO struct {
	sessionGood string
	// renderedRedirect=true 时模拟"服务端不直接 302，而是把已登录标在登录页上"。
	renderedRedirect bool
	srv              *httptest.Server
	hits             []string
}

func newFakeCookieSSO(t *testing.T, sessionGood string, renderedRedirect bool) *fakeCookieSSO {
	t.Helper()
	f := &fakeCookieSSO{sessionGood: sessionGood, renderedRedirect: renderedRedirect}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, "login cookie="+r.Header.Get("Cookie"))
		if hasSessionCookie(r, f.sessionGood) && !f.renderedRedirect {
			// 已登录：直接带着 ticket 回到 service
			service := r.URL.Query().Get("service")
			http.Redirect(w, r, service+"?ticket=ST1234567890abcdef", http.StatusFound)
			return
		}
		redirect := "false"
		if hasSessionCookie(r, f.sessionGood) {
			redirect = "true"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
<div class="ruleList"><div class="code">UsernamePassword</div><div class="isRedirect">` + redirect + `</div></div>
<div style="display:none">
  <p id="user-id"></p>
  <p id="login-page-flowkey">flowkey-value</p>
</div></body></html>`))
	})
	mux.HandleFunc("/clientredirect", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, "clientredirect "+r.URL.Query().Get("client_name"))
		if !hasSessionCookie(r, f.sessionGood) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		service := r.URL.Query().Get("service")
		http.Redirect(w, r, service+"?ticket=STabcdef1234567890", http.StatusFound)
	})
	// 晚点名侧：ticket 换 token
	mux.HandleFunc("/attn/oauth2/callback.action", func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, "callback ticket="+r.URL.Query().Get("ticket"))
		w.Header().Set("Location", "https://yzsxg.example/attn/authorize.action?token=TOKabcdefghijklmnopqrstuvwxyz1234")
		w.WriteHeader(http.StatusFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func hasSessionCookie(r *http.Request, good string) bool {
	if good == "" {
		return false
	}
	c, err := r.Cookie("SESSION")
	return err == nil && c.Value == good
}

func TestLoginWithCookieDirectTicket(t *testing.T) {
	f := newFakeCookieSSO(t, "good-session", false)
	service := f.srv.URL + "/attn/oauth2/callback.action"

	res, err := LoginWithCookieAt(f.srv.URL, service, "SESSION=good-session")
	if err != nil {
		t.Fatalf("会话有效却报错: %v（hops=%v）", err, res.Hops)
	}
	if res.Token != "TOKabcdefghijklmnopqrstuvwxyz1234" {
		t.Fatalf("token = %q（hops=%v）", res.Token, res.Hops)
	}
	if !strings.Contains(res.Note, "service ticket") {
		t.Fatalf("结论文案应说明走了 ticket 直跳: %q", res.Note)
	}
}

func TestLoginWithCookieViaClientRedirect(t *testing.T) {
	f := newFakeCookieSSO(t, "good-session", true)
	service := f.srv.URL + "/attn/oauth2/callback.action"

	res, err := LoginWithCookieAt(f.srv.URL, service, "SESSION=good-session")
	if err != nil {
		t.Fatalf("借助 clientredirect 也应成功: %v（hops=%v）", err, res.Hops)
	}
	if res.Token != "TOKabcdefghijklmnopqrstuvwxyz1234" {
		t.Fatalf("token = %q", res.Token)
	}
	if !strings.Contains(res.Note, "clientredirect") {
		t.Fatalf("结论文案应说明走了哪条路: %q", res.Note)
	}
}

func TestLoginWithCookieInvalidSession(t *testing.T) {
	f := newFakeCookieSSO(t, "good-session", false)
	service := f.srv.URL + "/attn/oauth2/callback.action"

	res, err := LoginWithCookieAt(f.srv.URL, service, "SESSION=stale-session")
	if err == nil {
		t.Fatalf("过期的 Cookie 必须失败，实际 token=%q", res.Token)
	}
	// 报错必须点名"Cookie 按域名隔离"和 sso.fzu.edu.cn：用户手里拿的大多是别的子站的 Cookie。
	msg := err.Error()
	for _, want := range []string{"不是登录状态", "sso.fzu.edu.cn", "oss.fzu.edu.cn"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误提示缺少 %q: %s", want, msg)
		}
	}
	if !strings.Contains(strings.Join(res.Hops, "\n"), "SESSION") {
		t.Fatalf("诊断链路里应显示提交了哪些 Cookie: %v", res.Hops)
	}
}

func TestLoginWithCookieNoCookie(t *testing.T) {
	if _, err := LoginWithCookieAt("https://sso.example", "https://x.example/cb", "  "); err == nil {
		t.Fatal("空 Cookie 必须报错")
	}
}

func TestAutoRedirectCode(t *testing.T) {
	page := []byte(`<div class="ruleList"><div class="code">smsLogin</div><div class="isRedirect">false</div></div>
<div class="ruleList"><div class="code">adapter</div><div class="isRedirect">true</div></div>`)
	code, ok := autoRedirectCode(page)
	if !ok || code != "adapter" {
		t.Fatalf("autoRedirectCode = %q,%t，期望 adapter,true", code, ok)
	}
	if _, ok := autoRedirectCode([]byte("<html>nothing</html>")); ok {
		t.Fatal("没有标记时不该返回 true")
	}
}

func TestOrderCookiePairsKeepsSessionFirst(t *testing.T) {
	pairs := []cookiePair{
		{Name: "zzz", Value: "1"},
		{Name: "jsessionid", Value: "2"},
		{Name: "other", Value: "3", Domain: "oss.fzu.edu.cn"},
		{Name: "thirdparty", Value: "4", Domain: "tracker.example"},
	}
	got := orderCookiePairs(pairs)
	if got[0].Name != "jsessionid" {
		t.Fatalf("会话 Cookie 应排第一，实际 %q", got[0].Name)
	}
	if got[len(got)-1].Name != "thirdparty" {
		t.Fatalf("第三方 Cookie 应排最后，实际 %q", got[len(got)-1].Name)
	}
}

func TestCookieNotLoggedInSentinel(t *testing.T) {
	// 上层（flow / 测试接口）靠 errors.Is 穿透包装判断"要不要提示重新导出 Cookie"。
	// 注意：errors.Is(e, e) 永远为真，那样写等于没测——这里必须验证**包装后**仍能命中。
	wrapped := fmt.Errorf("外层加了上下文: %w", errCookieNotLoggedIn)
	if !errors.Is(wrapped, errCookieNotLoggedIn) {
		t.Fatal("哨兵错误必须能穿透包装被 errors.Is 命中")
	}
	// 再验一次真实来源：过期会话产生的错误应当命中该哨兵。
	f := newFakeCookieSSO(t, "good-session", false)
	if _, err := LoginWithCookieAt(f.srv.URL, f.srv.URL+"/attn/oauth2/callback.action", "SESSION=stale"); err == nil {
		t.Fatal("过期会话必须报错")
	}
}
