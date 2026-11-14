package fzu

import (
	"math"
	"strings"
	"testing"

	"fzucheckin/internal/model"
)

func configWith(checkin model.CheckinConfig) model.Config {
	return model.Config{Checkin: checkin}
}

func poolConfig(items ...model.Location) model.Config {
	return configWith(model.CheckinConfig{Locations: items})
}

// ---------- 单点 ----------

func TestResolveLocationSinglePoint(t *testing.T) {
	cfg := configWith(model.CheckinConfig{
		Longitude:      " 119.20463 ",
		Latitude:       "26.04920",
		ActualLocation: " 福州大学旗山校区生活区X号楼 ",
	})
	got, err := ResolveLocation(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveLocation 返回错误: %v", err)
	}
	if got.Label != "single" {
		t.Errorf("Label = %q，期望 single", got.Label)
	}
	if !nearlyEqual(got.Lon, 119.20463) || !nearlyEqual(got.Lat, 26.04920) {
		t.Errorf("坐标解析错误: %v %v", got.Lon, got.Lat)
	}
	if got.ActualLocation != "福州大学旗山校区生活区X号楼" {
		t.Errorf("文案应去空白: %q", got.ActualLocation)
	}
}

func TestResolveLocationEmptyActualLocation(t *testing.T) {
	cfg := configWith(model.CheckinConfig{Longitude: "119.2", Latitude: "26.0", ActualLocation: "   "})
	got, err := ResolveLocation(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveLocation 返回错误: %v", err)
	}
	if got.ActualLocation != "" {
		t.Errorf("空白文案应归一成空串: %q", got.ActualLocation)
	}
}

func TestResolveLocationBadCoordinates(t *testing.T) {
	cases := []struct {
		name    string
		checkin model.CheckinConfig
		wantSub string
	}{
		{"缺经度", model.CheckinConfig{Latitude: "26.04920"}, "checkin.longitude"},
		{"缺纬度", model.CheckinConfig{Longitude: "119.20463"}, "checkin.latitude"},
		{"经度空串", model.CheckinConfig{Longitude: "", Latitude: "26.04920"}, "checkin.longitude"},
		{"纬度只有空白", model.CheckinConfig{Longitude: "119.20463", Latitude: "   "}, "checkin.latitude"},
		{"经度写错", model.CheckinConfig{Longitude: "abc", Latitude: "26.04920"}, "checkin.longitude"},
		{"纬度带单位", model.CheckinConfig{Longitude: "119.20463", Latitude: "26.04920N"}, "checkin.latitude"},
		{"NaN", model.CheckinConfig{Longitude: "NaN", Latitude: "26.0"}, "checkin.longitude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveLocation(configWith(tc.checkin), nil)
			if err == nil {
				t.Fatal("期望报错")
			}
			msg := err.Error()
			// 报错必须指出改哪儿、改成什么样，否则用户只能去猜。
			for _, sub := range []string{tc.wantSub, "config", "lbs.amap.com/tools/picker", "119.20463"} {
				if !strings.Contains(msg, sub) {
					t.Errorf("错误信息缺少 %q: %v", sub, msg)
				}
			}
		})
	}
}

// ---------- 位置池 ----------

func TestResolveLocationPoolWeights(t *testing.T) {
	cfg := poolConfig(
		model.Location{Longitude: "119.0", Latitude: "26.0", Weight: ptrFloat(9)},
		model.Location{Longitude: "119.1", Latitude: "26.1", Weight: ptrFloat(1)},
	)
	cases := []struct {
		draw float64
		want string
	}{
		{0.0, "locations[0]"},
		{0.5, "locations[0]"},   // 0.5*10 = 5 < 9
		{0.899, "locations[0]"}, // 8.99 < 9
		{0.95, "locations[1]"},  // 9.5 >= 9
	}
	for _, tc := range cases {
		got, err := ResolveLocation(cfg, scriptedDraw(tc.draw))
		if err != nil {
			t.Fatalf("draw=%v 报错: %v", tc.draw, err)
		}
		if got.Label != tc.want {
			t.Errorf("draw=%v 抽到 %s，期望 %s", tc.draw, got.Label, tc.want)
		}
	}
}

func TestResolveLocationPoolWeightDefaultsToOne(t *testing.T) {
	// 缺失 / 0 / 负数 / NaN 一律按 1，三项等权。
	cfg := poolConfig(
		model.Location{Longitude: "119.0", Latitude: "26.0", Weight: ptrFloat(0)},
		model.Location{Longitude: "119.1", Latitude: "26.1"},
		model.Location{Longitude: "119.2", Latitude: "26.2", Weight: ptrFloat(math.NaN())},
	)
	cases := []struct {
		draw float64
		want string
	}{
		{0.0, "locations[0]"},
		{0.5, "locations[1]"},
		{0.9, "locations[2]"},
	}
	for _, tc := range cases {
		got, err := ResolveLocation(cfg, scriptedDraw(tc.draw))
		if err != nil {
			t.Fatalf("draw=%v 报错: %v", tc.draw, err)
		}
		if got.Label != tc.want {
			t.Errorf("draw=%v 抽到 %s，期望 %s", tc.draw, got.Label, tc.want)
		}
	}
}

func TestResolveLocationPoolTextBinding(t *testing.T) {
	cfg := configWith(model.CheckinConfig{
		ActualLocation: "外层文案",
		Locations: []model.Location{
			{Longitude: "119.0", Latitude: "26.0", ActualLocation: "生活区X号楼"},
			{Longitude: "119.1", Latitude: "26.1"}, // 没写文案 → 回退外层
		},
	})
	first, err := ResolveLocation(cfg, scriptedDraw(0.0))
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if first.ActualLocation != "生活区X号楼" {
		t.Errorf("文案应绑定抽中的点: %q", first.ActualLocation)
	}
	second, err := ResolveLocation(cfg, scriptedDraw(0.9))
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if second.ActualLocation != "外层文案" {
		t.Errorf("池内没写文案时应回退外层: %q", second.ActualLocation)
	}
}

func TestResolveLocationPoolBadEntryNamesIndex(t *testing.T) {
	cfg := poolConfig(
		model.Location{Longitude: "119.0", Latitude: "26.0"},
		model.Location{Longitude: "", Latitude: "26.1"},
	)
	// 即使 draw 只会抽到好项，也必须整池预校验后报错。
	_, err := ResolveLocation(cfg, scriptedDraw(0.0))
	if err == nil {
		t.Fatal("池里有坏坐标必须报错")
	}
	if !strings.Contains(err.Error(), "checkin.locations[1].longitude") {
		t.Errorf("错误应点名第几项第几个键: %v", err)
	}
}

func TestResolveLocationPoolOverridesBrokenSinglePoint(t *testing.T) {
	cfg := configWith(model.CheckinConfig{
		Longitude: "", Latitude: "",
		Locations: []model.Location{{Longitude: "119.0", Latitude: "26.0"}},
	})
	got, err := ResolveLocation(cfg, scriptedDraw(0.5))
	if err != nil {
		t.Fatalf("池子生效时旧单点键写坏不该拦住: %v", err)
	}
	if got.Label != "locations[0]" {
		t.Errorf("Label = %q", got.Label)
	}
}

func TestResolveLocationEmptyPoolFallsBackToSingle(t *testing.T) {
	cfg := configWith(model.CheckinConfig{
		Longitude: "119.20463", Latitude: "26.04920",
		ActualLocation: "单点文案", Locations: []model.Location{},
	})
	got, err := ResolveLocation(cfg, scriptedDraw(0.5))
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if got.Label != "single" || got.ActualLocation != "单点文案" {
		t.Errorf("空池应回单单点: %+v", got)
	}
}

// ---------- 抖动半径 ----------

func TestJitterRadiusPriority(t *testing.T) {
	cases := []struct {
		name string
		newV *float64
		oldV *float64
		want float64
	}{
		{"都没配 → 200", nil, nil, 200},
		{"只有新键", ptrFloat(30), nil, 30},
		{"只有旧键", nil, ptrFloat(50), 50},
		{"新键优先", ptrFloat(30), ptrFloat(200), 30},
		{"新键显式 0 关抖动", ptrFloat(0), ptrFloat(200), 0},
		{"旧键显式 0 关抖动", nil, ptrFloat(0), 0},
		{"新键负数按 200", ptrFloat(-20), ptrFloat(50), 200},
		{"旧键负数按 200", nil, ptrFloat(-5), 200},
		{"新键 NaN 按 200", ptrFloat(math.NaN()), nil, 200},
		{"新键 +Inf 按 200", ptrFloat(math.Inf(1)), nil, 200},
		{"小数", ptrFloat(12.5), nil, 12.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configWith(model.CheckinConfig{LocationJitterM: tc.newV, JitterRadiusM: tc.oldV})
			if got := JitterRadius(cfg); got != tc.want {
				t.Errorf("JitterRadius = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ---------- 圆内抖动 ----------

// distanceM 用与被测代码同一套米/度换算反算两点距离，校验抖动点确实没超出半径。
func distanceM(lon, lat, baseLon, baseLat float64) float64 {
	mPerLat, mPerLon := metersPerDegree(baseLat)
	return math.Hypot((lat-baseLat)*mPerLat, (lon-baseLon)*mPerLon)
}

func TestJitterLocationStaysWithinRadius(t *testing.T) {
	const radius = 200.0
	draw := seededDraw(20261114)
	maxDist := 0.0
	for i := 0; i < 1000; i++ {
		lon, lat := JitterLocation(baseLon, baseLat, radius, draw)
		d := distanceM(lon, lat, baseLon, baseLat)
		if d > radius*(1+1e-9) {
			t.Fatalf("第 %d 次采样越界: %.6f 米 > %.1f", i, d, radius)
		}
		if d > maxDist {
			maxDist = d
		}
	}
	// 同时确认点在圆内散开，而不是全挤在圆心附近。
	if maxDist < radius*0.9 {
		t.Errorf("最大半径 %.3f 米，采样明显贴不住边界（分布有问题）", maxDist)
	}
}

func TestJitterLocationAreaUniform(t *testing.T) {
	// r = R*sqrt(u) 才会按面积均匀：落在内半圆的点占比应约 1/4。
	const radius = 200.0
	draw := seededDraw(42)
	const samples = 3000
	inner := 0
	for i := 0; i < samples; i++ {
		lon, lat := JitterLocation(baseLon, baseLat, radius, draw)
		if distanceM(lon, lat, baseLon, baseLat) <= radius/2 {
			inner++
		}
	}
	ratio := float64(inner) / float64(samples)
	if ratio < 0.18 || ratio > 0.32 {
		t.Errorf("内半圆占比 %.3f，应收敛到 0.25 附近", ratio)
	}
}

func TestJitterLocationDisabled(t *testing.T) {
	for _, radius := range []float64{0, -1, -200, math.NaN()} {
		lon, lat := JitterLocation(baseLon, baseLat, radius, scriptedDraw(0.5, 0.5))
		if lon != baseLon || lat != baseLat {
			t.Errorf("radius=%v 时应原样返回，实际 %v,%v", radius, lon, lat)
		}
	}
	// nil draw（生产路径）走 math/rand，点必须仍在圆内且不等于原点。
	lon, lat := JitterLocation(baseLon, baseLat, 200, nil)
	if d := distanceM(lon, lat, baseLon, baseLat); d > 200*(1+1e-9) {
		t.Errorf("生产路径抖动越界: %v", d)
	}
}

func TestJitterLocationExtremeDrawsAreClamped(t *testing.T) {
	// 注入的序列越界（1 或负数）不该把点甩出圆外或算出 NaN。
	for _, us := range [][]float64{{1, 1}, {-1, 0.5}, {2, math.NaN()}} {
		lon, lat := JitterLocation(baseLon, baseLat, 200, scriptedDraw(us...))
		if math.IsNaN(lon) || math.IsNaN(lat) {
			t.Fatalf("draw=%v 产生了 NaN", us)
		}
		if d := distanceM(lon, lat, baseLon, baseLat); d > 200*(1+1e-9) {
			t.Errorf("draw=%v 越界: %v", us, d)
		}
	}
}

func TestMetersPerDegree(t *testing.T) {
	mPerLat, mPerLon := metersPerDegree(26.0492)
	if mPerLat < 110000 || mPerLat > 112000 {
		t.Errorf("1° 纬度的米数不像样: %v", mPerLat)
	}
	if mPerLon < 90000 || mPerLon > 112000 {
		t.Errorf("1° 经度的米数不像样: %v", mPerLon)
	}
	// 近极点兜底：不能返回 0 或负数，否则会除零/反号。
	if _, mPerLon := metersPerDegree(90); mPerLon < 1e-9 {
		t.Errorf("近极点处 mPerLon 应被兜底: %v", mPerLon)
	}
}
