package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/fzu"
	"fzucheckin/internal/model"
)

// TestFlowTokenRejectedIsError 覆盖一次真实的线上故障：把别的系统的 Access-Token
// 当免登 token 填进来，晚点名回的是 HTTP 500 + 一个「信息提示」HTML 页。
//
// 这种失败绝不能归成 no_plan —— 界面上会显示「今日无计划」，用户以为今晚
// 不用签，然后漏签。必须报成 error 并给出可操作的方向。
func TestFlowTokenRejectedIsError(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	// 合成值：只需要保留「带 '-'、来自别的系统」这一形状特征（Normalize 靠它给出警告），
	// 绝不可填入真实抓包里的值——本文件在公开仓库里。
	cfg.User.Token = "SYNTHETIC-access-token-0001"
	env.saveConfig(cfg)
	env.school.setInitHTML(true)
	env.setNow(fixedDay(21, 40))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)

	if rec.Status != model.StatusError {
		t.Fatalf("HTML 错误页应判成 error（而不是 no_plan），实际 %s", describe(rec))
	}
	if !strings.Contains(rec.Error, "拒绝") {
		t.Fatalf("错误里应说明上游拒绝了 token: %q", rec.Error)
	}
	logs := strings.Join(rec.Logs, "\n")
	if !strings.Contains(logs, "浏览器 Cookie") {
		t.Fatalf("日志里应提示下一步（改用 Cookie 自动换新）: %s", logs)
	}
}

// TestFlowInitGatewayErrorStaysNoPlan：纯文本 500（网关抖动/维护）仍按 no_plan 处理，
// 不能被上面的 token 判定顺手改掉——那样每次上游抖一下就推一条"签到失败"。
func TestFlowInitGatewayErrorStaysNoPlan(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Username = ""
	cfg.User.Password = ""
	cfg.User.Token = "goodtoken0123456789"
	env.saveConfig(cfg)
	env.school.setInitFail(true)
	env.setNow(fixedDay(21, 40))

	id := env.acceptRun(env.authedClient(), map[string]any{"mode": "force"})
	rec := env.waitRun(id, 5*time.Second)
	if rec.Status != model.StatusNoPlan {
		t.Fatalf("纯文本 500 应保持 no_plan，实际 %s", describe(rec))
	}
}

// TestClientInitRejectsHTMLPage 是上一层的单元测试：Response 是 HTML 时必须
// 能被 errors.Is(err, fzu.ErrTokenRejected) 认出来。
func TestClientInitRejectsHTMLPage(t *testing.T) {
	env := newTestEnv(t)
	env.school.setInitHTML(true)
	c := env.srv.clientFactory("whatever-token")
	if _, err := c.Init(); err == nil {
		t.Fatal("HTML 错误页必须返回 error")
	} else if !errors.Is(err, fzu.ErrTokenRejected) {
		t.Fatalf("应能被 ErrTokenRejected 命中，实际: %v", err)
	}
}

// TestNormalizeWarnsOnForeignToken：填了别的系统的凭据时，保存配置要给出提示，
// 但绝不能硬拦（上游换编码方式时硬拦会让服务直接不可用）。
func TestNormalizeWarnsOnForeignToken(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.User.Token = "SYNTHETIC-access-token-0001"
	cfg.Checkin.Longitude = "119.20463"
	cfg.Checkin.Latitude = "26.04920"

	out, warnings := config.Normalize(cfg)
	if !strings.HasPrefix(out.User.Token, "SYNT") {
		t.Fatalf("token 不该被改写: %q", out.User.Token)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "非字母数字") {
		t.Fatalf("应给出 token 形状警告，实际: %v", warnings)
	}
	if err := config.Validate(out); err != nil {
		t.Fatalf("形状警告不能升级成校验失败: %v", err)
	}

	// 正常的晚点名 token 不该有这条警告。
	ok := config.DefaultConfig()
	ok.User.Token = "abc123XYZ789"
	_, warnings = config.Normalize(ok)
	if strings.Contains(strings.Join(warnings, "\n"), "非字母数字") {
		t.Fatalf("正常 token 不该被警告: %v", warnings)
	}
}
