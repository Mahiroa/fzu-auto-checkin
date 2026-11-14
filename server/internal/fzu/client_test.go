package fzu

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
	"fzucheckin/internal/secure"
)

// ---------- init.action ----------

func TestInitProtocolShape(t *testing.T) {
	type initObs struct {
		Method, ContentType, Body, Token, Agent, Path string
		Hits                                          int
	}
	obs := capture[initObs]{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		obs.update(func(o *initObs) {
			o.Hits++
			o.Method = r.Method
			o.ContentType = r.Header.Get("Content-Type")
			o.Token = r.Header.Get("token")
			o.Agent = r.Header.Get("User-Agent")
			o.Path = r.URL.Path
			o.Body = string(raw)
		})
		writeEnvelope(w, `{"userData":{"FState":"未签到"},"paramsData":{"FEndTime":"23:59"}}`)
	}))
	defer srv.Close()

	// BaseURL 已经带上接口前缀时不能再补一次（从抓包 URL 整段粘过来的情况）。
	c := NewClient("tok-init")
	c.BaseURL = srv.URL + apiPrefix
	data, err := c.Init()
	if err != nil {
		t.Fatalf("Init 返回错误: %v", err)
	}
	got := obs.get()
	if got.Hits != 1 {
		t.Fatalf("应只发 1 次 init，实际 %d", got.Hits)
	}
	if got.Path != apiPrefix+"/init.action" {
		t.Errorf("init 路径不对: %s", got.Path)
	}
	if got.Method != http.MethodPost {
		t.Errorf("init 必须是 POST，实际 %s", got.Method)
	}
	if got.ContentType != "application/json" {
		t.Errorf("init 的 Content-Type 应为 application/json，实际 %q", got.ContentType)
	}
	if got.Body != "{}" {
		t.Errorf("init 的 body 应为 {}，实际 %q", got.Body)
	}
	if got.Token != "tok-init" {
		t.Errorf("缺少 token 头，实际 %q", got.Token)
	}
	if got.Agent != uaAttn {
		t.Errorf("UA 与 H5 不一致：%q", got.Agent)
	}
	user, ok := data["userData"].(map[string]any)
	if !ok || user["FState"] != "未签到" {
		t.Fatalf("Init 返回的 data 结构不对: %#v", data)
	}
}

func TestEnvelopeFailureShapes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantSub []string
		wantErr bool
	}{
		{
			name:    "success 为 false 且带 msg",
			status:  200,
			body:    `{"success":false,"code":0,"msg":"token 已失效"}`,
			wantSub: []string{"init 失败", "token 已失效"},
			wantErr: true,
		},
		{
			name:    "success 为 true 但 code 不是 1，且 msg 为空时要带原文",
			status:  200,
			body:    `{"success":true,"code":2,"msg":"","data":null}`,
			wantSub: []string{"init 失败", `"code":2`},
			wantErr: true,
		},
		{
			name:    "HTTP 500 要带上响应体",
			status:  500,
			body:    "upstream boom",
			wantSub: []string{"HTTP 500", "upstream boom"},
			wantErr: true,
		},
		{
			name:    "响应体不是 JSON（常被网关或登录页顶掉）",
			status:  200,
			body:    `<html><title>502 Bad Gateway</title></html>`,
			wantSub: []string{"不是 JSON", "502 Bad Gateway"},
			wantErr: true,
		},
		{
			name:    "code 是字符串 \"1\" 也认",
			status:  200,
			body:    `{"success":true,"code":"1","data":{"a":1}}`,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient("tok")
			c.BaseURL = srv.URL
			data, err := c.Init()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功: %#v", data)
				}
				for _, sub := range tc.wantSub {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("错误信息缺少 %q: %v", sub, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if data["a"] == nil {
				t.Errorf("data 没解析出来: %#v", data)
			}
		})
	}
}

// ---------- currentTimestamp.action ----------

func TestServerTimeMs(t *testing.T) {
	const want = int64(1763201460123)
	cases := []struct {
		name    string
		data    string
		want    int64
		wantErr bool
	}{
		{name: "标准对象", data: `{"timestamp":1763201460123}`, want: want},
		{name: "裸数字", data: `1763201460123`, want: want},
		{name: "空对象", data: `{}`, wantErr: true},
		{name: "null", data: `null`, wantErr: true},
		{name: "字符串数字", data: `{"timestamp":"1763201460123"}`, want: want},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := capture[string]{}
			plainObs := capture[string]{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method.set(r.Method)
				plain, _, _ := decodeParam(t, r)
				plainObs.set(plain)
				if r.URL.Path != apiPrefix+"/currentTimestamp.action" {
					t.Errorf("路径不对: %s", r.URL.Path)
				}
				writeEnvelope(w, tc.data)
			}))

			c := NewClient("tok")
			c.BaseURL = srv.URL
			got, err := c.ServerTimeMs()
			srv.Close()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际拿到 %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got != tc.want {
				t.Errorf("时间戳 = %d，期望 %d", got, tc.want)
			}
			if method.get() != http.MethodGet {
				t.Errorf("currentTimestamp 必须是 GET，实际 %s", method.get())
			}
			if plain := plainObs.get(); plain != "{}" {
				t.Errorf("currentTimestamp 的明文应为 {}，实际 %q", plain)
			}
		})
	}
}

// ---------- check.action ----------

// TestCheckPayloadByteParity 锁死三件事：
//  1. 解密后的 JSON 与 Python 形状逐字节一致（字段顺序 + 紧凑 + range 是字符串）；
//  2. 密文 == secure.EncryptParams 的结果（说明没有自己拼 base64/JSON）；
//  3. 查询参数是双重百分号编码的密文。
func TestCheckPayloadByteParity(t *testing.T) {
	polygons := []json.RawMessage{
		json.RawMessage(`{"id":"A1","points":[[119.19,26.04],[119.21,26.06]]}`),
	}
	const wantRange = `[{"id":"A1","points":[[119.19,26.04],[119.21,26.06]]}]`
	const wantPlain = `{"mapType":"amap","longitude":119.20463,"latitude":26.0492,` +
		`"range":"[{\"id\":\"A1\",\"points\":[[119.19,26.04],[119.21,26.06]]}]"}`

	f := newFakeAttn(t)
	c := f.client("tok-check")
	if _, err := c.Check(119.20463, 26.0492, polygons); err != nil {
		t.Fatalf("Check 返回错误: %v", err)
	}

	calls := f.snapshotChecks()
	if len(calls) != 1 {
		t.Fatalf("应发 1 次 check，实际 %d", len(calls))
	}
	call := calls[0]
	if call.Plain != wantPlain {
		t.Errorf("check 明文与契约不一致\n实际: %s\n期望: %s", call.Plain, wantPlain)
	}
	if call.Range != wantRange {
		t.Errorf("range 应当是紧凑 JSON 字符串\n实际: %s\n期望: %s", call.Range, wantRange)
	}
	if call.MapType != "amap" {
		t.Errorf("mapType = %q，期望 amap", call.MapType)
	}
	if call.Method != http.MethodGet {
		t.Errorf("check 必须是 GET，实际 %s", call.Method)
	}
	if call.Token != "tok-check" {
		t.Errorf("check 缺少 token 头: %q", call.Token)
	}

	// 密文必须与 secure.EncryptParams 逐字节相同：这是"没有手搓 JSON/base64"的证明。
	wantCipher, err := secure.EncryptParams(checkPayload{
		MapType:   "amap",
		Longitude: 119.20463,
		Latitude:  26.0492,
		Range:     wantRange,
	})
	if err != nil {
		t.Fatalf("EncryptParams: %v", err)
	}
	if call.Cipher != wantCipher {
		t.Errorf("密文与 EncryptParams 不一致\n实际: %s\n期望: %s", call.Cipher, wantCipher)
	}
	// 收到的 param 必须是"再编码一次"的结果。
	if call.RawParam != encodeParam(wantCipher) {
		t.Errorf("param 不是双重编码\n实际: %s\n期望: %s", call.RawParam, encodeParam(wantCipher))
	}
	if !strings.Contains(call.RawParam, "%25") {
		t.Errorf("param 里没有出现 %%25，说明只编码了一次: %s", call.RawParam)
	}
}

func TestCheckNonListDataIsEmpty(t *testing.T) {
	f := newFakeAttn(t)
	f.checkFn = func(int, checkCall) string { return `{"weird":true}` }
	c := f.client("tok")
	got, err := c.Check(119.2, 26.0, nil)
	if err != nil {
		t.Fatalf("Check 返回错误: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("data 不是数组时应返回空列表，实际 %#v", got)
	}
	// nil polygons 要序列化成 []（Python json.dumps([]) 的形状），不是 null。
	calls := f.snapshotChecks()
	if calls[0].Range != "[]" {
		t.Errorf("range = %q，期望 []", calls[0].Range)
	}
}

// ---------- 百分号编码 ----------

func TestPercentEncodeMatchesPythonQuote(t *testing.T) {
	// 期望值来自 Python: urllib.parse.quote(s, safe="")
	cases := map[string]string{
		"":                     "",
		"abcXYZ019-._~":        "abcXYZ019-._~",
		"a b":                  "a%20b", // 关键：空格是 %20，不是 QueryEscape 的 '+'
		"a+b/c=":               "a%2Bb%2Fc%3D",
		"中":                    "%E4%B8%AD",
		"~!*'()":               "~%21%2A%27%28%29",
		"a\nb":                 "a%0Ab",
		"100%":                 "100%25",
		"QUJDRA==/+==":         "QUJDRA%3D%3D%2F%2B%3D%3D",
		"emoji😀":               "emoji%F0%9F%98%80",
		"0123456789abcdefABCD": "0123456789abcdefABCD",
	}
	for input, want := range cases {
		if got := percentEncode(input); got != want {
			t.Errorf("percentEncode(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// ---------- clockIn.action ----------

func TestClockInPayloadByteParity(t *testing.T) {
	p := model.ClockInParams{
		CampusID:       "C123",
		Lon:            119.20471,
		Lat:            26.04912,
		StartTime:      "21:30",
		EndTime:        "23:59",
		ActualLocation: "Fuzhou University Building X",
		CaIsNo:         "0",
		CollUnit:       "CU01",
		Way:            1,
		IsLate:         1,
	}
	const wantPlain = `{"campus":"C123","lon":119.20471,"lat":26.04912,` +
		`"startTime":"21:30","endTime":"23:59","actualLocation":"Fuzhou University Building X",` +
		`"caIsNo":"0","collUnit":"CU01","way":1,"isLate":1}`

	f := newFakeAttn(t)
	c := f.client("tok")
	if _, err := c.ClockIn(p); err != nil {
		t.Fatalf("ClockIn 返回错误: %v", err)
	}
	clocks := f.snapshotClocks()
	if len(clocks) != 1 {
		t.Fatalf("应发 1 次 clockIn，实际 %d", len(clocks))
	}
	if clocks[0] != wantPlain {
		t.Errorf("clockIn 明文与 Python 字段顺序不一致\n实际: %s\n期望: %s", clocks[0], wantPlain)
	}
}

// TestClockInNonASCIITextIsEscaped 说明为什么必须走 secure.PyMarshal：
// Python 的 json.dumps 默认 ensure_ascii=True，中文会被写成 \uXXXX。
func TestClockInNonASCIITextIsEscaped(t *testing.T) {
	p := model.ClockInParams{CampusID: "C1", ActualLocation: "生活区X号楼"}
	f := newFakeAttn(t)
	c := f.client("tok")
	if _, err := c.ClockIn(p); err != nil {
		t.Fatalf("ClockIn 返回错误: %v", err)
	}
	plain := f.snapshotClocks()[0]
	if !strings.Contains(plain, `\u751f\u6d3b\u533a`) {
		t.Errorf("中文应按 ensure_ascii 转义成 \\uXXXX: %s", plain)
	}
}

// ---------- DoCheckin 编排 ----------

func testInitMap() map[string]any {
	return map[string]any{
		"userData": map[string]any{
			"FState":         "未签到",
			"FDefaultCampus": "DEF-CAMPUS",
			"FCollUnit":      "CU01",
			"FWay":           float64(1),
		},
		"paramsData": map[string]any{
			"FStartTime": "21:30",
			"FEndTime":   "23:59",
			"FLateTime":  "22:30",
		},
		"schoolData": []any{
			map[string]any{"FPosition": `{"range":[{"id":"R1","points":[[119.19,26.04]]}]}`},
		},
	}
}

func testConfig(radius float64) model.Config {
	var cfg model.Config
	cfg.Checkin.Longitude = "119.20463"
	cfg.Checkin.Latitude = "26.04920"
	cfg.Checkin.ActualLocation = " 旗山校区生活区X号楼 "
	cfg.Checkin.LocationJitterM = ptrFloat(radius)
	return cfg
}

const baseLon, baseLat = 119.20463, 26.04920

func TestDoCheckinJitterRetriesThenHits(t *testing.T) {
	f := newFakeAttn(t)
	// 前两次抖动越界，第三次命中：验证"重取 ≤3 次"而不是一次就放弃。
	f.checkFn = func(index int, _ checkCall) string {
		if index < 2 {
			return "[]"
		}
		return `[{"id":"CAMPUS-3","name":"旗山校区"}]`
	}
	var logs []string
	res, err := DoCheckin(f.client("tok"), testConfig(200), testInitMap(),
		scriptedDraw(0.5, 0.25), func(format string, args ...any) {
			logs = append(logs, format)
		})
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}

	calls := f.snapshotChecks()
	if len(calls) != 3 {
		t.Fatalf("应发 3 次 check（两次越界 + 一次命中），实际 %d", len(calls))
	}
	if !res.Jittered {
		t.Error("命中的是抖动点，Jittered 应为 true")
	}
	if res.Campus != "CAMPUS-3" {
		t.Errorf("Campus = %q，期望命中项的 id", res.Campus)
	}
	if !res.Matched {
		t.Error("Matched 应为 true")
	}
	// 抖动点不得等于基准点（否则这条用例什么都没验证）。
	if calls[0].Lon == baseLon && calls[0].Lat == baseLat {
		t.Error("抖动点与基准点相同，说明抖动没生效")
	}
	if calls[0].Range != `[{"id":"R1","points":[[119.19,26.04]]}]` {
		t.Errorf("check 的 range 与 init 里的多边形不一致: %s", calls[0].Range)
	}
	// check 与 clockIn 必须用同一个点。
	if len(logs) != 2 {
		t.Errorf("应有 2 条\"越界重取\"日志，实际 %d 条: %v", len(logs), logs)
	}
	clocks := f.snapshotClocks()
	if len(clocks) != 1 {
		t.Fatalf("应发 1 次 clockIn，实际 %d", len(clocks))
	}
	var clocked map[string]any
	if err := json.Unmarshal([]byte(clocks[0]), &clocked); err != nil {
		t.Fatalf("clockIn 明文解析失败: %v", err)
	}
	if clocked["lon"] != calls[2].Lon || clocked["lat"] != calls[2].Lat {
		t.Errorf("clockIn 的坐标应等于最后一次 check 的点\ncheck: %v,%v\nclock: %v,%v",
			calls[2].Lon, calls[2].Lat, clocked["lon"], clocked["lat"])
	}
	if res.Place.Lon != clocked["lon"] || res.Place.Lat != clocked["lat"] {
		t.Errorf("返回的 Place 应是实际上报坐标: %v,%v", res.Place.Lon, res.Place.Lat)
	}
	if res.Place.ActualLocation != "旗山校区生活区X号楼" {
		t.Errorf("actualLocation 应取配置文案（去空白）: %q", res.Place.ActualLocation)
	}
	if clocked["startTime"] != "21:30" || clocked["endTime"] != "23:59" {
		t.Errorf("startTime/endTime 应来自 paramsData: %v", clocked)
	}
	if clocked["collUnit"] != "CU01" || clocked["caIsNo"] != "0" {
		t.Errorf("collUnit/caIsNo 不对: %v", clocked)
	}
	// isLate 必须在报文与返回值之间自洽。
	wantLate := 0.0
	if res.IsLate {
		wantLate = 1
	}
	if clocked["isLate"] != wantLate {
		t.Errorf("isLate 不一致：报文 %v，返回值 %v", clocked["isLate"], res.IsLate)
	}
}

func TestDoCheckinFallsBackToBasePoint(t *testing.T) {
	f := newFakeAttn(t)
	// 抖动点全部越界；只有基准点命中（配置点贴着校区边缘的典型情形）。
	f.checkFn = func(_ int, call checkCall) string {
		if nearlyEqual(call.Lon, baseLon) && nearlyEqual(call.Lat, baseLat) {
			return `[{"id":"BASE-1"}]`
		}
		return "[]"
	}
	var logs []string
	res, err := DoCheckin(f.client("tok"), testConfig(200), testInitMap(),
		scriptedDraw(0.5, 0.25), func(format string, _ ...any) {
			logs = append(logs, format)
		})
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}
	calls := f.snapshotChecks()
	if len(calls) != 4 {
		t.Fatalf("应发 4 次 check（3 次抖动 + 1 次基准点），实际 %d", len(calls))
	}
	if res.Jittered {
		t.Error("回退到基准点后 Jittered 应为 false")
	}
	if res.Campus != "BASE-1" {
		t.Errorf("Campus = %q，期望 BASE-1", res.Campus)
	}
	clocks := f.snapshotClocks()
	if len(clocks) != 1 {
		t.Fatalf("应发 1 次 clockIn，实际 %d", len(clocks))
	}
	var clocked map[string]any
	_ = json.Unmarshal([]byte(clocks[0]), &clocked)
	if !nearlyEqual(clocked["lon"].(float64), baseLon) || !nearlyEqual(clocked["lat"].(float64), baseLat) {
		t.Errorf("回退后应上报基准点，实际 %v,%v", clocked["lon"], clocked["lat"])
	}
	if len(logs) != 4 { // 3 条越界 + 1 条回退说明
		t.Errorf("日志条数不对（期望 3 次越界 + 1 次回退）: %v", logs)
	}
}

func TestDoCheckinOutOfRange(t *testing.T) {
	f := newFakeAttn(t) // 默认永远返回 []
	_, err := DoCheckin(f.client("tok"), testConfig(200), testInitMap(), scriptedDraw(0.5, 0.25), nil)
	if err == nil {
		t.Fatal("全部越界时必须报错")
	}
	if !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("错误应可 errors.Is(ErrOutOfRange)，实际 %v", err)
	}
	if len(f.snapshotClocks()) != 0 {
		t.Error("越界时不该调用 clockIn")
	}
	if len(f.snapshotChecks()) != 4 {
		t.Errorf("应发 3 次抖动 + 1 次基准点 check，实际 %d", len(f.snapshotChecks()))
	}
}

func TestDoCheckinLateByServerTime(t *testing.T) {
	f := newFakeAttn(t)
	// 服务器时间 22:31，迟到线 22:30 → 迟到。
	f.tsMs = time.Date(2026, 11, 14, 22, 31, 0, 0, cstZone).UnixMilli()
	f.checkFn = func(int, checkCall) string { return `[{"id":"C1"}]` }

	res, err := DoCheckin(f.client("tok"), testConfig(0), testInitMap(), nil, nil)
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}
	if !res.IsLate {
		t.Error("22:31 相对 22:30 迟到线应判定为迟到")
	}
	if !strings.Contains(f.snapshotClocks()[0], `"isLate":1`) {
		t.Errorf("clockIn 报文应带 isLate=1: %s", f.snapshotClocks()[0])
	}
}

func TestDoCheckinNotLateAtTheSameMinute(t *testing.T) {
	f := newFakeAttn(t)
	// 22:30:59 仍算 22:30（Python 只比 hour*60+minute），不该判迟到。
	f.tsMs = time.Date(2026, 11, 14, 22, 30, 59, 0, cstZone).UnixMilli()
	f.checkFn = func(int, checkCall) string { return `[{"id":"C1"}]` }

	res, err := DoCheckin(f.client("tok"), testConfig(0), testInitMap(), nil, nil)
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}
	if res.IsLate {
		t.Error("同一分钟内的 22:30:59 不该判迟到")
	}
	if !strings.Contains(f.snapshotClocks()[0], `"isLate":0`) {
		t.Errorf("clockIn 报文应带 isLate=0: %s", f.snapshotClocks()[0])
	}
}

func TestDoCheckinLateLineFallbacks(t *testing.T) {
	// FLateTime 缺失 → 退 FEndTime；FEndTime 也缺失 → 退 23:59。
	cases := []struct {
		name     string
		mutate   func(map[string]any)
		tsHour   int
		tsMinute int
		wantLate bool
	}{
		{
			name: "FLateTime 缺失时退到 FEndTime",
			mutate: func(m map[string]any) {
				p := m["paramsData"].(map[string]any)
				delete(p, "FLateTime")
				p["FEndTime"] = "21:00" // 迟到线跟着 endTime 走
			},
			tsHour: 21, tsMinute: 30, wantLate: true,
		},
		{
			name: "两个都缺失时退到 23:59",
			mutate: func(m map[string]any) {
				p := m["paramsData"].(map[string]any)
				delete(p, "FLateTime")
				delete(p, "FEndTime")
			},
			tsHour: 23, tsMinute: 58, wantLate: false,
		},
		{
			name: "23:59 这一分钟内仍算准点",
			mutate: func(m map[string]any) {
				p := m["paramsData"].(map[string]any)
				delete(p, "FLateTime")
				delete(p, "FEndTime")
			},
			tsHour: 23, tsMinute: 59, wantLate: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAttn(t)
			f.checkFn = func(int, checkCall) string { return `[{"id":"C1"}]` }
			initMap := testInitMap()
			tc.mutate(initMap)
			f.tsMs = time.Date(2026, 11, 14, tc.tsHour, tc.tsMinute, 0, 0, cstZone).UnixMilli()
			res, err := DoCheckin(f.client("tok"), testConfig(0), initMap, nil, nil)
			if err != nil {
				t.Fatalf("DoCheckin 返回错误: %v", err)
			}
			if res.IsLate != tc.wantLate {
				t.Errorf("IsLate = %v，期望 %v", res.IsLate, tc.wantLate)
			}
		})
	}
}

func TestDoCheckinCampusFallsBackToDefault(t *testing.T) {
	f := newFakeAttn(t)
	f.checkFn = func(int, checkCall) string { return `[{"name":"没有 id 的命中项"}]` }
	res, err := DoCheckin(f.client("tok"), testConfig(0), testInitMap(), nil, nil)
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}
	if res.Campus != "DEF-CAMPUS" {
		t.Errorf("命中项没有 id 时应回退 FDefaultCampus，实际 %q", res.Campus)
	}
	if !strings.Contains(f.snapshotClocks()[0], `"campus":"DEF-CAMPUS"`) {
		t.Errorf("clockIn 的 campus 不对: %s", f.snapshotClocks()[0])
	}
}

func TestDoCheckinZeroRadiusSkipsJitter(t *testing.T) {
	f := newFakeAttn(t)
	f.checkFn = func(int, checkCall) string { return `[{"id":"C1"}]` }
	res, err := DoCheckin(f.client("tok"), testConfig(0), testInitMap(), nil, nil)
	if err != nil {
		t.Fatalf("DoCheckin 返回错误: %v", err)
	}
	if len(f.snapshotChecks()) != 1 {
		t.Errorf("关闭抖动后只该发 1 次 check，实际 %d", len(f.snapshotChecks()))
	}
	if res.Jittered {
		t.Error("关闭抖动时 Jittered 应为 false")
	}
	if !nearlyEqual(res.Place.Lon, baseLon) {
		t.Errorf("关闭抖动后应上报基准经度，实际 %v", res.Place.Lon)
	}
}

func TestDoCheckinNoPolygons(t *testing.T) {
	f := newFakeAttn(t)
	initMap := testInitMap()
	initMap["schoolData"] = []any{}
	_, err := DoCheckin(f.client("tok"), testConfig(200), initMap, nil, nil)
	if !errors.Is(err, ErrNoPlan) {
		t.Fatalf("无签到范围时应返回 ErrNoPlan，实际 %v", err)
	}
	if len(f.snapshotChecks()) != 0 {
		t.Error("无多边形时不该发 check")
	}
}

func TestDoCheckinSurvivesServerTimeFailure(t *testing.T) {
	f := newFakeAttn(t)
	f.tsFail = true
	f.checkFn = func(int, checkCall) string { return `[{"id":"C1"}]` }
	var logs []string
	_, err := DoCheckin(f.client("tok"), testConfig(0), testInitMap(), nil,
		func(format string, _ ...any) { logs = append(logs, format) })
	if err != nil {
		t.Fatalf("服务器时间不可用不该中止打卡: %v", err)
	}
	if len(f.snapshotClocks()) != 1 {
		t.Error("时间接口失败后仍应提交 clockIn")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "服务器时间不可用") {
		t.Errorf("应记录一条降级日志，实际 %v", logs)
	}
}

func TestDoCheckinInvalidConfigStillFails(t *testing.T) {
	f := newFakeAttn(t)
	var cfg model.Config // 没有任何坐标
	_, err := DoCheckin(f.client("tok"), cfg, testInitMap(), nil, nil)
	if err == nil {
		t.Fatal("缺少坐标必须报错")
	}
	if !strings.Contains(err.Error(), "checkin") {
		t.Errorf("坐标错误应点名 checkin 段: %v", err)
	}
	if len(f.snapshotChecks()) != 0 {
		t.Error("配置无效时不该发 check")
	}
}

// nearlyEqual 用于比较坐标（float 抖动点与基准点的等价判断）。
func nearlyEqual(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}
