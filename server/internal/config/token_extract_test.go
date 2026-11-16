package config

import (
	"strings"
	"testing"
)

// TestNormalizeExtractsTokenFromURL：用户最常做的动作是把整条页面地址粘进 token 框。
// 直接透传会让上游回 500 错误页（地址不是 token），这里必须能自己抽出 token。
func TestNormalizeExtractsTokenFromURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		hit  bool // 是否发生了抽取
	}{
		{
			name: "整条 H5 地址",
			raw:  "https://yzsxg.fzu.edu.cn/plug-in/livecloud/project/fzu/attn/index.action?token=AbC123xyz789&contextPath=",
			want: "AbC123xyz789",
			hit:  true,
		},
		{
			name: "只有半截查询串",
			raw:  "?token=AbC123xyz789",
			want: "AbC123xyz789",
			hit:  true,
		},
		{
			name: "token= 开头",
			raw:  "token=AbC123xyz789",
			want: "AbC123xyz789",
			hit:  true,
		},
		{
			name: "本来就是 token",
			raw:  "AbC123xyz789",
			want: "AbC123xyz789",
			hit:  false,
		},
		{
			name: "地址里没有 token 参数",
			raw:  "https://yzsxg.fzu.edu.cn/plug-in/livecloud/index.action",
			want: "https://yzsxg.fzu.edu.cn/plug-in/livecloud/index.action",
			hit:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.User.Token = tc.raw
			out, warnings := Normalize(cfg)
			if out.User.Token != tc.want {
				t.Fatalf("token = %q，期望 %q", out.User.Token, tc.want)
			}
			gotHit := strings.Contains(strings.Join(warnings, "\n"), "自动抽出")
			if gotHit != tc.hit {
				t.Fatalf("是否抽取 = %t，期望 %t（warnings=%v）", gotHit, tc.hit, warnings)
			}
			// 抽出来的 token 是纯字母数字，不该再触发形状警告。
			if tc.hit && strings.Contains(strings.Join(warnings, "\n"), "非字母数字") {
				t.Fatalf("抽取成功后不该再报形状警告: %v", warnings)
			}
		})
	}
}
