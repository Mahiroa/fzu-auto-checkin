package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fzucheckin/internal/model"
)

func warnText(warnings []string) string { return strings.Join(warnings, "\n") }

// hasWarn 断言"某条警告里同时出现这些片段"，避免测试锁死整句文案。
func hasWarn(t *testing.T, warnings []string, parts ...string) {
	t.Helper()
	for _, w := range warnings {
		all := true
		for _, p := range parts {
			if !strings.Contains(w, p) {
				all = false
				break
			}
		}
		if all {
			return
		}
	}
	t.Fatalf("没有找到包含 %q 的警告，实际警告：\n%s", parts, warnText(warnings))
}

func ptr(v float64) *float64 { return &v }

func baseConfig() model.Config {
	cfg := DefaultConfig()
	cfg.User.Username = "2021001"
	cfg.User.Password = "pw"
	cfg.Checkin.Longitude = "119.20463"
	cfg.Checkin.Latitude = "26.04920"
	return cfg
}

// 默认模板承担"首次启动就能跑"的责任：数值必须与 Python 版口径一致。
func TestDefaultConfigIsSafe(t *testing.T) {
	cfg := DefaultConfig()
	s := cfg.Checkin.Schedule
	if s.WindowStart != "21:00" || s.WindowEnd != "23:59" {
		t.Fatalf("默认窗口不对: %s-%s", s.WindowStart, s.WindowEnd)
	}
	if len(s.Times) != 1 || s.Times[0] != "21:35" {
		t.Fatalf("默认时刻不对: %v", s.Times)
	}
	if s.MaxWaitMinutes == nil || *s.MaxWaitMinutes != 12 {
		t.Fatalf("默认 max_wait 不对: %v", s.MaxWaitMinutes)
	}
	if s.ToleranceMinutes == nil || *s.ToleranceMinutes != 5 {
		t.Fatalf("默认 tolerance 不对: %v", s.ToleranceMinutes)
	}
	if s.TimeJitterMinutes == nil || *s.TimeJitterMinutes != 0 {
		t.Fatalf("默认 time_jitter 不对: %v", s.TimeJitterMinutes)
	}
	if cfg.Checkin.JitterRadiusM == nil || *cfg.Checkin.JitterRadiusM != 200 {
		t.Fatalf("默认抖动半径不对: %v", cfg.Checkin.JitterRadiusM)
	}
	if cfg.Checkin.LocationJitterM != nil {
		t.Fatalf("新键 location_jitter_m 默认应为未配置（nil）")
	}
	if cfg.Server.Listen != "127.0.0.1:8787" || cfg.Server.DataDir != "./data" {
		t.Fatalf("默认服务参数不对: %+v", cfg.Server)
	}
	if cfg.Notify.Type != "none" {
		t.Fatalf("默认推送通道应为 none: %q", cfg.Notify.Type)
	}
	if len(cfg.Vacation.SkipRanges) != 2 {
		t.Fatalf("默认应预填寒暑假: %+v", cfg.Vacation.SkipRanges)
	}
	if cfg.Vacation.SkipRanges[0].Start != "2027-01-23" || cfg.Vacation.SkipRanges[0].End != "2027-02-21" {
		t.Fatalf("寒假区间不对: %+v", cfg.Vacation.SkipRanges[0])
	}
	if cfg.Vacation.SkipRanges[1].Start != "2027-07-03" || cfg.Vacation.SkipRanges[1].End != "2027-08-29" {
		t.Fatalf("暑假区间不对: %+v", cfg.Vacation.SkipRanges[1])
	}
	// 凭据与坐标留空：首次启动必须进初始化向导，不能带任何示例值上线
	if cfg.User.Token != "" || cfg.User.Password != "" || cfg.Checkin.Longitude != "" {
		t.Fatalf("默认模板不该预填凭据/坐标")
	}
	// 默认模板本身必须是"零警告"的：第一眼就看到一堆警告会让人以为坏了
	if _, warns := Normalize(cfg); len(warns) != 0 {
		t.Fatalf("默认配置不该产生警告: %v", warns)
	}
}

// 未加引号的 21:35 会被 YAML 解析成 1295，必须还原成时刻并警告。
func TestNormalizeRestoresSexagesimal(t *testing.T) {
	cfg := baseConfig()
	cfg.Checkin.Schedule.Times = []string{"1295"}
	cfg.Checkin.Schedule.WindowStart = "1260" // 21:00
	cfg.Checkin.Schedule.WindowEnd = "1310"   // 21:50
	out, warns := Normalize(cfg)
	if got := out.Checkin.Schedule.Times; len(got) != 1 || got[0] != "21:35" {
		t.Fatalf("60 进制时刻没还原: %v", got)
	}
	if out.Checkin.Schedule.WindowStart != "21:00" || out.Checkin.Schedule.WindowEnd != "21:50" {
		t.Fatalf("窗口没还原: %s-%s", out.Checkin.Schedule.WindowStart, out.Checkin.Schedule.WindowEnd)
	}
	hasWarn(t, warns, "times 第 1 项", "1295", "21:35")
	hasWarn(t, warns, "window_start", "1260")
}

func TestNormalizeTimes(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		want  []string
		warns []string
	}{
		{"正常", []string{"21:35", "22:10"}, []string{"21:35", "22:10"}, nil},
		{"首尾空白", []string{" 21:35 "}, []string{"21:35"}, nil},
		{"全角冒号", []string{"21：35"}, []string{"21:35"}, nil},
		{"单位数月日", []string{"9:5"}, []string{"09:05"}, nil},
		{"秒为 0 时省略", []string{"21:35:00"}, []string{"21:35"}, nil},
		{"带秒", []string{"21:35:30"}, []string{"21:35:30"}, nil},
		{"坏项被丢弃", []string{"21:35", "bad", "22:10"}, []string{"21:35", "22:10"}, []string{"第 2 项", "bad"}},
		{"越界小时", []string{"25:00"}, []string{"21:35"}, []string{"第 1 项", "25:00"}},
		{"越界小时回退默认", []string{"25:00"}, []string{"21:35"}, []string{"回退默认"}},
		{"空列表回退", nil, []string{"21:35"}, nil},
		{"空字符串项", []string{"", "21:50"}, []string{"21:50"}, []string{"第 1 项", "空值"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Checkin.Schedule.Times = c.in
			out, warns := Normalize(cfg)
			if len(out.Checkin.Schedule.Times) != len(c.want) {
				t.Fatalf("时刻数量不对: %v", out.Checkin.Schedule.Times)
			}
			for i := range c.want {
				if out.Checkin.Schedule.Times[i] != c.want[i] {
					t.Fatalf("时刻不对: got=%v want=%v", out.Checkin.Schedule.Times, c.want)
				}
			}
			if c.warns != nil {
				hasWarn(t, warns, c.warns...)
			} else if len(warns) != 0 {
				t.Fatalf("不该有警告: %v", warns)
			}
		})
	}
}

func TestNormalizeWindowFallback(t *testing.T) {
	cases := []struct{ start, end string }{
		{"23:00", "01:00"}, // 跨日：不支持
		{"21:00", "21:00"}, // 相等：零长度窗口没意义
		{"23:59", ""},      // 终点缺失→默认 23:59，与起点相等，同样要拦
	}
	for _, c := range cases {
		cfg := baseConfig()
		cfg.Checkin.Schedule.WindowStart = c.start
		cfg.Checkin.Schedule.WindowEnd = c.end
		out, warns := Normalize(cfg)
		if out.Checkin.Schedule.WindowStart != DefaultWindowStart || out.Checkin.Schedule.WindowEnd != DefaultWindowEnd {
			t.Fatalf("%q-%q 应回退默认窗口，实际 %s-%s", c.start, c.end,
				out.Checkin.Schedule.WindowStart, out.Checkin.Schedule.WindowEnd)
		}
		hasWarn(t, warns, "跨日")
	}
}

func TestNormalizeBadWindowString(t *testing.T) {
	cfg := baseConfig()
	cfg.Checkin.Schedule.WindowStart = "晚上九点"
	out, warns := Normalize(cfg)
	if out.Checkin.Schedule.WindowStart != DefaultWindowStart {
		t.Fatalf("坏窗口应回退默认: %q", out.Checkin.Schedule.WindowStart)
	}
	hasWarn(t, warns, "window_start", "晚上九点")
}

// 数值项：负数按 0（负数没有语义），NaN/Inf 视为未配置。
func TestNormalizeNumbers(t *testing.T) {
	t.Run("负数按 0", func(t *testing.T) {
		cfg := baseConfig()
		s := &cfg.Checkin.Schedule
		s.TimeJitterMinutes = ptr(-3)
		s.MaxWaitMinutes = ptr(-1)
		s.ToleranceMinutes = ptr(-0.5)
		out, warns := Normalize(cfg)
		for _, p := range []*float64{
			out.Checkin.Schedule.TimeJitterMinutes,
			out.Checkin.Schedule.MaxWaitMinutes,
			out.Checkin.Schedule.ToleranceMinutes,
		} {
			if p == nil || *p != 0 {
				t.Fatalf("负数应归 0: %v", p)
			}
		}
		hasWarn(t, warns, "负数", "已按 0 处理")
	})

	t.Run("NaN 与 Inf 视为未配置", func(t *testing.T) {
		cfg := baseConfig()
		s := &cfg.Checkin.Schedule
		s.TimeJitterMinutes = ptr(math.NaN())
		s.MaxWaitMinutes = ptr(math.Inf(1))
		out, warns := Normalize(cfg)
		if out.Checkin.Schedule.TimeJitterMinutes != nil || out.Checkin.Schedule.MaxWaitMinutes != nil {
			t.Fatalf("NaN/Inf 应清成 nil: %+v %+v",
				out.Checkin.Schedule.TimeJitterMinutes, out.Checkin.Schedule.MaxWaitMinutes)
		}
		hasWarn(t, warns, "NaN/Inf")
	})

	t.Run("半径负数按 200，显式 0 保留", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.JitterRadiusM = ptr(-10)
		cfg.Checkin.LocationJitterM = ptr(0)
		out, warns := Normalize(cfg)
		if out.Checkin.JitterRadiusM == nil || *out.Checkin.JitterRadiusM != 200 {
			t.Fatalf("负数半径应回 200: %v", out.Checkin.JitterRadiusM)
		}
		if out.Checkin.LocationJitterM == nil || *out.Checkin.LocationJitterM != 0 {
			t.Fatalf("显式 0 必须保留（=关闭抖动）: %v", out.Checkin.LocationJitterM)
		}
		hasWarn(t, warns, "jitter_radius_m", "负数")
	})

	t.Run("半径 NaN 视为未配置", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.JitterRadiusM = ptr(math.NaN())
		out, _ := Normalize(cfg)
		if out.Checkin.JitterRadiusM != nil {
			t.Fatalf("NaN 半径应清成 nil")
		}
	})
}

// Normalize 先 Clone：返回修好的副本，不污染调用方手里的对象。
func TestNormalizeDoesNotMutateInput(t *testing.T) {
	cfg := baseConfig()
	cfg.Checkin.JitterRadiusM = ptr(-10)
	cfg.Checkin.Schedule.Times = []string{"1295"}
	out, _ := Normalize(cfg)
	if *cfg.Checkin.JitterRadiusM != -10 {
		t.Fatalf("入参被改了: %v", *cfg.Checkin.JitterRadiusM)
	}
	if cfg.Checkin.Schedule.Times[0] != "1295" {
		t.Fatalf("入参的 times 被改了: %v", cfg.Checkin.Schedule.Times)
	}
	if *out.Checkin.JitterRadiusM != 200 || out.Checkin.Schedule.Times[0] != "21:35" {
		t.Fatalf("返回值没修好: %+v", out.Checkin)
	}
}

func TestNormalizeLocations(t *testing.T) {
	t.Run("剔除缺坐标的项并报索引", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.Locations = []model.Location{
			{Longitude: "119.20463", Latitude: "26.04920", ActualLocation: "宿舍"},
			{Longitude: "119.20510"},                 // 缺纬度
			{Longitude: "abc", Latitude: "26.04880"}, // 非数字
		}
		out, warns := Normalize(cfg)
		if len(out.Checkin.Locations) != 1 {
			t.Fatalf("应只剩 1 项: %+v", out.Checkin.Locations)
		}
		hasWarn(t, warns, "第 2、3 项", AMAPPicker)
	})

	t.Run("位置池非空时不校验单点坐标", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.Longitude = "119.2046O" // 字母 O
		cfg.Checkin.Locations = []model.Location{
			{Longitude: "119.20463", Latitude: "26.04920"},
		}
		_, warns := Normalize(cfg)
		for _, w := range warns {
			if strings.Contains(w, "checkin.longitude") {
				t.Fatalf("位置池非空时不该再警告单点坐标: %s", w)
			}
		}
	})

	t.Run("单点坐标非数字给出键名与改法", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.Latitude = "26.04O920"
		_, warns := Normalize(cfg)
		hasWarn(t, warns, "checkin.latitude", "26.04O920", AMAPPicker)
	})

	t.Run("权重", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Checkin.Locations = []model.Location{
			{Longitude: "119.1", Latitude: "26.1", Weight: ptr(0)},
			{Longitude: "119.2", Latitude: "26.2", Weight: ptr(3)},
			{Longitude: "119.3", Latitude: "26.3", Weight: ptr(math.NaN())},
			{Longitude: "119.4", Latitude: "26.4"}, // nil 保持 nil，由下游按 1 处理
		}
		out, warns := Normalize(cfg)
		if len(out.Checkin.Locations) != 4 {
			t.Fatalf("权重问题不该剔除项: %d", len(out.Checkin.Locations))
		}
		if got := out.Checkin.Locations[0].Weight; got == nil || *got != 1 {
			t.Fatalf("weight=0 应改成 1: %v", got)
		}
		if got := out.Checkin.Locations[1].Weight; got == nil || *got != 3 {
			t.Fatalf("合法权重应保留: %v", got)
		}
		if got := out.Checkin.Locations[2].Weight; got == nil || *got != 1 {
			t.Fatalf("NaN 权重应改成 1: %v", got)
		}
		if out.Checkin.Locations[3].Weight != nil {
			t.Fatalf("nil 权重应保持 nil（不凭空写出 weight: 1）")
		}
		hasWarn(t, warns, "第 1 项", "weight")
	})
}

func TestNormalizeVacation(t *testing.T) {
	cfg := baseConfig()
	cfg.Vacation.SkipRanges = []model.VacationRange{
		{Name: "寒假", Start: "2027-01-23", End: "2027-02-21"},
		{Name: "写错日期", Start: "2027-1-3", End: "2027-02-01"},
		{Name: "颠倒", Start: "2027-03-01", End: "2027-02-01"},
		{Start: "2027-04-01", End: "2027-04-07"}, // 没写名字
	}
	out, warns := Normalize(cfg)
	if len(out.Vacation.SkipRanges) != 2 {
		t.Fatalf("应只保留 2 个合法区间: %+v", out.Vacation.SkipRanges)
	}
	if out.Vacation.SkipRanges[1].Name == "" {
		t.Fatalf("没写名字的区间应自动补一个可读名字")
	}
	hasWarn(t, warns, "第 2 项", "YYYY-MM-DD")
	hasWarn(t, warns, "第 3 项", "早于")
}

func TestNormalizeNotifyAndServer(t *testing.T) {
	cfg := baseConfig()
	cfg.Notify.Type = "  ServerChan "
	cfg.Notify.ServerChanKey = " KEY "
	cfg.Server.Listen = ""
	cfg.Server.DataDir = "  "
	cfg.Server.AllowOrigins = []string{"", " http://localhost:5173 ", "  "}
	out, warns := Normalize(cfg)
	if out.Notify.Type != "serverchan" {
		t.Fatalf("type 应小写去空白: %q", out.Notify.Type)
	}
	if out.Notify.ServerChanKey != "KEY" {
		t.Fatalf("key 应去空白: %q", out.Notify.ServerChanKey)
	}
	if out.Server.Listen != DefaultListen || out.Server.DataDir != DefaultDataDir {
		t.Fatalf("空值应回默认: %+v", out.Server)
	}
	if len(out.Server.AllowOrigins) != 1 || out.Server.AllowOrigins[0] != "http://localhost:5173" {
		t.Fatalf("跨源白名单应清理空项: %v", out.Server.AllowOrigins)
	}
	if len(warns) != 0 {
		t.Fatalf("这些都不该是警告: %v", warns)
	}

	cfg2 := baseConfig()
	cfg2.Notify.Type = "telegram"
	out2, warns2 := Normalize(cfg2)
	if out2.Notify.Type != "none" {
		t.Fatalf("未知通道应降级 none: %q", out2.Notify.Type)
	}
	hasWarn(t, warns2, "telegram", "serverchan | pushplus | bark | wecom | none")
}

// ---------- 校验 ----------

func TestValidate(t *testing.T) {
	t.Run("通过：token", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.User.Token = "tok"
		cfg.Checkin.Longitude = "119.20463"
		cfg.Checkin.Latitude = "26.04920"
		if err := Validate(cfg); err != nil {
			t.Fatalf("应通过: %v", err)
		}
	})

	t.Run("通过：账密 + 位置池", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.User.Username = "2021001"
		cfg.User.Password = "pw"
		cfg.Checkin.Locations = []model.Location{{Longitude: "119.1", Latitude: "26.1"}}
		if err := Validate(cfg); err != nil {
			t.Fatalf("应通过: %v", err)
		}
	})

	t.Run("缺凭据", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Checkin.Longitude = "119.20463"
		cfg.Checkin.Latitude = "26.04920"
		err := Validate(cfg)
		if err == nil {
			t.Fatal("应报错")
		}
		for _, want := range []string{"二选一", "user.token", "username", "password"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("报错里缺 %q:\n%s", want, err.Error())
			}
		}
	})

	t.Run("缺坐标给出两种写法", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.User.Token = "tok"
		err := Validate(cfg)
		if err == nil {
			t.Fatal("应报错")
		}
		for _, want := range []string{AMAPPicker, "写法一", "写法二", "locations:", "weight: 1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("报错里缺 %q:\n%s", want, err.Error())
			}
		}
	})

	t.Run("坐标非数字时点名键与值", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.User.Token = "tok"
		cfg.Checkin.Longitude = "119.2046O"
		err := Validate(cfg)
		if err == nil {
			t.Fatal("应报错")
		}
		if !strings.Contains(err.Error(), "checkin.longitude") || !strings.Contains(err.Error(), "119.2046O") {
			t.Fatalf("应点名是哪个键、原值是多少:\n%s", err.Error())
		}
	})

	t.Run("一次列出全部问题", func(t *testing.T) {
		cfg := DefaultConfig()
		err := Validate(cfg)
		if err == nil {
			t.Fatal("应报错")
		}
		msg := err.Error()
		if !strings.Contains(msg, "认证信息没配好") || !strings.Contains(msg, "签到位置没配好") {
			t.Fatalf("缺凭据与缺坐标应一次性全部报出:\n%s", msg)
		}
	})

	t.Run("非法推送通道", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Notify.Type = "telegram"
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "serverchan") {
			t.Fatalf("应报枚举: %v", err)
		}
	})

	t.Run("Normalize 之后不再因通道报错", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Notify.Type = "telegram"
		out, _ := Normalize(cfg)
		if err := Validate(out); err != nil {
			t.Fatalf("降级后应通过: %v", err)
		}
	})

	t.Run("坐标是 NaN 不算可用", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.User.Token = "tok"
		cfg.Checkin.Longitude = "NaN"
		cfg.Checkin.Latitude = "26.04"
		if err := Validate(cfg); err == nil {
			t.Fatal("NaN 坐标不应通过")
		}
	})
}

// ---------- 脱敏 ----------

func secretConfig() model.Config {
	cfg := baseConfig()
	cfg.User.Password = "hunter2-should-not-leak"
	cfg.User.Token = "tok-abcdef-should-not-leak"
	cfg.Notify.Type = "serverchan"
	cfg.Notify.ServerChanKey = "SCKEY-should-not-leak"
	cfg.Notify.PushPlusToken = "PP-should-not-leak"
	cfg.Notify.BarkURL = "https://bark.day.app/SECRET-should-not-leak"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/SECRET-should-not-leak"
	return cfg
}

func TestMask(t *testing.T) {
	cfg := secretConfig()
	masked := Mask(cfg)

	for _, got := range []string{
		masked.User.Password, masked.User.Token, masked.Notify.ServerChanKey,
		masked.Notify.PushPlusToken, masked.Notify.BarkURL, masked.Notify.WecomWebhook,
	} {
		if got != MaskSentinel {
			t.Fatalf("敏感字段应变成哨兵: %q", got)
		}
	}
	// 坐标/学号不脱敏：用户要看得见自己配的地点
	if masked.Checkin.Longitude != cfg.Checkin.Longitude || masked.User.Username != cfg.User.Username {
		t.Fatalf("非敏感字段不该被脱敏")
	}
	// 原对象不能被改（Mask 返回值是新副本）
	if cfg.User.Password != "hunter2-should-not-leak" {
		t.Fatalf("Mask 改到了原对象")
	}

	raw, err := json.Marshal(masked)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	text := string(raw)
	for _, leak := range []string{
		"hunter2-should-not-leak", "tok-abcdef-should-not-leak", "SCKEY-should-not-leak",
		"PP-should-not-leak", "SECRET-should-not-leak",
	} {
		if strings.Contains(text, leak) {
			t.Fatalf("脱敏后的 JSON 里仍出现 %q:\n%s", leak, text)
		}
	}
	if !strings.Contains(text, MaskSentinel) {
		t.Fatalf("JSON 里应有哨兵值:\n%s", text)
	}
	// 幂等：再脱敏一次结果不变
	if again := Mask(masked); again.User.Password != MaskSentinel {
		t.Fatalf("哨兵应保持哨兵: %q", again.User.Password)
	}
}

func TestMaskKeepsEmptyAsEmpty(t *testing.T) {
	cfg := baseConfig() // 只有账密，没有 token 与推送 key
	masked := Mask(cfg)
	if masked.User.Token != "" {
		t.Fatalf("空字段应保持空（否则前端没法表达“清空”）: %q", masked.User.Token)
	}
	if masked.User.Password != MaskSentinel {
		t.Fatalf("有值的字段应脱敏")
	}
}

func TestMaskedFields(t *testing.T) {
	cfg := secretConfig()
	cfg.Notify.PushPlusToken = "   " // 只有空格算未配置
	got := MaskedFields(cfg)
	if len(got) != len(sensitivePaths) {
		t.Fatalf("应返回全部 %d 个敏感路径，实际 %d: %v", len(sensitivePaths), len(got), got)
	}
	if !got["user.password"] || !got["user.token"] || !got["notify.serverchan_key"] {
		t.Fatalf("已配置的字段应为 true: %v", got)
	}
	if got["notify.pushplus_token"] {
		t.Fatalf("只有空格的字段应视为未配置: %v", got)
	}

	empty := MaskedFields(DefaultConfig())
	for k, v := range empty {
		if v {
			t.Fatalf("默认配置不该有已配置字段: %s", k)
		}
	}
}

func TestMergeMasked(t *testing.T) {
	oldCfg := secretConfig()
	oldCfg.User.Password = "old-pw"
	oldCfg.Notify.ServerChanKey = "old-key"
	oldCfg.Notify.BarkURL = "old-bark"

	t.Run("哨兵保持原值", func(t *testing.T) {
		in := Mask(oldCfg)
		in.Checkin.Schedule.Times = []string{"22:00"}
		out := MergeMasked(oldCfg, in)
		if out.User.Password != "old-pw" || out.Notify.ServerChanKey != "old-key" || out.Notify.BarkURL != "old-bark" {
			t.Fatalf("哨兵应保持原值: %+v %+v", out.User, out.Notify)
		}
		if out.Checkin.Schedule.Times[0] != "22:00" {
			t.Fatalf("非敏感字段应取 incoming")
		}
	})

	t.Run("新值生效", func(t *testing.T) {
		in := Mask(oldCfg)
		in.User.Password = "new-pw"
		out := MergeMasked(oldCfg, in)
		if out.User.Password != "new-pw" {
			t.Fatalf("新值应生效: %q", out.User.Password)
		}
		if out.User.Token != "tok-abcdef-should-not-leak" {
			t.Fatalf("没改的字段应保持原值: %q", out.User.Token)
		}
	})

	t.Run("空串表示清空", func(t *testing.T) {
		in := Mask(oldCfg)
		in.Notify.ServerChanKey = ""
		in.User.Password = ""
		out := MergeMasked(oldCfg, in)
		if out.Notify.ServerChanKey != "" || out.User.Password != "" {
			t.Fatalf("空串应清空: %+v %+v", out.User, out.Notify)
		}
	})

	t.Run("不改动入参", func(t *testing.T) {
		old2 := secretConfig()
		in := Mask(old2)
		_ = MergeMasked(old2, in)
		if in.User.Password != MaskSentinel {
			t.Fatalf("MergeMasked 改到了 incoming")
		}
		if old2.User.Password != "hunter2-should-not-leak" {
			t.Fatalf("MergeMasked 改到了 oldCfg")
		}
	})
}

// ---------- YAML 导入 ----------

func TestImportYAMLFull(t *testing.T) {
	src := `
# 完整示例
user:
  token: "tok"
  username: "2021001"
  password: "pw"
checkin:
  longitude: "119.20463"
  latitude: "26.04920"
  actual_location: "福州大学旗山校区生活区X号楼"
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "宿舍"
      weight: 3
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "教学楼"
  jitter_radius_m: 150
  location_jitter_m: 80
  schedule:
    window_start: "21:00"
    window_end: "23:30"
    times: ["21:35", "22:10"]
    time_jitter_minutes: 2.5
    max_wait_minutes: 10
    tolerance_minutes: 4
vacation:
  notify: true
  skip_ranges:
    - name: 寒假
      start: "2027-01-23"
      end: "2027-02-21"
notify:
  type: "pushplus"
  pushplus_token: "pp"
server:
  listen: "0.0.0.0:9000"
  data_dir: "/var/lib/fzu"
  allow_origins:
    - "http://a.example"
`
	cfg, warns, err := ImportYAML(src)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("完整配置不该有警告: %v", warns)
	}
	if cfg.User.Token != "tok" || cfg.User.Username != "2021001" || cfg.User.Password != "pw" {
		t.Fatalf("user 段不对: %+v", cfg.User)
	}
	if cfg.Checkin.Longitude != "119.20463" || cfg.Checkin.ActualLocation != "福州大学旗山校区生活区X号楼" {
		t.Fatalf("单点不对: %+v", cfg.Checkin)
	}
	if len(cfg.Checkin.Locations) != 2 {
		t.Fatalf("位置池不对: %+v", cfg.Checkin.Locations)
	}
	if w := cfg.Checkin.Locations[0].Weight; w == nil || *w != 3 {
		t.Fatalf("权重不对: %v", w)
	}
	if cfg.Checkin.JitterRadiusM == nil || *cfg.Checkin.JitterRadiusM != 150 {
		t.Fatalf("jitter_radius_m 不对: %v", cfg.Checkin.JitterRadiusM)
	}
	if cfg.Checkin.LocationJitterM == nil || *cfg.Checkin.LocationJitterM != 80 {
		t.Fatalf("location_jitter_m 不对: %v", cfg.Checkin.LocationJitterM)
	}
	s := cfg.Checkin.Schedule
	if s.WindowEnd != "23:30" || len(s.Times) != 2 || s.Times[1] != "22:10" {
		t.Fatalf("schedule 不对: %+v", s)
	}
	if s.TimeJitterMinutes == nil || *s.TimeJitterMinutes != 2.5 {
		t.Fatalf("抖动分钟不对: %v", s.TimeJitterMinutes)
	}
	if len(cfg.Vacation.SkipRanges) != 1 || cfg.Vacation.SkipRanges[0].Name != "寒假" {
		t.Fatalf("假期不对: %+v", cfg.Vacation.SkipRanges)
	}
	if !cfg.Vacation.Notify {
		t.Fatalf("vacation.notify 应为 true")
	}
	if cfg.Notify.Type != "pushplus" || cfg.Notify.PushPlusToken != "pp" {
		t.Fatalf("notify 不对: %+v", cfg.Notify)
	}
	if cfg.Server.Listen != "0.0.0.0:9000" || len(cfg.Server.AllowOrigins) != 1 {
		t.Fatalf("server 不对: %+v", cfg.Server)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("这份配置应通过校验: %v", err)
	}
}

// 导入旧 config.yaml 是这条链路的核心用途，直接拿仓库里的示例文件回归。
func TestImportExampleConfigYAML(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("示例配置不存在（%s），跳过", path)
	}
	cfg, warns, ierr := ImportYAML(string(raw))
	if ierr != nil {
		t.Fatalf("示例配置导入失败: %v", ierr)
	}
	// 示例里 location_jitter_m 是空串、注释很多，这些都不该变成警告
	if len(warns) != 0 {
		t.Fatalf("示例配置不该有警告: %v", warns)
	}
	if len(cfg.Checkin.Schedule.Times) != 2 || cfg.Checkin.Schedule.Times[0] != "21:35" {
		t.Fatalf("示例时刻不对: %v", cfg.Checkin.Schedule.Times)
	}
	if cfg.Checkin.JitterRadiusM == nil || *cfg.Checkin.JitterRadiusM != 200 {
		t.Fatalf("示例半径不对: %v", cfg.Checkin.JitterRadiusM)
	}
	if cfg.Notify.Type != "serverchan" {
		t.Fatalf("示例通道不对: %q", cfg.Notify.Type)
	}
	if len(cfg.Vacation.SkipRanges) != 2 {
		t.Fatalf("示例假期不对: %+v", cfg.Vacation.SkipRanges)
	}
	if len(cfg.Checkin.Locations) != 0 {
		t.Fatalf("示例 locations 是空列表: %+v", cfg.Checkin.Locations)
	}
}

func TestImportYAMLWarnings(t *testing.T) {
	t.Run("未加引号的时刻", func(t *testing.T) {
		cfg, warns, err := ImportYAML("checkin:\n  schedule:\n    times:\n      - 21:35\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		if len(cfg.Checkin.Schedule.Times) != 1 || cfg.Checkin.Schedule.Times[0] != "21:35" {
			t.Fatalf("应还原成时刻: %v", cfg.Checkin.Schedule.Times)
		}
		hasWarn(t, warns, "第 1 项", "21:35")
	})

	t.Run("times 写成字符串", func(t *testing.T) {
		cfg, warns, err := ImportYAML("checkin:\n  schedule:\n    times: \"21:35\"\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		if !hasWarnContains(warns, "times 应该是列表") {
			t.Fatalf("应警告 times 类型不对: %v", warns)
		}
		if len(cfg.Checkin.Schedule.Times) != 1 || cfg.Checkin.Schedule.Times[0] != "21:35" {
			t.Fatalf("类型写错时应回默认时刻: %v", cfg.Checkin.Schedule.Times)
		}
	})

	t.Run("locations 写成标量", func(t *testing.T) {
		_, warns, err := ImportYAML("checkin:\n  locations: \"119.2,26.0\"\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		hasWarn(t, warns, "locations 应该是列表", "119.20463")
	})

	t.Run("schedule 写成标量", func(t *testing.T) {
		_, warns, err := ImportYAML("checkin:\n  schedule: \"21:35\"\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		hasWarn(t, warns, "schedule 应该是一个映射")
	})

	t.Run("未知顶层段", func(t *testing.T) {
		_, warns, err := ImportYAML("user:\n  token: \"t\"\ncheckin1:\n  a: 1\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		hasWarn(t, warns, `"checkin1"`, "不是本服务认识的配置段")
	})

	t.Run("数值项写成文字", func(t *testing.T) {
		cfg, warns, err := ImportYAML("checkin:\n  schedule:\n    max_wait_minutes: \"十分钟\"\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		// 写错的值被丢弃，落到默认模板的 12 分钟（等价于 Python 版"丢键走默认"）
		if cfg.Checkin.Schedule.MaxWaitMinutes == nil || *cfg.Checkin.Schedule.MaxWaitMinutes != 12 {
			t.Fatalf("非数字应回落到默认 12 分钟: %v", cfg.Checkin.Schedule.MaxWaitMinutes)
		}
		hasWarn(t, warns, "max_wait_minutes", "十分钟")
	})

	t.Run("user 段写成列表", func(t *testing.T) {
		_, warns, err := ImportYAML("user:\n  - a\n")
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		hasWarn(t, warns, "user 段应该是映射")
	})

	t.Run("位置池里缺坐标的项按索引剔除", func(t *testing.T) {
		src := "checkin:\n  locations:\n    - longitude: \"119.1\"\n      latitude: \"26.1\"\n    - longitude: \"119.2\"\n    - 不是映射\n"
		cfg, warns, err := ImportYAML(src)
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		if len(cfg.Checkin.Locations) != 1 {
			t.Fatalf("应只剩 1 项: %+v", cfg.Checkin.Locations)
		}
		hasWarn(t, warns, "第 2、3 项")
	})
}

// hasWarnContains 是 hasWarn 的宽松版：只看子串。
func hasWarnContains(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestImportYAMLErrors(t *testing.T) {
	t.Run("空文档", func(t *testing.T) {
		if _, _, err := ImportYAML(""); err == nil {
			t.Fatal("空文档应报错")
		}
	})
	t.Run("顶层是列表", func(t *testing.T) {
		if _, _, err := ImportYAML("- a\n- b\n"); err == nil || !strings.Contains(err.Error(), "顶层必须是映射") {
			t.Fatalf("应报顶层结构错: %v", err)
		}
	})
	t.Run("语法错误带行号", func(t *testing.T) {
		_, _, err := ImportYAML("user:\n\ttoken: \"x\"\n")
		if err == nil || !strings.Contains(err.Error(), "第 2 行") {
			t.Fatalf("应报行号: %v", err)
		}
	})
	t.Run("块标量", func(t *testing.T) {
		_, _, err := ImportYAML("notify:\n  type: |\n    多行\n")
		if err == nil || !strings.Contains(err.Error(), "块标量") {
			t.Fatalf("应报块标量不支持: %v", err)
		}
	})
}

// 导入不该凭空塞进假期：否则 2027 年 1 月用户会莫名其妙"不签到了"。
func TestImportYAMLDoesNotInjectVacation(t *testing.T) {
	cfg, _, err := ImportYAML("user:\n  token: \"t\"\ncheckin:\n  longitude: \"119.2\"\n  latitude: \"26.0\"\n")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(cfg.Vacation.SkipRanges) != 0 {
		t.Fatalf("不该注入默认假期: %+v", cfg.Vacation.SkipRanges)
	}
}

func TestImportYAMLDefaultsWhenSectionsMissing(t *testing.T) {
	cfg, warns, err := ImportYAML("user:\n  token: \"t\"\n")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("缺段不报错: %v", warns)
	}
	if cfg.Checkin.Schedule.WindowStart != DefaultWindowStart || cfg.Checkin.Schedule.Times[0] != "21:35" {
		t.Fatalf("缺 schedule 应落默认: %+v", cfg.Checkin.Schedule)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Fatalf("缺 server 段应落默认 listen: %q", cfg.Server.Listen)
	}
	if cfg.Notify.Type != "none" {
		t.Fatalf("缺 notify 段应落 none: %q", cfg.Notify.Type)
	}
}

// ---------- YAML 导出 ----------

func TestToYAMLRoundTrip(t *testing.T) {
	cfg := secretConfig()
	cfg.Checkin.Locations = []model.Location{
		{Longitude: "119.20463", Latitude: "26.04920", ActualLocation: "宿舍", Weight: ptr(3)},
		{Longitude: "119.20510", Latitude: "26.04880", ActualLocation: "教学楼"},
	}
	cfg.Checkin.LocationJitterM = ptr(80)
	cfg.Checkin.Schedule.Times = []string{"21:35", "22:10"}
	cfg.Vacation.SkipRanges = []model.VacationRange{{Name: "寒假", Start: "2027-01-23", End: "2027-02-21"}}
	cfg.Server.AllowOrigins = []string{"http://a.example"}
	masked := Mask(cfg)

	text := ToYAML(masked)
	for _, leak := range []string{"hunter2-should-not-leak", "SCKEY-should-not-leak"} {
		if strings.Contains(text, leak) {
			t.Fatalf("导出文本泄漏了 %q:\n%s", leak, text)
		}
	}
	if strings.Contains(text, "明文凭据") {
		t.Fatalf("脱敏后不该出现明文警告:\n%s", text)
	}

	back, warns, err := ImportYAML(text)
	if err != nil {
		t.Fatalf("导出的 YAML 无法读回: %v\n%s", err, text)
	}
	if len(warns) != 0 {
		t.Fatalf("往返不该产生警告: %v\n%s", warns, text)
	}
	if back.User.Password != MaskSentinel || back.User.Token != MaskSentinel {
		t.Fatalf("哨兵应原样往返: %+v", back.User)
	}
	if back.Checkin.Longitude != masked.Checkin.Longitude || back.Checkin.ActualLocation != masked.Checkin.ActualLocation {
		t.Fatalf("单点坐标没往返一致: %+v", back.Checkin)
	}
	if len(back.Checkin.Locations) != 2 {
		t.Fatalf("位置池没往返一致: %+v", back.Checkin.Locations)
	}
	if w := back.Checkin.Locations[0].Weight; w == nil || *w != 3 {
		t.Fatalf("权重没往返一致: %v", w)
	}
	if back.Checkin.Locations[1].Weight != nil {
		t.Fatalf("nil 权重应往返成 nil")
	}
	if back.Checkin.JitterRadiusM == nil || *back.Checkin.JitterRadiusM != *masked.Checkin.JitterRadiusM {
		t.Fatalf("jitter_radius_m 没往返一致")
	}
	if back.Checkin.LocationJitterM == nil || *back.Checkin.LocationJitterM != 80 {
		t.Fatalf("location_jitter_m 没往返一致")
	}
	if len(back.Checkin.Schedule.Times) != 2 || back.Checkin.Schedule.Times[1] != "22:10" {
		t.Fatalf("times 没往返一致: %v", back.Checkin.Schedule.Times)
	}
	if back.Checkin.Schedule.MaxWaitMinutes == nil || *back.Checkin.Schedule.MaxWaitMinutes != 12 {
		t.Fatalf("max_wait 没往返一致")
	}
	if len(back.Vacation.SkipRanges) != 1 || back.Vacation.SkipRanges[0].Start != "2027-01-23" {
		t.Fatalf("假期没往返一致: %+v", back.Vacation.SkipRanges)
	}
	if back.Notify.Type != masked.Notify.Type {
		t.Fatalf("通道没往返一致: %q", back.Notify.Type)
	}
	if back.Server.Listen != DefaultListen || len(back.Server.AllowOrigins) != 1 {
		t.Fatalf("server 段没往返一致: %+v", back.Server)
	}
}

func TestToYAMLRoundTripDefault(t *testing.T) {
	text := ToYAML(DefaultConfig())
	back, warns, err := ImportYAML(text)
	if err != nil {
		t.Fatalf("默认配置导出后读不回: %v\n%s", err, text)
	}
	if len(warns) != 0 {
		t.Fatalf("默认配置往返不该有警告: %v\n%s", warns, text)
	}
	if len(back.Vacation.SkipRanges) != 2 {
		t.Fatalf("默认假期没往返: %+v", back.Vacation.SkipRanges)
	}
}

// 未脱敏就导出时必须显式提醒：一次 GET /config/export 就能把 token 泄漏到剪贴板。
func TestToYAMLWarnsOnPlainSecrets(t *testing.T) {
	text := ToYAML(secretConfig())
	if !strings.Contains(text, "明文凭据") {
		t.Fatalf("未脱敏导出应有明显警告:\n%s", text)
	}
}

func TestParseClock(t *testing.T) {
	ok := map[string]string{
		"21:35":    "21:35",
		" 9:5 ":    "09:05",
		"21：35":    "21:35",
		"21:35:00": "21:35",
		"21:35:59": "21:35:59",
		"0:0":      "00:00",
		"23:59":    "23:59",
	}
	for in, want := range ok {
		got, valid := parseClock(in)
		if !valid || got != want {
			t.Fatalf("parseClock(%q) = %q,%v；期望 %q", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "24:00", "21:60", "21:35:60", "21:35:10:00", "abc", "21-35", "21:"} {
		if got, valid := parseClock(in); valid {
			t.Fatalf("parseClock(%q) 应失败，实际 %q", in, got)
		}
	}
}

func TestRestoreSexagesimal(t *testing.T) {
	// 1295 = 21*60+35；更大的只可能是"时:分:秒"的秒数
	if got, ok := restoreSexagesimal("1295"); !ok || got != "21:35" {
		t.Fatalf("1295 应还原成 21:35，实际 %q %v", got, ok)
	}
	if got, ok := restoreSexagesimal("1310"); !ok || got != "21:50" {
		t.Fatalf("1310 应还原成 21:50，实际 %q", got)
	}
	if got, ok := restoreSexagesimal("76259"); !ok || got != "21:10:59" {
		t.Fatalf("76259 应按秒还原，实际 %q", got)
	}
	for _, in := range []string{"", "abc", "-5", "87000", "21:35"} {
		if got, ok := restoreSexagesimal(in); ok {
			t.Fatalf("%q 不该被还原，实际 %q", in, got)
		}
	}
}

func TestParseCoordinateAndFormatNum(t *testing.T) {
	if v, ok := parseCoordinate(" 119.20463 "); !ok || v != 119.20463 {
		t.Fatalf("坐标解析失败: %v %v", v, ok)
	}
	for _, in := range []string{"", "  ", "abc", "119.2.3", "NaN", "Inf"} {
		if _, ok := parseCoordinate(in); ok {
			t.Fatalf("%q 不该被当成可用坐标", in)
		}
	}
	cases := map[float64]string{0: "0", 200: "200", -3: "-3", 2.5: "2.5", 0.1: "0.1"}
	for in, want := range cases {
		if got := formatNum(in); got != want {
			t.Fatalf("formatNum(%v) = %q want %q", in, got, want)
		}
	}
	if formatNum(math.NaN()) != "NaN" || formatNum(math.Inf(1)) != "Inf" || formatNum(math.Inf(-1)) != "-Inf" {
		t.Fatalf("特殊值格式不对: %q %q %q", formatNum(math.NaN()), formatNum(math.Inf(1)), formatNum(math.Inf(-1)))
	}
}

// Clone 必须真的深拷贝：浅拷贝会让并发读写变成数据竞争。
func TestCloneIsDeep(t *testing.T) {
	src := secretConfig()
	src.Checkin.Locations = []model.Location{{Longitude: "119.1", Latitude: "26.1", Weight: ptr(2)}}
	src.Checkin.Schedule.Times = []string{"21:35"}
	src.Server.AllowOrigins = []string{"http://a"}
	cp := Clone(src)

	cp.Checkin.Locations[0].Longitude = "改了"
	*cp.Checkin.Locations[0].Weight = 99
	cp.Checkin.Schedule.Times[0] = "改了"
	cp.Server.AllowOrigins[0] = "改了"
	*cp.Checkin.JitterRadiusM = 999

	if src.Checkin.Locations[0].Longitude != "119.1" || *src.Checkin.Locations[0].Weight != 2 {
		t.Fatalf("位置池被浅拷贝了")
	}
	if src.Checkin.Schedule.Times[0] != "21:35" || src.Server.AllowOrigins[0] != "http://a" {
		t.Fatalf("切片被浅拷贝了")
	}
	if *src.Checkin.JitterRadiusM != 200 {
		t.Fatalf("指针字段被浅拷贝了: %v", *src.Checkin.JitterRadiusM)
	}
}
