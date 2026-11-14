// Package server 把五个基础包装配成一个能常驻的进程：HTTP 路由与中间件、鉴权、
// 调度循环、一次签到的完整编排（flow）和 SSE 实时日志。
//
// 这一层刻意不做的事：协议细节（fzu）、时间语义（schedule）、校验/脱敏（config）、
// 落盘（store）统统留在各自包里。这里只负责把它们按"一次请求 / 一次 tick"串起来，
// 以及所有**跨请求共享状态**的并发安全——进程是单实例的，但 HTTP handler、
// 调度 goroutine、SSE 广播、进行中的 flow 是并发的。
//
// 契约：docs/go-service-design.md §3 §5.2 §8 §9 §10 §11.3 §12。
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
	"fzucheckin/internal/secure"
	"fzucheckin/internal/store"
	"fzucheckin/web"
)

// Version 是服务版本号，/health 与 /status 都会带出去。
const Version = "1.0.0"

// dateLayout 是 state.json / DayPlan 里日期的唯一写法。
const dateLayout = "2006-01-02"

// 时序常量。放在这里而不是散在各处，是为了让"最长要等多久"一眼可见：
// 优雅退出的最坏耗时 ≈ httpShutdownGrace + runWaitGrace（默认 25s+60s）。
const (
	// httpShutdownGrace 是停止接受新请求后给在飞请求的时间。
	// 取 25s 而不是 60s：SSE 长连接永远等不到自然结束，Shutdown 到点后由 Close 强制断开。
	httpShutdownGrace = 25 * time.Second
	// runWaitGrace 是等待进行中签到的时间上限（§10.3 规定 60s）。
	runWaitGrace = 60 * time.Second
)

// Deps 是装配参数。除 Store 外全部可为零值：零值即生产默认。
//
// 后四个字段是给测试的注入点，生产留 nil——学校上游、SSO、推送网关都是外部网络，
// 端到端测试必须能把它们换成 httptest，否则测试就得联网，而联网的测试等于随机失败。
type Deps struct {
	Store  *store.Store
	Logger *log.Logger
	// Now 允许测试把"现在"钉死在某个时刻（窗口内/窗口外/已过末档）。
	// 生产为 nil → schedule.NowCN，保证与调度内核用的是同一个时间基准。
	Now func() time.Time

	ClientFactory    func(token string) *fzu.Client
	SSOLogin         func(user, pass string) (string, error)
	Notifier         func(cfg model.Config, title, content string) error
	DisableScheduler bool

	// TrustProxy 表示"这个进程前面确实有可信反向代理"（FZU_TRUST_PROXY=1）。
	// 只有它为真时才采信 X-Forwarded-For；默认 false 一律用 RemoteAddr 做限速身份，
	// 否则任何客户端换一个 XFF 头就绕过了登录限速。
	TrustProxy bool
}

// Server 是一个装配好的服务实例。零值不可用，必须经 New 构造。
type Server struct {
	store  *store.Store
	logger *log.Logger
	now    func() time.Time
	signer secure.Signer
	hub    *sseHub
	limits *loginLimiter
	static fs.FS

	clientFactory func(token string) *fzu.Client
	ssoLogin      func(user, pass string) (string, error)
	notifier      func(cfg model.Config, title, content string) error

	schedulerDisabled bool
	trustProxy        bool
	startedAt         time.Time

	// addr 可被 main 的 -listen 覆盖（SetListen）。用锁而不是只写一次：
	// 横幅打印（goroutine）与 Run 读取可能并发。
	addrMu sync.Mutex
	addr   string

	// 日志阈值。默认 info：debug 会把每个请求都打一行，常驻服务不需要。
	levelMu  sync.RWMutex
	minLevel logLevel

	// running 是单飞开关：调度器与手动触发共用同一把。用 atomic 而不是 mutex，
	// 是因为它只在"进入/退出一次运行"时翻转，读多写少且绝不能在持有时阻塞。
	running atomic.Bool
	// closing 在优雅退出时置位：此后再也不接受新的运行，避免 WaitGroup 的
	// "Wait 开始后还 Add" 这种未定义行为。
	closing atomic.Bool
	runWG   sync.WaitGroup

	// 调度器自身的可观测状态（/status 会读）。
	schedMu      sync.Mutex
	stopped      bool
	lastDecision string
	// stateMu 串行化 state.json 的"读-改-写"。store.SaveState 是整份覆盖写，
	// 两个 goroutine 各自"读→改→写"会互相吞掉对方刚写进去的字段（典型表现是
	// DayCache 被一条并发的 LastSlotKey 更新覆盖掉）。
	stateMu sync.Mutex
	// setupMu 串行化"检查未初始化 + 写密码哈希"。两个并发的 setup 若不串行，
	// 后一个会在"检查"与"写入"之间被前一个插队，结果是两个都能过检查、密码被覆盖两次。
	setupMu sync.Mutex

	// wake 是"立刻重新规划"的信号（配置保存后、手动触发收到时）。
	// 容量 1 + 非阻塞发送：通知迟到可以合并，但绝不能因为没人在听而卡住 handler。
	wake chan struct{}

	httpMu  sync.Mutex
	httpSrv *http.Server
}

// New 读配置、准备签名器/SSE hub/限速器，返回可用的 Server。
//
// 这里只做"进程级"的准备，不监听端口：httptest 需要直接拿 Handler()，
// 监听是 Run 的事（main 才有信号处理与横幅）。
func New(deps Deps) (*Server, error) {
	if deps.Store == nil {
		return nil, errors.New("server.New 需要非空的 Deps.Store")
	}
	now := deps.Now
	if now == nil {
		// 与调度内核同一个时间基准：本机时区一旦参与，21:35 就取决于跑在哪台机器上。
		now = schedule.NowCN
	}
	logger := deps.Logger
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	key, err := deps.Store.SessionKey()
	if err != nil {
		return nil, fmt.Errorf("读取会话密钥失败: %w", err)
	}
	staticFS, err := fs.Sub(web.FS, web.StaticDir)
	if err != nil {
		return nil, fmt.Errorf("内嵌前端资源不可用: %w", err)
	}

	factory := deps.ClientFactory
	if factory == nil {
		factory = fzu.NewClient
	}
	sso := deps.SSOLogin
	if sso == nil {
		sso = fzu.Login
	}
	notifier := deps.Notifier
	if notifier == nil {
		// httpc 传 nil：fzu.Notify 会自己造一个 15s 超时的客户端。
		// 不传接口客户端——它带着晚点名 token 头，不该发给第三方推送网关。
		notifier = func(cfg model.Config, title, content string) error {
			return fzu.Notify(cfg, title, content, nil)
		}
	}

	s := &Server{
		store:             deps.Store,
		trustProxy:        deps.TrustProxy,
		logger:            logger,
		now:               now,
		signer:            secure.Signer{Key: key},
		hub:               newSSEHub(maxSSEClients),
		limits:            newLoginLimiter(loginWindow, loginMaxFailures, now),
		static:            staticFS,
		clientFactory:     factory,
		ssoLogin:          sso,
		notifier:          notifier,
		schedulerDisabled: deps.DisableScheduler,
		startedAt:         now(),
		minLevel:          levelInfo,
		wake:              make(chan struct{}, 1),
	}
	cfg := deps.Store.Config()
	s.addr = strings.TrimSpace(cfg.Server.Listen)
	if s.addr == "" {
		s.addr = config.DefaultListen
	}
	s.setSchedulerStopped(deps.DisableScheduler)
	s.logf("info", "", "服务初始化完成 version=%s data=%s listen=%s scheduler=%s",
		Version, deps.Store.Dir, s.addr, enabledWord(!deps.DisableScheduler))
	return s, nil
}

// Handler 返回完整的 HTTP 处理链，供 httptest 直接使用。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	// 中间件顺序即 §B 的顺序：recover → 安全头 → CORS → 请求日志 → 鉴权/CSRF。
	// recover 必须在最外层，否则内层任何一个 panic 都会打掉整条连接而没有日志。
	return s.recoverMW(s.securityHeadersMW(s.corsMW(s.requestLogMW(s.guardMW(mux)))))
}

// Run 启动调度器与 HTTP 服务，ctx 取消后优雅关闭并返回。
//
// 关闭顺序是刻意的：先停调度器（不再产生新运行）→ 再停 HTTP（不再接受新请求，
// 顺带断开 SSE）→ 然后等进行中的 flow 最多 60s（一次打卡可能正卡在 clockIn，
// 半途杀掉就会留下"学校端签了、本地没记成功"的错位）→ 最后落一次 state。
func (s *Server) Run(ctx context.Context) error {
	addr := s.Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// 加固参数（§8）：全部显式给值，不依赖 http.Server 的零值（零值 = 永不超时）。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// 30s 写超时对 SSE 是致命的，所以 SSE handler 会自己把写截止时间清掉，
		// 见 handleLogStream 里的 ResponseController.SetWriteDeadline。
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	s.httpMu.Lock()
	s.httpSrv = srv
	s.httpMu.Unlock()

	schedCtx, cancelSched := context.WithCancel(ctx)
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		s.runScheduler(schedCtx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	s.logf("info", "", "HTTP 服务已就绪 http://%s", displayAddr(addr))

	var runErr error
	select {
	case <-ctx.Done():
		s.logf("info", "", "收到退出信号，开始优雅关闭")
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("HTTP 服务异常退出: %w", err)
			s.logf("error", "", "%v", runErr)
		}
	}

	cancelSched()
	<-schedDone

	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	// Shutdown 不会主动断开 SSE 这类长连接，它只是等；等不到就强制关。
	_ = srv.Close()

	s.waitRuns(runWaitGrace)

	// 关停期间调度器可能刚清过 last_slot_key、flow 可能刚写过 day_cache，
	// 这里再整份写一次，保证"进程停下时的 state.json"就是内存里的最终形态。
	if st := s.store.State(); true {
		if err := s.store.SaveState(st); err != nil {
			s.logf("warn", "", "退出时写入 state.json 失败：%v", err)
		}
	}
	s.logf("info", "", "服务已停止")
	return runErr
}

// Addr 返回当前监听地址（Run 之前调用可打印横幅）。
func (s *Server) Addr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.addr
}

// SetListen 让 main 的 -listen 参数优先于配置里的 server.listen。
//
// 只影响本次进程的监听地址，绝不回写 config.json：一次启动参数（或 systemd unit
// 里的覆盖）不该改掉用户拖到配置文件里的值，否则下次不带参数启动就跑到了别处。
func (s *Server) SetListen(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	s.addrMu.Lock()
	s.addr = addr
	s.addrMu.Unlock()
	s.logf("info", "", "监听地址已被命令行参数覆盖为 %s", addr)
}

// SetLogLevel 接受 debug|info|warn|error（大小写不敏感）。
// 无法识别的值保持原阈值并留一条警告：宁可多打日志，也不要因为一个拼错的
// FZU_LOG_LEVEL 把日志彻底关掉。
func (s *Server) SetLogLevel(level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return
	}
	parsed, ok := lookupLogLevel(level)
	if !ok {
		s.logf("warn", "", "无法识别的日志级别 %q（可选 debug|info|warn|error），继续使用当前级别", level)
		return
	}
	s.levelMu.Lock()
	s.minLevel = parsed
	s.levelMu.Unlock()
}

// Store 暴露底层 store，便于 main 打印数据目录等只读信息。
func (s *Server) Store() *store.Store { return s.store }

// ---------- 日志 ----------

// logLevel 是日志阈值。级别只影响 stdout：内存环与 SSE 永远收全量
// ——前端"日志"页就是调试入口，按阈值过滤掉 warn 以下的记录等于让人猜。
type logLevel int

const (
	levelDebug logLevel = iota
	levelInfo
	levelWarn
	levelError
)

func lookupLogLevel(name string) (logLevel, bool) {
	switch name {
	case "debug":
		return levelDebug, true
	case "info":
		return levelInfo, true
	case "warn", "warning":
		return levelWarn, true
	case "error":
		return levelError, true
	}
	return levelInfo, false
}

func (l logLevel) allows(msg logLevel) bool { return msg >= l }

func levelOf(name string) logLevel {
	if l, ok := lookupLogLevel(strings.ToLower(strings.TrimSpace(name))); ok {
		return l
	}
	return levelInfo
}

// logf 写一条日志：内存环（/logs 与 SSE 的数据源）+ stdout。
//
// 调用方负责保证 message 里没有 token/密码/推送 key（见 sanitizeError）；
// 这个函数不做二次脱敏——它拿不到配置，也就不该假装自己能判断什么算敏感。
func (s *Server) logf(level, runID, format string, args ...any) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		level = "info"
	}
	msg := fmt.Sprintf(format, args...)
	ev := model.LogEvent{TS: s.now(), Level: level, RunID: runID, Msg: msg}
	s.store.AppendLog(ev)
	s.hub.broadcast(eventLog, ev)

	s.levelMu.RLock()
	min := s.minLevel
	s.levelMu.RUnlock()
	if s.logger != nil && min.allows(levelOf(level)) {
		// 格式固定成"时间 级别 run=… msg=…"，便于 grep/sort（§16）。
		s.logger.Printf("%s %-5s run=%s msg=%s",
			ev.TS.Format(time.RFC3339), strings.ToUpper(level), orDash(runID), msg)
	}
}

// ---------- 共享状态 ----------

// setLastDecision 记录最近一次调度结论（/status 的 scheduler.last_decision）。
func (s *Server) setLastDecision(reason string) {
	s.schedMu.Lock()
	s.lastDecision = reason
	s.schedMu.Unlock()
}

func (s *Server) lastDecisionValue() string {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.lastDecision
}

func (s *Server) setSchedulerStopped(v bool) {
	s.schedMu.Lock()
	s.stopped = v
	s.schedMu.Unlock()
}

func (s *Server) schedulerStopped() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.stopped
}

func (s *Server) schedulerEnabled() bool { return !s.schedulerDisabled }

// tryBeginRun 抢占单飞开关。手动触发与调度器共用：两个入口都"看见空闲才进"，
// 所以同一时刻最多只有一次打卡——重复打卡在上游看来就是脚本行为。
func (s *Server) tryBeginRun() bool {
	if s.closing.Load() {
		return false
	}
	return s.running.CompareAndSwap(false, true)
}

func (s *Server) endRun() { s.running.Store(false) }

// waitRuns 等待进行中的 flow 结束，最多 grace。
func (s *Server) waitRuns(grace time.Duration) {
	done := make(chan struct{})
	go func() {
		s.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		s.logf("warn", "", "仍有运行在进行，等待超过 %s，先退出（记录可能不完整）", grace)
	}
}

// updateState 串行地"读-改-写"state.json。
//
// 为什么必须串行：store.SaveState 是整份覆盖，flow 与调度器若各自读到旧副本再写回，
// 后写的一方会把对方刚更新的字段（DayCache / LastSlotKey）抹掉，而这两种丢失都不会
// 报错，只会在第二天表现为"仪表盘没有计划"或"同一档重复签"。
func (s *Server) updateState(mut func(*model.State)) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st := s.store.State()
	if st.Notified == nil {
		st.Notified = map[string][]string{}
	}
	mut(&st)
	if err := s.store.SaveState(st); err != nil {
		s.logf("warn", "", "写入 state.json 失败：%v", err)
	}
}

// wakeScheduler 通知调度器"配置变了，立刻重新规划"。
// 非阻塞：wake 里已经有一条待处理的通知就够了，多发的信号没有额外信息量。
func (s *Server) wakeScheduler(reason string) {
	if reason != "" {
		s.logf("debug", "", "唤醒调度器：%s", reason)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ---------- 小工具 ----------

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// displayAddr 把 ":8787" / "0.0.0.0:8787" 之类的监听地址变成能直接点开的 URL。
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// staticContentType 决定静态资源的 Content-Type。
// 显式覆盖 .js：不同 Go 版本对 js 的 MIME 有差异（application/javascript vs
// text/javascript），浏览器两种都认，但测试与前端调试都需要一个确定值。
func staticContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
