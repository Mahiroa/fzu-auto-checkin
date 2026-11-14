package fzu

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"fzucheckin/internal/secure"
)

// checkCall 是一次 check.action 请求解密后的入参快照。
type checkCall struct {
	Lon     float64
	Lat     float64
	Range   string
	MapType string
	Token   string
	Method  string
	// Plain 是解密后的完整明文，RawParam 是原样收到的 param 值（双重编码态），
	// Cipher 是解两层后拿到的 Base64 密文 —— 三者一起才能同时锁住
	// 字段顺序、双重编码、以及 AES 输入。
	Plain    string
	RawParam string
	Cipher   string
}

// fakeAttn 是一个"假学校"：把 4 个接口挂在本地 httptest.Server 上，
// 测试逐次改它的行为（第几次 check 越界、服务器时间是多少），全程零联网。
type fakeAttn struct {
	srv *httptest.Server
	t   *testing.T

	mu      sync.Mutex
	checks  []checkCall
	clocks  []string // 每次 clockIn 的明文 JSON
	inits   int
	tsCalls int

	// checkFn 决定第 index 次（从 0 起）check 返回的 data 原文，默认全部越界。
	checkFn func(index int, call checkCall) string
	// tsMs 是 currentTimestamp 的返回值；tsFail 为真时回 500。
	tsMs   int64
	tsFail bool
}

func newFakeAttn(t *testing.T) *fakeAttn {
	t.Helper()
	f := &fakeAttn{
		t:       t,
		checkFn: func(int, checkCall) string { return "[]" },
		tsMs:    1763201460000, // 2026-11-15 00:51:00 +08 附近，具体值不参与断言
	}
	mux := http.NewServeMux()
	mux.HandleFunc(apiPrefix+"/init.action", f.handleInit)
	mux.HandleFunc(apiPrefix+"/check.action", f.handleCheck)
	mux.HandleFunc(apiPrefix+"/clockIn.action", f.handleClockIn)
	mux.HandleFunc(apiPrefix+"/currentTimestamp.action", f.handleTimestamp)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client 造一个指向假上游的客户端。
func (f *fakeAttn) client(token string) *Client {
	c := NewClient(token)
	c.BaseURL = f.srv.URL
	return c
}

func (f *fakeAttn) handleInit(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inits++
	f.mu.Unlock()

	if r.Method != http.MethodPost {
		http.Error(w, "want POST", http.StatusMethodNotAllowed)
		return
	}
	body, _ := json.Marshal(map[string]any{"success": true, "code": 1, "data": map[string]any{
		"userData":   map[string]any{"FState": "未签到", "FDefaultCampus": "DEF", "FCollUnit": "CU", "FWay": 1},
		"paramsData": map[string]any{"FStartTime": "21:30", "FEndTime": "23:59", "FLateTime": "22:30"},
	}})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (f *fakeAttn) handleCheck(w http.ResponseWriter, r *http.Request) {
	plain, cipher, raw := decodeParam(f.t, r)
	var payload struct {
		Lon float64 `json:"longitude"`
		Lat float64 `json:"latitude"`
		Rng string  `json:"range"`
		Map string  `json:"mapType"`
	}
	if err := json.Unmarshal([]byte(plain), &payload); err != nil {
		f.t.Errorf("check 明文不是合法 JSON: %v（原文 %s）", err, plain)
	}
	call := checkCall{
		Lon: payload.Lon, Lat: payload.Lat, Range: payload.Rng, MapType: payload.Map,
		Token: r.Header.Get("token"), Method: r.Method,
		Plain: plain, RawParam: raw, Cipher: cipher,
	}

	f.mu.Lock()
	index := len(f.checks)
	f.checks = append(f.checks, call)
	fn := f.checkFn
	f.mu.Unlock()

	data := "[]"
	if fn != nil {
		data = fn(index, call)
	}
	writeEnvelope(w, data)
}

func (f *fakeAttn) handleClockIn(w http.ResponseWriter, r *http.Request) {
	plain, _, _ := decodeParam(f.t, r)
	f.mu.Lock()
	f.clocks = append(f.clocks, plain)
	f.mu.Unlock()
	writeEnvelope(w, `{"attn":{"id":"X"}}`)
}

func (f *fakeAttn) handleTimestamp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tsCalls++
	fail, ms := f.tsFail, f.tsMs
	f.mu.Unlock()
	if fail {
		http.Error(w, "upstream down", http.StatusInternalServerError)
		return
	}
	writeEnvelope(w, `{"timestamp":`+itoa(ms)+`}`)
}

// snapshotChecks 返回已记录的 check 入参副本。
func (f *fakeAttn) snapshotChecks() []checkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]checkCall(nil), f.checks...)
}

// snapshotClocks 返回已记录的 clockIn 明文副本。
func (f *fakeAttn) snapshotClocks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clocks...)
}

func (f *fakeAttn) initCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inits
}

// writeEnvelope 按对端的信封格式回包。
func writeEnvelope(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true,"code":1,"msg":"","data":` + data + `}`))
}

// decodeParam 取 param 查询参数、解两层百分号编码、再解密，
// 返回「明文 JSON / Base64 密文 / 原样收到的 param 值」。
//
// 这里的"解两层"本身就是对**双重编码**的断言：只解一层拿到的仍是
// percent-encoded 文本，直接丢给 AES 会立刻报 base64 非法。
func decodeParam(t *testing.T, r *http.Request) (plain, cipher, rawParam string) {
	t.Helper()
	raw := r.URL.RawQuery
	const prefix = "param="
	if !strings.HasPrefix(raw, prefix) {
		t.Errorf("查询串不是 param= 开头: %q", raw)
		return "{}", "", ""
	}
	rawParam = strings.TrimPrefix(raw, prefix)
	once, err := url.QueryUnescape(rawParam)
	if err != nil {
		t.Errorf("第一层百分号解码失败: %v", err)
		return "{}", "", rawParam
	}
	cipher, err = url.QueryUnescape(once)
	if err != nil {
		t.Errorf("第二层百分号解码失败（说明只编码了一次）: %v", err)
		return "{}", "", rawParam
	}
	plain, err = secure.DecryptParams(cipher)
	if err != nil {
		t.Errorf("param 解密失败（密文可能被改写）: %v", err)
		return "{}", cipher, rawParam
	}
	return plain, cipher, rawParam
}

// itoa 拼时间戳回包用。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// scriptedDraw 按给定序列循环返回随机数，用来把抖动/加权抽签钉死在确定结果上。
func scriptedDraw(us ...float64) RandFunc {
	index := 0
	return func() float64 {
		u := us[index%len(us)]
		index++
		return u
	}
}

// seededDraw 给分布类用例用的确定性伪随机源（固定种子，跨平台一致）。
func seededDraw(seed int64) RandFunc {
	r := rand.New(rand.NewSource(seed))
	return r.Float64
}

// ptrFloat 造 *float64（model 里用指针区分"没配"与"显式 0"）。
func ptrFloat(v float64) *float64 { return &v }

// capture 是 handler goroutine 与测试 goroutine 之间传值的带锁小容器。
//
// 为什么必须加锁：逻辑上"响应发出之后测试才读"是对的，但 race detector 不认
// socket 作为同步边，裸变量共享会被判成数据竞争。测试本身就是验收的一部分，
// 不能自己先脏。
type capture[T any] struct {
	mu sync.Mutex
	v  T
}

func (c *capture[T]) set(v T) {
	c.mu.Lock()
	c.v = v
	c.mu.Unlock()
}

func (c *capture[T]) get() T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

func (c *capture[T]) update(fn func(*T)) {
	c.mu.Lock()
	fn(&c.v)
	c.mu.Unlock()
}

// responder 让测试在请求之间切换假上游的回包（状态码 + 响应体）。
type responder struct {
	mu     sync.Mutex
	status int
	body   string
}

func (r *responder) set(status int, body string) {
	r.mu.Lock()
	r.status, r.body = status, body
	r.mu.Unlock()
}

// serve 直接当作 http.HandlerFunc 用（需要 http.Handler 时用 *responder.ServeHTTP）。
func (r *responder) serve(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	status, body := r.status, r.body
	r.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ServeHTTP 让 *responder 直接满足 http.Handler。
func (r *responder) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.serve(w, req) }
