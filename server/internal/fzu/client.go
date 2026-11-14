// Package fzu 是「学校侧协议」的唯一实现处：晚点名 4 个接口、SSO 换 token、
// 位置解析与抖动、4 种推送、假期区间匹配，全部只用标准库。
//
// 为什么把协议细节全关在这个包里：双重百分号编码、KEY==IV 的 AES、range 是
// "字符串里的 JSON"、迟到线缺失要退 FEndTime 再退 23:59 —— 这些怪癖一旦漏到
// flow/调度层，对端改版时就得满仓库找。上层只认 model 里的类型与 error。
package fzu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fzucheckin/internal/model"
	"fzucheckin/internal/secure"
)

const (
	// defaultBaseURL 晚点名 H5 的站点根（不含接口前缀）。测试把它换成本地
	// httptest.Server，所以它只做 NewClient 的默认值，不参与任何硬编码拼接。
	defaultBaseURL = "https://yzsxg.fzu.edu.cn"
	// apiPrefix 4 个接口共用的路径前缀。
	apiPrefix = "/livecloud/project/fzu/attn"

	// uaAttn 与 H5 里 WebView 的 UA 对齐：对端会按 UA 判定"是不是 App 内页面"，
	// 换个 curl 的 UA 有时会拿到一个不完整的 init 响应。
	uaAttn = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 " +
		"Chrome/120 Mobile Safari/537.36"

	// maxBodyBytes 上游响应上限：正常回包只有几 KB，限死是为了让一个跑飞的
	// 网关（返回 HTML 大页面）不至于把进程内存吃掉。
	maxBodyBytes = 1 << 20
	// bodySnippetLen 报错时截取的响应体长度：够看出"是登录页/网关页/改版提示"即可。
	bodySnippetLen = 300

	// jitterMaxAttempts 抖动点落在范围外的重取次数（与 Python 的
	// JITTER_MAX_ATTEMPTS 一致）。重取是有限次的：多点 check 会让请求特征变怪，
	// 且真的站在边界外时重试多少次都没用，不如退回配置坐标。
	jitterMaxAttempts = 3
	// defaultLateLine 连 FEndTime 都没有时的迟到线兜底（Python 同样写 23:59）。
	defaultLateLine = "23:59"
)

// ErrOutOfRange 定位不在任何签到范围内。单独一个哨兵错误是为了让 flow 能
// errors.Is 后归类为 out_of_range —— 这类失败要推送给用户（"去别处签"），
// 与"网络挂了/对端改版"的 error 处理方式完全不同。
var ErrOutOfRange = errors.New("定位校验未命中任何校区")

// ErrNoPlan 当天没有可用的签到范围（schoolData 为空）。flow 应归类为 no_plan
// 并静默，而不是当作失败重试。
var ErrNoPlan = errors.New("当天没有晚点名计划（schoolData 为空）")

// cstZone 北京时间。迟到判定必须以学校所在的 UTC+8 为准：服务可能跑在
// UTC 容器里（旧版 GitHub Actions 就踩过），用本机时区算会整体偏 8 小时。
var cstZone = time.FixedZone("CST", 8*3600)

// Client 是晚点名接口客户端。HTTP 与 BaseURL 可被测试覆盖，生产路径由
// NewClient 装配好。
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

// NewClient 用免登 token 造一个客户端。
func NewClient(token string) *Client {
	return &Client{HTTP: newHTTPClient(), BaseURL: defaultBaseURL, Token: token}
}

// newHTTPClient 给上游请求用的默认客户端：15s 总超时、最多 5 跳重定向、
// 连接复用。不使用 http.DefaultClient —— 它没有超时，一个卡住的对端会把
// 常驻进程的 goroutine 永久挂住。
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向超过 5 跳")
			}
			return nil
		},
	}
}

// httpClient 容忍调用方手写 &Client{}（没设 HTTP）：宁可现场造一个客户端，
// 也不要因为 nil 指针 panic 掉整轮签到。
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return newHTTPClient()
}

// apiURL 拼出某个 action 的完整地址。
//
// 允许 BaseURL 只写到站点根，也容忍它已经带上接口前缀（生产调试时常有人从
// 抓包 URL 里整段粘过来）；两种写法都必须能用，所以按后缀判断是否要补前缀。
func (c *Client) apiURL(action string) string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	if !strings.HasSuffix(base, apiPrefix) {
		base += apiPrefix
	}
	return base + "/" + action
}

// auth 给请求挂上认证头与 UA。token 即便为空也照发：Python 版就是这么发的，
// 少一个头会让对端的分支判定与本机调试时的抓包对不上。
func (c *Client) auth(req *http.Request) {
	req.Header.Set("User-Agent", uaAttn)
	req.Header.Set("token", c.Token)
}

// Init 查询今日晚点名计划与状态（POST 明文 JSON）。
func (c *Client) Init() (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, c.apiURL("init.action"), strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("init 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)

	env, err := c.do(req, "init")
	if err != nil {
		return nil, err
	}
	return decodeObject(env.Data), nil
}

// ServerTimeMs 取服务器毫秒时间戳，用于迟到判定（以服务器时间为准）。
//
// 返回 error 而不是"猜一个现在"：调用方需要知道自己拿到的到底是不是服务器时间，
// 才能决定要不要在日志里标注"按本机时间判定"。
func (c *Client) ServerTimeMs() (int64, error) {
	env, err := c.encGet("currentTimestamp.action", struct{}{})
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(env.Data))
	if raw == "" || raw == "null" {
		return 0, errors.New("currentTimestamp 返回空 data")
	}
	// 正常形状是 {"timestamp": 1763...}；也见过把裸数字直接放 data 的版本。
	if raw[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal(env.Data, &obj); err != nil {
			return 0, fmt.Errorf("currentTimestamp 响应解析失败: %w", err)
		}
		if ms, ok := toMillis(obj["timestamp"]); ok {
			return ms, nil
		}
		return 0, fmt.Errorf("currentTimestamp 响应里没有可用的 timestamp（对端可能改版）: %s",
			truncate(env.Data))
	}
	var v any
	if err := json.Unmarshal(env.Data, &v); err != nil {
		return 0, fmt.Errorf("currentTimestamp 响应解析失败: %w", err)
	}
	if ms, ok := toMillis(v); ok {
		return ms, nil
	}
	return 0, fmt.Errorf("currentTimestamp 返回值不是数字: %s", truncate(env.Data))
}

// checkPayload 是 check.action 的加密明文。
//
// 必须是 struct 而不是 map：map 会按 key 排序序列化，字段顺序一变密文就变，
// 与 Python/前端逐字节对齐的断言立刻失效。
type checkPayload struct {
	MapType   string  `json:"mapType"`
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
	Range     string  `json:"range"`
}

// Check 校验定位是否落在签到范围内，返回命中的校区/网格列表。
//
// 返回空列表 + nil error 表示"请求成功但没命中"——这与网络/协议失败是两回事，
// 调用方要靠这个区别决定"换个点重试"还是"放弃并报错"。
func (c *Client) Check(lon, lat float64, polygons []json.RawMessage) ([]model.Campus, error) {
	// nil 切片会被 encoding/json 写成 "null"，而 Python 侧永远是 "[]"：
	// 对端拿 range 去解析多边形，null 不是合法输入，先把形状摆正。
	if polygons == nil {
		polygons = []json.RawMessage{}
	}
	rangeJSON, err := compactJSON(polygons)
	if err != nil {
		return nil, fmt.Errorf("check 的 range 序列化失败: %w", err)
	}
	env, err := c.encGet("check.action", checkPayload{
		MapType:   "amap",
		Longitude: lon,
		Latitude:  lat,
		Range:     string(rangeJSON),
	})
	if err != nil {
		return nil, err
	}
	return parseCampuses(env.Data), nil
}

// ClockIn 提交打卡。
func (c *Client) ClockIn(p model.ClockInParams) (map[string]any, error) {
	env, err := c.encGet("clockIn.action", p)
	if err != nil {
		return nil, err
	}
	return decodeObject(env.Data), nil
}

// encGet 走 GET + 加密 param 的通用通道（check / clockIn / currentTimestamp）。
func (c *Client) encGet(action string, payload any) (envelope, error) {
	cipher, err := secure.EncryptParams(payload)
	if err != nil {
		return envelope{}, fmt.Errorf("%s 参数加密失败: %w", action, err)
	}
	// 前端 axios 会对已 encodeURIComponent 的值再编码一次，服务端自己解一层，
	// 所以这里是两次百分号编码。不能用 url.Values/QueryEscape：前者会重新排序
	// 并可能插 '+'，后者把空格写成 '+'，两者都会让对端解出的密文对不上。
	url := c.apiURL(action) + "?param=" + encodeParam(cipher)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return envelope{}, fmt.Errorf("%s 请求构造失败: %w", action, err)
	}
	c.auth(req)
	return c.do(req, action)
}

// envelope 是对端统一响应信封：只有 success && code==1 才算成功。
type envelope struct {
	Success bool            `json:"success"`
	Code    json.RawMessage `json:"code"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (e envelope) ok() bool { return e.Success && rawIsOne(e.Code) }

// do 发请求并把"HTTP 状态 / 非 JSON / 业务失败"三类情况翻译成带原文的 error。
//
// 为什么要分类：这三类的处置完全不同——500 与超时是"稍后重试"，非 JSON 通常
// 是被网关/登录页顶掉了（token 失效），业务失败则要照抄 msg 给用户看。全都压成
// 一句 "request failed" 就只能靠猜。
func (c *Client) do(req *http.Request, op string) (envelope, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("%s 请求失败: %w", op, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return envelope{}, fmt.Errorf("%s 读取响应失败: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return envelope{}, fmt.Errorf("%s HTTP %d: %s", op, resp.StatusCode, truncate(body))
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return envelope{}, fmt.Errorf("%s 响应不是 JSON（网关拦截或对端改版？）: %s", op, truncate(body))
	}
	if !env.ok() {
		msg := strings.TrimSpace(env.Msg)
		if msg == "" {
			// msg 为空时给原文：改版后的报错常常只体现在 body 结构里，
			// 少了这段，线上只能看到"失败"两个字。
			msg = truncate(body)
		}
		return env, fmt.Errorf("%s 失败: %s", op, msg)
	}
	return env, nil
}

// ---------- 编解码小工具 ----------

// percentEncode 复刻 urllib.parse.quote(s, safe="")：只放行 A-Z a-z 0-9 - . _ ~，
// 其余字节一律按 UTF-8 写成大写 %XX。
//
// 自己实现而不是用 url.QueryEscape：后者把空格写成 '+'（服务端按 form 解码才会
// 还原成空格，query 里不该出现 '+'），且与前端 encodeURIComponent 的保留集合
// 有差异。差一个字符，密文就解不开。
func percentEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[ch>>4])
			b.WriteByte(hexDigits[ch&0x0f])
		}
	}
	return b.String()
}

// encodeParam 做双重百分号编码（前端 axios 的既成事实）。
func encodeParam(cipher string) string {
	return percentEncode(percentEncode(cipher))
}

// compactJSON 输出紧凑 JSON（无空格、不转义 HTML 实体）。
//
// 这段文本会被当成 **普通字符串** 塞进 check 的 range 字段、由对端再解析一次，
// 所以它要长得像 Python 的 json.dumps(..., separators=(",",":"))：不能用
// secure.PyMarshal（那是给整份加密参数用的，会把中文转成 \uXXXX，等于让对端
// 收到一层二次转义的范围数据），也不能用裸 json.Marshal（它把 & < > 转成
// \u0026 之类，与 Python 的默认行为不一致）。
func compactJSON(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}

// rawIsOne 判断信封里的 code 是否等于 1，容忍 1 / 1.0 / "1" 三种写法。
func rawIsOne(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return false
		}
		s = strings.TrimSpace(str)
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 1
}

// decodeObject 把信封里的 data 归一成对象；缺失/为 null/不是对象都返回空对象，
// 让 flow 用"有没有字段"判断 no_plan，而不是先判 nil。
func decodeObject(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s[0] != '{' {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// parseCampuses 解析 check.action 的 data 数组。data 不是数组时按"没命中"处理
// （Python: `data if isinstance(data, list) else []`）。
func parseCampuses(raw json.RawMessage) []model.Campus {
	out := []model.Campus{}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s[0] != '[' {
		return out
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return out
	}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, model.Campus{
			ID:   scalarString(obj["id"]),
			Name: scalarString(obj["name"]),
		})
	}
	return out
}

// scalarString 把 JSON 里的标量转成字符串。id 有时是数字（12345）有时是字符串，
// 统一成字符串才能一路传进 clockIn 的 campus 字段。
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// toMillis 把时间戳标量统一成毫秒整数。
func toMillis(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 {
			return 0, false
		}
		return int64(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil || f <= 0 {
			return 0, false
		}
		return int64(f), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || f <= 0 {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// truncate 截断响应体便于放进错误信息，保证在多字节字符边界上切。
func truncate(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "（空响应体）"
	}
	if len(text) > bodySnippetLen {
		return strings.ToValidUTF8(text[:bodySnippetLen], "") +
			fmt.Sprintf("…（原文共 %d 字节）", len(body))
	}
	return text
}

// ---------- 一次签到的编排 ----------

// DoCheckin 完成 resolve_location → jitter → check → serverTime → clockIn。
//
// rng/draw 由调用方注入（测试用固定序列），logf 为 nil 时静默：这个包不打印
// 任何东西到 stdout，日志归属由上层（含 SSE 广播）统一决定。
func DoCheckin(c *Client, cfg model.Config, init map[string]any, draw RandFunc,
	logf func(string, ...any)) (model.CheckinResult, error) {

	if logf == nil {
		logf = func(string, ...any) {}
	}
	if c == nil {
		return model.CheckinResult{}, errors.New("DoCheckin 需要非空 client")
	}
	plan, err := ParseInit(init)
	if err != nil {
		return model.CheckinResult{}, err
	}
	if len(plan.Polygons) == 0 {
		return model.CheckinResult{}, ErrNoPlan
	}

	// 基准点可能来自位置池（每次运行换一个），文案与基准点绑定，统一由
	// ResolveLocation 解析；抖动半径单独由 JitterRadius 决定（0 = 不抖）。
	place, err := ResolveLocation(cfg, draw)
	if err != nil {
		return model.CheckinResult{}, err
	}
	if place.Label != "single" {
		text := place.ActualLocation
		if text == "" {
			text = "（无文案）"
		}
		logf("本次签到基准位置取自位置池 %s：%s", place.Label, text)
	}
	radius := JitterRadius(cfg)

	// check 与 clockIn 使用同一个点：模拟真实设备"一次定位、一路提交"，
	// 也避免抖动半径写大时 check 命中而 clockIn 坐标对不上的自相矛盾。
	lon, lat := place.Lon, place.Lat
	var (
		matched  []model.Campus
		jittered bool
	)
	if radius > 0 {
		for attempt := 1; attempt <= jitterMaxAttempts; attempt++ {
			jLon, jLat := JitterLocation(place.Lon, place.Lat, radius, draw)
			got, checkErr := c.Check(jLon, jLat, plan.Polygons)
			if checkErr != nil {
				return model.CheckinResult{}, checkErr
			}
			if len(got) > 0 {
				lon, lat, matched, jittered = jLon, jLat, got, true
				break
			}
			logf("第 %d 次抖动点落在签到范围外，重新取点…", attempt)
		}
	}
	if len(matched) == 0 {
		// 未开启抖动，或抖动点全部越界（配置点贴着校区边缘时的常态）：
		// 宁可上报固定坐标，也不能因为抖动漏签。
		if radius > 0 {
			logf("抖动点均在范围外，回退到配置坐标打卡。")
		}
		got, checkErr := c.Check(place.Lon, place.Lat, plan.Polygons)
		if checkErr != nil {
			return model.CheckinResult{}, checkErr
		}
		if len(got) == 0 {
			return model.CheckinResult{}, fmt.Errorf(
				"%w（基准点经度 %.5f 纬度 %.5f，文案 %q）",
				ErrOutOfRange, place.Lon, place.Lat, place.ActualLocation)
		}
		lon, lat, matched, jittered = place.Lon, place.Lat, got, false
	}

	// 迟到判定以服务器时间为准；拿不到就退回本机时间（同样按北京时间换算），
	// 绝不能因为一个"校准用"的接口失败就放弃打卡 —— Python 也是 try/except 吞掉。
	now := time.Now().In(cstZone)
	if ts, tsErr := c.ServerTimeMs(); tsErr != nil {
		logf("服务器时间不可用，按本机时间判定迟到：%v", tsErr)
	} else {
		now = time.UnixMilli(ts).In(cstZone)
	}
	isLate := isLateAt(now, lateLineOf(plan))

	campus := strings.TrimSpace(plan.CampusDefault)
	if id := strings.TrimSpace(matched[0].ID); id != "" {
		campus = id
	}
	params := model.ClockInParams{
		CampusID:       campus,
		Lon:            lon,
		Lat:            lat,
		StartTime:      plan.Start,
		EndTime:        plan.End,
		ActualLocation: place.ActualLocation,
		CaIsNo:         "0", // H5 URL 上的 FCaIsNo，默认 "0"
		CollUnit:       plan.CollUnit,
		Way:            plan.Way,
	}
	if isLate {
		params.IsLate = 1
	}
	if _, err := c.ClockIn(params); err != nil {
		return model.CheckinResult{}, err
	}

	// 返回的坐标用 **实际上报** 的点（抖动后的），因为 RunRecord.location 要
	// 记录真实上报值；label 与文案仍绑定池里的基准点，两者一起才复现得出当时
	// 到底抽了哪个位置池条目。
	reported := place
	reported.Lon, reported.Lat = lon, lat
	return model.CheckinResult{
		Place:    reported,
		Jittered: jittered,
		Matched:  true,
		Campus:   campus,
		IsLate:   isLate,
	}, nil
}

// lateLineOf 决定迟到线：FLateTime 优先，缺失退 FEndTime，再缺退 23:59。
func lateLineOf(p Plan) string {
	if v := strings.TrimSpace(p.LateLine); v != "" {
		return v
	}
	if v := strings.TrimSpace(p.End); v != "" {
		return v
	}
	return defaultLateLine
}

// isLateAt 按"分钟"比较（与 Python 的 hour*60+minute 一致，秒不参与）。
// 迟到线写坏时按不迟到处理：宁可把一次迟到报成准点，也不要因为一行配置错误
// 把整次打卡判成异常。
func isLateAt(now time.Time, lateLine string) bool {
	hour, minute, ok := parseClock(lateLine)
	if !ok {
		return false
	}
	return now.Hour()*60+now.Minute() > hour*60+minute
}

// parseClock 解析 "HH:MM" / "HH:MM:SS"（容忍全角冒号与首尾空白）。
func parseClock(text string) (hour, minute int, ok bool) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(text), "：", ":"), " ", "")
	parts := strings.Split(normalized, ":")
	if len(parts) < 2 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 24 {
		return 0, 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
