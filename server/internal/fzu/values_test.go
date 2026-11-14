package fzu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fzucheckin/internal/model"
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("tok")
	if c.HTTP == nil {
		t.Fatal("NewClient 必须装配好 HTTP 客户端")
	}
	if c.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q，期望 %q", c.BaseURL, defaultBaseURL)
	}
	if c.Token != "tok" {
		t.Errorf("Token = %q", c.Token)
	}
	// 重定向跟随上限：SSO 需要几跳，但绝不能无限跟。
	req := httptest.NewRequest(http.MethodGet, "http://example.test/next", nil)
	if err := c.HTTP.CheckRedirect(req, make([]*http.Request, 5)); err == nil {
		t.Error("第 6 跳应被拒绝")
	}
	if err := c.HTTP.CheckRedirect(req, make([]*http.Request, 2)); err != nil {
		t.Errorf("正常跳转不该被拒绝: %v", err)
	}
	// 手写 &Client{} 也不能因为 nil 客户端 panic。
	if (&Client{}).httpClient() == nil {
		t.Error("httpClient 不该返回 nil")
	}
}

func TestClientInvalidBaseURL(t *testing.T) {
	c := NewClient("tok")
	c.BaseURL = "://bad url"
	if _, err := c.Init(); err == nil {
		t.Error("非法 BaseURL 时 Init 应报错")
	}
	if _, err := c.Check(119.2, 26.0, nil); err == nil {
		t.Error("非法 BaseURL 时 Check 应报错")
	}
	if _, err := c.ClockIn(model.ClockInParams{}); err == nil {
		t.Error("非法 BaseURL 时 ClockIn 应报错")
	}
}

func TestScalarString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{" id-1 ", "id-1"},
		{float64(12345), "12345"}, // 数字 id 不能被写成 12345.0
		{float64(1.5), "1.5"},
		{json.Number("9876543210987654321"), "9876543210987654321"},
		{true, "true"},
		{false, "false"},
		{map[string]any{"x": 1}, ""},
	}
	for _, tc := range cases {
		if got := scalarString(tc.in); got != tc.want {
			t.Errorf("scalarString(%#v) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestToMillis(t *testing.T) {
	const want = int64(1763201460123)
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(1763201460123), want, true},
		{json.Number("1763201460123"), want, true},
		{"1763201460123", want, true},
		{" 1763201460123 ", want, true},
		{"abc", 0, false},
		{float64(0), 0, false},
		{float64(-1), 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, tc := range cases {
		got, ok := toMillis(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("toMillis(%#v) = %d,%v，期望 %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRawIsOne(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"1", true},
		{"1.0", true},
		{`"1"`, true},
		{"  1  ", true},
		{"2", false},
		{"0", false},
		{"null", false},
		{"", false},
		{"true", false},
		{`"abc"`, false},
	}
	for _, tc := range cases {
		if got := rawIsOne(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("rawIsOne(%q) = %v，期望 %v", tc.raw, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(nil); got != "（空响应体）" {
		t.Errorf("空响应体文案 = %q", got)
	}
	if got := truncate([]byte("  hi  ")); got != "hi" {
		t.Errorf("应去掉首尾空白: %q", got)
	}
	long := strings.Repeat("a", bodySnippetLen+50)
	got := truncate([]byte(long))
	if !strings.Contains(got, "原文共") {
		t.Errorf("超长响应体应标注原文长度: %q", got)
	}
	// 多字节字符不能在中间切断（否则错误信息里出现半个汉字）。
	if got := truncate([]byte(strings.Repeat("中", 200))); strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("截断破坏了 UTF-8: %q", got)
	}
}

func TestParseCampusesWireFormats(t *testing.T) {
	raw := json.RawMessage(`[
		{"id":"A1","name":"旗山"},
		{"id":12345,"name":"铜盘"},
		{"name":"没有 id"},
		"不是对象",
		{"id":null}
	]`)
	got := parseCampuses(raw)
	if len(got) != 4 {
		t.Fatalf("应跳过非对象项，剩 4 个，实际 %d: %#v", len(got), got)
	}
	if got[0].ID != "A1" || got[0].Name != "旗山" {
		t.Errorf("第一个命中的解析不对: %#v", got[0])
	}
	if got[1].ID != "12345" {
		t.Errorf("数字 id 应转成字符串: %#v", got[1])
	}
	if got[2].ID != "" || got[2].Name != "没有 id" {
		t.Errorf("id 缺失时应留空、name 照常解析: %#v", got[2])
	}
	if got[3].ID != "" {
		t.Errorf("id 为 null 时应留空: %#v", got[3])
	}
	if got := parseCampuses(json.RawMessage(`null`)); len(got) != 0 {
		t.Error("null 应返回空列表")
	}
	if got := parseCampuses(nil); len(got) != 0 {
		t.Error("nil 应返回空列表")
	}
}

func TestDecodeObjectShapes(t *testing.T) {
	if got := decodeObject(nil); len(got) != 0 {
		t.Error("nil 应返回空对象")
	}
	if got := decodeObject(json.RawMessage(`null`)); len(got) != 0 {
		t.Error("null 应返回空对象")
	}
	if got := decodeObject(json.RawMessage(`[1,2]`)); len(got) != 0 {
		t.Error("数组应返回空对象")
	}
	if got := decodeObject(json.RawMessage(`{"a":1}`)); got["a"] != float64(1) {
		t.Errorf("对象应正常解析: %#v", got)
	}
}

func TestStringFieldAndIntFieldTolerance(t *testing.T) {
	m := map[string]any{
		"s":     " v ",
		"num":   float64(21),
		"numb":  json.Number("7"),
		"bool":  true,
		"other": map[string]any{},
	}
	if got := stringField(m, "s"); got != " v " {
		t.Errorf("stringField 不该自作主张 trim（由调用方决定）: %q", got)
	}
	if got := stringField(m, "num"); got != "21" {
		t.Errorf("数字应转字符串: %q", got)
	}
	if got := stringField(m, "numb"); got != "7" {
		t.Errorf("json.Number 应转字符串: %q", got)
	}
	if got := stringField(m, "bool"); got != "true" {
		t.Errorf("布尔应转字符串: %q", got)
	}
	if got := stringField(m, "other"); got != "" {
		t.Errorf("对象应返回空串: %q", got)
	}
	if got := intField(m, "num", 1); got != 21 {
		t.Errorf("intField(num) = %d", got)
	}
	if got := intField(m, "missing", 9); got != 9 {
		t.Errorf("缺失应回默认: %d", got)
	}
	if got := sliceField(m, "missing"); got != nil {
		t.Error("缺失的数组应为 nil")
	}
	if got := stringField(nil, "x"); got != "" {
		t.Error("nil map 应返回空串")
	}
	if got := intField(nil, "x", 3); got != 3 {
		t.Error("nil map 应回默认")
	}
}
