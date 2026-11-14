package server

// 独立验收用例（verify-agent）。
//
// 定位：既有测试已经覆盖了"实现做对了什么"，本文件只针对 docs/go-service-design.md
// 逐条**证伪**——尤其是"契约写了但既有用例没断言"的点、以及并发/时序上的角落。
// 全部离线（无真实上游）、无 20s 级等待、确定性断言，可反复 -count=5 跑。
//
// 命名统一 TestReview*，便于单独执行：
//   go test ./internal/server -run 'TestReview' -v
//
// 其中两条用例断言的是**契约要求而当前实现不满足**的行为，会在报告里标注
// 「此测试当前失败，对应缺陷 #N」——刻意保留失败，不为了绿灯放宽断言：
//   - TestReviewDryRunMustNotMarkDayChecked（缺陷 #1，P0）
//   - TestReviewConfigUpdateDuringRunIsNotLost（缺陷 #3，P1）
// 一条用例是"记录现状"的 characterization test（缺陷 #2，P1，XFF 限速旁路）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
	"fzucheckin/internal/secure"
	"fzucheckin/internal/store"
)

// ============ 本文件私有小工具 ============

// reviewErrCode 宽松地从错误信封里取 code（不是 JSON 时返回空串，便于断言"不是信封"）。
func reviewErrCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// reviewCookie 从响应的 Set-Cookie 里取出会话 Cookie 的值。
func reviewCookie(t *testing.T, header http.Header) string {
	t.Helper()
	for _, raw := range header.Values("Set-Cookie") {
		if !strings.HasPrefix(raw, sessionCookieName+"=") {
			continue
		}
		rest := strings.TrimPrefix(raw, sessionCookieName+"=")
		if i := strings.Index(rest, ";"); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	t.Fatalf("响应里没有 %s Cookie：%v", sessionCookieName, header.Values("Set-Cookie"))
	return ""
}

// reviewGetConfig 用一枚手工 Cookie 请求 GET /config（不经过 cookie jar，便于篡改）。
func reviewGetConfig(t *testing.T, env *testEnv, cookieValue string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.ts.URL+apiPrefix+"/config", nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	resp, err := env.client().Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, reviewErrCode(body)
}

// reviewFlip 把第 i 个字符换成另一个**合法**字符（base64url / 十六进制都认 A/B）。
func reviewFlip(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

// reviewRawRequest 发一个完全手工构造的请求（自定义 body/头），用于测试 env.do 表达不了的场景。
// client 必须显式给：带 Cookie 的用例要用登录过的客户端，否则 401 会掩盖真正要断言的 403/413。
func reviewRawRequest(t *testing.T, env *testEnv, c *http.Client, method, path, body string,
	headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, env.ts.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败：%v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, out, resp.Header.Clone()
}

// ============ 1. Session Cookie 篡改（§8） ============

func TestReviewSessionCookieTampering(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	_ = env.authedClient() // 先把密码设好

	login := env.client()
	res := env.do(login, http.MethodPost, apiPrefix+"/auth/login",
		map[string]string{"password": testPassword})
	env.expectStatus(res, http.StatusOK, "正确密码登录")
	valid := reviewCookie(t, res.header)
	if valid == "" {
		t.Fatalf("Cookie 值为空")
	}

	// 基线：未改动的 Cookie 必须放行（否则下面所有 401 都没有意义）。
	if status, code := reviewGetConfig(t, env, valid); status != http.StatusOK {
		t.Fatalf("原始 Cookie 应放行，实际 %d %s", status, code)
	}

	dot := strings.LastIndex(valid, ".")
	if dot <= 0 || dot == len(valid)-1 {
		t.Fatalf("Cookie 形态不是 payload.signature：%q", valid)
	}
	payload, sign := valid[:dot], valid[dot+1:]

	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("载荷不是 base64url：%v", err)
	}
	// 换一把密钥签名（攻击者自造 key）。
	otherSigner := secure.Signer{Key: []byte("attacker-controlled-key-material-0123456789")}

	// 签名长度随算法固定；越界保护。
	if len(sign) < 4 || len(payload) < 8 {
		t.Fatalf("Cookie 段太短：payload=%d sign=%d", len(payload), len(sign))
	}

	cases := []struct {
		name  string
		value string
	}{
		{"载荷改一位", reviewFlip(payload, len(payload)/2) + "." + sign},
		{"签名改一位", payload + "." + reviewFlip(sign, len(sign)/2)},
		{"载荷与签名都改", reviewFlip(payload, 3) + "." + reviewFlip(sign, 5)},
		{"换成别的 key 签名", payload + "." + otherSigner.Sign(raw)},
		{"sub 被改成 root（签名正确）", encodeSessionForTest(env.srv, []byte(`{"sub":"root","iat":1,"exp":9999999999}`))},
		{"对 base64 文本而非原文签名", encodeSessionForTest(env.srv, []byte(payload))},
		{"缺少点号", payload},
		{"点号在末尾", payload + "."},
		{"点号在开头", "." + sign},
		{"空值", ""},
		{"载荷不是 base64url", "!!!." + sign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := reviewGetConfig(t, env, tc.value)
			if status != http.StatusUnauthorized {
				t.Fatalf("%s：期望 401，实际 %d（code=%s）", tc.name, status, code)
			}
			if code != codeUnauthorized {
				t.Fatalf("%s：错误码期望 %s，实际 %q", tc.name, codeUnauthorized, code)
			}
		})
	}
}

// ============ 2. POST /auth/setup 的边界（§8） ============

func TestReviewSetupBodyEdges(t *testing.T) {
	env := newTestEnv(t)
	c := env.client()

	// 空 body 必须是 400，而不是 500（decodeJSON 把空体当"没带参数"）。
	res := env.do(c, http.MethodPost, apiPrefix+"/auth/setup", nil)
	env.expectStatus(res, http.StatusBadRequest, "空 body setup")
	if code := errCode(t, res.body); code != codeWeakPassword {
		t.Fatalf("空 body 的错误码期望 %s，实际 %s", codeWeakPassword, code)
	}
	if env.store.PasswordHash() != "" {
		t.Fatalf("被拒绝的 setup 不该写入密码哈希")
	}

	// {"password": null} 等价于空串。
	status, body, _ := reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/auth/setup",
		`{"password":null}`, map[string]string{csrfHeaderName: csrfHeaderValue})
	if status != http.StatusBadRequest {
		t.Fatalf("password=null 期望 400，实际 %d %s", status, truncateForLog(body))
	}

	// 不是 JSON → 400（不是 500）。
	status, body, _ = reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/auth/setup",
		`password=abc`, map[string]string{csrfHeaderName: csrfHeaderValue})
	if status != http.StatusBadRequest {
		t.Fatalf("非 JSON body 期望 400，实际 %d %s", status, truncateForLog(body))
	}

	// 7 个字符 → 400。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": "1234567"})
	env.expectStatus(res, http.StatusBadRequest, "7 字符密码")

	// 契约按"字符"计数（utf8.RuneCountInString）：8 个汉字只有 24 字节但确实是 8 个字符。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": "密码密码密码密码"})
	env.expectStatus(res, http.StatusOK, "8 个汉字密码")
	if env.store.PasswordHash() == "" {
		t.Fatalf("setup 成功后应写入密码哈希")
	}
	if reviewCookie(t, res.header) == "" {
		t.Fatalf("setup 成功应同时下发会话 Cookie")
	}

	// 再用 8 个 ASCII 字符 setup → 409 already_initialized（不能覆盖密码）。
	res = env.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": "abcdefgh"})
	env.expectStatus(res, http.StatusConflict, "重复 setup")
	if code := errCode(t, res.body); code != codeAlreadyInitialized {
		t.Fatalf("错误码期望 %s，实际 %s", codeAlreadyInitialized, code)
	}
	// 原密码仍然可用：用中文密码登录必须成功（证明没被覆盖）。
	res = env.do(env.client(), http.MethodPost, apiPrefix+"/auth/login",
		map[string]string{"password": "密码密码密码密码"})
	env.expectStatus(res, http.StatusOK, "用首次设置的密码登录")
}

// ============ 3. 登录限速的细节（§8） ============

func TestReviewLoginLimitRetryAfterAndReset(t *testing.T) {
	t.Run("RetryAfter是正数秒且第6次仍限速", func(t *testing.T) {
		env := newTestEnv(t)
		env.saveConfig(baseConfig())
		_ = env.authedClient()
		c := env.client()
		wrong := map[string]string{"password": "definitely-not-the-password"}

		for i := 1; i <= 4; i++ {
			res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
			if res.status != http.StatusUnauthorized {
				t.Fatalf("第 %d 次错密期望 401，实际 %d", i, res.status)
			}
			if ra := res.header.Get("Retry-After"); ra != "" {
				t.Fatalf("第 %d 次错密还没到上限，不该带 Retry-After：%q", i, ra)
			}
		}

		res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
		if res.status != http.StatusTooManyRequests {
			t.Fatalf("第 5 次错密期望 429，实际 %d", res.status)
		}
		raw := strings.TrimSpace(res.header.Get("Retry-After"))
		secs, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("Retry-After 必须是纯秒数（不是 HTTP-date、不是空）：%q", raw)
		}
		if secs <= 0 {
			t.Fatalf("Retry-After 必须 > 0，实际 %d", secs)
		}
		if secs > int(loginWindow/time.Second) {
			t.Fatalf("Retry-After(%d) 不该超过窗口长度 %s", secs, loginWindow)
		}
		// 连密码正确也一样 429：限速判定在验密之前。
		res = env.do(c, http.MethodPost, apiPrefix+"/auth/login",
			map[string]string{"password": testPassword})
		if res.status != http.StatusTooManyRequests {
			t.Fatalf("第 6 次（密码正确）仍应 429，实际 %d", res.status)
		}
		if code := errCode(t, res.body); code != codeTooManyAttempts {
			t.Fatalf("第 6 次错误码期望 %s，实际 %s", codeTooManyAttempts, code)
		}
		// 第 6 次的 Retry-After 仍然必须是正数秒（不能因为"已经限速了"就不给）。
		raw = strings.TrimSpace(res.header.Get("Retry-After"))
		secs6, err := strconv.Atoi(raw)
		if err != nil || secs6 <= 0 {
			t.Fatalf("第 6 次 429 的 Retry-After 必须是正数秒，实际 %q（err=%v）", raw, err)
		}
	})

	t.Run("成功登录后失败计数清零", func(t *testing.T) {
		env := newTestEnv(t)
		env.saveConfig(baseConfig())
		_ = env.authedClient()
		c := env.client()
		wrong := map[string]string{"password": "nope-nope-nope"}

		for i := 0; i < 4; i++ {
			if res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong); res.status != http.StatusUnauthorized {
				t.Fatalf("第 %d 次错密期望 401，实际 %d", i+1, res.status)
			}
		}
		// 4 次失败后成功一次 → 计数清零。
		res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": testPassword})
		env.expectStatus(res, http.StatusOK, "4 次失败后正确密码登录")

		// 清零后再错一次只能是 401（若没清零，就是第 5 次 → 429）。
		res = env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("成功登录后失败计数应清零（这次应 401），实际 %d %s", res.status, truncateForLog(res.body))
		}
		// 再攒满 5 次 → 第 5 次重新 429。
		var last httpResult
		for i := 0; i < 4; i++ {
			last = env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
		}
		if last.status != http.StatusTooManyRequests {
			t.Fatalf("清零后重新攒满 5 次失败应 429，实际 %d", last.status)
		}
	})
}

// 真实 RemoteAddr 的隔离（不依赖 X-Forwarded-For）：127.0.0.1 打满后，
// 从 127.0.0.2 发起的登录不受影响；反之计数也不该是进程级全局。
func TestReviewLoginLimitIsolatedByRemoteAddr(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	_ = env.authedClient()

	dialFrom := func(ip string) *http.Client {
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
				return d.DialContext(ctx, network, addr)
			},
		}
		return &http.Client{Transport: transport, Timeout: 10 * time.Second}
	}

	first := dialFrom("127.0.0.1")
	second := dialFrom("127.0.0.2")
	wrong := map[string]string{"password": "wrong-from-127-0-0-1"}

	for i := 0; i < 5; i++ {
		env.do(first, http.MethodPost, apiPrefix+"/auth/login", wrong)
	}
	if res := env.do(first, http.MethodPost, apiPrefix+"/auth/login", wrong); res.status != http.StatusTooManyRequests {
		t.Fatalf("127.0.0.1 打满 5 次后应 429，实际 %d", res.status)
	}

	// 另一个真实来源必须不受影响（Limit 是按 IP 的，不是全局的）。
	res := env.do(second, http.MethodPost, apiPrefix+"/auth/login", wrong)
	if res.status != http.StatusUnauthorized {
		t.Fatalf("127.0.0.2 不该被 127.0.0.1 的失败计数牵连，实际 %d %s",
			res.status, truncateForLog(res.body))
	}
	// 而 127.0.0.1 仍然被限速。
	res = env.do(first, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": testPassword})
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("127.0.0.1 仍应被限速，实际 %d", res.status)
	}
}

// 缺陷 #2（P1）：限速身份直接取 X-Forwarded-For 第一段，且没有任何"仅信任可信代理"的开关
// 或来源校验。任何一个能直连 127.0.0.1:8787 的进程（同机其它用户、被诱导访问的反代、
// 容器内邻居）都能靠每次换一个伪造 XFF 值把 429 彻底旁路，15 分钟窗口形同不存在。
//
// 本用例断言的是**当前（不安全）行为**，属于 characterization test：
// 修复后（改成"只在配置声明可信代理时才认 XFF"）本用例会变红，届时应改写为断言 429。
func TestReviewXFFDoesNotBypassLoginLimit(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	_ = env.authedClient()

	// 用同一个 TCP 来源（127.0.0.1，不带 XFF）打满 5 次失败。
	c := env.client()
	wrong := map[string]string{"password": "same-source-wrong"}
	for i := 0; i < 5; i++ {
		res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong)
		if res.status != http.StatusUnauthorized && res.status != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次错密期望 401/429，实际 %d", i+1, res.status)
		}
	}
	if res := env.do(c, http.MethodPost, apiPrefix+"/auth/login", wrong); res.status != http.StatusTooManyRequests {
		t.Fatalf("打满 5 次后应 429，实际 %d", res.status)
	}

	// 同一个 TCP 来源，只加一个伪造 XFF → 当前实现把它当成新身份，直接放行。
	status, body, header := reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/auth/login",
		`{"password":"`+testPassword+`"}`,
		map[string]string{
			csrfHeaderName:      csrfHeaderValue,
			"X-Forwarded-For":   "1.2.3.4",
			"X-Forwarded-Proto": "https",
		})
	// 缺陷 #2 已修复：限速身份默认取 TCP 来源（RemoteAddr），X-Forwarded-For 只在
	// FZU_TRUST_PROXY=1 时才被采信。因此伪造 XFF 依然应当 429。
	if status != http.StatusTooManyRequests {
		t.Fatalf("伪造 X-Forwarded-For 后仍应 429（默认不采信 XFF），实际 %d %s", status, truncateForLog(body))
	}
	if header.Get("Retry-After") == "" {
		t.Fatalf("429 应带 Retry-After")
	}
	t.Logf("缺陷 #2 已修复：同一来源伪造 XFF 仍被限速（Retry-After=%q）", header.Get("Retry-After"))
}

// ============ 4. CSRF 的作用域与顺序（§8） ============

func TestReviewCSRFScopeAndOrdering(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	// 契约只要求"所有非 GET 请求"带 CSRF 头：GET /logs/stream 不需要
	// （openStream 不发 X-Requested-With，能收到快照帧即证明）。
	stream := env.openStream(c)
	if frame := stream.next(t, 3*time.Second); frame.name != eventLog {
		t.Fatalf("不带 CSRF 头的 GET /logs/stream 应正常返回快照帧，实际 %q", frame.name)
	}
	stream.close()

	// 头的值必须逐字匹配（大小写不敏感地放过等于没有校验）。
	// 注意：HTTP 头值首尾空白会被 text/proto 规范化掉，所以"fzu-web "与"fzu-web"等价，
	// 这里只断言真正不同的值被拒。
	for _, bad := range []string{"FZU-WEB", "Fzu-Web", "fzu_web", "fzu-web extra", "1"} {
		status, body, _ := reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/auth/logout", "",
			map[string]string{csrfHeaderName: bad})
		if status != http.StatusForbidden {
			t.Fatalf("X-Requested-With=%q 期望 403，实际 %d %s", bad, status, truncateForLog(body))
		}
		if code := reviewErrCode(body); code != codeCSRF {
			t.Fatalf("X-Requested-With=%q 的错误码期望 %s，实际 %q", bad, codeCSRF, code)
		}
	}

	// CSRF 判定在路由/鉴权之前：一个只注册了 GET 的接口被 POST 时，
	// 缺头 = 403（而不是 404/405），这保证跨站请求连"探路"都做不到。
	status, body, _ := reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/status", "", nil)
	if status != http.StatusForbidden {
		t.Fatalf("POST 只读接口缺 CSRF 头期望 403（CSRF 在路由之前），实际 %d %s", status, truncateForLog(body))
	}

	// 更严格的顺序断言：**未登录**且缺 CSRF 头时必须是 403 而不是 401，
	// 否则跨站请求就能通过 401/403 的差别判断"这个服务有没有被登录过"。
	status, body, _ = reviewRawRequest(t, env, env.client(), http.MethodPost, apiPrefix+"/config", "", nil)
	if status != http.StatusForbidden {
		t.Fatalf("未登录且缺 CSRF 头期望 403（先判 CSRF 再判登录），实际 %d %s", status, truncateForLog(body))
	}

	// 带齐头的同类请求：路由层拒绝。契约未规定 404 还是 405，这里只断言"不是 2xx/5xx"。
	status, body, header := reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/status", "",
		map[string]string{csrfHeaderName: csrfHeaderValue})
	if status < 400 || status >= 500 {
		t.Fatalf("方法不匹配的 API 请求应回 4xx，实际 %d %s", status, truncateForLog(body))
	}
	t.Logf("现状（契约未定）：POST /api/v1/status → %d（Allow=%q，code=%q）",
		status, header.Get("Allow"), reviewErrCode(body))

	// POST /logs/stream（只注册了 GET）同样被拒。
	status, body, _ = reviewRawRequest(t, env, c, http.MethodPost, apiPrefix+"/logs/stream", "",
		map[string]string{csrfHeaderName: csrfHeaderValue})
	if status < 400 || status >= 500 {
		t.Fatalf("POST /logs/stream 应回 4xx，实际 %d %s", status, truncateForLog(body))
	}
}

// ============ 5. /health 的最小暴露面 + 静态资源安全头（§8 §9） ============

func TestReviewHealthExposureAndHeaders(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(configWithSecrets())

	res := env.do(env.client(), http.MethodGet, apiPrefix+"/health", nil)
	env.expectStatus(res, http.StatusOK, "GET /health")

	// 字段白名单：多一个字段就是多一分泄漏面。sse_* 两个是代码里新增的诊断量（已注释）。
	allowed := map[string]bool{
		"status": true, "version": true, "uptime_s": true, "goroutines": true,
		"heap_mb": true, "sse_clients": true, "sse_dropped": true,
	}
	for key := range jsonMap(t, res.body) {
		if !allowed[key] {
			t.Fatalf("/health 出现了契约外的字段 %q：%s", key, truncateForLog(res.body))
		}
	}

	text := string(res.body)
	for _, secret := range []string{
		testSchoolPwd, "token-abcdefghijklmnopqrstuvwxyz", "SCT123456secretkey",
		testUser, env.dir, config.MaskSentinel,
	} {
		if secret == "" {
			continue
		}
		if strings.Contains(text, secret) {
			t.Fatalf("/health 泄漏了 %q：%s", secret, truncateForLog(res.body))
		}
	}

	// 基础安全头覆盖所有响应（含静态资源）。
	for _, path := range []string{apiPrefix + "/health", "/", "/app.js"} {
		got := env.do(env.client(), http.MethodGet, path, nil)
		env.expectStatus(got, http.StatusOK, "GET "+path)
		for key, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			value := got.header.Get(key)
			if strings.TrimSpace(value) == "" {
				t.Fatalf("%s 缺少安全头 %s", path, key)
			}
			if value != want {
				t.Fatalf("%s 的 %s 期望 %q，实际 %q", path, key, want, value)
			}
		}
		// CSP 只加在自有页面上：API 若也带 connect-src 'self'，
		// 「前端放 nginx、后端只跑 API」的跨源部署会被自己的策略打死。
		csp := got.header.Get("Content-Security-Policy")
		if strings.HasPrefix(path, apiPrefix) {
			if csp != "" {
				t.Fatalf("%s 不该带 CSP，实际 %q", path, csp)
			}
			if cc := got.header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
				t.Fatalf("%s 应带 Cache-Control: no-store，实际 %q", path, cc)
			}
			continue
		}
		if strings.TrimSpace(csp) == "" {
			t.Fatalf("%s 缺少 Content-Security-Policy", path)
		}
	}

	// HEAD /health 必须与 GET 一样放行：不少负载均衡/监控探针默认用 HEAD，
	// 401 会被判成「服务挂了」。
	req, err := http.NewRequest(http.MethodHead, env.ts.URL+apiPrefix+"/health", nil)
	if err != nil {
		t.Fatalf("构造 HEAD 请求失败：%v", err)
	}
	resp, err := env.client().Do(req)
	if err != nil {
		t.Fatalf("HEAD /health 请求失败：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /api/v1/health 应放行（探针常用 HEAD），实际 %d", resp.StatusCode)
	}
}

// ============ 6. 凭据在所有出口都不出现明文（§8 脱敏） ============

// 本用例把"会承载配置/日志/历史"的每一个出口都过一遍：GET /config、PUT /config 响应、
// import-yaml 响应、export yaml/json、SSE 事件流、runs.jsonl 落盘文件。
func TestReviewSecretsNeverLeakOnAnySurface(t *testing.T) {
	env := newTestEnv(t)

	const (
		wecomHook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=WECOMSECRET123456"
		barkURL   = "https://api.day.app/BARKDEVICEKEY123"
		sctKey    = "SCTSECRETKEY123456"
		ppToken   = "PPTOKEN1234567890"
	)
	cfg := baseConfig() // 无 token → 第一次运行会走 SSO 并回写 token
	cfg.Notify.Type = "wecom"
	cfg.Notify.WecomWebhook = wecomHook
	cfg.Notify.ServerChanKey = sctKey
	cfg.Notify.PushPlusToken = ppToken
	cfg.Notify.BarkURL = barkURL
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	stream := env.openStream(c)
	defer stream.close()

	// collect 攒一段 SSE 文本（帧名 + data）。
	collect := func(d time.Duration) string {
		var sb strings.Builder
		deadline := time.After(d)
		for {
			select {
			case f := <-stream.frames:
				sb.WriteString("event: ")
				sb.WriteString(f.name)
				sb.WriteString("\ndata: ")
				sb.WriteString(f.data)
				sb.WriteString("\n\n")
			case <-deadline:
				return sb.String()
			}
		}
	}

	// 第一次运行：SSO 成功 → 回写 token（日志里应是 ss***（len=N））。
	collect(100 * time.Millisecond) // 快照帧
	firstID := env.acceptRun(c, map[string]any{"mode": "force"})
	if rec := env.waitRun(firstID, 5*time.Second); rec.Status != model.StatusSuccess {
		t.Fatalf("第一次运行期望 success，实际 %s", describe(rec))
	}
	ssoToken := env.store.Config().User.Token
	if ssoToken != env.ssoToken {
		t.Fatalf("SSO token 未回写，后续断言会失效：%q", ssoToken)
	}

	// 第二次运行：让 SSO 的**错误原文**里塞满凭据，检验 sanitizeError 的三个出口
	// （RunRecord.error / 日志 / SSE / runs.jsonl）。
	drop := env.store.Config()
	drop.User.Token = ""
	env.saveConfig(drop)
	env.setSSOErr(errors.New("SSO 401 password=" + testSchoolPwd +
		" hook=" + wecomHook + " bark=" + barkURL + " sct=" + sctKey + " pp=" + ppToken))
	secondID := env.acceptRun(c, map[string]any{"mode": "force"})
	if rec := env.waitRun(secondID, 5*time.Second); rec.Status != model.StatusError {
		t.Fatalf("第二次运行期望 error，实际 %s", describe(rec))
	}
	sseText := collect(300 * time.Millisecond)

	// 复位渠道，让后续 GET/PUT/import/export 的配置里带着全部凭据。
	env.setSSOErr(nil)
	restore := env.store.Config()
	restore.Notify.WecomWebhook = wecomHook
	restore.Notify.ServerChanKey = sctKey
	restore.Notify.PushPlusToken = ppToken
	restore.Notify.BarkURL = barkURL
	restore.User.Password = testSchoolPwd
	restore.User.Token = ssoToken
	env.saveConfig(restore)

	var artifacts []struct {
		name string
		body string
	}
	add := func(name string, body []byte) {
		artifacts = append(artifacts, struct {
			name string
			body string
		}{name, string(body)})
	}

	getRes := env.do(c, http.MethodGet, apiPrefix+"/config", nil)
	env.expectStatus(getRes, http.StatusOK, "GET /config")
	add("GET /config", getRes.body)

	var got struct {
		Config model.Config `json:"config"`
	}
	decodeBody(t, getRes.body, &got)
	putRes := env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": got.Config})
	env.expectStatus(putRes, http.StatusOK, "PUT /config（回传脱敏值）")
	add("PUT /config 响应", putRes.body)

	yamlText := strings.Join([]string{
		"user:",
		"  username: \"" + testUser + "\"",
		"  password: \"" + testSchoolPwd + "\"",
		"  token: \"" + ssoToken + "\"",
		"checkin:",
		"  longitude: \"119.20463\"",
		"  latitude: \"26.04920\"",
		"  schedule:",
		"    window_start: \"21:00\"",
		"    window_end: \"23:59\"",
		"    times: [\"21:35\"]",
		"notify:",
		"  type: \"wecom\"",
		"  serverchan_key: \"" + sctKey + "\"",
		"  pushplus_token: \"" + ppToken + "\"",
		"  bark_url: \"" + barkURL + "\"",
		"  wecom_webhook: \"" + wecomHook + "\"",
	}, "\n")
	impRes := env.do(c, http.MethodPost, apiPrefix+"/config/import-yaml", map[string]string{"yaml": yamlText})
	env.expectStatus(impRes, http.StatusOK, "import-yaml")
	add("import-yaml 响应", impRes.body)

	for _, format := range []string{"yaml", "json"} {
		res := env.do(c, http.MethodGet, apiPrefix+"/config/export?format="+format, nil)
		env.expectStatus(res, http.StatusOK, "export "+format)
		add("export "+format, res.body)
	}

	add("SSE 事件流", []byte(sseText))
	runsRaw, err := os.ReadFile(filepath.Join(env.dir, "runs.jsonl"))
	if err != nil {
		t.Fatalf("读取 runs.jsonl 失败：%v", err)
	}
	add("runs.jsonl", runsRaw)

	secrets := map[string]string{
		"学号密码":           testSchoolPwd,
		"SSO token":      ssoToken,
		"serverchan key": sctKey,
		"pushplus token": ppToken,
		"bark url":       barkURL,
		"wecom webhook":  wecomHook,
	}
	for _, art := range artifacts {
		if strings.TrimSpace(art.body) == "" {
			t.Fatalf("%s 抓到的内容是空的，断言会变成假绿", art.name)
		}
		for name, secret := range secrets {
			if strings.Contains(art.body, secret) {
				t.Fatalf("%s 里出现了明文%s %q", art.name, name, secret)
			}
		}
	}

	// 反向验证：脱敏不是"把字段删掉"，哨兵必须出现（前端靠它表达"已配置/保持原值"）。
	if !strings.Contains(string(getRes.body), config.MaskSentinel) {
		t.Fatalf("GET /config 未出现脱敏哨兵，脱敏可能被实现成「删除字段」")
	}
	if !strings.Contains(sseText, "***") {
		t.Fatalf("SSE 里连脱敏痕迹（***）都没有，脱敏可能根本没生效：%s", truncateForLog([]byte(sseText)))
	}
}

// ============ 7. PUT /config 的畸形请求体（§8 1MB / §9 400） ============

func TestReviewConfigPutMalformedBodies(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(configWithSecrets())
	c := env.authedClient()

	assertRejected := func(name string, status int, body []byte, before int64) {
		t.Helper()
		if status < 400 || status >= 500 {
			t.Fatalf("%s：期望 4xx，实际 %d %s", name, status, truncateForLog(body))
		}
		if got := env.store.ConfigVersion(); got != before {
			t.Fatalf("%s：被拒绝的请求不该推进 config_version（%d → %d）", name, before, got)
		}
		if env.store.Config().User.Password != testSchoolPwd {
			t.Fatalf("%s：被拒绝的请求改动了已存凭据", name)
		}
		t.Logf("%-14s → %d %s（配置未改动）", name, status, reviewErrCode(body))
	}

	before := env.store.ConfigVersion()

	// config 为 null。
	res := env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": nil})
	assertRejected("config=null", res.status, res.body, before)

	// 缺 config 字段。
	res = env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{})
	assertRejected("缺 config 字段", res.status, res.body, before)

	// 不是对象而是数组/字符串。
	status, body, _ := reviewRawRequest(t, env, c, http.MethodPut, apiPrefix+"/config", `{"config":[]}`,
		map[string]string{csrfHeaderName: csrfHeaderValue, "Content-Type": "application/json"})
	assertRejected("config=[]", status, body, before)

	// 超过 1MB：必须是 4xx（413 也算），绝不能 5xx 也不能把内存吃掉。
	huge := `{"config":{"user":{"username":"` + strings.Repeat("x", maxRequestBody+1024) + `"}}}`
	status, body, _ = reviewRawRequest(t, env, c, http.MethodPut, apiPrefix+"/config", huge,
		map[string]string{csrfHeaderName: csrfHeaderValue, "Content-Type": "application/json"})
	assertRejected("超大 body", status, body, before)
	if status != http.StatusRequestEntityTooLarge && status != http.StatusBadRequest {
		t.Logf("现状：超大 body 返回 %d（契约 §9 未列 413，属可接受扩展）", status)
	}

	// 未知字段：实现刻意"容忍未知字段（前向兼容）"。
	// 验收清单期望 400，但这是设计层面的取舍，因此这里只断言"不是 panic / 不是 5xx"，
	// 差异写进报告的「契约偏差」小节由 lead 裁决。
	rawCfg, err := json.Marshal(env.store.Config())
	if err != nil {
		t.Fatalf("序列化配置失败：%v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(rawCfg, &asMap); err != nil {
		t.Fatalf("反序列化配置失败：%v", err)
	}
	asMap["future_section"] = map[string]any{"unexpected": 1}
	status, body, _ = reviewRawRequest(t, env, c, http.MethodPut, apiPrefix+"/config",
		mustJSON(t, map[string]any{"config": asMap}),
		map[string]string{csrfHeaderName: csrfHeaderValue, "Content-Type": "application/json"})
	if status >= 500 {
		t.Fatalf("未知字段不该导致 5xx，实际 %d %s", status, truncateForLog(body))
	}
	if status == http.StatusOK {
		if got := env.store.ConfigVersion(); got != before+1 {
			t.Fatalf("接受未知字段时版本应只 +1（%d → %d）", before, got)
		}
	}
	t.Logf("现状（与验收清单不一致）：带未知字段的 PUT /config → %d", status)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	return string(raw)
}

// ============ 8. /status 的 JSON 形态（§9.1） ============

func TestReviewStatusRawJSONShape(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Checkin.Schedule.Times = []string{"21:35", "22:10"}
	env.saveConfig(cfg)
	env.setNow(fixedDay(23, 30)) // 两档都已过、窗口内、今日未签
	c := env.authedClient()

	res := env.do(c, http.MethodGet, apiPrefix+"/status", nil)
	env.expectStatus(res, http.StatusOK, "GET /status")
	raw := string(res.body)

	// 原始 JSON 里必须是 null 字面量，而不是 Go 零值时间 "0001-01-01T00:00:00Z"。
	if strings.Contains(raw, "0001-01-01") {
		t.Fatalf("/status 里出现了零值时间（前端会显示 0001 年）：%s", truncateForLog(res.body))
	}
	if !strings.Contains(raw, `"next_run_at":null`) {
		t.Fatalf("没有未来档时 next_run_at 必须是 null：%s", truncateForLog(res.body))
	}
	if !strings.Contains(raw, `"vacation":null`) {
		t.Fatalf("无假期时 vacation 必须是 null（不能是 {}）：%s", truncateForLog(res.body))
	}
	if !strings.Contains(raw, `"last_run":null`) {
		t.Fatalf("还没有运行记录时 last_run 必须是 null：%s", truncateForLog(res.body))
	}

	var out statusResponse
	decodeBody(t, res.body, &out)
	if len(out.Slots) != 2 {
		t.Fatalf("期望 2 档，实际 %d", len(out.Slots))
	}
	for _, slot := range out.Slots {
		if slot.State != slotStateMissed {
			t.Fatalf("两档都已过且未签到，state 应全为 missed，实际 %+v", out.Slots)
		}
	}

	// 今日已签 → 每一档都是 done（不是只有第一档）。
	env.srv.updateState(func(st *model.State) { st.LastSuccessDate = env.today() })
	res = env.do(c, http.MethodGet, apiPrefix+"/status", nil)
	decodeBody(t, res.body, &out)
	if !out.Day.CheckedIn {
		t.Fatalf("last_success_date=今天时 day.checked_in 应为 true")
	}
	for _, slot := range out.Slots {
		if slot.State != slotStateDone {
			t.Fatalf("今日已签时 slots[].state 必须全为 done，实际 %+v", out.Slots)
		}
	}
}

// ============ 9. force 在窗口外仍执行 / 预检不写状态 ============

func TestReviewForceRunsOutsideWindow(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(6, 0)) // 06:00，远在 21:00-23:59 窗口之外
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("force 在窗口外也必须真签，实际 %s", describe(rec))
	}
	if rec.Mode != "force" {
		t.Fatalf("mode 期望 force，实际 %q", rec.Mode)
	}
	initCalls, checkCalls, clockInCalls := env.school.counts()
	if initCalls == 0 {
		t.Fatalf("force 必须真的调用 init（窗口外也不能被拦）")
	}
	if checkCalls == 0 || clockInCalls != 1 {
		t.Fatalf("force 必须走完 check+clockIn，实际 check=%d clockIn=%d", checkCalls, clockInCalls)
	}
	// 窗口外执行必须留痕（只 WARN 不拦截）。
	sawWarn := false
	for _, ev := range env.store.Logs(0) {
		if ev.Level == "warn" && strings.Contains(ev.Msg, "窗口校验未通过") {
			sawWarn = true
			break
		}
	}
	if !sawWarn {
		t.Fatalf("窗口外执行应留下「窗口校验未通过」告警日志")
	}
}

// 预检（dry_run）允许发起的请求只有 init 与 check：不许 clockIn，也不许 currentTimestamp
// （后者会把"迟到判定"这一真实副作用带进预检）。
func TestReviewProbeMakesNoWriteCalls(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(6, 0)) // 窗口外也照样能预检
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "normal", "dry_run": true})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("预检命中范围期望 success，实际 %s", describe(rec))
	}
	if rec.Mode != "probe" {
		t.Fatalf("dry_run 的 mode 期望 probe，实际 %q", rec.Mode)
	}
	initCalls, checkCalls, clockInCalls := env.school.counts()
	env.school.mu.Lock()
	tsCalls := env.school.timestampCalls
	env.school.mu.Unlock()
	if clockInCalls != 0 {
		t.Fatalf("dry_run 绝不能 clockIn，实际 %d 次", clockInCalls)
	}
	if tsCalls != 0 {
		t.Fatalf("dry_run 不该请求 currentTimestamp（迟到判定是真实签到的副作用），实际 %d 次", tsCalls)
	}
	if initCalls != 1 || checkCalls == 0 {
		t.Fatalf("预检应恰好 init 一次并至少 check 一次，实际 init=%d check=%d", initCalls, checkCalls)
	}
}

// 缺陷 #1（P0）：dry_run 的预检通过后也被记成 status=success，而 finishRun 对
// "任何 success" 都做两件**真实签到才该有**的副作用：
//  1. 写 state.last_success_date=今天 → 当晚调度器 tick 看到 already_checked_today
//     直接跳过，真正的晚点名再也不会签（漏签）；
//  2. 走 §12 的推送（success 必推）→ 用户手机收到"✅ 晚点名签到成功"，但根本没签。
//
// 前端「定位预检」按钮发的正是 {mode:"normal",dry_run:true}（app.js:1490）。
//
// 本用例断言契约语义（§11.3 第 11 步、§12 只对真实结论生效），当前实现会失败。
func TestReviewDryRunMustNotMarkDayChecked(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Notify.Type = "wecom"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fake"
	env.saveConfig(cfg)
	env.setNow(fixedDay(15, 0))
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "normal", "dry_run": true})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("预检期望 success，实际 %s", describe(rec))
	}

	st := env.store.State()
	if st.LastSuccessDate == env.today() {
		t.Errorf("缺陷 #1（P0）：预检把 last_success_date 写成了今天（%q），"+
			"当晚调度器会据此判定 already_checked_today 并跳过真签到（漏签）。last_slot_key=%q",
			st.LastSuccessDate, st.LastSlotKey)
	}
	if titles := env.notifyTitles(); len(titles) != 0 {
		t.Errorf("缺陷 #1 连带表现：预检触发了推送 %v（用户会收到「签到成功」，但实际没有任何 clockIn）", titles)
	}

	// 连带后果：仪表盘会显示"今日已签到"。
	res := env.do(c, http.MethodGet, apiPrefix+"/status", nil)
	var out statusResponse
	decodeBody(t, res.body, &out)
	if out.Day.CheckedIn {
		t.Errorf("缺陷 #1 连带表现：预检后 /status.day.checked_in=true（用户会以为已经签过了）")
	}

	// 直接演示后果：把时钟推到当晚 23:30（窗口内、已过末档），启动补跑判定应当补签一次；
	// 因为预检已经写了 last_success_date，catchUp 在 scheduler.go:181 就直接 return 了。
	env.waitIdle(2 * time.Second)
	env.setNow(fixedDay(23, 30))
	beforeRuns, _ := env.store.Runs(0)
	env.srv.catchUp()
	time.Sleep(300 * time.Millisecond)
	afterRuns, _ := env.store.Runs(0)
	if len(afterRuns) == len(beforeRuns) {
		t.Errorf("缺陷 #1 后果：23:30（窗口内、已过末档）的补跑判定没有产生任何运行记录 —— "+
			"这一晚的晚点名被预检彻底顶掉（漏签）。运行记录数 %d → %d",
			len(beforeRuns), len(afterRuns))
	}
}

// 缺陷 #3（P1）：flow 在开始时读一份配置快照，SSO 换 token 后整份 SaveConfig 写回。
// 如果这中间用户通过 PUT /config 改了配置（或改密码），改动会被旧快照覆盖（lost update）。
func TestReviewConfigUpdateDuringRunIsNotLost(t *testing.T) {
	gate := newReviewGate()
	env := newTestEnv(t, withGatedSSO(gate, "sso-token-for-lost-update-0123456789"))
	env.saveConfig(baseConfig()) // 无 token → 必走 SSO
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	id := env.acceptRun(c, map[string]any{"mode": "force"})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待 SSO 进入超时")
	}

	// 运行卡在 SSO 时，用户保存一档新时间。
	desired := env.store.Config()
	desired.Checkin.Schedule.Times = []string{"22:10"}
	res := env.do(c, http.MethodPut, apiPrefix+"/config", map[string]any{"config": desired})
	env.expectStatus(res, http.StatusOK, "运行中 PUT /config")
	versionAfterPut := env.store.ConfigVersion()

	close(gate.release)
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}

	got := env.store.Config()
	if len(got.Checkin.Schedule.Times) != 1 || got.Checkin.Schedule.Times[0] != "22:10" {
		t.Fatalf("缺陷 #3（P1）：运行线程回写 token 时用旧快照整份覆盖，抹掉了用户刚保存的配置。"+
			"times 期望 [22:10]，实际 %v（保存后 version=%d，最终 version=%d，token=%q）",
			got.Checkin.Schedule.Times, versionAfterPut, env.store.ConfigVersion(), got.User.Token)
	}
	if got.User.Token != "sso-token-for-lost-update-0123456789" {
		t.Fatalf("token 应已回写，实际 %q", got.User.Token)
	}
}

// ============ 11. 假期：不碰上游 + 只在 vacation.notify 时推送（§11.3 §12） ============

func TestReviewVacationSilentUnlessEnabled(t *testing.T) {
	build := func(t *testing.T, vacationNotify bool) *testEnv {
		t.Helper()
		env := newTestEnv(t)
		cfg := baseConfig()
		cfg.Vacation.SkipRanges = []model.VacationRange{
			{Name: "校历假期", Start: "2026-11-01", End: "2026-11-30"},
		}
		cfg.Vacation.Notify = vacationNotify
		cfg.Notify.Type = "wecom"
		cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fake"
		env.saveConfig(cfg)
		env.setNow(fixedDay(21, 40))
		return env
	}

	t.Run("notify=false 时静默", func(t *testing.T) {
		env := build(t, false)
		c := env.authedClient()
		rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
		if rec.Status != model.StatusVacation {
			t.Fatalf("期望 vacation，实际 %s", describe(rec))
		}
		if titles := env.notifyTitles(); len(titles) != 0 {
			t.Fatalf("vacation.notify=false 时不该推送，实际 %d 条：%v", len(titles), titles)
		}
		initCalls, checkCalls, clockInCalls := env.school.counts()
		if initCalls+checkCalls+clockInCalls != 0 {
			t.Fatalf("假期内不该有任何上游请求，实际 init=%d check=%d clockIn=%d",
				initCalls, checkCalls, clockInCalls)
		}
	})

	t.Run("notify=true 时推一次", func(t *testing.T) {
		env := build(t, true)
		c := env.authedClient()
		rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
		if rec.Status != model.StatusVacation {
			t.Fatalf("期望 vacation，实际 %s", describe(rec))
		}
		if titles := env.notifyTitles(); len(titles) != 1 {
			t.Fatalf("vacation.notify=true 时应推 1 条，实际 %d 条：%v", len(titles), titles)
		}
		if n := env.ssoCallCount(); n != 0 {
			t.Fatalf("假期内不该换 token，实际 SSO %d 次", n)
		}
	})
}

// ============ 12. 幂等判定顺序：FState 优先于"无计划"（§11.3 第 4/5 步） ============

func TestReviewAlreadyCheckedBeatsNoPlan(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.Notify.Type = "wecom"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fake"
	env.saveConfig(cfg)
	env.setNow(fixedDay(21, 40))
	// 同时满足"学校端已签"与"schoolData 为空"：按 §11.3 第 4 步应先判 already_checked。
	env.school.setChecked(true)
	env.school.setNoPlan(true)

	c := env.authedClient()
	rec := env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	if rec.Status != model.StatusAlready {
		t.Fatalf("FState=SUCCESS 且 schoolData 为空时应先判 already_checked，实际 %s", describe(rec))
	}
	if _, _, clockInCalls := env.school.counts(); clockInCalls != 0 {
		t.Fatalf("已签到不该 clockIn，实际 %d 次", clockInCalls)
	}
	if titles := env.notifyTitles(); len(titles) != 0 {
		t.Fatalf("already_checked 必须静默，实际推送 %d 条：%v", len(titles), titles)
	}
}

// ============ 13. 通知去重：按天 + error 的 30 分钟窗口（§12） ============

func TestReviewNotifyDedupPerDayAndErrorWindow(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "preset-token-0123456789"
	cfg.Notify.Type = "serverchan"
	cfg.Notify.ServerChanKey = "SCTfakekey123"
	env.saveConfig(cfg)
	day1 := fixedDay(21, 40)
	env.setNow(day1)
	c := env.authedClient()

	runOnce := func(what string) model.RunRecord {
		t.Helper()
		return env.waitRun(env.acceptRun(c, map[string]any{"mode": "force"}), 5*time.Second)
	}

	if rec := runOnce("day1 #1"); rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}
	if n := len(env.notifyTitles()); n != 1 {
		t.Fatalf("首次 success 应推 1 条，实际 %d", n)
	}
	runOnce("day1 #2")
	if n := len(env.notifyTitles()); n != 1 {
		t.Fatalf("同一天同一状态只推一次，实际 %d", n)
	}

	// 跨天：去重键是日期，新的一天必须再推一次。
	env.setNow(day1.AddDate(0, 0, 1))
	if rec := runOnce("day2 #1"); rec.Status != model.StatusSuccess {
		t.Fatalf("第二天期望 success，实际 %s", describe(rec))
	}
	if n := len(env.notifyTitles()); n != 2 {
		t.Fatalf("跨天后应再推一次（共 2 条），实际 %d", n)
	}

	// error：允许重试时再推，但 30 分钟内不重复。
	env.school.mu.Lock()
	env.school.clockInFail = true
	env.school.mu.Unlock()
	if rec := runOnce("day2 error #1"); rec.Status != model.StatusError {
		t.Fatalf("期望 error，实际 %s", describe(rec))
	}
	if n := len(env.notifyTitles()); n != 3 {
		t.Fatalf("首次 error 应推 1 条（共 3），实际 %d", n)
	}
	runOnce("day2 error #2")
	if n := len(env.notifyTitles()); n != 3 {
		t.Fatalf("30 分钟内重复失败不该再推，实际 %d", n)
	}
	env.setNow(day1.AddDate(0, 0, 1).Add(31 * time.Minute))
	runOnce("day2 error #3")
	if n := len(env.notifyTitles()); n != 4 {
		t.Fatalf("超过 30 分钟窗口后 error 应再推一次（共 4），实际 %d", n)
	}
}

// ============ 14. 调度循环的 last_slot_key 闸（§10.3） ============

func TestReviewSchedulerLastSlotKeyGate(t *testing.T) {
	env := newTestEnv(t, withScheduler())
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 36)) // 21:35 已过 1 分钟，仍在 tolerance(5min) 内
	env.srv.updateState(func(st *model.State) {
		st.LastSlotKey = env.today() + "|21:35|21:35:00"
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.srv.runScheduler(ctx)
	}()
	waitSchedulerStarted(t, env)

	// 第一轮：决策是 run/on_target，但键命中 → 不许真的发起运行。
	env.srv.wakeScheduler("")
	waitValue(t, 3*time.Second, "调度器给出 on_target 决策", func() bool {
		return env.srv.lastDecisionValue() == "on_target"
	})
	time.Sleep(200 * time.Millisecond)
	if runs, _ := env.store.Runs(10); len(runs) != 0 {
		t.Fatalf("last_slot_key 命中时不该重复触发运行，实际 %d 条", len(runs))
	}

	// 清掉键 → 同一决策必须真的跑一次（顺带覆盖 tick 循环的 run 分支）。
	env.srv.updateState(func(st *model.State) { st.LastSlotKey = "" })
	env.srv.wakeScheduler("")
	waitValue(t, 5*time.Second, "调度器发起运行并落盘", func() bool {
		runs, _ := env.store.Runs(1)
		return len(runs) == 1 && !runs[0].FinishedAt.IsZero()
	})
	runs, err := env.store.Runs(1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("读取运行记录失败：%v %d", err, len(runs))
	}
	rec := runs[0]
	if rec.Mode != "scheduled" || rec.Decision != "on_target" {
		t.Fatalf("调度发起的运行应为 scheduled/on_target，实际 %s", describe(rec))
	}
	if rec.Slot != "21:35" || rec.Target != "21:35:00" {
		t.Fatalf("slot/target 不符：%q / %q", rec.Slot, rec.Target)
	}
	if rec.Status != model.StatusSuccess {
		t.Fatalf("期望 success，实际 %s", describe(rec))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("调度器退出超时")
	}
}

// ============ 15. SSE：慢消费者不许拖住广播（§9.2 §G） ============

// 三段确定性断言（不依赖调度时序，-cpu=1 下同样稳定）：
//  1. 慢消费者把缓冲区占满后，广播仍然是**非阻塞**的（200 帧耗时远小于 2s）；
//  2. 慢消费者的缓冲区恰好停在 sseClientBuffer，多出来的帧被丢弃并计入 /health 的 sse_dropped；
//  3. 在慢消费者"堵着"的状态下，另一个有空间的连接仍能立刻收到新事件。
func TestReviewSSESlowConsumerDoesNotBlock(t *testing.T) {
	env := newTestEnv(t)
	hub := env.srv.hub

	slow, ok := hub.subscribe()
	if !ok {
		t.Fatalf("第 1 个订阅失败")
	}
	fast, ok := hub.subscribe()
	if !ok {
		t.Fatalf("第 2 个订阅失败")
	}
	defer hub.unsubscribe(slow)
	defer hub.unsubscribe(fast)

	const burst = 200
	start := time.Now()
	for i := 0; i < burst; i++ {
		hub.broadcast(eventLog, model.LogEvent{Level: "info", Msg: "burst"})
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("广播被慢消费者阻塞：%d 次广播耗时 %s（广播路径上不允许任何阻塞）", burst, elapsed)
	}

	clients, dropped := hub.stats()
	if clients != 2 {
		t.Fatalf("当前连接数期望 2，实际 %d", clients)
	}
	if got := len(slow.events); got != sseClientBuffer {
		t.Fatalf("从未读取的连接缓冲区应恰好停在 %d，实际 %d", sseClientBuffer, got)
	}
	// 两个连接各丢 (burst - 缓冲区) 帧；这里只要求"确实丢了"且量级正确。
	if want := uint64(burst - sseClientBuffer); dropped < want {
		t.Fatalf("丢弃计数应 ≥ %d，实际 %d", want, dropped)
	}

	// 把"快"连接的积压排空（模拟浏览器读完了），慢连接保持堵塞。
	for {
		select {
		case <-fast.events:
			continue
		default:
		}
		break
	}

	// 此刻慢连接的队列仍是满的；再广播一条，必须立刻出现在有空间的连接里。
	hub.broadcast(eventLog, model.LogEvent{Level: "info", Msg: "SENTINEL-AFTER-BURST"})
	select {
	case ev := <-fast.events:
		if !strings.Contains(string(ev.data), "SENTINEL-AFTER-BURST") {
			t.Fatalf("收到的不是哨兵事件：%s", truncateForLog(ev.data))
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("慢消费者堵塞时，另一个连接收不到新事件（广播路径被饿死）")
	}
	if _, droppedAfter := hub.stats(); droppedAfter < dropped {
		t.Fatalf("丢弃计数不该回退：%d → %d", dropped, droppedAfter)
	}
}

// ============ 16. SSE 断开后 goroutine 回落（§14） ============

func TestReviewSSEGoroutinesReclaimed(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	// 预热：先开一条再关掉，让 transport/服务端的连接 goroutine 稳定下来。
	warm := env.openStream(c)
	warm.next(t, 3*time.Second)
	warm.close()
	env.waitHubClients(0, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	const n = 6
	streams := make([]*sseStream, 0, n)
	for i := 0; i < n; i++ {
		streams = append(streams, env.openStream(c))
	}
	env.waitHubClients(n, 5*time.Second)
	during := runtime.NumGoroutine()
	if during <= before {
		t.Fatalf("打开 %d 条 SSE 后 goroutine 没增加（before=%d during=%d），测不出回落", n, before, during)
	}

	for _, s := range streams {
		s.close()
	}
	env.waitHubClients(0, 5*time.Second)

	after := during
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("goroutine: before=%d during=%d after=%d", before, during, after)
	if after > before+3 {
		t.Fatalf("断开 %d 条 SSE 后 goroutine 未回落（before=%d during=%d after=%d），疑似泄漏",
			n, before, during, after)
	}
	if after >= during {
		t.Fatalf("断开后 goroutine 应明显减少（during=%d after=%d）", during, after)
	}
}

// ============ 18. config.json 损坏后重启仍能服务（§7 §15.7） ============

func TestReviewCorruptConfigStillServes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("首次 Open 失败：%v", err)
	}
	if _, err := st.SaveConfig(baseConfig(), nil); err != nil {
		t.Fatalf("写入测试配置失败：%v", err)
	}
	if err := st.SetPasswordHash("pbkdf2$sha256$120000$c2FsdA==$aGFzaA=="); err != nil {
		t.Fatalf("写入测试密码哈希失败：%v", err)
	}
	// 直接把配置文件写成半个 JSON 花括号（最典型的"进程被杀在写一半"现场）。
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("写坏配置失败：%v", err)
	}

	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatalf("损坏的 config.json 不该让 store.Open 报错：%v", err)
	}
	corrupt, err := filepath.Glob(filepath.Join(dir, "config.json.corrupt-*"))
	if err != nil {
		t.Fatalf("Glob 失败：%v", err)
	}
	if len(corrupt) != 1 {
		t.Fatalf("应留下一个 config.json.corrupt-* 备份，实际 %v", corrupt)
	}
	if got := reopened.Config().Checkin.Schedule.WindowStart; got != config.DefaultWindowStart {
		t.Fatalf("损坏后应回落到默认配置，实际 window_start=%q", got)
	}

	srv, err := New(Deps{
		Store:            reopened,
		Logger:           log.New(io.Discard, "", 0),
		DisableScheduler: true,
	})
	if err != nil {
		t.Fatalf("server.New 不该因损坏配置失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	check := func(path string, want int) []byte {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s 失败：%v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("GET %s 期望 %d，实际 %d：%s", path, want, resp.StatusCode, truncateForLog(body))
		}
		return body
	}

	health := check(apiPrefix+"/health", http.StatusOK)
	if !strings.Contains(string(health), `"status":"ok"`) {
		t.Fatalf("健康探针内容不符：%s", truncateForLog(health))
	}
	check("/", http.StatusOK) // 静态资源仍可用（能进初始化向导）

	// 配置坏了但 auth.json 还在 → 已是"已初始化"，setup 必须 409（不能悄悄重置密码）。
	req, err := http.NewRequest(http.MethodPost, ts.URL+apiPrefix+"/auth/setup",
		strings.NewReader(`{"password":"another-pass-1"}`))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, csrfHeaderValue)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("setup 请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("已初始化时 setup 期望 409（或未初始化 401 之外的状态都不该是 200），实际 %d：%s",
			resp.StatusCode, truncateForLog(body))
	}
}

// ============ 20/21. 静态资源与 CSP 的前端契约（§13 §G1） ============

// staticFromEnv 从内嵌文件系统里取静态资源的原始内容（用于体积/内联脚本检查）。
func staticFromEnv(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	res := env.do(env.client(), http.MethodGet, "/"+name, nil)
	env.expectStatus(res, http.StatusOK, "GET /"+name)
	return string(res.body)
}

func TestReviewFrontendAndCSPContract(t *testing.T) {
	env := newTestEnv(t)

	index := staticFromEnv(t, env, "index.html")
	appJS := staticFromEnv(t, env, "app.js")
	styleCSS := staticFromEnv(t, env, "style.css")

	if !strings.Contains(strings.ToLower(index), "<!doctype html>") {
		t.Fatalf("index.html 缺少 <!doctype html>")
	}

	// CSP 足够让现有前端工作：只有外链 module 脚本、外链样式，没有内联脚本/内联事件处理器。
	if err := checkNoInlineScript(index); err != nil {
		t.Fatalf("index.html 含内联脚本，script-src 'self' 会挡掉它：%v", err)
	}
	if !strings.Contains(index, `<script type="module" src="./app.js">`) {
		t.Fatalf("index.html 应以外链 module 方式加载 ./app.js")
	}
	for _, bad := range []string{`<script src="http`, `<script src="//`, `<link rel="stylesheet" href="http`,
		`<link rel="stylesheet" href="//`, `@import url(http`} {
		if strings.Contains(index, bad) {
			t.Fatalf("前端引用了外部资源 %q（违反离线可用 + CSP 'self'）：%s", bad, "")
		}
	}
	// CSP 没有 'unsafe-eval'，所以代码里不能有 eval / new Function。
	for _, banned := range []string{"eval(", "new Function("} {
		if strings.Contains(appJS, banned) {
			t.Fatalf("app.js 使用了 %q，CSP 无 'unsafe-eval' 会直接抛错", banned)
		}
	}
	// app.js 的既有约定是"动态内容一律 textContent"，不许拼 innerHTML（XSS 面）。
	if strings.Contains(appJS, ".innerHTML") || strings.Contains(appJS, "innerHTML =") {
		t.Fatalf("app.js 真的使用了 innerHTML（注释里提到不算）：既有约定是 textContent")
	}

	// 体积预算（§13）：合计 ≤ 120KB，单文件 ≤ 60KB。
	const (
		totalBudget = 120 << 10
		fileBudget  = 60 << 10
	)
	total := len(index) + len(appJS) + len(styleCSS)
	if total > totalBudget {
		t.Fatalf("前端总体积 %d 超过预算 %d", total, totalBudget)
	}
	for name, size := range map[string]int{"index.html": len(index), "app.js": len(appJS), "style.css": len(styleCSS)} {
		if size > fileBudget {
			t.Fatalf("%s 体积 %d 超过单文件预算 %d", name, size, fileBudget)
		}
	}
	t.Logf("前端体积：index=%d app.js=%d style.css=%d 合计=%d（预算 120KB，单文件 60KB）",
		len(index), len(appJS), len(styleCSS), total)

	// CSP 头本身。
	res := env.do(env.client(), http.MethodGet, "/", nil)
	csp := res.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self'", "img-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP 缺少 %q：%q", want, csp)
		}
	}
	if strings.Contains(csp, "'unsafe-eval'") {
		t.Fatalf("CSP 不该允许 'unsafe-eval'：%q", csp)
	}
	if strings.Contains(csp, "script-src") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("script-src 不该允许 'unsafe-inline'：%q", csp)
	}
	// 现状记录：style-src 带了 'unsafe-inline'，但 index.html 与 app.js 都没有
	// 内联 style 属性/CSSOM 注入，可以收紧（属加固建议，不是缺陷）。
	if strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Logf("加固建议：CSP 的 style-src 含 'unsafe-inline'，但前端无内联 style 用法，可去掉（%s）", csp)
	}

	// MIME：.js 必须是 javascript 类型（不同 Go 版本默认值不一，实现已显式覆盖）。
	jsRes := env.do(env.client(), http.MethodGet, "/app.js", nil)
	ct := jsRes.header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("/app.js 的 Content-Type 期望 text/javascript 或 application/javascript，实际 %q", ct)
	}
	if got := env.do(env.client(), http.MethodGet, "/style.css", nil).header.Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Fatalf("/style.css 的 Content-Type 期望 text/css，实际 %q", got)
	}

	// /index.html 直接访问也必须可用（书签/硬刷新）。
	res = env.do(env.client(), http.MethodGet, "/index.html", nil)
	env.expectStatus(res, http.StatusOK, "GET /index.html")

	// 不存在的静态路径：契约未定，这里记录实际行为并断言"不返回 HTML"。
	res = env.do(env.client(), http.MethodGet, "/no-such-file.png", nil)
	if res.status != http.StatusNotFound {
		t.Fatalf("不存在的静态路径期望 404，实际 %d %s", res.status, truncateForLog(res.body))
	}
	if ct := res.header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("不存在的静态路径应回 JSON 错误信封，实际 Content-Type=%q", ct)
	}
	t.Logf("现状（契约未定）：未知静态路径 → 404 + JSON 错误信封（不回 index.html，前端 hash 路由不依赖它）")
}

// checkNoInlineScript 报告内联 <script> 与内联事件处理器（on*=）的位置。
func checkNoInlineScript(html string) error {
	lower := strings.ToLower(html)
	idx := 0
	for {
		i := strings.Index(lower[idx:], "<script")
		if i < 0 {
			break
		}
		start := idx + i
		end := strings.Index(lower[start:], ">")
		if end < 0 {
			break
		}
		tag := lower[start : start+end+1]
		if !strings.Contains(tag, " src=") {
			return errString("内联 <script> 标签：" + tag)
		}
		idx = start + end + 1
	}
	for _, attr := range []string{" onclick=", " onchange=", " onsubmit=", " onload=", " oninput=", " onerror="} {
		if strings.Contains(lower, attr) {
			return errString("内联事件处理器：" + strings.TrimSpace(attr))
		}
	}
	if strings.Contains(lower, "javascript:") {
		return errString("javascript: 伪协议（CSP 下会失效）")
	}
	return nil
}

// errString 是本文件用的小错误类型（避免为一个字符串引入 fmt.Errorf 包装）。
type errString string

func (e errString) Error() string { return string(e) }

// ============ 并发写配置的互斥（§7 写并发） ============

// reviewGate 让注入的 SSO 登录可以被人为挂起，用来构造"运行正在进行"的确定时刻。
type reviewGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newReviewGate() *reviewGate {
	return &reviewGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// withGatedSSO 把 SSO 登录换成一个"进入即通知、放行才返回"的桩。
func withGatedSSO(gate *reviewGate, token string) envOption {
	return func(d *Deps) {
		d.SSOLogin = func(user, pass string) (string, error) {
			gate.once.Do(func() { close(gate.entered) })
			<-gate.release
			return token, nil
		}
	}
}
