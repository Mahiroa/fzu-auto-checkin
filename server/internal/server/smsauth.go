package server

// 这个文件把 fzu 包里的「校外 IP 短信二次认证」搬进面板：四条 HTTP 接口 + 一份进程内的
// challenge 注册表。协议细节（发码 / 校验 / 换票 / CSRF 头 / flowkey）全在 fzu/sms.go 里，
// 这一层只做三件事：
//
//  1. 把一条 PendingSecondAuth 会话登记成一个**不透明句柄**（32 位 hex 的 challenge），
//     面板只拿得到这个句柄：手机号只给打码版，flowkey 与 token 一个字符都不出进程。
//  2. 把上游错误分类成 409/410/429/502，前端按 reason 分支而不是匹配中文。
//  3. 验证通过后按 /login/refresh 的**同一套语义**把 token 写回配置：先 init 校验、
//     再落盘、最后唤醒调度器（见 login_refresh.go 顶部的注释），失败不落盘。
//
// 为什么 challenge 要有注册表而不是把会话塞进 Cookie：PendingSecondAuth 里是活的
// http.CookieJar + flowkey，它必须留在服务进程内（不能落盘、不能跨实例）；面板能看到的
// 只有"我手上这张票是几号"。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
)

const (
	// smsChallengeTTL 是一条 challenge 的最长寿命（按会话的 CreatedAt() 算）。
	// 十分钟后验证码（300s）早就过期、学校侧的二次认证会话也多半失效，留着只会让用户
	// 对着一个必失败的按钮点。
	smsChallengeTTL = 10 * time.Minute
	// smsChallengeMax 是同时存在的 challenge 上限。5 条足够"手机 + 电脑 + 重试几次"，
	// 又堵住了"刷 start 把进程内存和学校风控一起拖垮"这条路。
	smsChallengeMax = 5
	// smsVerifyMaxAttempts 是同一个 challenge 允许错几次验证码。6 位数字 5 次命中概率
	// 约 5e-6，再配 10 分钟 TTL 与 120 秒重发冷却就够；放宽只会变成爆破入口。
	smsVerifyMaxAttempts = 5
)

// smsChallengeExpiredMessage 是 410 的唯一文案：说清"为什么没了"和"下一步做什么"。
const smsChallengeExpiredMessage = "这条短信验证已经过期或已经被用掉（每条 challenge 只有 10 分钟、且成功一次即作废）：请重新发起一次短信验证"

// smsPhoneUnboundMessage 是 409 phone_unbound 的文案：给出另外两条真能走通的路。
const smsPhoneUnboundMessage = "这个账号没有绑定手机号，短信验证走不通：请改用「浏览器 Cookie」，或直接在浏览器里打开晚点名页面把地址栏的 token 粘进「免登凭证」"

// smsPendingAuth 是「密码已过、只等短信验证码」的会话在 internal/server 里被要求的能力集。
//
// 为什么用接口而不是直接写 *fzu.PendingSecondAuth：那个类型的字段全不导出，包外造不出
// 一个假货；而它的每个方法在测试里都会打真 SSO——每次尝试都给学校风控记一笔，还真的会
// 发一条短信到用户手机上（sms.go 顶部的取证就是这么来的）。接口让 fzu 包一个字符都不用
// 改，*fzu.PendingSecondAuth 天然满足它；假实现见 smsauth_test.go 的 fakePending。
type smsPendingAuth interface {
	PhoneMasked() string
	Tip() string
	CreatedAt() time.Time
	SendInterval() time.Duration
	CodeExpire() time.Duration
	ResendAfter(now time.Time) time.Duration
	SendCode() (fzu.SendCodeResult, error)
	VerifyCode(code string) (string, error)
}

// startSecondAuthFn 是「用学号密码把会话推到等短信」的唯一生产入口，测试里替换它即可
// 完全离线（不联网、不发真短信）。
//
// 这里刻意包一层再返回接口，而不是 `var startSecondAuthFn = fzu.StartSecondAuth`：
// 真实实现返回 (*fzu.PendingSecondAuth)(nil) 时会被装进一个**非 nil 的接口值**，
// 于是"这次不需要短信"这个最关键的判断永远不成立（走密码直接成功的那条分支会静默丢掉）。
// 包一层能顺手把 typed-nil 折成真 nil。
var startSecondAuthFn = func(username, password string) (smsPendingAuth, string, error) {
	pending, token, err := fzu.StartSecondAuth(username, password)
	if pending == nil {
		return nil, token, err
	}
	return pending, token, err
}

// ---------- challenge 注册表 ----------

// smsChallengeEntry 是一条被面板持有句柄的二次认证会话。
type smsChallengeEntry struct {
	pending smsPendingAuth
	// registered 是登记时刻：只用来在"超过 5 条"时判定谁是"最老的"。
	registered time.Time
	// attempts 是这条 challenge 还能错几次验证码。
	attempts int
}

// smsChallengeRegistry 是进程内的 challenge 表。
//
// 为什么是个包级变量而不是 Server 的字段：Go 没法在包外给结构体加字段，而 server.go
// 不在这次改动范围里（它是稳定面）。隔离性由"按 Server 分桶"保证——多个 Server 实例
// （测试里每个用例一个）各自看到自己的 challenge，上限与淘汰也只在自己的桶里算，
// 所以既不会串号，也不会因为上一个用例没清干净而让下一个用例看到 5 条已满。
type smsChallengeRegistry struct {
	mu    sync.Mutex
	bySrv map[*Server]map[string]*smsChallengeEntry
}

var smsChallenges = &smsChallengeRegistry{bySrv: make(map[*Server]map[string]*smsChallengeEntry)}

// findLive 找一条"同一个打码手机号、还没过期"的 challenge。
//
// 存在的意义是**别烧学校的短信额度**：用户点两次「发送验证码」、或者上一次发完报了个错
// 又点一次，都会真的再发一条短信。学校的限额很低（实测一天几条就回 13001016/60008），
// 所以这里在发码之前先看一眼有没有还活着的那条——有就直接复用，一次请求都不打上游。
func (r *smsChallengeRegistry) findLive(s *Server, phoneMasked string, now time.Time) (string, smsPendingAuth, bool) {
	if phoneMasked == "" {
		return "", nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pruneLocked(s, now)
	var (
		bestID string
		best   smsPendingAuth
	)
	for id, e := range m {
		if e == nil || e.pending == nil || e.pending.PhoneMasked() != phoneMasked {
			continue
		}
		// 多条同号时取最近登记的那条（its 冷却最晚结束，复用它对用户最省事）。
		if bestID == "" || e.registered.After(m[bestID].registered) {
			bestID, best = id, e.pending
		}
	}
	return bestID, best, bestID != ""
}

// bucketLocked 取（必要时创建）某个 Server 的桶；调用方必须已持锁。
func (r *smsChallengeRegistry) bucketLocked(s *Server) map[string]*smsChallengeEntry {
	m := r.bySrv[s]
	if m == nil {
		m = make(map[string]*smsChallengeEntry)
		r.bySrv[s] = m
	}
	return m
}

// pruneLocked 丢掉过期条目并返回该 Server 的桶（调用方已持锁）。
func (r *smsChallengeRegistry) pruneLocked(s *Server, now time.Time) map[string]*smsChallengeEntry {
	m := r.bucketLocked(s)
	for id, e := range m {
		if smsChallengeExpired(e, now) {
			delete(m, id)
		}
	}
	return m
}

// dropEmptyLocked 在桶空时连桶一起删掉：常驻几天的进程里不该为每个走过的 Server 留一个空 map。
func (r *smsChallengeRegistry) dropEmptyLocked(s *Server, m map[string]*smsChallengeEntry) {
	if len(m) == 0 {
		delete(r.bySrv, s)
	}
}

// smsChallengeExpired 按会话的 CreatedAt() 判 TTL（不是按登记时刻）。
//
// 为什么用 CreatedAt()：一条会话在进程里的年龄就该由它自己说——登记时间只差几毫秒，
// 但语义上"这条会话还能不能用"是学校侧会话的年龄决定的。
func smsChallengeExpired(e *smsChallengeEntry, now time.Time) bool {
	if e == nil || e.pending == nil {
		return true
	}
	created := e.pending.CreatedAt()
	if created.IsZero() {
		created = e.registered
	}
	return now.Sub(created) > smsChallengeTTL
}

// register 登记一条 challenge 并返回 32 位 hex 句柄（熵源异常时返回空串，调用方回 500）。
func (r *smsChallengeRegistry) register(s *Server, pending smsPendingAuth) string {
	now := s.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pruneLocked(s, now)

	if len(m) >= smsChallengeMax {
		// 淘汰最老的一条并留 warn：这条日志是"有人在猛刷 start"的唯一线索。
		var (
			oldestID string
			oldest   time.Time
		)
		for id, e := range m {
			if oldestID == "" || e.registered.Before(oldest) {
				oldestID, oldest = id, e.registered
			}
		}
		delete(m, oldestID)
		s.logf("warn", "", "面板短信验证：同时存在的 challenge 已达上限 %d，淘汰最老的一条（%s）",
			smsChallengeMax, shortChallenge(oldestID))
	}

	id := newChallengeID()
	if id == "" {
		r.dropEmptyLocked(s, m)
		return ""
	}
	m[id] = &smsChallengeEntry{pending: pending, registered: now, attempts: smsVerifyMaxAttempts}
	return id
}

// ---------- 发码冷却（按手机号，跨 challenge 生效） ----------

// smsSendCooldown 是「同一台手机两次发码之间至少隔多久」。
//
// 为什么不是用学校回的 sendInterval（实测 120s）：2026-11-18 深夜连测发现，学校真正
// 放行间隔在 **4~5 分钟**——22:23:10 发成功，之后 22:23:25 / 22:24:05 / 22:25:35 /
// 22:27:06 全部被 `13001016` 拒掉，直到 22:28:36 才又成功。按 120s 显示倒计时，用户会
// 在 2 分钟时点一次、被拒一次，再以为功能坏了。这里取 5 分钟：宁可让用户多等，也不要
// 让他反复撞"发不出去"。
const smsSendCooldown = 5 * time.Minute

// smsSendClock 记"这台手机上次成功发码是什么时候"，按 Server 分桶（与 challenge 同理，
// 常驻进程里不能因为换实例而串号）。
type smsSendClock struct {
	mu    sync.Mutex
	bySrv map[*Server]map[string]time.Time
}

var smsSends = &smsSendClock{bySrv: make(map[*Server]map[string]time.Time)}

// wait 返回这台手机还要等多久才能再发（没发过或已过冷却返回 0）。
func (c *smsSendClock) wait(s *Server, phoneMasked string, now time.Time) time.Duration {
	if phoneMasked == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.bySrv[s]
	if m == nil {
		return 0
	}
	at, ok := m[phoneMasked]
	if !ok {
		return 0
	}
	// 顺手清掉已经过了两倍冷却的老记录，避免长期运行时这个 map 只增不减。
	for k, v := range m {
		if now.Sub(v) > 2*smsSendCooldown {
			delete(m, k)
		}
	}
	if d := smsSendCooldown - now.Sub(at); d > 0 {
		return d
	}
	return 0
}

// mark 记一次成功发码。
func (c *smsSendClock) mark(s *Server, phoneMasked string, now time.Time) {
	if phoneMasked == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.bySrv[s]
	if m == nil {
		m = make(map[string]time.Time)
		c.bySrv[s] = m
	}
	m[phoneMasked] = now
}

// get 取一条仍在有效期内的 challenge。
func (r *smsChallengeRegistry) get(s *Server, id string, now time.Time) (*smsChallengeEntry, bool) {
	if strings.TrimSpace(id) == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pruneLocked(s, now)
	e, ok := m[id]
	if !ok || e == nil {
		r.dropEmptyLocked(s, m)
		return nil, false
	}
	return e, true
}

// drop 删掉一条 challenge（成功、取消、会话死亡都走它：单次有效）。
func (r *smsChallengeRegistry) drop(s *Server, id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pruneLocked(s, s.now())
	delete(m, id)
	r.dropEmptyLocked(s, m)
}

// failAttempt 记一次错码，返回剩余次数；用到 0 就把 challenge 删掉（防爆破）。
func (r *smsChallengeRegistry) failAttempt(s *Server, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pruneLocked(s, s.now())
	e, ok := m[id]
	if !ok || e == nil {
		r.dropEmptyLocked(s, m)
		return 0
	}
	e.attempts--
	if e.attempts <= 0 {
		delete(m, id)
		r.dropEmptyLocked(s, m)
		return 0
	}
	return e.attempts
}

// count 是给测试与日志用的"当前有几条"（不触发淘汰，只做过期清理）。
func (r *smsChallengeRegistry) count(s *Server) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pruneLocked(s, s.now()))
}

// newChallengeID 造 16 字节随机数的 hex 形态（32 字符）。
func newChallengeID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 熵源坏了就没法签发不可猜的句柄，宁可让这次请求失败也不要给出可预测的值。
		return ""
	}
	return hex.EncodeToString(buf)
}

// shortChallenge 只取句柄前 8 位用于日志：日志是排查入口，不需要完整句柄。
func shortChallenge(id string) string {
	if len(id) <= 8 {
		return orDash(id)
	}
	return id[:8] + "…"
}

// ---------- 响应小工具 ----------

// writeSMSReject 输出面板短信流程的"平铺错误体"：{"ok":false,"reason":…,"message":…}。
//
// 为什么这四个接口不走 errors.go 的 {"error":{…}} 信封：前端要按 reason 分支
// （code_rejected / challenge_expired / too_frequent / phone_unbound / no_credentials），
// reason 是稳定机器码。只有"上游炸了、没有分支价值"的失败才继续用信封（见各 handler 的
// writeError 调用点），那样 message 里能带逐跳诊断，也不破坏 §9 的统一错误形态。
func writeSMSReject(w http.ResponseWriter, status int, reason, message string, extra map[string]any) {
	body := make(map[string]any, 3+len(extra))
	body["ok"] = false
	body["reason"] = reason
	body["message"] = message
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// smsSeconds 把时长换成向上取整的秒（前端显示"还要等 N 秒"用）。
func smsSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// smsResendWait 返回"还要等多久才能重发"：刚发过就是 SendInterval，否则可能是 0。
func smsResendWait(p smsPendingAuth, now time.Time) time.Duration {
	if d := p.ResendAfter(now); d > 0 {
		return d
	}
	return p.SendInterval()
}

// smsEntry 取一条 challenge（challenge 为空或已过期都返回 false）。
func (s *Server) smsEntry(id string) (*smsChallengeEntry, bool) {
	return smsChallenges.get(s, id, s.now())
}

// commitSMSToken 把短信验证换到的 token 写回配置。
//
// 与 /login/refresh 的区别只有一条，但很关键：**init 没过也照样落盘**。
// 埋由：这条 token 是用户等了一条短信才换来的，上游一时的 500（维护页/网关抖动）
// 并不代表 token 无效；丢掉它，用户只能再等一条短信。所以这里先写、再把 init 的
// 结论作为 initNote 交回调用方，让面板显示成"已保存，但学校端此刻没确认"。
// 写入失败（磁盘/权限）才算真失败。
func (s *Server) commitSMSToken(token string, probe model.Config) (plan string, version int64, initNote string, apiErr *apiError) {
	plan, initErr := s.initCheck(token)
	version, saveErr := s.store.UpdateConfig(func(latest *model.Config) []string {
		latest.User.Token = token
		return nil
	})
	if saveErr != nil {
		msg := s.sanitizeError(saveErr.Error(), probe)
		s.logf("warn", "", "面板短信验证：token 换到了但写入配置失败：%s", msg)
		return "", 0, "", newError(http.StatusInternalServerError, codeSaveFailed,
			"token 换到了，但写入配置失败："+msg)
	}
	s.wakeScheduler("面板短信验证已更新免登凭证")
	if initErr != nil {
		msg := s.sanitizeError(initErr.Error(), probe)
		s.logf("warn", "", "面板短信验证：token 已保存，但学校端 init 此刻没通过：%s", msg)
		return "", version, "（token 已保存，下一轮会自动重试；学校端 init 此刻没通过：" + msg + "）", nil
	}
	return plan, version, "", nil
}

// ---------- 发码核心（/login/sms/start 与 409 second_auth 共用的唯一实现） ----------

// smsBegin 是 beginSecondAuth 的结果：前端要的一切都在这个结构里。
//
// 为什么要有个结构而不是在 handler 里直接写响应：同一个流程有两个入口——面板上的
// 「发送验证码」（POST /login/sms/start）与登录被判二次认证时的 409（/login/test、
// /login/refresh）。前者把这些字段摊平成 200，后者把同样的字段塞进 409 体。抽出来之后
// 两条路的判断（发码、复用、冷却、失败分类）只写一遍，就不可能各自漂移。
type smsBegin struct {
	// Challenge 非空表示"手上已经有一条能用的验证码会话"。
	Challenge string
	// Phone 是打码手机号，Tip 是学校原话。
	Phone string
	Tip   string
	// ExpiresIn / ResendAfter 是给前端倒计时用的秒数（过期 / 还需等多久才能重发）。
	ExpiresIn   int
	ResendAfter int
	// Sent=true 表示这一轮真的让学校发了一条短信；Reused=true 表示复用了还在有效期内的
	// 上一条（Sent=false，但码确实已经在用户手机上）。
	Sent   bool
	Reused bool
	// Mode=="password" 表示这次登录根本没触发二次认证（校内 IP）：token 已经落盘，
	// 用户可以跳过短信这一步。空串表示"在等短信"。
	Mode string
	// Plan / Version / TokenHint 只在 Mode=="password" 时有意义。
	Plan      string
	Version   int64
	TokenHint string
	// Message 是这一分支要给人看的那句话（发码成功时为空：前端自己会拼文案）。
	Message string
}

// liveChallenge 只在"码确实在用户手机上"时返回句柄。
//
// 这条不变式是前端的安全带：challenge 非空 ⇒ 用户可以立刻去翻短信。发码失败、手机还在
// 冷却里都返回空串，哪怕注册表里确实躺着一条旧的。
func (b *smsBegin) liveChallenge() string {
	if b == nil {
		return ""
	}
	if b.Sent || b.Reused {
		return b.Challenge
	}
	return ""
}

// beginSecondAuth 用一组凭据把会话推到"等短信"，发码（含复用与冷却判断），
// 返回前端要的一切。
//
// 失败时返回 (begin, err)：begin 可能非 nil，携带"要平铺进响应体"的字段
// （resend_after / challenge）——*apiError 只有状态码/机器码/文案，装不下它们。
// 注意失败时 begin.liveChallenge() 一律为空：码没发出去就不能让前端弹验证码框。
func (s *Server) beginSecondAuth(username, password string) (*smsBegin, *apiError) {
	// 脱敏快照：这次可能用的是"只在内存里过一遍"的表单凭据，把它装进 probe，
	// 上游错误（可能带请求上下文）落进响应/日志前才抹得掉。
	probe := s.store.Config()
	probe.User.Username = username
	probe.User.Password = password

	if strings.TrimSpace(username) == "" || password == "" {
		return nil, newError(http.StatusConflict, "no_credentials",
			"面板内短信验证需要学号与密码：先在「账号」里填好学号密码再点这个按钮"+
				"（也可以在请求里临时带上学号密码，那样不会落盘）")
	}

	pending, token, err := startSecondAuthFn(username, password)

	// 情况一：这次登录根本没走到短信（校内 IP）——token 已经是成品。
	if pending == nil && err == nil {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, newError(http.StatusBadGateway, codeLoginFailed,
				"学号密码登录走完了，但既没有取到晚点名 token、也没有被要求短信验证（登录页结构可能已变化）")
		}
		plan, version, initNote, apiErr := s.commitSMSToken(token, probe)
		if apiErr != nil {
			return nil, apiErr
		}
		s.logf("info", "", "面板短信验证：未触发二次认证，直接用学号密码换到 token 并保存")
		return &smsBegin{
			Mode:      "password",
			Plan:      plan,
			Version:   version,
			TokenHint: maskSecret(token),
			Message: fmt.Sprintf("这台服务器没有触发短信二次认证（看起来在校内 IP），"+
				"晚点名 token 已保存：%s%s", plan, initNote),
		}, nil
	}

	// 情况二：会话停在二次认证页，或上游直接失败。
	if err != nil {
		msg := s.sanitizeError(err.Error(), probe)
		s.logf("warn", "", "面板短信验证：发起二次认证失败：%s", msg)
		if errors.Is(err, fzu.ErrSMSPhoneUnbound) {
			return nil, newError(http.StatusConflict, "phone_unbound", smsPhoneUnboundMessage)
		}
		if errors.Is(err, fzu.ErrSMSSessionExpired) {
			return nil, newError(http.StatusGone, "challenge_expired", smsChallengeExpiredMessage)
		}
		return nil, newError(http.StatusBadGateway, codeLoginFailed, "发起短信验证失败："+msg)
	}
	if pending == nil {
		return nil, newError(http.StatusInternalServerError, codeInternal,
			"短信验证流程内部状态异常：既没有会话也没有 token")
	}

	// 情况二点五：同一台手机已经有一条还活着的验证码 → 直接复用，不再打学校的发码接口。
	//
	// 为什么值得单独一条分支：学校的短信额度按天算，实测发几条之后就回 13001016 / 60008
	// 拒发；而用户的典型操作恰恰是"点两次发送"或"上次报错了再点一次"（现在是"登录被判
	// 二次认证了两次"）。复用它既省额度，也避免用户收到第二、第三条内容相同、只有最后
	// 一份能用的验证码。
	now := s.now()
	phone := pending.PhoneMasked()
	cool := smsSends.wait(s, phone, now)
	if id, old, ok := smsChallenges.findLive(s, phone, now); ok {
		wait := cool
		if w := old.ResendAfter(now); w > wait {
			wait = w
		}
		if wait > 0 {
			secs := smsSeconds(wait)
			s.logf("info", "", "面板短信验证：复用还在有效期内的验证码 challenge=%s（%d 秒后才会允许重发），未再次请求学校",
				shortChallenge(id), secs)
			return &smsBegin{
				Challenge:   id,
				Phone:       old.PhoneMasked(),
				Tip:         old.Tip(),
				ExpiresIn:   smsSeconds(old.CodeExpire()),
				ResendAfter: secs,
				Reused:      true,
				Message: fmt.Sprintf("上一条验证码还在有效期内（手机上最近收到的那条就能用），%d 秒后才允许重发。",
					secs),
			}, nil
		}
	} else if cool > 0 {
		// 没有活着的 challenge、但手机还在冷却里：直接说等多久，别去打学校（打了也只会被拒，
		// 还多记一次风控）。这条分支就是"用户连点"时最该给出的回答。
		secs := smsSeconds(cool)
		s.logf("info", "", "面板短信验证：%s 还在发码冷却里（还剩 %d 秒），未请求学校", phone, secs)
		return &smsBegin{Phone: phone, ResendAfter: secs},
			newError(http.StatusTooManyRequests, "too_frequent",
				fmt.Sprintf("这半小时内已经给 %s 发过验证码了，学校那边至少要隔 %d 分钟才放行（实测间隔）。"+
					"再等 %d 秒；急着用就在浏览器里打开晚点名，把地址栏 token= 后面那串粘进来", phone,
					int(smsSendCooldown.Minutes()), secs))
	}

	// 情况三：真正去发码。发不出去就不算 sent（sent 只能是"码确实发出去了"）。
	id := smsChallenges.register(s, pending)
	if id == "" {
		return nil, newError(http.StatusInternalServerError, codeInternal,
			"生成短信验证句柄失败（系统熵源异常），请重启服务后再试")
	}

	if _, sendErr := pending.SendCode(); sendErr != nil {
		return s.smsSendFailure(id, pending, sendErr, probe, false)
	}
	// 真发出去了才记账：发失败不占冷却，否则用户会被自己的失败次数锁住。
	smsSends.mark(s, pending.PhoneMasked(), s.now())

	s.logf("info", "", "面板短信验证：已向 %s 发送验证码 challenge=%s",
		pending.PhoneMasked(), shortChallenge(id))
	now = s.now()
	return &smsBegin{
		Challenge:   id,
		Phone:       pending.PhoneMasked(),
		Tip:         pending.Tip(),
		ExpiresIn:   smsSeconds(pending.CodeExpire()),
		ResendAfter: smsSeconds(smsResendWait(pending, now)),
		Sent:        true,
	}, nil
}

// smsSendFailure 把"发码失败"分类成状态码 + reason + 人话（**不写响应**）。
//
// 拆出来的理由：/login/sms/start 要按 reason 回 429/409/410/502；而 /login/test、
// /login/refresh 那条路必须仍然是 409 second_auth，只能把结论塞进 sms_error。
// 分类规则只该写一遍。
//
// keepChallenge=false 时连 challenge 一起删掉（start 的路径：响应里没有句柄，留着没人能用，
// 只会占住 5 条上限）；resend 的路径传 true —— 那条请求已经带着句柄，网络抖一下之后还能重试。
func (s *Server) smsSendFailure(id string, pending smsPendingAuth, sendErr error,
	probe model.Config, keepChallenge bool) (*smsBegin, *apiError) {

	msg := s.sanitizeError(sendErr.Error(), probe)
	now := s.now()
	switch {
	case errors.Is(sendErr, fzu.ErrSMSRateLimited):
		// 刚发过：会话本身是好的，冷却由学校给。留着 challenge 让用户等够了直接重发。
		s.logf("info", "", "面板短信验证：发码被冷却拦住 challenge=%s：%s", shortChallenge(id), msg)
		// 还要等多久以这条会话自己的 lastSend 为准（sms.go 在收到 8000068 时会把
		// lastSend 回拨到与学校一致），这样 resend_after 和 message 里的秒数永远同一份。
		return &smsBegin{Challenge: id, Phone: pending.PhoneMasked(), ResendAfter: smsSeconds(smsResendWait(pending, now))},
			newError(http.StatusTooManyRequests, "too_frequent", msg)
	case errors.Is(sendErr, fzu.ErrSMSPhoneUnbound):
		smsChallenges.drop(s, id)
		s.logf("warn", "", "面板短信验证：账号没绑手机，短信这条路走不通：%s", msg)
		return nil, newError(http.StatusConflict, "phone_unbound", smsPhoneUnboundMessage)
	case errors.Is(sendErr, fzu.ErrSMSSessionExpired):
		smsChallenges.drop(s, id)
		return nil, newError(http.StatusGone, "challenge_expired", smsChallengeExpiredMessage)
	default:
		if !keepChallenge {
			smsChallenges.drop(s, id)
		}
		s.logf("warn", "", "面板短信验证：验证码没发出去 challenge=%s：%s", shortChallenge(id), msg)
		return nil, newError(http.StatusBadGateway, codeLoginFailed, "验证码没发出去："+msg)
	}
}

// writeSMSSendFailure 是 smsSendFailure 的"直接写响应"形态（resend 路径继续用它）。
func (s *Server) writeSMSSendFailure(w http.ResponseWriter, id string, pending smsPendingAuth,
	sendErr error, probe model.Config, keepChallenge bool) {

	begin, apiErr := s.smsSendFailure(id, pending, sendErr, probe, keepChallenge)
	writeSMSFailure(w, begin, apiErr)
}

// writeSMSFailure 输出一次"发码失败"的响应。
//
// 平铺体只给前端要按 reason 分支的那几个码；其它（上游炸了、写入失败）继续走统一错误
// 信封 —— 那样 message 里能带逐跳诊断。Retry-After 只在"确实还要等"时给。
func writeSMSFailure(w http.ResponseWriter, begin *smsBegin, e *apiError) {
	if e == nil {
		writeError(w, newError(http.StatusInternalServerError, codeInternal, "未知错误"))
		return
	}
	if begin == nil {
		begin = &smsBegin{}
	}
	if e.Code == "too_frequent" && begin.ResendAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(begin.ResendAfter))
	}
	if !smsFlatReason(e.Code) {
		writeError(w, e)
		return
	}
	extra := map[string]any{}
	if e.Code == "too_frequent" || begin.ResendAfter > 0 {
		extra["resend_after"] = begin.ResendAfter
	}
	if begin.Challenge != "" {
		extra["challenge"] = begin.Challenge
	}
	if len(extra) == 0 {
		extra = nil
	}
	writeSMSReject(w, e.Status, e.Code, e.Message, extra)
}

// smsFlatReason 判断某个码是不是"前端按 reason 分支"的平铺体（见 writeSMSReject）。
func smsFlatReason(code string) bool {
	switch code {
	case "no_credentials", "too_frequent", "phone_unbound", "challenge_expired":
		return true
	}
	return false
}

// ---------- handlers ----------

// handleSMSStart 是 POST /api/v1/login/sms/start：用学号密码把会话推到"等短信"，
// 登记成 challenge，并**真的**给手机发一条验证码。
//
// 现在这一层只是薄壳：解析 body → 取凭据（表单优先、其次配置）→ 交给
// beginSecondAuth（与 409 second_auth 的自动发码共用同一份实现）→ 按结果写响应。
//
// 语义约定（前端依赖）：
//   - 200 + challenge 非空：短信已发出，等用户填 6 位数字；
//   - 200 + challenge 为空 + mode=password：这台服务器没触发二次认证（校内 IP），
//     token 已经写回配置，用户可以跳过短信这一步；
//   - 409 no_credentials / phone_unbound：缺凭据 / 账号没绑手机；
//   - 429 too_frequent：刚发过，等 resend_after 秒；
//   - 502 login_failed：登录链路本身失败（密码错、网络、页面结构变化），message 是原文。
//
// 绝不把"码没发出去"包装成 200：前端会据此弹验证码输入框，用户等到超时也不会收到短信，
// 最后只会去改密码。
func (s *Server) handleSMSStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeSMSReject(w, e.Status, "bad_request", e.Message, nil)
		return
	}

	saved := s.store.Config()
	formUser, _ := givenCredential(body.Username)
	formPass, _ := givenCredential(body.Password)

	begin, apiErr := s.beginSecondAuth(
		firstNonEmpty(formUser, saved.User.Username),
		firstNonEmpty(formPass, saved.User.Password))
	if apiErr != nil {
		s.logf("info", "", "面板短信验证：start 没有发码 reason=%s ip=%s", apiErr.Code, s.clientIP(r))
		writeSMSFailure(w, begin, apiErr)
		return
	}
	s.logf("info", "", "面板短信验证：start 完成 mode=%s sent=%t reused=%t challenge=%s ip=%s",
		orDash(begin.Mode), begin.Sent, begin.Reused, shortChallenge(begin.Challenge), s.clientIP(r))
	writeSMSBeginOK(w, begin)
}

// writeSMSBeginOK 把"这次发起的结果"写成 /login/sms/start 的 200 体。
//
// 三种形状逐条对齐改造前的实现（前端按 mode / reused / sent 分支，一个字段都不能少）：
// 校内 IP（mode=password）、复用上一条（reused:true、sent:false、带 message）、
// 真发了码（sent:true，没有 message）。
func writeSMSBeginOK(w http.ResponseWriter, b *smsBegin) {
	if b == nil || (b.Mode != "password" && b.Challenge == "") {
		writeError(w, newError(http.StatusInternalServerError, codeInternal,
			"短信验证流程内部状态异常：没有会话也没有 token"))
		return
	}
	if b.Mode == "password" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"challenge":  "",
			"mode":       "password",
			"message":    b.Message,
			"token_hint": b.TokenHint,
		})
		return
	}
	if b.Reused {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"challenge":    b.Challenge,
			"phone":        b.Phone,
			"tip":          b.Tip,
			"expires_in":   b.ExpiresIn,
			"resend_after": b.ResendAfter,
			"sent":         false,
			"reused":       true,
			"message":      b.Message,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"challenge":    b.Challenge,
		"phone":        b.Phone,
		"tip":          b.Tip,
		"expires_in":   b.ExpiresIn,
		"resend_after": b.ResendAfter,
		"sent":         true,
	})
}

// handleSMSResend 是 POST /api/v1/login/sms/resend：同一份会话再发一条验证码。
//
// 冷却优先自己判一次：SendCode 也会拦，但自己判能给出准确的 resend_after，
// 前端不必先收一个 429 再猜还要等多久。
func (s *Server) handleSMSResend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Challenge string `json:"challenge"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeSMSReject(w, e.Status, "bad_request", e.Message, nil)
		return
	}
	id := strings.TrimSpace(body.Challenge)
	if id == "" {
		writeSMSReject(w, http.StatusBadRequest, "bad_request", "缺少 challenge：请先点「发短信验证码」发起一次二次认证", nil)
		return
	}
	entry, ok := s.smsEntry(id)
	if !ok {
		writeSMSReject(w, http.StatusGone, "challenge_expired", smsChallengeExpiredMessage, nil)
		return
	}

	probe := s.store.Config()
	now := s.now()
	// 两把尺子取大的：会话自己的冷却（学校给的 sendInterval）与按手机号记的实测冷却
	// （smsSendCooldown，5 分钟）。只用前者时用户会在 2 分钟点一次、被学校拒一次。
	wait := entry.pending.ResendAfter(now)
	if w := smsSends.wait(s, entry.pending.PhoneMasked(), now); w > wait {
		wait = w
	}
	if wait > 0 {
		secs := smsSeconds(wait)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeSMSReject(w, http.StatusTooManyRequests, "too_frequent",
			fmt.Sprintf("还要等 %d 秒才能重发（学校那边实测要隔 %d 分钟才放行）", secs, int(smsSendCooldown.Minutes())),
			map[string]any{"challenge": id, "resend_after": secs})
		return
	}

	if _, err := entry.pending.SendCode(); err != nil {
		s.writeSMSSendFailure(w, id, entry.pending, err, probe, true)
		return
	}
	smsSends.mark(s, entry.pending.PhoneMasked(), s.now())

	s.logf("info", "", "面板短信验证：重发验证码 challenge=%s ip=%s", shortChallenge(id), s.clientIP(r))
	now = s.now()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"resend_after": smsSeconds(smsResendWait(entry.pending, now)),
		"expires_in":   smsSeconds(entry.pending.CodeExpire()),
	})
}

// handleSMSVerify 是 POST /api/v1/login/sms/verify：提交 6 位验证码，换 token 并落盘。
//
// 三条收尾纪律：
//   - 成功即删 challenge（单次有效）：验证码已经被学校消费掉，留着只会让下一次提交
//     得到一句莫名其妙的"验证码不对"；
//   - 连错 5 次也删（防爆破），每次响应都带 attempts_left；
//   - 落盘失败/init 不通过时**不写配置**，并同样把 challenge 删掉（这次验证码已经用掉了，
//     留着也换不出新 token）。
func (s *Server) handleSMSVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeSMSReject(w, e.Status, "bad_request", e.Message, nil)
		return
	}
	id := strings.TrimSpace(body.Challenge)
	if id == "" {
		writeSMSReject(w, http.StatusBadRequest, "bad_request", "缺少 challenge：请先点「发短信验证码」发起一次二次认证", nil)
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeSMSReject(w, http.StatusBadRequest, "bad_request", "验证码是空的：请把手机上收到的 6 位数字填进来", nil)
		return
	}
	entry, ok := s.smsEntry(id)
	if !ok {
		writeSMSReject(w, http.StatusGone, "challenge_expired", smsChallengeExpiredMessage, nil)
		return
	}

	probe := s.store.Config()
	token, err := entry.pending.VerifyCode(code)
	if err != nil {
		s.writeSMSVerifyFailure(w, id, err, probe)
		return
	}

	plan, _, initNote, apiErr := s.commitSMSToken(token, probe)
	// 无论落盘成功与否，这次验证码都已经被学校消费掉了：删掉句柄，避免用户拿着一个
	// 必然失败的 challenge 反复点。
	smsChallenges.drop(s, id)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.logf("info", "", "面板短信验证：验证码通过，token 已保存 ip=%s", s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"token_hint": maskSecret(token),
		"school_ok":  initNote == "",
		"plan":       plan,
		"message":    fmt.Sprintf("短信验证通过，晚点名 token 已保存：%s%s", plan, initNote),
	})
}

// writeSMSVerifyFailure 把"验证码提交"的失败分类成 400/409/410/429/502。
func (s *Server) writeSMSVerifyFailure(w http.ResponseWriter, id string, verifyErr error, probe model.Config) {
	switch {
	case errors.Is(verifyErr, fzu.ErrSMSCodeRejected):
		left := smsChallenges.failAttempt(s, id)
		s.logf("warn", "", "面板短信验证：验证码被拒 challenge=%s 剩余 %d 次", shortChallenge(id), left)
		// 第 5 次之后 attempts_left 归零，challenge 已被删除：再提交就是 410。
		writeSMSReject(w, http.StatusBadRequest, "code_rejected",
			"短信验证码不对或已过期", map[string]any{"attempts_left": left})
	case errors.Is(verifyErr, fzu.ErrSMSRateLimited):
		msg := s.sanitizeError(verifyErr.Error(), probe)
		secs := smsSeconds(s.smsResendWaitFor(id))
		if secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		writeSMSReject(w, http.StatusTooManyRequests, "too_frequent", msg,
			map[string]any{"challenge": id, "resend_after": secs})
	case errors.Is(verifyErr, fzu.ErrSMSPhoneUnbound):
		smsChallenges.drop(s, id)
		writeSMSReject(w, http.StatusConflict, "phone_unbound", smsPhoneUnboundMessage, nil)
	case errors.Is(verifyErr, fzu.ErrSMSSessionExpired):
		smsChallenges.drop(s, id)
		writeSMSReject(w, http.StatusGone, "challenge_expired", smsChallengeExpiredMessage, nil)
	default:
		msg := s.sanitizeError(verifyErr.Error(), probe)
		s.logf("warn", "", "面板短信验证：校验验证码失败 challenge=%s：%s", shortChallenge(id), msg)
		writeError(w, newError(http.StatusBadGateway, codeLoginFailed, "校验验证码失败："+msg))
	}
}

// smsResendWaitFor 查一条 challenge 的重发冷却（查不到就 0）。
func (s *Server) smsResendWaitFor(id string) time.Duration {
	entry, ok := s.smsEntry(id)
	if !ok {
		return 0
	}
	return entry.pending.ResendAfter(s.now())
}

// handleSMSCancel 是 POST /api/v1/login/sms/cancel：用户放弃这次短信验证。
//
// 幂等：challenge 不存在（已过期、已经成功、前端关弹窗时又发了一次）也回 200 ——
// 取消这个动作在用户眼里只有"关掉了"一种结果，多一种失败码只是多一次无意义的报错。
func (s *Server) handleSMSCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Challenge string `json:"challenge"`
	}
	if e := decodeJSON(w, r, &body); e != nil {
		writeSMSReject(w, e.Status, "bad_request", e.Message, nil)
		return
	}
	if id := strings.TrimSpace(body.Challenge); id != "" {
		if _, ok := s.smsEntry(id); ok {
			s.logf("info", "", "面板短信验证：用户取消了 challenge=%s ip=%s", shortChallenge(id), s.clientIP(r))
		}
		smsChallenges.drop(s, id)
	}
	writeOK(w, http.StatusOK)
}
