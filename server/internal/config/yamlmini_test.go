package config

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func mustParseYAML(t *testing.T, src string) *yamlNode {
	t.Helper()
	root, err := parseYAML(src)
	if err != nil {
		t.Fatalf("解析失败: %v\n源码:\n%s", err, src)
	}
	if root == nil {
		t.Fatalf("解析结果为空（期望至少是空映射）:\n%s", src)
	}
	return root
}

func wantText(t *testing.T, n *yamlNode, want string) {
	t.Helper()
	if n == nil {
		t.Fatalf("节点不存在，期望 %q", want)
	}
	got, ok := n.asText()
	if !ok {
		t.Fatalf("不是标量（kind=%d），期望 %q", n.kind, want)
	}
	if got != want {
		t.Fatalf("值不对: got=%q want=%q", got, want)
	}
}

// wantParseError 断言报错且带正确行号：行号是本解析器最核心的可用性特征。
func wantParseError(t *testing.T, src string, wantLine int, wantSub string) {
	t.Helper()
	root, err := parseYAML(src)
	if err == nil {
		t.Fatalf("期望报错，实际解析成功: %q（root=%+v）", src, root)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("第 %d 行", wantLine)) {
		t.Fatalf("错误行号不对，期望第 %d 行: %v", wantLine, err)
	}
	if wantSub != "" && !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("错误信息里没有 %q: %v", wantSub, err)
	}
}

// 正常语法：每条用例对应真实配置里会出现的一种写法。
func TestParseYAMLNormal(t *testing.T) {
	t.Run("单层映射", func(t *testing.T) {
		root := mustParseYAML(t, "a: 1\nb: hello\n")
		wantText(t, root.get("a"), "1")
		wantText(t, root.get("b"), "hello")
	})

	t.Run("两层嵌套", func(t *testing.T) {
		root := mustParseYAML(t, "user:\n  username: \"2021001\"\n  password: \"pw\"\n")
		user := root.get("user")
		if user == nil || user.kind != yamlKindMap {
			t.Fatalf("user 不是映射: %+v", user)
		}
		wantText(t, user.get("username"), "2021001")
		wantText(t, user.get("password"), "pw")
	})

	t.Run("三层嵌套", func(t *testing.T) {
		src := "checkin:\n  schedule:\n    times:\n      - \"21:35\"\n"
		root := mustParseYAML(t, src)
		times := root.get("checkin").get("schedule").get("times")
		if times == nil || times.kind != yamlKindSeq || len(times.items) != 1 {
			t.Fatalf("times 不是单元素列表: %+v", times)
		}
		wantText(t, times.items[0], "21:35")
	})

	t.Run("四空格缩进也接受", func(t *testing.T) {
		root := mustParseYAML(t, "a:\n    b: 1\n")
		wantText(t, root.get("a").get("b"), "1")
	})

	t.Run("字符串列表", func(t *testing.T) {
		root := mustParseYAML(t, "list:\n  - a\n  - b\n")
		items := root.get("list").items
		if len(items) != 2 {
			t.Fatalf("列表长度不对: %d", len(items))
		}
		wantText(t, items[0], "a")
		wantText(t, items[1], "b")
	})

	t.Run("映射列表", func(t *testing.T) {
		src := "locations:\n" +
			"  - longitude: \"119.20463\"\n" +
			"    latitude: \"26.04920\"\n" +
			"    weight: 3\n" +
			"  - longitude: \"119.20510\"\n" +
			"    latitude: \"26.04880\"\n"
		root := mustParseYAML(t, src)
		locs := root.get("locations").items
		if len(locs) != 2 {
			t.Fatalf("项数不对: %d", len(locs))
		}
		wantText(t, locs[0].get("longitude"), "119.20463")
		wantText(t, locs[0].get("weight"), "3")
		// 第二项的键必须回到 indent+2 这一层，不能被漏掉或错挂
		wantText(t, locs[1].get("latitude"), "26.04880")
		if locs[1].get("weight") != nil {
			t.Fatalf("第二项不该有 weight")
		}
	})

	t.Run("列表项里再套列表", func(t *testing.T) {
		src := "a:\n  - b:\n      - 1\n      - 2\n"
		root := mustParseYAML(t, src)
		inner := root.get("a").items[0].get("b")
		if inner == nil || inner.kind != yamlKindSeq || len(inner.items) != 2 {
			t.Fatalf("内层列表不对: %+v", inner)
		}
		wantText(t, inner.items[1], "2")
	})

	t.Run("列表项里带行内列表", func(t *testing.T) {
		src := "a:\n  - t: [1, 2]\n"
		root := mustParseYAML(t, src)
		ts := root.get("a").items[0].get("t")
		if ts == nil || ts.kind != yamlKindSeq || len(ts.items) != 2 {
			t.Fatalf("行内列表不对: %+v", ts)
		}
		wantText(t, ts.items[0], "1")
	})

	t.Run("空行内列表", func(t *testing.T) {
		root := mustParseYAML(t, "locations: []\n")
		n := root.get("locations")
		if n == nil || n.kind != yamlKindSeq || len(n.items) != 0 {
			t.Fatalf("空列表不对: %+v", n)
		}
	})

	t.Run("行内引号列表", func(t *testing.T) {
		root := mustParseYAML(t, "times: [\"21:35\", \"21:50\"]\n")
		items := root.get("times").items
		if len(items) != 2 {
			t.Fatalf("长度不对: %d", len(items))
		}
		if items[0].skind != scalarString || items[0].text != "21:35" {
			t.Fatalf("引号内应保持字符串: %+v", items[0])
		}
	})

	t.Run("行内列表允许收尾逗号", func(t *testing.T) {
		root := mustParseYAML(t, "a: [1, 2, ]\n")
		if len(root.get("a").items) != 2 {
			t.Fatalf("收尾逗号被当成了空项")
		}
	})

	t.Run("双引号里含冒号与井号", func(t *testing.T) {
		root := mustParseYAML(t, "bark_url: \"https://x/y?z=1#frag\"\n")
		wantText(t, root.get("bark_url"), "https://x/y?z=1#frag")
	})

	t.Run("单引号里含井号", func(t *testing.T) {
		root := mustParseYAML(t, "a: 'x # not comment'\n")
		wantText(t, root.get("a"), "x # not comment")
	})

	t.Run("单引号里两个单引号是转义", func(t *testing.T) {
		root := mustParseYAML(t, "a: 'it''s ok'\n")
		wantText(t, root.get("a"), "it's ok")
	})

	t.Run("双引号转义", func(t *testing.T) {
		root := mustParseYAML(t, "a: \"line1\\nline2 \\\"q\\\" \\\\ \\u0041\"\n")
		got, _ := root.get("a").asText()
		want := "line1\nline2 \"q\" \\ A"
		if got != want {
			t.Fatalf("转义结果不对: got=%q want=%q", got, want)
		}
	})

	t.Run("行尾注释被切掉", func(t *testing.T) {
		root := mustParseYAML(t, "a: 1 # 这是注释\n")
		wantText(t, root.get("a"), "1")
	})

	t.Run("纯注释与空行忽略", func(t *testing.T) {
		root := mustParseYAML(t, "# 注释\n\n   \na: 1\n# 末尾注释\n")
		wantText(t, root.get("a"), "1")
	})

	t.Run("裸标量里的井号属于内容", func(t *testing.T) {
		root := mustParseYAML(t, "url: http://a/b#c\n")
		wantText(t, root.get("url"), "http://a/b#c")
	})

	t.Run("井号前有空格则视为注释", func(t *testing.T) {
		root := mustParseYAML(t, "url: http://a/b #c\n")
		wantText(t, root.get("url"), "http://a/b")
	})

	t.Run("空值", func(t *testing.T) {
		root := mustParseYAML(t, "user:\n  token:\n  username: \"x\"\n")
		if !root.get("user").get("token").isNull() {
			t.Fatalf("空值应为 null")
		}
	})

	t.Run("冒号后什么都不写并跟子块", func(t *testing.T) {
		root := mustParseYAML(t, "checkin:\n  longitude: \"1\"\n")
		wantText(t, root.get("checkin").get("longitude"), "1")
	})

	t.Run("波浪号与 null", func(t *testing.T) {
		root := mustParseYAML(t, "a: ~\nb: null\nc: Null\n")
		for _, k := range []string{"a", "b", "c"} {
			if !root.get(k).isNull() {
				t.Fatalf("%s 应为 null: %+v", k, root.get(k))
			}
		}
	})

	t.Run("布尔", func(t *testing.T) {
		root := mustParseYAML(t, "a: true\nb: false\nc: True\nd: no\ne: off\n")
		for _, k := range []string{"a", "c"} {
			v, ok := root.get(k).asBool()
			if !ok || !v {
				t.Fatalf("%s 应为 true", k)
			}
		}
		for _, k := range []string{"b", "d", "e"} {
			v, ok := root.get(k).asBool()
			if !ok || v {
				t.Fatalf("%s 应为 false", k)
			}
		}
	})

	t.Run("整数与负数", func(t *testing.T) {
		root := mustParseYAML(t, "a: 200\nb: -3\nc: +7\n")
		for k, want := range map[string]float64{"a": 200, "b": -3, "c": 7} {
			v, ok := root.get(k).asNumber()
			if !ok || v != want {
				t.Fatalf("%s 数字不对: %v %v", k, v, ok)
			}
		}
	})

	t.Run("浮点", func(t *testing.T) {
		root := mustParseYAML(t, "a: 119.20463\nb: 1e3\n")
		if v, _ := root.get("a").asNumber(); v != 119.20463 {
			t.Fatalf("a 不对: %v", v)
		}
		if v, _ := root.get("b").asNumber(); v != 1000 {
			t.Fatalf("b 不对: %v", v)
		}
	})

	t.Run("inf 与 nan", func(t *testing.T) {
		root := mustParseYAML(t, "a: .inf\nb: -.inf\nc: .nan\n")
		if v, _ := root.get("a").asNumber(); !math.IsInf(v, 1) {
			t.Fatalf("a 应为 +Inf: %v", v)
		}
		if v, _ := root.get("b").asNumber(); !math.IsInf(v, -1) {
			t.Fatalf("b 应为 -Inf: %v", v)
		}
		if v, _ := root.get("c").asNumber(); !math.IsNaN(v) {
			t.Fatalf("c 应为 NaN: %v", v)
		}
	})

	t.Run("未加引号的时刻是 60 进制整数", func(t *testing.T) {
		root := mustParseYAML(t, "times:\n  - 21:35\n")
		item := root.get("times").items[0]
		if item.skind != scalarInt || item.ival != 1295 || !item.sexa {
			t.Fatalf("21:35 应解析为 60 进制整数 1295: %+v", item)
		}
		if item.text != "1295" {
			t.Fatalf("文本形态应是十进制 1295，交给 Normalize 还原: %q", item.text)
		}
	})

	t.Run("不是合法 60 进制就保持字符串", func(t *testing.T) {
		root := mustParseYAML(t, "a: 21:99\n")
		if root.get("a").skind != scalarString {
			t.Fatalf("21:99 不该被当成数字: %+v", root.get("a"))
		}
		wantText(t, root.get("a"), "21:99")
	})

	t.Run("日期是字符串", func(t *testing.T) {
		root := mustParseYAML(t, "start: \"2027-01-23\"\n")
		wantText(t, root.get("start"), "2027-01-23")
	})

	t.Run("URL 值不被当 key", func(t *testing.T) {
		root := mustParseYAML(t, "bark_url: https://api.day.app/abc/标题\n")
		wantText(t, root.get("bark_url"), "https://api.day.app/abc/标题")
	})

	t.Run("引号 key", func(t *testing.T) {
		root := mustParseYAML(t, "\"a:b\": 1\n'a c': 2\n")
		wantText(t, root.get("a:b"), "1")
		wantText(t, root.get("a c"), "2")
	})

	t.Run("重复键后写的生效", func(t *testing.T) {
		root := mustParseYAML(t, "locations: []\nlocations:\n  - longitude: \"1\"\n    latitude: \"2\"\n")
		n := root.get("locations")
		if n.kind != yamlKindSeq || len(n.items) != 1 {
			t.Fatalf("重复键应以后者为准: %+v", n)
		}
		if len(root.keys) != 1 {
			t.Fatalf("重复键不该在 keys 里出现两次: %v", root.keys)
		}
	})

	t.Run("BOM 前缀", func(t *testing.T) {
		root := mustParseYAML(t, "\ufeffuser:\n  token: \"t\"\n")
		wantText(t, root.get("user").get("token"), "t")
	})

	t.Run("CRLF 换行", func(t *testing.T) {
		root := mustParseYAML(t, "user:\r\n  token: \"t\"\r\n")
		wantText(t, root.get("user").get("token"), "t")
	})

	t.Run("行尾空格", func(t *testing.T) {
		root := mustParseYAML(t, "a: 1   \nb: \"x\"   \n")
		wantText(t, root.get("a"), "1")
		wantText(t, root.get("b"), "x")
	})

	t.Run("深层混合嵌套", func(t *testing.T) {
		src := "a:\n" +
			"  b:\n" +
			"    c:\n" +
			"      - d: 1\n" +
			"        e: 2\n" +
			"      - d: 3\n"
		root := mustParseYAML(t, src)
		items := root.get("a").get("b").get("c").items
		if len(items) != 2 {
			t.Fatalf("项数不对: %d", len(items))
		}
		wantText(t, items[0].get("e"), "2")
		wantText(t, items[1].get("d"), "3")
	})

	t.Run("列表项里的映射带子块", func(t *testing.T) {
		src := "a:\n  - b:\n      c: 1\n    d: 2\n"
		root := mustParseYAML(t, src)
		item := root.get("a").items[0]
		wantText(t, item.get("b").get("c"), "1")
		wantText(t, item.get("d"), "2")
	})

	t.Run("列表项里的映射再带列表", func(t *testing.T) {
		src := "a:\n  - b: 1\n    c:\n      - 2\n      - 3\n"
		root := mustParseYAML(t, src)
		item := root.get("a").items[0]
		wantText(t, item.get("b"), "1")
		if got := item.get("c"); got == nil || len(got.items) != 2 {
			t.Fatalf("内层列表不对: %+v", got)
		}
	})

	t.Run("空文档返回 nil", func(t *testing.T) {
		for _, src := range []string{"", "   \n\n", "# 只有注释\n"} {
			root, err := parseYAML(src)
			if err != nil {
				t.Fatalf("%q 不该报错: %v", src, err)
			}
			if root != nil {
				t.Fatalf("%q 应返回 nil，实际 %+v", src, root)
			}
		}
	})

	t.Run("冒号在纯标量里", func(t *testing.T) {
		root := mustParseYAML(t, "a: b: c\n")
		wantText(t, root.get("a"), "b: c")
	})
}

// 不支持语法：必须报错且指出行号，绝不静默猜测。
func TestParseYAMLUnsupported(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		sub  string
	}{
		{"tab 缩进", "user:\n\ttoken: \"x\"\n", 2, "tab"},
		{"根部 tab 缩进", "\ta: 1\n", 1, "tab"},
		{"块标量竖线", "a: 1\nb: |\n  hello\n", 2, "块标量"},
		{"块标量尖括号", "a: >-\n  hello\n", 1, "块标量"},
		{"锚点", "a: &anchor 1\n", 1, "锚点"},
		{"别名", "a: *ref\n", 1, "锚点"},
		{"多文档分隔符", "a: 1\n---\nb: 2\n", 2, "多文档"},
		{"文档结束符", "a: 1\n...\n", 2, "文档结束符"},
		{"YAML 指令", "%YAML 1.2\na: 1\n", 1, "指令"},
		{"行内映射", "a: {b: 1}\n", 1, "行内映射"},
		{"行内列表未闭合", "a: [1, 2\n", 1, "]"},
		{"行内列表嵌套", "a: [[1]]\n", 1, "嵌套"},
		{"行内列表空项", "a: [1, , 2]\n", 1, "空项"},
		{"双引号未闭合", "a: \"abc\n", 1, "双引号"},
		{"单引号未闭合", "a: 'abc\n", 1, "单引号"},
		{"引号后有多余内容", "a: \"x\" y\n", 1, "多余"},
		{"缺冒号", "a b\n", 1, "key: value"},
		{"冒号后缺空格", "a:[1]\n", 1, "key: value"},
		{"值后多余缩进", "a: 1\n  b: 2\n", 2, "缩进"},
		{"同层键缩进不齐", "a:\n    b: 1\n  c: 2\n", 3, "缩进"},
		{"映射里出列表项", "a: 1\n- b\n", 2, "列表项"},
		{"列表项缩进过深", "a:\n  - 1\n    - 2\n", 3, "缩进"},
		{"列表项下多余缩进", "a:\n  - 1\n   x: 2\n", 3, "缩进"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantParseError(t, c.src, c.line, c.sub)
		})
	}
}

// stripYAMLComment 的边界单独测：它决定"值里到底保留了哪些字符"。
func TestStripYAMLComment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a: 1 # x", "a: 1 "},
		{"a: 1#x", "a: 1#x"},
		{"#whole", ""},
		{"a: \"#\"", "a: \"#\""},
		{"a: '# here' ", "a: '# here' "},
		{"a: \"x\\\"#y\"", "a: \"x\\\"#y\""},
		{"a: 'it''s # x'", "a: 'it''s # x'"},
	}
	for _, c := range cases {
		if got := stripYAMLComment(c.in); got != c.want {
			t.Fatalf("stripYAMLComment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// splitFlowItems 必须忽略引号内的逗号，否则 "a,b" 这种值会被切开。
func TestSplitFlowItems(t *testing.T) {
	got := splitFlowItems(`1, "x,y", 'q,q'`)
	if len(got) != 3 {
		t.Fatalf("切分数量不对: %q", got)
	}
	if strings.TrimSpace(got[1]) != `"x,y"` || strings.TrimSpace(got[2]) != `'q,q'` {
		t.Fatalf("切分结果不对: %q", got)
	}
}
