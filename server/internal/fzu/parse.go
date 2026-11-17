package fzu

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Plan 是 init 响应的规范化视图：flow 只认它，不再摸 map[string]any。
//
// 为什么要把 init 拆成结构体：init 的 data 有 3 个来源不同、字段风格也不一致的
// 子对象（userData / paramsData / schoolData），而且 schoolData[i].FPosition 还是
// "字符串里的 JSON"。让每个调用方自己去 map 里翻，等于把这个协议细节复制 N 份。
type Plan struct {
	Start, End, LateLine string // FStartTime / FEndTime / FLateTime（缺失则空）
	FState               string // 未签到 | SUCCESS | 已签到
	Checked              bool   // FState ∈ {SUCCESS, 已签到}
	HasPlan              bool   // schoolData 非空
	CampusDefault        string // userData.FDefaultCampus
	CollUnit             string // userData.FCollUnit
	Way                  int    // userData.FWay，缺省 1
	Polygons             []json.RawMessage

	// RangeJSON 是 check.action 的 range 参数原文：一个**完整的 FPosition 对象**
	// 串 {"range":[...],"mapType":"amap"}，不是摊平后的多边形数组串。
	//
	// 为什么在这里就定型：学校端会把 range 字段再反序列化成一个 FPosition，
	// 只发数组会直接回「打卡地点校验发生异常」（2026-11 线上对照实验：同一个
	// token、同一个坐标，只有 range 形状不同，数组回 success=false）。把原文挂在
	// Plan 上，flow 与 client 发的就是 init 里那一份，协议再变只改这一处。
	RangeJSON string
}

const (
	// stateSuccessRaw / stateCheckedCN 是"学校端已签到"的两种 FState 取值：
	// 接口在不同版本里分别回 SUCCESS 与中文"已签到"，两个都得认，否则第二层
	// 幂等（FState 检查）会失效、同一晚重复打卡。
	stateSuccessRaw = "SUCCESS"
	stateCheckedCN  = "已签到"

	// defaultMapType 是 range 对象里 mapType 的兜底值。H5 一直发 amap，而空串
	// 会被对端当成"未知坐标系"，所以缺失时必须补上而不是留空。
	defaultMapType = "amap"

	// emptyRangeJSON 是"一个多边形都没有"时的 range 参数：仍然是一个完整
	// FPosition 对象。绝不能退成空串——对端拿到空串会在反序列化阶段就抛
	// 「打卡地点校验发生异常」，连"不在签到范围内"这个结论都换不回来。
	emptyRangeJSON = `{"range":[],"mapType":"` + defaultMapType + `"}`
)

// fposition 是 FPosition 的 Go 影子类型。字段顺序有意与对端原文一致
// （range 在前、mapType 在后）；check.action 的 range 参数必须是**这个对象**的
// JSON 文本，而不是它内部那个 range 数组。
type fposition struct {
	Range   []json.RawMessage `json:"range"`
	MapType string            `json:"mapType"`
}

// ParseInit 把 init 响应的 data 归一成 Plan。
//
// 只有"结构上不可能继续"的情况才返回 error（nil 数据、userData/paramsData 类型
// 不对）；字段缺失一律降级成零值——因为历史上见过只回 userData 的部分响应，
// 那种情况下 HasPlan=false 比"报错"更能让上层走 no_plan 的静默路径。
func ParseInit(init map[string]any) (Plan, error) {
	if init == nil {
		return Plan{}, errors.New("init 数据为空（上游没有返回 data）")
	}
	var plan Plan

	user, err := objectField(init, "userData")
	if err != nil {
		return Plan{}, err
	}
	params, err := objectField(init, "paramsData")
	if err != nil {
		return Plan{}, err
	}

	plan.FState = strings.TrimSpace(stringField(user, "FState"))
	plan.Checked = plan.FState == stateSuccessRaw || plan.FState == stateCheckedCN

	plan.Start = strings.TrimSpace(stringField(params, "FStartTime"))
	plan.End = strings.TrimSpace(stringField(params, "FEndTime"))
	plan.LateLine = strings.TrimSpace(stringField(params, "FLateTime"))

	plan.CampusDefault = strings.TrimSpace(stringField(user, "FDefaultCampus"))
	plan.CollUnit = strings.TrimSpace(stringField(user, "FCollUnit"))
	plan.Way = intField(user, "FWay", 1)

	school := sliceField(init, "schoolData")
	plan.HasPlan = len(school) > 0
	plan.Polygons = collectPolygons(school)
	plan.RangeJSON = buildRangeJSON(plan.Polygons, firstMapType(school))
	return plan, nil
}

// buildRangeJSON 把摊平后的多边形与地图类型打包成 check.action 的 range 参数
// 原文：{"range":[...],"mapType":"amap"}。
//
// 为什么要专门一个构造器而不是各调用点现拼：这份文本会被对端**再解析一次**
// 当成 FPosition，形状错一个括号就整晚签不上；只发 range 数组是本模块踩过的坑
// （对端回「打卡地点校验发生异常」）。序列化走 compactJSON（关掉 HTML 转义），
// 与 Python/H5 发出的原文一致，中文校区名、& 之类的字符不会被写成 \uXXXX。
func buildRangeJSON(polygons []json.RawMessage, mapType string) string {
	if polygons == nil {
		// nil 切片会被写成 null，而对端要的是数组：空范围也得是 []。
		polygons = []json.RawMessage{}
	}
	if strings.TrimSpace(mapType) == "" {
		mapType = defaultMapType
	}
	data, err := compactJSON(fposition{Range: polygons, MapType: mapType})
	if err != nil {
		// 理论上到不了：range 里每一段都来自一次成功的 json.Unmarshal。
		// 真到了也只能退空范围——退空串会让对端连"没命中"都答不出来。
		return emptyRangeJSON
	}
	return string(data)
}

// firstMapType 取第一个能解析出来的 mapType。
//
// 为什么是"第一个能解析的"而不是要求全部一致：schoolData 可能有多条（多校区），
// 只要有一条带了 mapType 就足以描述坐标系；全部缺失/写坏再退 amap。挑第一条
// 而不是拼起来，是因为对端只认单个枚举值。
func firstMapType(school []any) string {
	for _, item := range school {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		position, ok := obj["FPosition"]
		if !ok || position == nil {
			continue
		}
		switch typed := position.(type) {
		case string:
			var parsed struct {
				MapType string `json:"mapType"`
			}
			if err := json.Unmarshal([]byte(typed), &parsed); err != nil {
				continue
			}
			if mt := strings.TrimSpace(parsed.MapType); mt != "" {
				return mt
			}
		case map[string]any:
			if mt := strings.TrimSpace(stringField(typed, "mapType")); mt != "" {
				return mt
			}
		}
	}
	return defaultMapType
}

// collectPolygons 从 schoolData 里摊平出所有签到范围多边形。
//
// FPosition 是 **JSON 字符串**（对端把一个完整对象再序列化了一次），必须二次
// 解析才能拿到 range 数组。解析失败的项直接跳过：一条坏数据不该让整晚签不了，
// 剩下的多边形只要还能覆盖用户的位置就够了。
func collectPolygons(school []any) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(school))
	for _, item := range school {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		position, ok := obj["FPosition"]
		if !ok || position == nil {
			continue
		}
		switch typed := position.(type) {
		case string:
			var parsed struct {
				Range []json.RawMessage `json:"range"`
			}
			if err := json.Unmarshal([]byte(typed), &parsed); err != nil {
				continue
			}
			out = append(out, parsed.Range...)
		case map[string]any:
			// 少数回包直接把对象塞进来（不再二次序列化）。这里同样接受，
			// 免得对端改个类型就整晚签不上。
			if rangeVal, ok := typed["range"]; ok {
				if items, ok := rangeVal.([]any); ok {
					for _, entry := range items {
						if raw, err := json.Marshal(entry); err == nil {
							out = append(out, raw)
						}
					}
				}
			}
		}
	}
	return out
}

// objectField 取一个子对象。缺失/null 视为空（调用方按零值继续跑），
// 但类型不对要报错：那通常意味着对端把 data 的形状改了，早报早排查。
func objectField(m map[string]any, key string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("init.%s 不是对象（实际类型 %T），上游结构可能已变化", key, v)
	}
	return obj, nil
}

// stringField 取字符串字段，容忍数字/布尔被写成非字符串的版本。
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// intField 取整数字段，缺失/非法用 def。
func intField(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return def
		}
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return def
		}
		return int(n)
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return def
		}
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(f) {
			return int(f)
		}
		return def
	default:
		return def
	}
}

// sliceField 取数组字段；缺失/类型不对都返回 nil。
func sliceField(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if items, ok := m[key].([]any); ok {
		return items
	}
	return nil
}
