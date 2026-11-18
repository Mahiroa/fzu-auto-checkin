package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
//
// sv 是**会话代次**（auth.json 里的 session_epoch）。它把"无状态签名"补成一个
// 可撤销的会话：改密码、点退出都会让服务端换一个代次，于是所有旧 Cookie 立刻作废。
// 没有它，一次 Cookie 泄漏就是 7 天的通行证，改密码也拦不住。
type sessionPayload struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	SV  string `json:"sv"`
}

// ---------- Cookie ----------

// setSession 下发登录 Cookie：base64url(payload)+"."+HMAC(payload)。
//
// Secure 只在"确实走 HTTPS"时加：本地 http://127.0.0.1 调试若强行 Secure，
// 浏览器根本不会保存这个 Cookie，用户会看到"登录成功但立刻又要求登录"。
func (s *Server) setSession(w http.ResponseWriter, r *http.Request) error {
	now := s.now()
	epoch := s.store.SessionEpoch()
	if epoch == "" {
		// 没有代次的 Cookie 会被 authed 判无效，发出去只会让前端"登录成功又立刻要登录"。
		return errors.New("会话代次不可用：服务尚未初始化")
	}
	payload, err := json.Marshal(sessionPayload{
		Sub: sessionSubject,
		Iat: now.Unix(),
		Exp: now.Add(sessionTTL).Unix(),
		SV:  epoch,
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
	// 代次不匹配 = 这张 Cookie 签发于"上一次改密/退出"之前，按失效处理。
	// 空代次也拒：老载荷（升级前没有 sv 字段）因此一次性失效，用户重新登录一次。
	if epoch := s.store.SessionEpoch(); sp.SV == "" || sp.SV != epoch {
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
	// 顺手把老参数的哈希升级到当前迭代次数。密码本身没变，所以只重算哈希、
	// 不换会话代次（RehashPassword 与 SetPasswordHash 的区别就在这里）。
	// 失败不影响本次登录：哈希还是老的，但仍然能校验通过。
	if secure.NeedsRehash(hash) {
		if fresh, herr := secure.HashPassword(body.Password); herr == nil {
			if err := s.store.RehashPassword(fresh); err != nil {
				s.logf("warn", "", "登录成功，但密码哈希升级失败（下次登录会再试）：%v", err)
			}
		}
	}
	if err := s.setSession(w, r); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, err.Error()))
		return
	}
	s.logf("info", "", "管理员已登录 ip=%s", ip)
	writeOK(w, http.StatusOK)
}

// handleAuthChangePassword 改管理密码：要再输一次当前密码，成功后所有会话作废。
//
// 为什么必须有这个接口：没有它，想换密码只能删 data/auth.json 再重启——那等于
// "谁先打开面板谁就是管理员"，而且旧 Cookie 在这期间一直有效。
//
// 失败计数复用登录限速器：拿着一张偷来的 Cookie 反复试探"当前密码"，和在线爆破
// 是同一件事，没有理由给它更宽松的额度。
func (s *Server) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if wait, blocked := s.limits.blocked(ip); blocked {
		writeTooManyAttempts(w, wait)
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeError(w, e)
		return
	}
	hash := s.store.PasswordHash()
	if hash == "" {
		writeError(w, newError(http.StatusForbidden, codeNotInitialized, "服务尚未初始化：请先设置管理密码"))
		return
	}
	if !secure.VerifyPassword(hash, body.Current) {
		count, wait := s.limits.fail(ip)
		s.logf("warn", "", "改密码失败（当前密码不对）ip=%s 第 %d 次", ip, count)
		if wait > 0 {
			writeTooManyAttempts(w, wait)
			return
		}
		writeError(w, newError(http.StatusUnauthorized, codeBadPassword, "当前密码不正确"))
		return
	}
	if utf8.RuneCountInString(body.New) < minPasswordLen {
		writeError(w, newError(http.StatusBadRequest, codeWeakPassword,
			fmt.Sprintf("新密码至少 %d 个字符", minPasswordLen),
			fmt.Sprintf("当前长度 %d。口令长度比复杂度更值钱：一句只有你记得住的长短语就够。",
				utf8.RuneCountInString(body.New))))
		return
	}
	if body.New == body.Current {
		writeError(w, newError(http.StatusBadRequest, codeWeakPassword, "新密码与当前密码相同，等于没改"))
		return
	}
	fresh, err := secure.HashPassword(body.New)
	if err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "密码哈希失败："+err.Error()))
		return
	}
	// SetPasswordHash 会换会话代次：所有旧 Cookie（包括别的设备）当场失效。
	if err := s.store.SetPasswordHash(fresh); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "写入新密码失败："+err.Error()))
		return
	}
	s.limits.reset(ip)
	// 本次请求自己也要续一张新代次的 Cookie，否则用户改完密码就被自己的操作踢下线。
	if err := s.setSession(w, r); err != nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, err.Error()))
		return
	}
	s.logf("info", "", "管理密码已修改 ip=%s：所有已登录会话已失效", ip)
	writeOK(w, http.StatusOK)
}

// handleAuthLogout 退出登录：清 Cookie **并**轮换会话代次。
//
// 只清浏览器里的 Cookie 是不够的——签名是无状态的，被复制走的那份仍然能用满
// 7 天。单管理员面板上"退出 = 所有设备重新登录"是最省心的语义，也让这一步
// 真正具备撤销能力。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if err := s.store.RotateSessionEpoch(); err != nil {
		// 落盘失败时代次没变，旧 Cookie 仍然通过校验。这必须说出来，
		// 否则用户会以为"已经退出了"。
		s.logf("warn", "", "退出登录：轮换会话代次失败（%v），已签发的 Cookie 在进程重启前仍然有效", err)
		s.clearSession(w, r)
		writeOK(w, http.StatusOK)
		return
	}
	s.clearSession(w, r)
	s.logf("info", "", "管理员已退出登录 ip=%s（所有已签发会话已失效）", ip)
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
