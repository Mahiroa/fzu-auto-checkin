package model

import "strings"

// 上报地点文案（actualLocation）的规范形态与归一算法。
//
// 依据 docs/protocol.md §3：actualLocation 是「标题 + 空格 + 地址」，智汇福大 App
// 自己就是拿高德逆地理编码拼出来的。学校侧把这串原样存档、按人看，所以形态必须
// 跟 H5 里长出来的一模一样，三段、不缺不重：
//
//	旗山校区 福建省福州市闽侯县乌龙江北大道2号靠近福州大学旗山校区生活区X号楼
//	└校区标记┘ └── 省/市/县 + 道路门牌 ──┘└── 靠近 + POI 全称 ──┘
//
// 也就是「校区 + 空格 + 详细地址 + 靠近 + 最近 POI」。示例形态（取自学校 H5 与
// 历史记录）：`旗山校区 福建省福州市闽侯县学府南路辅路28号靠近福州大学旗山校区
// 生活区X号楼`——门牌级地址打头，POI 全称收尾，`靠近` 负责把两者接起来。
//
// 三类来源会写出"不规范"的形态，本函数逐一收口：
//
//  1. 地图选点：高德 regeo 的 formatted_address 已经写到楼栋时（"…生活区X号楼"
//     公寓"），再拼一句"靠近"同一栋楼，就成了"…学生公寓靠近…学生公寓"——同一处
//     说两遍，`靠近` 没有带来任何新信息，这时才合并。**注意这是例外，不是常态**：
//     常规形态里 `靠近` 是有用的（门牌 → 楼栋），不能被一律删掉。
//  2. 手抄 / 多行 YAML：换行、全角空格、制表符会把一条地址撑成几行。
//  3. 位置池：逐项手写，容易一项带校区标记、一项不带，一项有门牌、一项没有。
//
// 归一规则（按顺序执行，幂等）：
//
//	R1 空白折叠：任何空白（含全角空格、换行）→ 单个半角空格，去首尾；
//	R2 校区标记：以「旗山校区」开头则后接恰好一个空格；没有标记但以「福建省 /
//	   福州市 / 闽侯县」开头则补上标记；
//	R3 靠近重组：`靠近X` 里的 X 已出现在主地址里（同一处说了两遍）→ 丢掉整个
//	   「靠近X」；主地址结尾与 X 开头有 ≥4 字重叠 → 在重叠处拼接；都不成立 →
//	   **保留「靠近X」**（这就是规范形态的那一段）；
//	R4 相邻重复折叠：任何长度 ≥3 字的相邻完全重复（"XX"）只留一段。
//
// 为什么这里不补门牌号（如"乌龙江北大道2号"）：逆地理编码在校园内部常常只到楼栋，
// 门牌得靠人工补；脚本凭空编一个门牌，比留一条没门牌的地址更像假地址。要写全就
// 在配置里照示例手写，本函数不会再把它改回去（幂等）。
const (
	// ActualLocationCampus 是第一段：学校侧的校区名（后面接一个半角空格）。
	ActualLocationCampus = "旗山校区"

	// actualNearSep 是高德拼"靠近哪个 POI"时用的连接词，也是去重的判定点。
	actualNearSep = "靠近"

	// actualDupRunes 小于这个字数的相邻重复不折叠：两个字的重叠太容易误伤正常
	// 连读（"上街镇" 后面接 "镇上"），三字起才基本只在真重复时出现
	//（"闽侯县闽侯县"、"生活区X号楼生活区X号楼"）。
	actualDupRunes = 3

	// actualMergeRunes 两段首尾重叠短于这个字数就不当作同一段：1-3 字的重叠
	// 多半是巧合（"学园路" + "路28号"只重了 1 个字）。
	actualMergeRunes = 4
)

// NormalizeActualLocation 把任意来源的地址文案收敛成规范形态；空串仍是空串。
//
// 幂等：对已经规范的文案再调一次，结果逐字相同（配置保存、签到解析、前端回显三处
// 都会调用它，不幂等就会来回抖）。
//
// 顺序很重要：先把校区标记摘下来、再对正文做靠近合并。反过来的话，正文里"靠近"
// 之后的 POI 名往往包含"旗山校区"四个字，合并时会把整段标记吞掉，最后剩一条没有
// 校区标记的地址。
func NormalizeActualLocation(raw string) string {
	text := collapseLocationSpace(raw)
	if text == "" {
		return ""
	}
	body, tagged := splitCampusTag(text)
	// 标记已经占住第一段，正文开头再出现一次就是重复（"旗山校区 旗山校区生活区X号楼"）
	body = strings.TrimSpace(strings.TrimPrefix(body, ActualLocationCampus))
	body = mergeNearClauses(body)
	body = collapseAdjacentRepeat(body)
	body = strings.TrimSpace(body)
	if body == "" {
		if tagged {
			return ActualLocationCampus
		}
		return ""
	}
	if tagged || hasAdminPrefix(body) {
		return ActualLocationCampus + " " + body
	}
	return body
}

// collapseLocationSpace 做 R1：把零宽字符当空白、所有空白折叠成一个半角空格。
func collapseLocationSpace(raw string) string {
	cleaned := strings.NewReplacer(
		"\u200b", " ", "\ufeff", " ",
		"\u00a0", " ", "\u3000", " ",
	).Replace(raw)
	fields := strings.Fields(cleaned) // Fields 按 unicode.IsSpace 切，换行/制表一并处理
	return strings.Join(fields, " ")
}

// splitCampusTag 拆出 R2 的第一段：正文 + 是否已带校区标记。
func splitCampusTag(text string) (body string, tagged bool) {
	if rest, ok := strings.CutPrefix(text, ActualLocationCampus); ok {
		return strings.TrimSpace(rest), true
	}
	return text, false
}

// hasAdminPrefix 判断正文是否以行政区划开头：是的话补上校区标记，格式才完整。
//
// 只认"省 + 市 + 县"这一级的全称，不做前缀模糊匹配——"福州大学…"也以"福州"开头，
// 模糊匹配会把它误判成"福州市"再补一个校区标记，反而更难读。
func hasAdminPrefix(body string) bool {
	for _, head := range []string{"福建省", "福州市", "闽侯县"} {
		if strings.HasPrefix(body, head) {
			return true
		}
	}
	return false
}

// mergeNearClauses 做 R3：把「…靠近X」按重叠/包含关系并进主地址。
func mergeNearClauses(text string) string {
	parts := strings.Split(text, actualNearSep)
	if len(parts) < 2 {
		return text
	}
	acc := strings.TrimSpace(parts[0])
	for _, part := range parts[1:] {
		acc = mergeHeadTail(acc, strings.TrimSpace(part))
	}
	return acc
}

// mergeHeadTail 合并两段地址；返回结果里已经去掉了连接词（需要时才补回）。
func mergeHeadTail(head, tail string) string {
	switch {
	case tail == "":
		return head
	case head == "":
		return tail
	case strings.Contains(head, tail):
		// 主地址里已经有这栋楼：整段「靠近X」是纯重复，丢掉
		return head
	case strings.Contains(tail, head):
		// 反过来主地址被 POI 名包含（"福州大学旗山校区" + "福州大学旗山校区生活区X号楼"）：
		// 信息全在那一段里，直接用它，不出现两个"福州大学旗山校区"
		return tail
	}
	h, t := []rune(head), []rune(tail)
	limit := len(t)
	if len(h) < limit {
		limit = len(h)
	}
	for k := limit; k >= actualMergeRunes; k-- {
		if string(h[len(h)-k:]) == string(t[:k]) {
			return head + string(t[k:]) // 在重叠处拼接，不留"靠近"
		}
	}
	return head + actualNearSep + tail
}

// collapseAdjacentRepeat 做 R4：相邻两段逐字相同（"XX"）只留一段，直到不再变化。
//
// 一趟扫不全（"AAA" 折掉一段后前面还剩 "AA"），所以外层循环到稳定为止。
func collapseAdjacentRepeat(text string) string {
	runes := []rune(text)
	for {
		changed := false
		for i := 0; i+2*actualDupRunes <= len(runes); i++ {
			for k := (len(runes) - i) / 2; k >= actualDupRunes; k-- {
				if string(runes[i:i+k]) != string(runes[i+k:i+2*k]) {
					continue
				}
				runes = append(runes[:i+k], runes[i+2*k:]...)
				changed = true
				break
			}
			if changed {
				break
			}
		}
		if !changed {
			return string(runes)
		}
	}
}
