// 本文件实现一个只服务于「导入旧 config.yaml」的 YAML 子集解析器。
//
// 为什么自己写而不是引 yaml.v3：本项目硬要求零第三方依赖（离线可构建、二进制小、
// 供应链面为零），而旧配置用到的语法只有「映射 + 列表 + 引号 + 注释」这几样。
// 关键取舍是：**不支持的语法一律带行号报错，绝不猜**。猜错的代价是用户以为配置
// 生效了、当晚却没签到；报错至少能当场改。反过来，能修的（未加引号的 21:35）
// 也不在这里修 —— 那是 Normalize 的职责，解析器只忠实还原"文件里写了什么"。

package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// yamlKind 是节点形态；标量内部的类型细分见 scalarKind。
type yamlKind uint8

const (
	yamlKindNull yamlKind = iota
	yamlKindScalar
	yamlKindMap
	yamlKindSeq
)

// scalarKind 记录标量被识别成了什么，供上层决定"能不能当数字/布尔用"。
type scalarKind uint8

const (
	scalarString scalarKind = iota
	scalarBool
	scalarInt
	scalarFloat
)

// yamlNode 是一棵最简语法树。映射保留 keys 顺序：导出/调试时可复现文件书写顺序，
// 而 map 的随机遍历顺序会让"同一份配置两次报错顺序不同"。
type yamlNode struct {
	kind yamlKind
	line int

	skind scalarKind
	text  string // 字符串值，或数字/布尔的规范文本（供上层统一取值）
	bval  bool
	ival  int64
	fval  float64
	sexa  bool // 该整数是 YAML 1.1 的 60 进制写法（21:35 → 1295）

	keys  []string
	m     map[string]*yamlNode
	items []*yamlNode
}

func nullNode(line int) *yamlNode { return &yamlNode{kind: yamlKindNull, line: line} }

func stringNode(line int, s string) *yamlNode {
	return &yamlNode{kind: yamlKindScalar, skind: scalarString, text: s, line: line}
}

func boolNode(line int, v bool) *yamlNode {
	n := &yamlNode{kind: yamlKindScalar, skind: scalarBool, bval: v, line: line}
	if v {
		n.text = "true"
	} else {
		n.text = "false"
	}
	return n
}

func intNode(line int, v int64, sexa bool) *yamlNode {
	return &yamlNode{
		kind: yamlKindScalar, skind: scalarInt, ival: v, sexa: sexa,
		text: strconv.FormatInt(v, 10), line: line,
	}
}

func floatNode(line int, v float64) *yamlNode {
	return &yamlNode{
		kind: yamlKindScalar, skind: scalarFloat, fval: v,
		text: formatFloat(v), line: line,
	}
}

// ---------- 取值辅助 ----------

func (n *yamlNode) get(key string) *yamlNode {
	if n == nil || n.kind != yamlKindMap {
		return nil
	}
	return n.m[key]
}

func (n *yamlNode) set(key string, child *yamlNode) {
	if n.m == nil {
		n.m = make(map[string]*yamlNode, 8)
	}
	if _, dup := n.m[key]; !dup {
		n.keys = append(n.keys, key)
	}
	// 重复键：后写的生效（与 PyYAML 的 safe_load 一致，旧配置模板里也这样提示用户）
	n.m[key] = child
}

func (n *yamlNode) isNull() bool { return n == nil || n.kind == yamlKindNull }

// asText 把标量取成文本。数字取十进制文本是关键一步：`times: [21:35]` 的 1295
// 必须原样变成 "1295" 交给 Normalize 还原，而不是在这里就地变成 "21:35"，
// 否则"警告里指出用户写错了"这条体验就丢了。
func (n *yamlNode) asText() (string, bool) {
	if n == nil || n.kind != yamlKindScalar {
		return "", false
	}
	return n.text, true
}

func (n *yamlNode) asNumber() (float64, bool) {
	if n == nil || n.kind != yamlKindScalar {
		return 0, false
	}
	switch n.skind {
	case scalarInt:
		return float64(n.ival), true
	case scalarFloat:
		return n.fval, true
	case scalarString:
		v, err := strconv.ParseFloat(strings.TrimSpace(n.text), 64)
		if err != nil {
			return 0, false
		}
		return v, true
	default:
		// 布尔不当数字用：true/false 不该被读成 1/0
		return 0, false
	}
}

func (n *yamlNode) asBool() (bool, bool) {
	if n == nil || n.kind != yamlKindScalar {
		return false, false
	}
	if n.skind == scalarBool {
		return n.bval, true
	}
	if n.skind == scalarString {
		switch strings.ToLower(strings.TrimSpace(n.text)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

// ---------- 错误 ----------

// yamlParseError 一定带行号：YAML 的自查成本几乎全在"第几行"上。
type yamlParseError struct {
	line int
	msg  string
}

func (e *yamlParseError) Error() string {
	if e.line <= 0 {
		return "YAML " + e.msg
	}
	return fmt.Sprintf("第 %d 行：%s", e.line, e.msg)
}

func yamlErr(line int, format string, args ...any) error {
	return &yamlParseError{line: line, msg: fmt.Sprintf(format, args...)}
}

// ---------- 入口 ----------

// parseYAML 解析子集语法并返回语法树；文档为空返回 (nil, nil)。
func parseYAML(src string) (*yamlNode, error) {
	lines, err := splitYAMLLines(src)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	p := &yamlParser{lines: lines}
	root, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if _, ok := p.peek(); ok {
		return nil, yamlErr(p.lines[p.pos].num, "缩进对不上，无法归入任何层级（检查这一行的空格数）")
	}
	return root, nil
}

// ---------- 预处理：去 BOM、统一换行、切注释、拦不支持的语法 ----------

type yamlLine struct {
	indent int
	text   string
	num    int
}

func splitYAMLLines(src string) ([]yamlLine, error) {
	// Windows 记事本保存的配置带 BOM 和 CRLF，直接喂给缩进解析器会多出一个"隐形字符"。
	src = strings.TrimPrefix(src, "\ufeff")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")

	raw := strings.Split(src, "\n")
	out := make([]yamlLine, 0, len(raw))
	for i, line := range raw {
		num := i + 1
		indent := 0
		for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
			if line[indent] == '\t' {
				return nil, yamlErr(num, "缩进里出现了 tab；YAML 只允许空格缩进（本解析器按 2 空格缩进）")
			}
			indent++
		}
		body := strings.TrimRight(stripYAMLComment(line[indent:]), " \t")
		if body == "" {
			continue
		}
		switch {
		case body == "---" || strings.HasPrefix(body, "--- "):
			return nil, yamlErr(num, "不支持多文档分隔符（---）；请只保留一份配置内容")
		case body == "...":
			return nil, yamlErr(num, "不支持文档结束符（...）；请删掉这一行")
		case strings.HasPrefix(body, "%"):
			return nil, yamlErr(num, "不支持 YAML 指令行（%%YAML / %%TAG）；请删掉这一行")
		}
		out = append(out, yamlLine{indent: indent, text: body, num: num})
	}
	return out, nil
}

// stripYAMLComment 切掉行尾注释，但引号内的 # 属于内容，不能切。
// 例外：裸标量里前面没有空格的 # 也属于内容（如 URL 的锚点），这跟 YAML 规范一致。
func stripYAMLComment(s string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		default:
			switch c {
			case '"':
				inDouble = true
			case '\'':
				inSingle = true
			case '#':
				if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
					return s[:i]
				}
			}
		}
	}
	return s
}

// ---------- 结构解析 ----------

type yamlParser struct {
	lines []yamlLine
	pos   int
}

func (p *yamlParser) peek() (yamlLine, bool) {
	if p.pos >= len(p.lines) {
		return yamlLine{}, false
	}
	return p.lines[p.pos], true
}

// parseBlock 解析从当前位置开始、缩进不小于 minIndent 的一块内容。
// indent 取实际缩进值而不是写死 2，是为了容忍 4 空格缩进；但同一层必须左对齐，
// 对不齐会明确报错 —— 那几乎总是"少贴了两个空格"。
func (p *yamlParser) parseBlock(minIndent int) (*yamlNode, error) {
	cur, ok := p.peek()
	if !ok || cur.indent < minIndent {
		return nullNode(0), nil
	}
	if isSeqItemLine(cur.text) {
		return p.parseSeq(cur.indent)
	}
	return p.parseMap(cur.indent)
}

func (p *yamlParser) parseMap(indent int) (*yamlNode, error) {
	first := p.lines[p.pos]
	node := &yamlNode{kind: yamlKindMap, line: first.num}
	for {
		cur, ok := p.peek()
		if !ok || cur.indent < indent {
			break
		}
		if cur.indent > indent {
			return nil, yamlErr(cur.num, "缩进比同层的其他键更深，无法判断它属于哪个键（同层键必须左对齐）")
		}
		if isSeqItemLine(cur.text) {
			return nil, yamlErr(cur.num, "这里是映射（key: value），却出现了列表项（- ）；检查缩进是否少了两个空格")
		}
		if cur.text[0] == '&' || cur.text[0] == '*' {
			return nil, yamlErr(cur.num, "不支持 YAML 锚点/别名（& 或 *）；请把内容直接写出来")
		}
		key, rest, ok := trySplitMappingKey(cur.text)
		if !ok {
			return nil, yamlErr(cur.num, "这一行不是 \"key: value\" 的形式，无法解析（冒号后面要有一个空格）")
		}
		p.pos++
		child, err := p.parseValueAfterKey(rest, indent, cur.num)
		if err != nil {
			return nil, err
		}
		node.set(key, child)
	}
	return node, nil
}

func (p *yamlParser) parseValueAfterKey(rest string, indent, line int) (*yamlNode, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		// 值在下面若干行：缩进至少比本键深一格
		return p.parseBlock(indent + 1)
	}
	child, err := parseInlineValue(rest, line)
	if err != nil {
		return nil, err
	}
	if next, ok := p.peek(); ok && next.indent > indent {
		return nil, yamlErr(next.num, "值已经写在键的同一行，这一行却又缩进更深；检查是否少写了一个 key 或多缩了空格")
	}
	return child, nil
}

func (p *yamlParser) parseSeq(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlKindSeq, line: p.lines[p.pos].num}
	for {
		cur, ok := p.peek()
		if !ok || cur.indent < indent {
			break
		}
		if cur.indent > indent {
			return nil, yamlErr(cur.num, "列表项的缩进比同层其他项更深（同层 - 必须左对齐）")
		}
		if !isSeqItemLine(cur.text) {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(cur.text, "-"))
		if rest == "" {
			p.pos++
			child, err := p.parseBlock(indent + 1)
			if err != nil {
				return nil, err
			}
			node.items = append(node.items, child)
			continue
		}
		if _, _, isMap := trySplitMappingKey(rest); isMap {
			// "- key: value"：把本行剩余内容当成缩进 indent+2 的一行再走 parseMap，
			// 这样列表项里的映射与普通映射共用同一套逻辑（含后续同缩进的键）。
			p.lines[p.pos] = yamlLine{indent: indent + 2, text: rest, num: cur.num}
			child, err := p.parseMap(indent + 2)
			if err != nil {
				return nil, err
			}
			node.items = append(node.items, child)
			continue
		}
		p.pos++
		child, err := parseInlineValue(rest, cur.num)
		if err != nil {
			return nil, err
		}
		if next, ok := p.peek(); ok && next.indent > indent {
			return nil, yamlErr(next.num, "缩进无法归入上一条列表项；若这一项下面还有内容，请缩进到 - 的右侧两格以上")
		}
		node.items = append(node.items, child)
	}
	return node, nil
}

func isSeqItemLine(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// trySplitMappingKey 按 YAML 的规则切 key：冒号后必须是空格或行尾，
// 因此 `https://x`、`21:35` 这类内容不会被误认成 key。
func trySplitMappingKey(s string) (key, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' || s[0] == '\'' {
		val, n, err := scanQuoted(s)
		if err != nil {
			return "", "", false
		}
		tail := strings.TrimLeft(s[n:], " ")
		if !strings.HasPrefix(tail, ":") {
			return "", "", false
		}
		after := tail[1:]
		if after == "" || strings.HasPrefix(after, " ") {
			return val, strings.TrimLeft(after, " "), true
		}
		return "", "", false
	}
	if s[0] == '[' || s[0] == '{' || s[0] == '&' || s[0] == '*' {
		return "", "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		if i+1 == len(s) || s[i+1] == ' ' {
			k := strings.TrimRight(s[:i], " \t")
			if k == "" {
				return "", "", false
			}
			return k, strings.TrimLeft(s[i+1:], " "), true
		}
	}
	return "", "", false
}

// ---------- 值解析 ----------

func parseInlineValue(text string, line int) (*yamlNode, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nullNode(line), nil
	}
	switch text[0] {
	case '&', '*':
		return nil, yamlErr(line, "不支持 YAML 锚点/别名（& 或 *）；请把内容直接写出来")
	case '{':
		return nil, yamlErr(line, "不支持行内映射 {…}；请改成分行缩进的写法")
	case '[':
		return parseFlowSeq(text, line)
	case '|', '>':
		if isBlockScalarHeader(text) {
			return nil, yamlErr(line, "不支持块标量（| 或 >）；请把多行文本写在一行里或加引号")
		}
	}
	return parseScalar(text, line)
}

// isBlockScalarHeader 判断 "|"、">"、">-"、">+" 这类块标量头（后面只允许正负号与数字）。
func isBlockScalarHeader(text string) bool {
	if text == "" || (text[0] != '|' && text[0] != '>') {
		return false
	}
	for i := 1; i < len(text); i++ {
		switch text[i] {
		case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			return false
		}
	}
	return true
}

// parseFlowSeq 处理行内列表：[]、["a","b"]、[21:35]。
func parseFlowSeq(text string, line int) (*yamlNode, error) {
	inner := strings.TrimSpace(text[1:])
	if !strings.HasSuffix(inner, "]") {
		return nil, yamlErr(line, "行内列表缺少收尾的 ]；例如 times: [\"21:35\", \"21:50\"]")
	}
	inner = strings.TrimSpace(inner[:len(inner)-1])
	node := &yamlNode{kind: yamlKindSeq, line: line}
	if inner == "" {
		return node, nil
	}
	parts := splitFlowItems(inner)
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			// 允许一个收尾逗号（YAML 允许），中间出现空项则是笔误
			if i == len(parts)-1 {
				continue
			}
			return nil, yamlErr(line, "行内列表里有空项（多写了逗号？）")
		}
		if part[0] == '[' || part[0] == '{' {
			return nil, yamlErr(line, "行内列表不支持嵌套；请改成缩进的 - 列表写法")
		}
		item, err := parseInlineValue(part, line)
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, item)
	}
	return node, nil
}

// splitFlowItems 按逗号切分行内列表，引号内的逗号不算分隔符。
func splitFlowItems(s string) []string {
	var out []string
	start := 0
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		default:
			switch c {
			case '"':
				inDouble = true
			case '\'':
				inSingle = true
			case ',':
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func parseScalar(text string, line int) (*yamlNode, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nullNode(line), nil
	}
	switch text[0] {
	case '\'', '"':
		val, n, err := scanQuoted(text)
		if err != nil {
			return nil, yamlErr(line, "%s", err.Error())
		}
		if strings.TrimSpace(text[n:]) != "" {
			return nil, yamlErr(line, "引号字符串后面还有多余内容（引号没有包住整段值？）")
		}
		return stringNode(line, val), nil
	}

	switch strings.ToLower(text) {
	case "~", "null":
		return nullNode(line), nil
	case "true", "yes", "on":
		return boolNode(line, true), nil
	case "false", "no", "off":
		return boolNode(line, false), nil
	}

	if v, ok := parseSexagesimalInt(text); ok {
		// 21:35 在 YAML 1.1 里就是 60 进制整数 1295；这里忠实还原成 1295 并打标记，
		// 由 Normalize 负责"还原成时刻 + 警告"，好让提示里能同时出现原文和修正值。
		return intNode(line, v, true), nil
	}
	if v, ok := parseIntLiteral(text); ok {
		return intNode(line, v, false), nil
	}
	if v, ok := parseFloatLiteral(text); ok {
		return floatNode(line, v), nil
	}
	return stringNode(line, text), nil
}

func parseSexagesimalInt(s string) (int64, bool) {
	body := s
	neg := false
	if body != "" && (body[0] == '+' || body[0] == '-') {
		neg = body[0] == '-'
		body = body[1:]
	}
	parts := strings.Split(body, ":")
	if len(parts) < 2 {
		return 0, false
	}
	for i, p := range parts {
		if p == "" {
			return 0, false
		}
		if i > 0 && len(p) > 2 {
			return 0, false
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return 0, false
			}
		}
		if i > 0 {
			v, err := strconv.Atoi(p)
			if err != nil || v > 59 {
				return 0, false
			}
		}
	}
	var total int64
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}
	if neg {
		total = -total
	}
	return total, true
}

func parseIntLiteral(s string) (int64, bool) {
	body := s
	if body != "" && (body[0] == '+' || body[0] == '-') {
		body = body[1:]
	}
	if body == "" {
		return 0, false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseFloatLiteral(s string) (float64, bool) {
	switch strings.ToLower(s) {
	case ".nan":
		return math.NaN(), true
	case ".inf", "+.inf":
		return math.Inf(1), true
	case "-.inf":
		return math.Inf(-1), true
	}
	low := strings.ToLower(s)
	if !strings.ContainsAny(low, ".e") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// scanQuoted 解析单引号或双引号字符串，返回值和消耗的字节数。
func scanQuoted(s string) (string, int, error) {
	if s == "" {
		return "", 0, fmt.Errorf("引号字符串为空")
	}
	if s[0] == '\'' {
		var sb strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					sb.WriteByte('\'')
					i++
					continue
				}
				return sb.String(), i + 1, nil
			}
			sb.WriteByte(s[i])
		}
		return "", 0, fmt.Errorf("单引号字符串缺少收尾的单引号")
	}
	var sb strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			if i+1 >= len(s) {
				return "", 0, fmt.Errorf("双引号字符串以反斜杠结尾")
			}
			i++
			switch s[i] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case '0':
				sb.WriteByte(0)
			case 'u':
				if i+4 >= len(s) {
					return "", 0, fmt.Errorf("\\u 转义不完整（需要四位十六进制）")
				}
				v, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
				if err != nil {
					return "", 0, fmt.Errorf("\\u 转义不是四位十六进制")
				}
				sb.WriteRune(rune(v))
				i += 4
			default:
				sb.WriteByte(s[i])
			}
			continue
		}
		if c == '"' {
			return sb.String(), i + 1, nil
		}
		sb.WriteByte(c)
	}
	return "", 0, fmt.Errorf("双引号字符串缺少收尾的双引号")
}
