package fzu

import (
	"crypto/aes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fzucheckin/internal/secure"
)

// ssoCapture 是假 SSO 观测到的请求快照。
type ssoCapture struct {
	Gets      int
	Posts     int
	Form      url.Values
	Body      string
	Cookie    string
	UserAgent string
	Service   string
}

// fakeSSO 起一个假统一身份认证：GET 给登录页，POST 走 302 链把 token 交出来。
type fakeSSO struct {
	srv *httptest.Server

	croypto   string
	flowKey   string
	errorHTML string // POST 返回的错误页（非空则不重定向）

	got capture[ssoCapture]
}

func newFakeSSO(t *testing.T) *fakeSSO {
	t.Helper()
	f := &fakeSSO{
		// croypto 是页面下发的一次性 16 字节 AES 密钥（Base64）。
		croypto: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
		flowKey: "e1s1-flow-key",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", f.handleLogin)
	mux.HandleFunc("/callback.action", func(w http.ResponseWriter, r *http.Request) {
		// CAS 第二跳：拿 ticket 换 token
		http.Redirect(w, r, "/authorize.action?token=Tok1234567890abcdefghijXYZ", http.StatusFound)
	})
	mux.HandleFunc("/authorize.action", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>signed in</html>")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSSO) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f.got.update(func(c *ssoCapture) {
			c.Gets++
			c.Service = r.URL.Query().Get("service")
		})
		// 登录页必须先种下会话 Cookie，否则第二步会被当成新会话。
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "sess-1", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><body>
<p class="form-item">
  <input id="username" name="username">
</p>
<p id="login-croypto">`+f.croypto+`</p>
<p id="login-page-flowkey">`+f.flowKey+`</p>
</body></html>`)
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.got.update(func(c *ssoCapture) {
			c.Posts++
			c.Body = string(body)
			c.Form, _ = url.ParseQuery(c.Body)
			c.UserAgent = r.Header.Get("User-Agent")
			if cookie, err := r.Cookie("JSESSIONID"); err == nil {
				c.Cookie = cookie.Value
			}
		})
		if f.errorHTML != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, f.errorHTML)
			return
		}
		http.Redirect(w, r, "/callback.action?ticket=ST-abc", http.StatusFound)
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}

func TestLoginFollowsRedirectChainAndBuildsPassword(t *testing.T) {
	f := newFakeSSO(t)
	token, err := LoginAt(f.srv.URL, "https://yzsxg.test/oauth2/callback.action",
		"20210001", "s3cret-pass")
	if err != nil {
		t.Fatalf("LoginAt 返回错误: %v", err)
	}
	if token != "Tok1234567890abcdefghijXYZ" {
		t.Fatalf("token = %q", token)
	}
	got := f.got.get()
	if got.Gets != 1 || got.Posts != 1 {
		t.Errorf("应各发 1 次 GET/POST，实际 GET=%d POST=%d", got.Gets, got.Posts)
	}
	if got.Cookie != "sess-1" {
		t.Errorf("POST 没有带上 GET 种下的 Cookie（缺 cookie jar）：%q", got.Cookie)
	}
	if !strings.Contains(got.UserAgent, "Chrome") {
		t.Errorf("UA 不像浏览器: %q", got.UserAgent)
	}
	want := map[string]string{
		"type":        "UsernamePassword",
		"_eventId":    "submit",
		"geolocation": "",
		"execution":   f.flowKey,
		"croypto":     f.croypto,
		"username":    "20210001",
	}
	for key, wantVal := range want {
		if got := got.Form.Get(key); got != wantVal {
			t.Errorf("表单 %s = %q，期望 %q", key, got, wantVal)
		}
	}
	// 字段顺序按抓包来（对端是否在意未知，但不该和真机不一样）
	if !strings.HasPrefix(got.Body, "type=UsernamePassword&_eventId=submit&geolocation=&execution=") {
		t.Errorf("表单字段顺序不对: %s", got.Body[:minInt(len(got.Body), 120)])
	}
	// password 必须是 AES-ECB(croypto) 的可解密密文
	keyBytes, _ := base64.StdEncoding.DecodeString(f.croypto)
	raw, err := base64.StdEncoding.DecodeString(got.Form.Get("password"))
	if err != nil {
		t.Fatalf("password 不是合法 Base64: %v", err)
	}
	if plain := decryptECB(t, keyBytes, raw); plain != "s3cret-pass" {
		t.Errorf("password 解出来是 %q，期望明文密码", plain)
	}
}

func TestLoginUsesServiceParameter(t *testing.T) {
	gotService := capture[string]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/login" {
			gotService.set(r.URL.Query().Get("service"))
			_, _ = io.WriteString(w, `<p id="login-croypto">MDEyMzQ1Njc4OWFiY2RlZg==</p>
<p id="login-page-flowkey">k</p>`)
			return
		}
		http.Redirect(w, r, "/x?token=Tok1234567890abcdefghijXYZ", http.StatusFound)
	}))
	defer srv.Close()

	// ssoBase 只给站点根（不带 /login）也要能用。
	service := "https://yzsxg.test/cb.action"
	if _, err := LoginAt(srv.URL, service, "u", "p"); err != nil {
		t.Fatalf("LoginAt 返回错误: %v", err)
	}
	if got := gotService.get(); got != service {
		t.Errorf("service 参数 = %q，期望 %q", got, service)
	}
}

func TestLoginMissingPageElement(t *testing.T) {
	cases := []struct{ name, page string }{
		{"缺 croypto", `<p id="login-page-flowkey">k</p>`},
		{"缺 flowkey", `<p id="login-croypto">MDEyMzQ1Njc4OWFiY2RlZg==</p>`},
		{"页面上什么都没有", `<html>maintenance</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.page)
			}))
			defer srv.Close()
			_, err := LoginAt(srv.URL+"/login", "svc", "u", "p")
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), "登录页结构可能已变化") {
				t.Errorf("错误信息应点明登录页结构变化: %v", err)
			}
			if !strings.Contains(err.Error(), "login-") {
				t.Errorf("错误信息应点名缺了哪个元素: %v", err)
			}
		})
	}
}

func TestLoginEmptyPageElement(t *testing.T) {
	// 元素在但值为空，同样属于"页面结构变了"，不能拿空 key 去加密。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<p id="login-croypto">   </p><p id="login-page-flowkey">k</p>`)
	}))
	defer srv.Close()
	if _, err := LoginAt(srv.URL+"/login", "svc", "u", "p"); err == nil {
		t.Fatal("croypto 为空必须报错")
	}
}

func TestLoginBadCroyptoBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<p id="login-croypto">***not base64***</p>
<p id="login-page-flowkey">k</p>`)
	}))
	defer srv.Close()
	_, err := LoginAt(srv.URL+"/login", "svc", "u", "p")
	if err == nil {
		t.Fatal("croypto 不是 Base64 必须报错")
	}
	if !strings.Contains(err.Error(), "croypto") {
		t.Errorf("错误应点名 croypto: %v", err)
	}
}

func TestLoginReportsUpstreamErrorMessage(t *testing.T) {
	f := newFakeSSO(t)
	f.errorHTML = `<html><div class="alert"><span id="login-error-msg" class="text">需要输入验证码</span></div></html>`
	_, err := LoginAt(f.srv.URL, "svc", "u", "p")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "需要输入验证码") {
		t.Errorf("应带上对端给的原因: %v", err)
	}
}

func TestLoginWithoutErrorHint(t *testing.T) {
	f := newFakeSSO(t)
	f.errorHTML = `<html><body>forbidden</body></html>`
	_, err := LoginAt(f.srv.URL, "svc", "u", "p")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "可能需要验证码") {
		t.Errorf("没有 error-msg 时应提示验证码/页面变化: %v", err)
	}
}

func TestLoginTooManyRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/login":
			_, _ = io.WriteString(w, `<p id="login-croypto">MDEyMzQ1Njc4OWFiY2RlZg==</p>
<p id="login-page-flowkey">k</p>`)
		default:
			// 永远重定向：模拟"token 一直不出现"的环路。
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer srv.Close()

	_, err := LoginAt(srv.URL+"/login", "svc", "u", "p")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "重定向超过") {
		t.Errorf("应明确指出重定向跳数上限: %v", err)
	}
}

func TestLoginHTTPErrorOnPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := LoginAt(srv.URL+"/login", "svc", "u", "p")
	if err == nil {
		t.Fatal("登录页 503 必须报错")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("错误应带状态码: %v", err)
	}
}

func TestLoginRejectsMissingCredentials(t *testing.T) {
	if _, err := Login("", "p"); err == nil {
		t.Error("空用户名应报错")
	}
	if _, err := Login("u", ""); err == nil {
		t.Error("空密码应报错")
	}
}

func TestNormalizeLoginURL(t *testing.T) {
	cases := map[string]string{
		"":                        SSOBase,
		"https://sso.test":        "https://sso.test/login",
		"https://sso.test/":       "https://sso.test/login",
		"https://sso.test/login":  "https://sso.test/login",
		"https://sso.test/login/": "https://sso.test/login",
		"https://sso.test/a/b":    "https://sso.test/a/b",
		// 调用方带了旧 query 也要清掉，否则会拼出两个 '?'
		"https://sso.test/login?service=old": "https://sso.test/login",
	}
	for input, want := range cases {
		got, err := normalizeLoginURL(input)
		if err != nil {
			t.Fatalf("normalizeLoginURL(%q) 报错: %v", input, err)
		}
		if got != want {
			t.Errorf("normalizeLoginURL(%q) = %q，期望 %q", input, got, want)
		}
	}
	if _, err := normalizeLoginURL("not-a-url"); err == nil {
		t.Error("非法地址应报错")
	}
}

func TestTokenFromURL(t *testing.T) {
	cases := map[string]string{
		"https://x/authorize.action?token=Tok1234567890abcdefghijXYZ":     "Tok1234567890abcdefghijXYZ",
		"https://x/authorize.action?a=1&token=Tok1234567890abcdefghijXYZ": "Tok1234567890abcdefghijXYZ",
		"https://x/authorize.action?token=short":                          "", // 太短，多半是无关参数
		"https://x/authorize.action?other=1":                              "",
		"":                                                                "",
	}
	for input, want := range cases {
		if got := tokenFromURL(input); got != want {
			t.Errorf("tokenFromURL(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// decryptECB 用测试自己的一把钥匙解出 password 字段，与 secure.EncryptECBBlock 对拍。
func decryptECB(t *testing.T, key, cipherText []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	if len(cipherText)%block.BlockSize() != 0 {
		t.Fatalf("密文长度 %d 不是块大小整数倍", len(cipherText))
	}
	out := make([]byte, len(cipherText))
	for i := 0; i < len(cipherText); i += block.BlockSize() {
		block.Decrypt(out[i:i+block.BlockSize()], cipherText[i:i+block.BlockSize()])
	}
	plain, err := secure.PKCS7Unpad(out, block.BlockSize())
	if err != nil {
		t.Fatalf("PKCS7Unpad: %v", err)
	}
	return string(plain)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
