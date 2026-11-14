package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
	"fzucheckin/internal/schedule"
	"fzucheckin/internal/store"
)

// 测试共用常量。密码必须 ≥8 字符（§8）。
const (
	testPassword  = "test-pass-123"
	testUser      = "20210001"
	testSchoolPwd = "school-pass-xyz"
	testTime      = "21:35"
)

// cstTest 是测试里显式使用的北京时间，避免依赖宿主机时区。
var cstTest = time.FixedZone("CST", 8*3600)

// ============ 假学校上游（httptest，绝不联网） ============

// fakeSchool 模拟晚点名 4 个接口。所有计数都加锁：handler 跑在服务端 goroutine 里，
// 测试主 goroutine 同时在读计数，无锁读在 -race 下必报。
type fakeSchool struct {
	srv *httptest.Server

	mu             sync.Mutex
	initCalls      int
	checkCalls     int
	clockInCalls   int
	timestampCalls int
	lastToken      string
	lastClockIn    model.ClockInParams
	initFail       bool
	checkFail      bool
	clockInFail    bool
	checked        bool
	noPlan         bool
	noCheckHit     bool
	lateLine       string
	gate           *schoolGate
}

// schoolGate 让 init 请求挂住，用来构造"运行中"的并发场景。
type schoolGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newSchoolGate() *schoolGate {
	return &schoolGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func newFakeSchool(t *testing.T) *fakeSchool {
	t.Helper()
	f := &fakeSchool{lateLine: "22:30"}
	mux := http.NewServeMux()
	mux.HandleFunc("/livecloud/project/fzu/attn/init.action", f.handleInit)
	mux.HandleFunc("/livecloud/project/fzu/attn/check.action", f.handleCheck)
	mux.HandleFunc("/livecloud/project/fzu/attn/clockIn.action", f.handleClockIn)
	mux.HandleFunc("/livecloud/project/fzu/attn/currentTimestamp.action", f.handleTimestamp)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{"success": true, "code": 1, "msg": "", "data": data}
	_ = json.NewEncoder(w).Encode(payload)
}

func (f *fakeSchool) handleInit(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.initCalls++
	f.lastToken = r.Header.Get("token")
	gate := f.gate
	fail := f.initFail
	checked := f.checked
	noPlan := f.noPlan
	late := f.lateLine
	f.mu.Unlock()

	if gate != nil {
		gate.once.Do(func() { close(gate.entered) })
		<-gate.release
	}
	if fail {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
		return
	}

	state := "未签到"
	if checked {
		state = "SUCCESS"
	}
	data := map[string]any{
		"userData": map[string]any{
			"FState":         state,
			"FDefaultCampus": "campus-1",
			"FCollUnit":      "unit-9",
			"FWay":           1,
		},
		"paramsData": map[string]any{
			"FStartTime": "21:00",
			"FEndTime":   "23:59",
			"FLateTime":  late,
		},
	}
	if !noPlan {
		polygon := `{"range":[[[119.20,26.04],[119.21,26.04],[119.21,26.05],[119.20,26.05]]]}`
		data["schoolData"] = []any{map[string]any{"FPosition": polygon}}
	}
	writeEnvelope(w, data)
}

func (f *fakeSchool) handleCheck(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.checkCalls++
	fail := f.checkFail
	noHit := f.noCheckHit
	f.mu.Unlock()

	if fail {
		http.Error(w, "check exploded", http.StatusInternalServerError)
		return
	}
	if noHit {
		writeEnvelope(w, []any{})
		return
	}
	writeEnvelope(w, []any{map[string]any{"id": "campus-1", "name": "旗山校区"}})
}

func (f *fakeSchool) handleClockIn(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.clockInCalls++
	fail := f.clockInFail
	f.mu.Unlock()

	if fail {
		http.Error(w, "clockIn exploded", http.StatusInternalServerError)
		return
	}
	writeEnvelope(w, map[string]any{"FState": "SUCCESS"})
}

func (f *fakeSchool) handleTimestamp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.timestampCalls++
	f.mu.Unlock()
	writeEnvelope(w, map[string]any{"timestamp": time.Now().UnixMilli()})
}

func (f *fakeSchool) counts() (init, check, clockIn int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initCalls, f.checkCalls, f.clockInCalls
}

func (f *fakeSchool) setInitFail(v bool) {
	f.mu.Lock()
	f.initFail = v
	f.mu.Unlock()
}

func (f *fakeSchool) setChecked(v bool) {
	f.mu.Lock()
	f.checked = v
	f.mu.Unlock()
}

func (f *fakeSchool) setNoPlan(v bool) {
	f.mu.Lock()
	f.noPlan = v
	f.mu.Unlock()
}

func (f *fakeSchool) setNoCheckHit(v bool) {
	f.mu.Lock()
	f.noCheckHit = v
	f.mu.Unlock()
}

func (f *fakeSchool) setGate(g *schoolGate) {
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
}

// ============ 测试环境 ============

type testEnv struct {
	t      *testing.T
	dir    string
	store  *store.Store
	srv    *Server
	ts     *httptest.Server
	school *fakeSchool

	mu          sync.Mutex
	nowFixed    time.Time
	nowSet      bool
	ssoCalls    int
	ssoToken    string
	ssoErr      error
	notifyCalls []string
	notifyErr   error
}

type envOption func(*Deps)

// withScheduler 打开调度器（默认关闭：绝大多数 HTTP 用例不该有后台 ticker 干扰）。
func withScheduler() envOption { return func(d *Deps) { d.DisableScheduler = false } }

func newTestEnv(t *testing.T, opts ...envOption) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("打开临时数据目录失败: %v", err)
	}
	env := &testEnv{
		t:        t,
		dir:      dir,
		store:    st,
		school:   newFakeSchool(t),
		ssoToken: "sso-token-0123456789abcdef0123456789",
	}
	deps := Deps{
		Store: st,
		// 日志写进 io.Discard：内存环与 SSE 仍然收全量，只是不污染 go test 输出。
		Logger: log.New(io.Discard, "", 0),
		Now:    env.now,
		ClientFactory: func(token string) *fzu.Client {
			c := fzu.NewClient(token)
			c.BaseURL = env.school.srv.URL
			return c
		},
		SSOLogin: func(user, pass string) (string, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.ssoCalls++
			if env.ssoErr != nil {
				return "", env.ssoErr
			}
			return env.ssoToken, nil
		},
		Notifier: func(cfg model.Config, title, content string) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.notifyCalls = append(env.notifyCalls, title)
			return env.notifyErr
		},
		DisableScheduler: true,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	srv, err := New(deps)
	if err != nil {
		t.Fatalf("装配 server 失败: %v", err)
	}
	env.srv = srv
	env.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(env.ts.Close)
	return env
}

func (e *testEnv) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nowSet {
		return e.nowFixed
	}
	return schedule.NowCN()
}

// setNow 把"现在"钉死在某个时刻（北京时间）。
func (e *testEnv) setNow(t time.Time) {
	e.mu.Lock()
	e.nowFixed = t.In(cstTest)
	e.nowSet = true
	e.mu.Unlock()
}

func (e *testEnv) today() string { return e.now().Format(dateLayout) }

func (e *testEnv) ssoCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ssoCalls
}

func (e *testEnv) notifyTitles() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notifyCalls...)
}

func (e *testEnv) setNotifierErr(err error) {
	e.mu.Lock()
	e.notifyErr = err
	e.mu.Unlock()
}

func (e *testEnv) setSSOErr(err error) {
	e.mu.Lock()
	e.ssoErr = err
	e.mu.Unlock()
}

// ============ 配置 ============

// baseConfig 是一份"能通过 config.Validate"的最小可用配置。
// 显式关掉位置抖动（jitter=0）：预检/打卡的 check 次数才可断言。
func baseConfig() model.Config {
	cfg := config.DefaultConfig()
	cfg.User.Username = testUser
	cfg.User.Password = testSchoolPwd
	cfg.Checkin.Longitude = "119.20463"
	cfg.Checkin.Latitude = "26.04920"
	cfg.Checkin.ActualLocation = "旗山校区生活区3号楼"
	cfg.Checkin.JitterRadiusM = float64Ptr(0)
	cfg.Checkin.LocationJitterM = nil
	cfg.Checkin.Schedule.Times = []string{testTime}
	cfg.Notify.Type = "none"
	return cfg
}

func float64Ptr(v float64) *float64 { return &v }

func (e *testEnv) saveConfig(cfg model.Config) int64 {
	e.t.Helper()
	version, err := e.store.SaveConfig(cfg, nil)
	if err != nil {
		e.t.Fatalf("保存测试配置失败: %v", err)
	}
	return version
}

// ============ HTTP 客户端 ============

func (e *testEnv) client() *http.Client {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("创建 cookie jar 失败: %v", err)
	}
	return &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		// 不跟随重定向：本服务不发重定向，跟随只会掩盖问题。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type httpResult struct {
	status int
	body   []byte
	header http.Header
}

// do 发一个带 CSRF 头的请求（前端 apiFetch 的行为）。
func (e *testEnv) do(c *http.Client, method, path string, payload any) httpResult {
	e.t.Helper()
	return e.doRaw(c, method, path, payload, true)
}

// doRaw 可以显式关掉 CSRF 头，用于验证 403。
func (e *testEnv) doRaw(c *http.Client, method, path string, payload any, csrf bool) httpResult {
	e.t.Helper()
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(csrfHeaderName, csrfHeaderValue)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读取响应失败: %v", err)
	}
	return httpResult{status: resp.StatusCode, body: body, header: resp.Header.Clone()}
}

// authedClient 完成"设置密码或登录"，返回带 Cookie 的客户端。
func (e *testEnv) authedClient() *http.Client {
	e.t.Helper()
	c := e.client()
	if e.store.PasswordHash() == "" {
		res := e.do(c, http.MethodPost, apiPrefix+"/auth/setup", map[string]string{"password": testPassword})
		if res.status != http.StatusOK {
			e.t.Fatalf("初始化密码失败: %d %s", res.status, res.body)
		}
		return c
	}
	res := e.do(c, http.MethodPost, apiPrefix+"/auth/login", map[string]string{"password": testPassword})
	if res.status != http.StatusOK {
		e.t.Fatalf("登录失败: %d %s", res.status, res.body)
	}
	return c
}

// ============ 断言与解码 ============

func decodeBody(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n原文: %s", err, truncateForLog(body))
	}
}

func jsonMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	decodeBody(t, body, &out)
	return out
}

// errCode 取错误信封里的 code。
func errCode(t *testing.T, body []byte) string {
	t.Helper()
	env := jsonMap(t, body)
	raw, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 error 信封: %s", truncateForLog(body))
	}
	code, _ := raw["code"].(string)
	return code
}

func truncateForLog(body []byte) string {
	const limit = 400
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}

func (e *testEnv) expectStatus(res httpResult, want int, what string) {
	e.t.Helper()
	if res.status != want {
		e.t.Fatalf("%s: 期望 HTTP %d，实际 %d\n响应: %s", what, want, res.status, truncateForLog(res.body))
	}
}

// ============ 等待辅助 ============

// waitRun 轮询到某条运行记录落盘完成，并等单飞开关释放。
//
// 只等记录落盘是不够的：finishRun 是 flowRun 的 defer，记录写完之后
// "running=false" 还要等 flowRun 真正返回。测试紧接着再发一次 POST /runs
// 就会撞上 409——那是正确行为，不是 bug，但会让用例变成偶发失败。
func (e *testEnv) waitRun(id string, timeout time.Duration) model.RunRecord {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec, ok := e.store.RunByID(id); ok && !rec.FinishedAt.IsZero() {
			e.waitIdle(timeout)
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("等待运行 %s 完成超时（%s）", id, timeout)
	return model.RunRecord{}
}

// waitIdle 等到单飞开关被释放。
func (e *testEnv) waitIdle(timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !e.srv.running.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("等待运行结束超时（%s）", timeout)
}

// waitHubClients 等到 SSE 连接数达到 want（或掉到 want）。
func (e *testEnv) waitHubClients(want int, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.srv.hub.count() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("等待 SSE 连接数变成 %d 超时（当前 %d）", want, e.srv.hub.count())
}

// waitValue 轮询直到 cond 成立（用于跨 goroutine 的条件断言）。
func waitValue(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件成立超时（%s）", what)
}

// waitSchedulerStarted 等调度循环真正跑起来。
// 判据是它写的启动日志而不是内存标志位：标志位在 New 里就可能已经是"未停止"，
// 那样等待会立刻通过，测试就失去意义了。
func waitSchedulerStarted(t *testing.T, env *testEnv) {
	t.Helper()
	waitValue(t, 3*time.Second, "调度器已写出启动日志", func() bool {
		for _, event := range env.store.Logs(100) {
			if strings.Contains(event.Msg, "调度器已启动") {
				return true
			}
		}
		return false
	})
}

// ============ SSE 客户端 ============

type sseFrame struct {
	name string
	data string
}

type sseStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	frames chan sseFrame
	done   chan struct{}
	once   sync.Once
}

// openStream 建立一条 SSE 连接并起一个泵把帧送进 channel。
func (e *testEnv) openStream(c *http.Client) *sseStream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+apiPrefix+"/logs/stream", nil)
	if err != nil {
		cancel()
		e.t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		e.t.Fatalf("连接 SSE 失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		e.t.Fatalf("SSE 返回 %d: %s", resp.StatusCode, truncateForLog(body))
	}
	s := &sseStream{resp: resp, cancel: cancel, frames: make(chan sseFrame, 128), done: make(chan struct{})}
	go s.pump()
	e.t.Cleanup(s.close)
	return s
}

func (s *sseStream) pump() {
	defer close(s.done)
	reader := bufio.NewReader(s.resp.Body)
	var name strings.Builder
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if name.Len() > 0 || data.Len() > 0 {
					s.frames <- sseFrame{name: name.String(), data: data.String()}
				}
				name.Reset()
				data.Reset()
			case strings.HasPrefix(trimmed, "event:"):
				name.Reset()
				name.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")))
			case strings.HasPrefix(trimmed, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *sseStream) next(t *testing.T, timeout time.Duration) sseFrame {
	t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case <-time.After(timeout):
		t.Fatalf("等待 SSE 帧超时（%s）", timeout)
		return sseFrame{}
	}
}

func (s *sseStream) close() {
	s.once.Do(func() {
		s.cancel()
		_ = s.resp.Body.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	})
}

// describe 只用于失败信息。
func describe(rec model.RunRecord) string {
	return fmt.Sprintf("id=%s mode=%s status=%s decision=%s error=%s",
		rec.ID, rec.Mode, rec.Status, rec.Decision, rec.Error)
}

// encodeSessionForTest 用服务端会话密钥给一段载荷签名，用来构造"签名正确但内容可疑"
// 的 Cookie（过期、sub 不对）。前端拿不到密钥，所以这不是可被利用的伪造路径。
func encodeSessionForTest(s *Server, payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." + s.signer.Sign(payload)
}
