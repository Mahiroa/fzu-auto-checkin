package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fzucheckin/internal/secure"
)

// 鉴权常量（§8）。
const (
	sessionCookieName = "fzu_sess"
	sessionSubject    = "admin"
	// sessionTTL 与 Cookie 的 Max-Age 必须一致，否则会出现"浏览器还带着 Cookie，
	// 服务端已经判过期"的 401 循环。
	sessionTTL = 7 * 24 * time.Hour

	csrfHeaderName  = "X-Requested-With"
	csrfHeaderValue = "fzu-web"

	minPasswordLen   = 8
	loginWindow      = 15 * time.Minute
	loginMaxFailures = 5

	// forwardedForHeader / forwardedProtoHeader 是反向代理约定。
	forwardedForHeader   = "X-Forwarded-For"
	forwardedProtoHeader = "X-Forwarded-Proto"

	// envName 回给前端的运行环境标识（前端只做展示）。
	envName = "prod"
)

// sessionPayload 是 Cookie 里签名保护的内容。只用标准库 hmac，无需 JWT 库。
//
// exp 冗余地放在载荷里（Cookie 自身也有 Max-Age）：浏览器丢掉 Cookie 的时间点
// 与服务端的判定必须能独立成立，否则一个手工构造的 Cookie 就能绕过 Max-Age。
type sessionPayload struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// ---------- Cookie ----------

// setSession 下发登录 Cookie：base64url(payload)+"."+HMAC(payload)。
//
// Secure 只在"确实走 HTTPS"时加：本地 http://127.0.0.1 调试若强行 Secure，
// 浏览器根本不会保存这个 Cookie，用户会看到"登录成功但立刻又要求登录"。
func (s *Server) setSession(w http.ResponseWriter, r *http.Request) error {
	now := s.now()
	payload, err := json.Marshal(sessionPayload{
		Sub: sessionSubject,
		Iat: now.Unix(),
		Exp: now.Add(sessionTTL).Unix(),
	})
	if err != nil {
		return fmt.Errorf("会话载荷序列化失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + s.signer.Sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsSecure(r),
	})
	return nil
}

// clearSession 让浏览器立刻丢弃 Cookie（Max-Age=-1），并保持其余属性一致，
// 否则浏览器可能认为是"另一个 Cookie"而留下旧的。
func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsSecure(r),
	})
}

// requestIsSecure 判断这次请求对客户端来说是不是 HTTPS。
// 反向代理终结 TLS 时 r.TLS 是 nil，只能看 X-Forwarded-Proto。
func requestIsSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(forwardedProtoHeader)), "https")
}

// authed 校验 Cookie 签名与有效期。任何一步不成立都返回 false（不给"为什么"：
// 未登录的调用方不需要知道是签名错还是过期）。
func (s *Server) authed(r *http.Request) bool {
	if r == nil {
		return false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	value := cookie.Value
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return false
	}
	// 签名覆盖的是**载荷原文**（JSON 字节），Cookie 里传的是它的 base64url。
	// 所以先解码再验签：对着 base64 文本验签会永远失败，而且失败得毫无提示。
	raw, err := base64.RawURLEncoding.DecodeString(value[:idx])
	if err != nil {
		return false
	}
	if !s.signer.Verify(raw, value[idx+1:]) {
		return false
	}
	var sp sessionPayload
	if err := json.Unmarshal(raw, &sp); err != nil {
		return false
	}
	if sp.Sub != sessionSubject {
		return false
	}
	return sp.Exp > s.now().Unix()
}

// ---------- 登录限速 ----------

// loginLimiter 是进程内失败计数器（§8：进程重启归零，可接受）。
//
// 为什么用额外一条 mutex 包住整个"判定 + 记账"：登录是低频接口，争用成本可以忽略；
// 而拆成两次加锁会让并发爆破在两次锁之间挤进多于一发的尝试，限速形同虚设。
type loginLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	now    func() time.Time
	// failures 按 IP 存失败时刻。只保留窗口内的记录，空键立刻删掉：
	// 一个被扫段扫过的部署会在这里留下大量一次性 IP，不清就是内存泄漏。
	failures map[string][]time.Time
}

func newLoginLimiter(window time.Duration, max int, now func() time.Time) *loginLimiter {
	if window <= 0 {
		window = 15 * time.Minute
	}
	if max <= 0 {
		max = 5
	}
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{window: window, max: max, now: now, failures: make(map[string][]time.Time)}
}

// prune 丢掉窗口外的失败记录（调用方必须已持锁）。
func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	items := l.failures[ip]
	kept := items[:0]
	for _, ts := range items {
		if now.Sub(ts) < l.window {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, ip)
		return nil
	}
	l.failures[ip] = kept
	return kept
}

// blocked 返回"还要等多久"以及是否已被限速。
// 等待时间以"最早一条仍有效的失败记录离开窗口"为准：那是限速自然解除的时刻。
func (l *loginLimiter) blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	items := l.prune(ip, now)
	if len(items) < l.max {
		return 0, false
	}
	oldest := items[len(items)-l.max]
	wait := l.window - now.Sub(oldest)
	if wait < time.Second {
		wait = time.Second
	}
	return wait, true
}

// fail 记一次失败，返回当前窗口内的失败次数与（达到上限时的）等待时间。
//
// 达到上限的那一次本身回 429：只回 401 再让下一次 429，用户会以为"第 5 次还
// 有希望"，而实际上条件已经满足（§8 的"15 分钟内失败 5 次 → 429"）。
func (l *loginLimiter) fail(ip string) (int, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	items := append(l.prune(ip, now), now)
	l.failures[ip] = items
	if len(items) < l.max {
		return len(items), 0
	}
	oldest := items[len(items)-l.max]
	wait := l.window - now.Sub(oldest)
	if wait < time.Second {
		wait = time.Second
	}
	return len(items), wait
}

// reset 在登录成功后清零该 IP 的计数。
// 不清零的后果是"白天试错 5 次，晚上密码正确却要等到窗口过期"。
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.failures, ip)
	l.mu.Unlock()
}

// clientIP 是**登录限速的身份**，因此它的默认口径必须是最难伪造的那个。
//
// X-Forwarded-For 是客户端可随手写的头：无条件采信它，等于给爆破者一个"每次换头
// 就换身份"的开关（实测：同一来源失败 5 次被 429，加一个 XFF 立刻 200）。
// 所以默认只认 TCP 层的 RemoteAddr；只有当服务确实跑在可信反向代理后面
// （FZU_TRUST_PROXY=1）时才读 XFF 第一段——那种部署下 RemoteAddr 是代理本身，
// 不读 XFF 会让所有用户共用一个计数桶。
func (s *Server) clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if s == nil || !s.trustProxy {
		return host
	}
	if xff := r.Header.Get(forwardedForHeader); xff != "" {
		parts := strings.Split(xff, ",")
		if first := strings.TrimSpace(parts[0]); first != "" {
			return first
		}
	}
	return host
}

// ---------- 处理器 ----------

// handleAuthSession 是前端启动的第一步（未初始化 → 设置密码；未登录 → 登录页）。
// 它自己不能要求登录，否则前端永远进不了应用。
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	initialized := s.store.PasswordHash() != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": initialized,
		"authed":      initialized && s.authed(r),
		"env":         envName,
	})
}

// handleAuthSetup 只在"从未设过密码"时可用一次（§8）。
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	if s.store.PasswordHash() != "" {
		writeError(w, newError(http.StatusConflict, codeAlreadyInitialized,
			"管理密码已经设置过了，请直接登录；忘记密码可在 data/auth.json 删除后重启重新初始化"))
		return
	}
	// 首尾空格可能是密码的一部分，不做 Trim；只数长度。
	if utf8.RuneCountInString(body.Password) < minPasswordLen {
		writeError(w, newError(http.StatusBadRequest, codeWeakPassword,
			fmt.Sprintf("密码至少 %d 个字符", minPasswordLen),
			fmt.Sprintf("当前长度 %d。建议用一句只有你记得住的长短语，而不是短口令。", utf8.RuneCountInString(body.Password))))
		return
	}
	hash, err := secure.HashPassword(body.Password)
	if err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "密码哈希失败："+err.Error()))
		return
	}
	if err := s.store.SetPasswordHash(hash); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "写入管理密码失败："+err.Error()))
		return
	}
	if err := s.setSession(w, r); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, err.Error()))
		return
	}
	s.logf("info", "", "管理密码已初始化，管理员已登录 ip=%s", s.clientIP(r))
	writeOK(w, http.StatusOK)
}

// handleAuthLogin 校验密码并下发 Cookie，失败按 IP 限速。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if wait, blocked := s.limits.blocked(ip); blocked {
		writeTooManyAttempts(w, wait)
		s.logf("warn", "", "登录被限速 ip=%s 还需等待 %s", ip, wait)
		return
	}

	hash := s.store.PasswordHash()
	if hash == "" {
		writeError(w, newError(http.StatusForbidden, codeNotInitialized,
			"服务尚未初始化：请先设置管理密码"))
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}
	if !secure.VerifyPassword(hash, body.Password) {
		count, wait := s.limits.fail(ip)
		// 失败原因对外只说"密码错误"：区分"没设密码/格式不对"等于给爆破指路。
		s.logf("warn", "", "登录失败 ip=%s 第 %d 次", ip, count)
		if wait > 0 {
			writeTooManyAttempts(w, wait)
			return
		}
		writeError(w, newError(http.StatusUnauthorized, codeBadPassword, "密码不正确"))
		return
	}

	s.limits.reset(ip)
	if err := s.setSession(w, r); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, err.Error()))
		return
	}
	s.logf("info", "", "管理员已登录 ip=%s", ip)
	writeOK(w, http.StatusOK)
}

// handleAuthLogout 清 Cookie。它是"需要登录"的接口：未登录调用拿 401，
// 前端据此知道会话本来就已经失效了。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	s.logf("info", "", "管理员已退出登录 ip=%s", s.clientIP(r))
	writeOK(w, http.StatusOK)
}

// writeTooManyAttempts 输出 429 + Retry-After（秒，向上取整）。
func writeTooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	secs := int64(wait / time.Second)
	if wait%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	writeError(w, newError(http.StatusTooManyRequests, codeTooManyAttempts,
		fmt.Sprintf("密码错误次数过多，请 %d 秒后再试（同一 IP %d 分钟内最多 %d 次失败）",
			secs, int(loginWindow/time.Minute), loginMaxFailures)))
}
