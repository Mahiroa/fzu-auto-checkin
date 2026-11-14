package fzu

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseInitCheckedStates(t *testing.T) {
	cases := []struct {
		fstate  any
		checked bool
	}{
		{"SUCCESS", true},
		{"已签到", true},
		{"  SUCCESS  ", true}, // 去空白：多一个换行就让幂等判断失效太脆
		{"未签到", false},
		{"", false},
		{nil, false},
		{"success", false}, // 大小写不同不算，宁可多问一次也不误判"已签"
	}
	for _, tc := range cases {
		init := map[string]any{"userData": map[string]any{"FState": tc.fstate}}
		plan, err := ParseInit(init)
		if err != nil {
			t.Fatalf("ParseInit(%v) 报错: %v", tc.fstate, err)
		}
		if plan.Checked != tc.checked {
			t.Errorf("FState=%v → Checked=%v，期望 %v", tc.fstate, plan.Checked, tc.checked)
		}
	}
}

func TestParseInitFieldsAndDefaults(t *testing.T) {
	init := map[string]any{
		"userData": map[string]any{
			"FState":         "未签到",
			"FDefaultCampus": " 旗山校区 ",
			"FCollUnit":      " 学院A ",
			"FWay":           float64(2),
		},
		"paramsData": map[string]any{
			"FStartTime": " 21:30 ",
			"FEndTime":   "23:59\n",
			"FLateTime":  " 22:30",
		},
		"schoolData": []any{map[string]any{"FPosition": `{"range":[{"a":1}]}`}},
	}
	plan, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit 报错: %v", err)
	}
	if plan.Start != "21:30" || plan.End != "23:59" || plan.LateLine != "22:30" {
		t.Errorf("时刻字段没去空白: %+v", plan)
	}
	if plan.CampusDefault != "旗山校区" || plan.CollUnit != "学院A" {
		t.Errorf("校区/学院字段不对: %+v", plan)
	}
	if plan.Way != 2 {
		t.Errorf("Way = %d，期望 2", plan.Way)
	}
	if !plan.HasPlan || len(plan.Polygons) != 1 {
		t.Errorf("HasPlan/Polygons 不对: %+v", plan)
	}
}

func TestParseInitWayDefaultsToOne(t *testing.T) {
	cases := []struct {
		value any
		want  int
	}{
		{nil, 1},
		{float64(1), 1},
		{float64(3), 3},
		{"2", 2},   // 字符串数字也认
		{"abc", 1}, // 写错回默认
		{true, 1},  // 类型不对回默认
		{2.9, 2},   // 浮点截断
	}
	for _, tc := range cases {
		init := map[string]any{"userData": map[string]any{"FWay": tc.value}}
		plan, err := ParseInit(init)
		if err != nil {
			t.Fatalf("ParseInit 报错: %v", err)
		}
		if plan.Way != tc.want {
			t.Errorf("FWay=%v → Way=%d，期望 %d", tc.value, plan.Way, tc.want)
		}
	}
}

func TestParseInitPolygons(t *testing.T) {
	init := map[string]any{
		"schoolData": []any{
			// 正常：FPosition 是"字符串里的 JSON"
			map[string]any{"FPosition": `{"range":[{"id":"p1"},{"id":"p2"}]}`},
			// 坏数据：解析失败应当跳过，不影响其它项
			map[string]any{"FPosition": `not a json`},
			// 缺失 FPosition
			map[string]any{"other": 1},
			// 不是对象
			"garbage",
			// 直接给对象（不再二次序列化）也要能吃
			map[string]any{"FPosition": map[string]any{"range": []any{
				map[string]any{"id": "p3"},
			}}},
			// range 缺失
			map[string]any{"FPosition": `{"other":true}`},
		},
	}
	plan, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit 报错: %v", err)
	}
	if !plan.HasPlan {
		t.Error("schoolData 非空时 HasPlan 应为 true")
	}
	if len(plan.Polygons) != 3 {
		t.Fatalf("应摊出 3 个多边形，实际 %d: %v", len(plan.Polygons), plan.Polygons)
	}
	want := []string{`{"id":"p1"}`, `{"id":"p2"}`, `{"id":"p3"}`}
	for i, w := range want {
		if string(plan.Polygons[i]) != w {
			t.Errorf("第 %d 个多边形 = %s，期望 %s", i, plan.Polygons[i], w)
		}
	}
}

func TestParseInitNoSchoolData(t *testing.T) {
	plan, err := ParseInit(map[string]any{"userData": map[string]any{"FState": "未签到"}})
	if err != nil {
		t.Fatalf("ParseInit 报错: %v", err)
	}
	if plan.HasPlan {
		t.Error("没有 schoolData 时 HasPlan 应为 false")
	}
	if len(plan.Polygons) != 0 {
		t.Errorf("不该凭空造出多边形: %v", plan.Polygons)
	}
}

func TestParseInitStructuralErrors(t *testing.T) {
	if _, err := ParseInit(nil); err == nil {
		t.Error("nil 数据应报错")
	}
	_, err := ParseInit(map[string]any{"userData": "不是对象"})
	if err == nil {
		t.Fatal("userData 类型不对应报错（说明对端改了 data 形状）")
	}
	if !strings.Contains(err.Error(), "userData") {
		t.Errorf("错误应点名字段: %v", err)
	}
}

func TestParseInitCompactsRawPolygons(t *testing.T) {
	// FPosition 里带多余空白/换行时，摊出来的多边形应当是可再次序列化的原名。
	init := map[string]any{
		"schoolData": []any{
			map[string]any{"FPosition": "{\n  \"range\": [ {\"id\": \"p1\"} ]\n}"},
		},
	}
	plan, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit 报错: %v", err)
	}
	if len(plan.Polygons) != 1 {
		t.Fatalf("应摊出 1 个多边形: %v", plan.Polygons)
	}
	var decoded map[string]any
	if err := json.Unmarshal(plan.Polygons[0], &decoded); err != nil {
		t.Fatalf("多边形不是合法 JSON: %v", err)
	}
	if decoded["id"] != "p1" {
		t.Errorf("多边形内容不对: %s", plan.Polygons[0])
	}
}
