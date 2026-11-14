package store

import (
	"testing"

	"fzucheckin/internal/model"
)

// UpdateConfig 的存在理由就是防止 lost update：flow 拿着"运行开始时"的配置快照，
// SSO 换到新 token 后整份写回，会把这期间用户改的密码/坐标覆盖掉。
// 这条用例锁死语义：只改调用方声明要改的字段，其余以最新落盘值为准。
func TestUpdateConfigKeepsConcurrentChanges(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	base := st.Config()
	base.User.Username = "20230001"
	base.User.Password = "old-password"
	base.Checkin.Schedule.Times = []string{"21:35"}
	if _, err := st.SaveConfig(base, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// 模拟"运行期间用户改了配置"：密码与档位都换了。
	edited := st.Config()
	edited.User.Password = "new-password"
	edited.Checkin.Schedule.Times = []string{"22:10"}
	v, err := st.SaveConfig(edited, nil)
	if err != nil {
		t.Fatalf("SaveConfig(edit): %v", err)
	}

	// 再模拟 flow 只回写 token：不能把上面的改动抹掉。
	v2, err := st.UpdateConfig(func(cfg *model.Config) []string {
		cfg.User.Token = "sso-token-abcdefghijklmnopqrstuvwxyz"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if v2 <= v {
		t.Fatalf("版本号应自增：%d -> %d", v, v2)
	}

	got := st.Config()
	if got.User.Password != "new-password" {
		t.Fatalf("用户刚改的密码被回写覆盖了：%q", got.User.Password)
	}
	if len(got.Checkin.Schedule.Times) != 1 || got.Checkin.Schedule.Times[0] != "22:10" {
		t.Fatalf("用户刚改的档位被回写覆盖了：%v", got.Checkin.Schedule.Times)
	}
	if got.User.Token != "sso-token-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("token 没写进去：%q", got.User.Token)
	}
	if got.Version != int(v2) {
		t.Fatalf("落盘版本号应为 %d，实际 %d", v2, got.Version)
	}

	// 重开一次，确认落盘的是合并后的结果而不是内存幻觉。
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	if st2.Config().User.Token == "" || st2.Config().User.Password != "new-password" {
		t.Fatalf("重开后的配置不对：token=%q password=%q",
			st2.Config().User.Token, st2.Config().User.Password)
	}
}

// nil 回调不应改变任何东西（避免调用方传空时白涨一个版本号）。
func TestUpdateConfigNilIsNoop(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := st.ConfigVersion()
	after, err := st.UpdateConfig(nil)
	if err != nil {
		t.Fatalf("UpdateConfig(nil): %v", err)
	}
	if after != before || st.ConfigVersion() != before {
		t.Fatalf("nil 回调不该改版本号：%d -> %d", before, after)
	}
}
