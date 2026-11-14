package fzu

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"

	"fzucheckin/internal/model"
)

// RandFunc 是位置抖动/位置池抽签用的随机源（返回 [0,1)）。
//
// 不用 *rand.Rand 而用最小函数接口：测试要的是"按固定序列复现"，而调度包
// 用的是它自己的 Source 抽象；两个包各自定义、互不 import，靠调用方注入同一
// 串 u 就能对齐。
type RandFunc func() float64

const (
	// defaultJitterRadiusM 抖动半径默认值（米）。与 Python 一致。
	defaultJitterRadiusM = 200.0

	// longitude / latitude 的报错文案要给出"照抄就能过"的示例，所以这里放常量
	// 而不是在几处地方各写一遍。
	amapPickerURL = "https://lbs.amap.com/tools/picker"
)

// ResolveLocation 解析本次要上报的基准位置。
//
// 位置池非空时完全接管单点键：池子的存在本身就表示"我要轮换"，此时旧单点键
// 写坏了也不该拦住打卡。
func ResolveLocation(cfg model.Config, draw RandFunc) (model.Place, error) {
	if pool := cfg.Checkin.Locations; len(pool) > 0 {
		return locationFromPool(cfg.Checkin, pool, draw)
	}
	lon, err := parseCoordinate(cfg.Checkin.Longitude, "checkin.longitude")
	if err != nil {
		return model.Place{}, err
	}
	lat, err := parseCoordinate(cfg.Checkin.Latitude, "checkin.latitude")
	if err != nil {
		return model.Place{}, err
	}
	return model.Place{
		Lon:            lon,
		Lat:            lat,
		ActualLocation: actualText(cfg.Checkin.ActualLocation, ""),
		Label:          "single",
	}, nil
}

// locationFromPool 先整池校验、再按 weight 加权抽签。
//
// 整池预校验而不是只校验抽中的那项：坐标抄错时应当立刻报错并指出第几项，
// 而不是某个随机值恰好抽到才偶发失败——那种 bug 排查起来像幽灵。
func locationFromPool(checkin model.CheckinConfig, pool []model.Location,
	draw RandFunc) (model.Place, error) {

	type candidate struct {
		index  int
		lon    float64
		lat    float64
		weight float64
		text   string
	}
	candidates := make([]candidate, 0, len(pool))
	for i, item := range pool {
		where := fmt.Sprintf("checkin.locations[%d]", i)
		lon, err := parseCoordinate(item.Longitude, where+".longitude")
		if err != nil {
			return model.Place{}, err
		}
		lat, err := parseCoordinate(item.Latitude, where+".latitude")
		if err != nil {
			return model.Place{}, err
		}
		candidates = append(candidates, candidate{
			index:  i,
			lon:    lon,
			lat:    lat,
			weight: weightOf(item.Weight),
			text:   item.ActualLocation,
		})
	}

	total := 0.0
	for _, cand := range candidates {
		total += cand.weight // weightOf 已保证 >0，total 必然 >0
	}
	// 加权抽签：U(0,total) 落在哪一段就取哪个点，权重越大区间越宽。
	point := clamp01(drawOr(draw)) * total
	chosen := candidates[len(candidates)-1] // 兜底取最后一项（与 Python 一致）
	cursor := 0.0
	for _, cand := range candidates {
		cursor += cand.weight
		if point < cursor {
			chosen = cand
			break
		}
	}

	// 池内某项没写 actual_location 时沿用外层文案，避免上报名突然变成空串。
	fallback := actualText(checkin.ActualLocation, "")
	return model.Place{
		Lon:            chosen.lon,
		Lat:            chosen.lat,
		ActualLocation: actualText(chosen.text, fallback),
		Label:          fmt.Sprintf("locations[%d]", chosen.index),
	}, nil
}

// JitterRadius 本次打卡的位置抖动半径（米），0 表示不抖动。
//
// 两个键的优先级：location_jitter_m（新）> jitter_radius_m（旧）> 200。指针为 nil
// 表示"没配"，继续看下一个键；显式 0 是用户明确要关闭抖动，必须尊重——不能因为
// "0 看起来跟没填一样"又悄悄恢复成 200 米。
//
// 负数/NaN/Inf 一律按 200 处理：一个手滑的负号会把坐标抖动静默去掉，而这恰恰是
// 最不容易被发现的降级；报错又太重（配置里一个字符的事），所以退回默认值。
func JitterRadius(cfg model.Config) float64 {
	for _, raw := range []*float64{cfg.Checkin.LocationJitterM, cfg.Checkin.JitterRadiusM} {
		if raw == nil {
			continue
		}
		v := *raw
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return defaultJitterRadiusM
		}
		return v
	}
	return defaultJitterRadiusM
}

// JitterLocation 在以 (lon,lat) 为圆心、radiusM 米为半径的圆内均匀取一点。
//
// 半径按面积均匀采样（r = R*sqrt(u)），否则点会明显堆在圆心，连续多晚的坐标
// 分布比固定坐标更容易被看出是脚本；方位角 θ 在 [0,2π) 均匀分布。
// radiusM <= 0 原样返回（不抖动）；draw 为 nil 时用 math/rand 默认源。
func JitterLocation(lon, lat, radiusM float64, draw RandFunc) (float64, float64) {
	// 用 !(radiusM > 0) 而不是 radiusM <= 0：NaN 走进来时也按"关闭抖动"处理，
	// 免得一个 NaN 参数把坐标一路污染到打卡报文里。
	if !(radiusM > 0) {
		return lon, lat
	}
	u1 := clamp01(drawOr(draw))
	u2 := clamp01(drawOr(draw))
	r := radiusM * math.Sqrt(u1)
	theta := 2 * math.Pi * u2
	northM := r * math.Cos(theta)
	eastM := r * math.Sin(theta)
	mPerLat, mPerLon := metersPerDegree(lat)
	return lon + eastM/mPerLon, lat + northM/mPerLat
}

// metersPerDegree 该纬度下 1° 纬度 / 1° 经度对应的米数（WGS-84 椭球近似展开式）。
//
// 200 米量级下这点近似误差远小于高德坐标本身的取舍误差，只用来把"米"换成
// 经纬度增量。近极点处 cos(φ)→0 会让 mPerLon 趋零，用 1e-9 兜底避免除零放大。
func metersPerDegree(latitude float64) (mPerLat, mPerLon float64) {
	phi := latitude * math.Pi / 180
	mPerLat = 111132.92 - 559.82*math.Cos(2*phi) + 1.175*math.Cos(4*phi)
	mPerLon = 111412.84*math.Cos(phi) - 93.5*math.Cos(3*phi)
	if mPerLon < 1e-9 {
		mPerLon = 1e-9
	}
	return mPerLat, mPerLon
}

// parseCoordinate 配置里的经纬度文本 → float。
//
// 为什么不用裸 strconv.ParseFloat：坐标是从高德拾取器手抄进配置的，最常见错法是
// 留空或粘进多余字符。这里必须把"哪个键空了/写错了"讲明白，而不是抛一句
// invalid syntax 让人从头排。
func parseCoordinate(text, where string) (float64, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, coordinateError(where, text)
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, coordinateError(where, text)
	}
	return value, nil
}

// coordinateError 构造"改哪个键、改成什么样"的报错文案。
func coordinateError(where, shown string) error {
	display := "空（未配置）"
	if strings.TrimSpace(shown) != "" {
		display = fmt.Sprintf("%q", shown)
	}
	return fmt.Errorf("checkin 未配置有效坐标：%s 的值是%s。"+
		"请修改配置的 checkin 段（data/config.json，或导入用的 config.yaml），"+
		"填高德坐标拾取器（%s）复制来的数字，"+
		`如 longitude: "119.20463" / latitude: "26.04920"；`+
		"也可以改用 checkin.locations 位置池，在每一项里分别填写 longitude / latitude",
		where, display, amapPickerURL)
}

// weightOf 位置池权重：缺失/非法/<=0 一律按 1。
//
// 权重只是"偏好"而不是硬约束，一处笔误不该让某个点永远抽不到（若把 <=0 当真，
// total 可能变成 0 并除零）。
func weightOf(w *float64) float64 {
	if w == nil {
		return 1
	}
	v := *w
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 1
	}
	return v
}

// actualText 地址文案收口成去空白字符串；空/缺失用 fallback。
//
// actualLocation 原样提交给学校接口，带换行的多行值会让接口校验失败。
func actualText(value, fallback string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return fallback
	}
	return text
}

// drawOr 取一次随机数；draw 为 nil 时用 math/rand 默认源（并发安全）。
func drawOr(draw RandFunc) float64 {
	if draw == nil {
		return rand.Float64()
	}
	return draw()
}

// clamp01 把随机源夹到 [0,1)：注入的序列/浮点误差不该把点甩出圆外。
func clamp01(u float64) float64 {
	if math.IsNaN(u) || u < 0 {
		return 0
	}
	if u >= 1 {
		return math.Nextafter(1, 0) // 略小于 1，避免 sqrt/cos 落在闭区间端点
	}
	return u
}
