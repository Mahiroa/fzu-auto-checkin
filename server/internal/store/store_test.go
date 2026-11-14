package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写测试文件失败 %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败 %s: %v", path, err)
	}
	return string(raw)
}

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	return s, dir
}

func glob(t *testing.T, dir, pattern string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("Glob 失败: %v", err)
	}
	return got
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	raw := readFile(t, path)
	n := 0
	for _, ln := range strings.Split(raw, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// 首次启动必须落下可用的默认配置：前端第一次打开就能进初始化向导。
func TestOpenCreatesDefaults(t *testing.T) {
	s, dir := openTemp(t)
	if !filepath.IsAbs(s.Dir) {
		t.Fatalf("Dir 应是绝对路径: %q", s.Dir)
	}
	if s.ConfigVersion() != 1 {
		t.Fatalf("默认配置版本应为 1，实际 %d", s.ConfigVersion())
	}
	if got := s.Config(); got.Version != 1 {
		t.Fatalf("配置里的 version 应为 1: %d", got.Version)
	}
	if got := s.Config(); got.Checkin.Schedule.WindowStart != config.DefaultWindowStart {
		t.Fatalf("默认窗口不对: %+v", got.Checkin.Schedule)
	}
	if len(glob(t, dir, configFileName)) != 1 {
		t.Fatalf("应写出 %s", configFileName)
	}
	if len(glob(t, dir, configBakName)) != 0 {
		t.Fatalf("首次写配置不该产生 .bak（没有旧文件可备份）")
	}
	if len(glob(t, dir, secretFileName)) != 1 {
		t.Fatalf("应生成 %s", secretFileName)
	}
	if len(glob(t, dir, "*.tmp")) != 0 {
		t.Fatalf("原子写不应留下 .tmp")
	}
	// 配置文件必须是合法 JSON 且带 version
	var cfg model.Config
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, configFileName))), &cfg); err != nil {
		t.Fatalf("落盘配置不是合法 JSON: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("落盘 version 不对: %d", cfg.Version)
	}
}

func TestOpenLoadsExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Version = 7
	cfg.Server.Listen = "0.0.0.0:9000"
	cfg.Checkin.Schedule.Times = []string{"22:00"}
	raw, _ := json.Marshal(cfg)
	writeFile(t, filepath.Join(dir, configFileName), string(raw))

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if s.ConfigVersion() != 7 {
		t.Fatalf("应从文件恢复版本号 7，实际 %d", s.ConfigVersion())
	}
	if s.Config().Server.Listen != "0.0.0.0:9000" {
		t.Fatalf("应从文件恢复配置: %+v", s.Config().Server)
	}
	if len(glob(t, dir, configBakName)) != 0 {
		t.Fatalf("正常加载不该产生 .bak")
	}
}

func TestOpenVersionFallback(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Version = 0
	raw, _ := json.Marshal(cfg)
	writeFile(t, filepath.Join(dir, configFileName), string(raw))

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConfigVersion() != 1 {
		t.Fatalf("version<=0 应回落到 1，实际 %d", s.ConfigVersion())
	}
}

// 坏配置不能让服务起不来：改名留证 + 用默认值继续。
func TestOpenCorruptConfigRecovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, configFileName), "{ this is not json")

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("损坏的 config.json 不该让 Open 失败: %v", err)
	}
	if got := s.Config(); got.Checkin.Schedule.WindowStart != config.DefaultWindowStart {
		t.Fatalf("应回到默认配置: %+v", got.Checkin.Schedule)
	}
	if s.ConfigVersion() != 1 {
		t.Fatalf("应回到默认版本 1: %d", s.ConfigVersion())
	}
	corrupt := glob(t, dir, configFileName+".corrupt-*")
	if len(corrupt) != 1 {
		t.Fatalf("应留下一个 .corrupt-* 备份，实际 %v", corrupt)
	}
	if !strings.Contains(readFile(t, corrupt[0]), "not json") {
		t.Fatalf("备份里应是损坏原文")
	}
	// 新的 config.json 必须是可解析的
	var cfg model.Config
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, configFileName))), &cfg); err != nil {
		t.Fatalf("重建的配置不可解析: %v", err)
	}
	if !hasLog(s, "config.json 解析失败") {
		t.Fatalf("应在日志环里留下警告")
	}
}

func TestOpenCorruptStateRecovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, stateFileName), "[1,2,3")

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("损坏的 state.json 不该让 Open 失败: %v", err)
	}
	if st := s.State(); st.LastSuccessDate != "" || st.LastRunID != "" {
		t.Fatalf("应回到空状态: %+v", st)
	}
	if len(glob(t, dir, stateFileName+".corrupt-*")) != 1 {
		t.Fatalf("应留下 state.json.corrupt-* 备份")
	}
	if !hasLog(s, "state.json 解析失败") {
		t.Fatalf("应在日志环里留下警告")
	}
}

func TestOpenCorruptAuthRecovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, authFileName), "not json at all")

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("损坏的 auth.json 不该让 Open 失败: %v", err)
	}
	if s.PasswordHash() != "" {
		t.Fatalf("损坏后应视为未初始化: %q", s.PasswordHash())
	}
	if len(glob(t, dir, authFileName+".corrupt-*")) != 1 {
		t.Fatalf("应留下 auth.json.corrupt-* 备份")
	}
}

func hasLog(s *Store, sub string) bool {
	for _, e := range s.Logs(0) {
		if strings.Contains(e.Msg, sub) {
			return true
		}
	}
	return false
}

func TestSaveConfigVersionAndBackup(t *testing.T) {
	s, dir := openTemp(t)

	cfg := s.Config()
	cfg.Server.Listen = "127.0.0.1:9900"
	v2, err := s.SaveConfig(cfg, nil)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if v2 != 2 {
		t.Fatalf("版本应自增到 2，实际 %d", v2)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, configBakName)), `"listen": "127.0.0.1:8787"`) {
		t.Fatalf(".bak 应是上一版内容")
	}

	cfg = s.Config()
	cfg.Server.Listen = "127.0.0.1:9901"
	v3, err := s.SaveConfig(cfg, nil)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if v3 != 3 || s.ConfigVersion() != 3 {
		t.Fatalf("版本应到 3，实际 %d / %d", v3, s.ConfigVersion())
	}
	// .bak 现在应是 v2（listen 9900）
	bak := readFile(t, filepath.Join(dir, configBakName))
	if !strings.Contains(bak, "127.0.0.1:9900") {
		t.Fatalf(".bak 应滚到上一版: %s", bak)
	}
	var bakCfg model.Config
	if err := json.Unmarshal([]byte(bak), &bakCfg); err != nil {
		t.Fatalf(".bak 不是合法 JSON: %v", err)
	}
	if bakCfg.Version != 2 {
		t.Fatalf(".bak 里的 version 应为 2: %d", bakCfg.Version)
	}
	// 保存后配置里的 version 与返回值一致
	var cur model.Config
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, configFileName))), &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Version != 3 || cur.Server.Listen != "127.0.0.1:9901" {
		t.Fatalf("当前配置不对: %+v", cur.Server)
	}
	if len(glob(t, dir, "*.tmp")) != 0 {
		t.Fatalf("原子写不该留下 .tmp")
	}
}

func TestSaveConfigReopenConsistent(t *testing.T) {
	s, dir := openTemp(t)
	cfg := s.Config()
	cfg.User.Token = "tok"
	cfg.Checkin.Locations = []model.Location{
		{Longitude: "119.20463", Latitude: "26.04920", ActualLocation: "宿舍", Weight: floatPtr(2)},
	}
	cfg.Checkin.Schedule.Times = []string{"21:35", "22:10"}
	if _, err := s.SaveConfig(cfg, []string{"警告一", "警告二"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	got := again.Config()
	if got.User.Token != "tok" || len(got.Checkin.Locations) != 1 {
		t.Fatalf("重开后读回不一致: %+v", got.User)
	}
	if w := got.Checkin.Locations[0].Weight; w == nil || *w != 2 {
		t.Fatalf("权重没读回: %v", w)
	}
	if len(got.Checkin.Schedule.Times) != 2 {
		t.Fatalf("times 没读回: %v", got.Checkin.Schedule.Times)
	}
	if again.ConfigVersion() != 2 {
		t.Fatalf("重开后版本应为 2: %d", again.ConfigVersion())
	}
	if !hasLog(again, "已恢复") && !hasLog(again, "数据目录就绪") {
		t.Fatalf("重开后应有就绪日志")
	}
}

func TestSaveConfigLogsWarnings(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.SaveConfig(s.Config(), []string{"time_jitter 是负数"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if !hasLog(s, "配置警告：time_jitter 是负数") {
		t.Fatalf("警告应进日志环: %+v", s.Logs(3))
	}
	if !hasLog(s, "配置已保存 config_version=2") {
		t.Fatalf("应有保存成功日志: %+v", s.Logs(3))
	}
	if s.Logs(1)[0].Level != "info" {
		t.Fatalf("最新一条应是 info: %+v", s.Logs(1)[0])
	}
}

// 没有旧文件时不该凭空造一个空 .bak（回滚不到任何东西，只会误导）。
func TestSaveConfigWithoutPriorFileSkipsBak(t *testing.T) {
	s, dir := openTemp(t)
	if err := os.Remove(filepath.Join(dir, configFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfig(s.Config(), nil); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(glob(t, dir, configBakName)) != 0 {
		t.Fatalf("没有旧文件时不该产生 .bak")
	}
	if len(glob(t, dir, configFileName)) != 1 {
		t.Fatalf("应写出新配置")
	}
}

func TestConfigReturnedIsIsolated(t *testing.T) {
	s, _ := openTemp(t)
	got := s.Config()
	got.User.Password = "被改了"
	*got.Checkin.JitterRadiusM = 999
	got.Checkin.Schedule.Times[0] = "被改了"

	fresh := s.Config()
	if fresh.User.Password != "" {
		t.Fatalf("Config() 应返回副本，字符串被串改: %q", fresh.User.Password)
	}
	if *fresh.Checkin.JitterRadiusM != 200 || fresh.Checkin.Schedule.Times[0] != "21:35" {
		t.Fatalf("Config() 应返回副本，指针/切片被串改: %v %v",
			*fresh.Checkin.JitterRadiusM, fresh.Checkin.Schedule.Times)
	}
}

// ---------- 状态 ----------

func TestStateRoundTrip(t *testing.T) {
	s, dir := openTemp(t)
	st := model.State{
		ConfigVersion:   3,
		LastSuccessDate: "2026-11-13",
		LastAttemptAt:   time.Date(2026, 11, 13, 21, 35, 12, 0, time.UTC),
		LastRunID:       "20261113T213512-abcd",
		LastSlotKey:     "2026-11-13|21:35|21:35:12",
		Notified:        map[string][]string{"2026-11-13": {"success", "error"}},
	}
	if err := s.SaveState(st); err != nil {
		t.Fatalf("保存状态失败: %v", err)
	}
	if got := s.State(); got.LastSuccessDate != st.LastSuccessDate || len(got.Notified["2026-11-13"]) != 2 {
		t.Fatalf("内存状态不对: %+v", got)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	got := again.State()
	if got.LastSuccessDate != st.LastSuccessDate || got.LastSlotKey != st.LastSlotKey {
		t.Fatalf("重开后状态不一致: %+v", got)
	}
	if !got.LastAttemptAt.Equal(st.LastAttemptAt) {
		t.Fatalf("时间戳不一致: %v vs %v", got.LastAttemptAt, st.LastAttemptAt)
	}
	if len(got.Notified["2026-11-13"]) != 2 {
		t.Fatalf("通知记录不一致: %+v", got.Notified)
	}
	if len(glob(t, dir, stateFileName)) != 1 || len(glob(t, dir, "*.tmp")) != 0 {
		t.Fatalf("状态文件落盘不对")
	}
}

func TestStateReturnedIsIsolated(t *testing.T) {
	s, _ := openTemp(t)
	st := model.State{Notified: map[string][]string{"d": {"success"}}}
	if err := s.SaveState(st); err != nil {
		t.Fatal(err)
	}
	got := s.State()
	got.Notified["d"][0] = "被改了"
	got.Notified["新键"] = []string{"x"}

	fresh := s.State()
	if fresh.Notified["d"][0] != "success" {
		t.Fatalf("State() 应返回深拷贝")
	}
	if _, ok := fresh.Notified["新键"]; ok {
		t.Fatalf("State() 的 map 被串改了")
	}
}

// ---------- 鉴权 ----------

func TestPasswordHashAndAuthFile(t *testing.T) {
	s, dir := openTemp(t)
	if s.PasswordHash() != "" {
		t.Fatalf("初始应为空")
	}
	if err := s.SetPasswordHash("pbkdf2$sha256$1$abc$def"); err != nil {
		t.Fatalf("写入密码哈希失败: %v", err)
	}
	if s.PasswordHash() != "pbkdf2$sha256$1$abc$def" {
		t.Fatalf("内存未更新: %q", s.PasswordHash())
	}
	var doc authDoc
	raw := readFile(t, filepath.Join(dir, authFileName))
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("auth.json 不是合法 JSON: %v", err)
	}
	if doc.PasswordHash != "pbkdf2$sha256$1$abc$def" || doc.InitializedAt == "" {
		t.Fatalf("auth.json 内容不对: %+v", doc)
	}
	firstInit := doc.InitializedAt

	// 再改一次密码：initialized_at 不该被改（它是部署的出生时间）
	if err := s.SetPasswordHash("pbkdf2$sha256$2$xyz$uvw"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	if again.PasswordHash() != "pbkdf2$sha256$2$xyz$uvw" {
		t.Fatalf("重开后密码哈希不对: %q", again.PasswordHash())
	}
	if again.auth.InitializedAt != firstInit {
		t.Fatalf("initialized_at 不该变化: %q vs %q", again.auth.InitializedAt, firstInit)
	}
}

// ---------- 会话密钥 ----------

func TestSessionKeyStable(t *testing.T) {
	s, dir := openTemp(t)
	key1, err := s.SessionKey()
	if err != nil {
		t.Fatalf("取密钥失败: %v", err)
	}
	if len(key1) < 16 {
		t.Fatalf("密钥太短: %d", len(key1))
	}
	fileContent := strings.TrimSpace(readFile(t, filepath.Join(dir, secretFileName)))
	if string(key1) != fileContent {
		t.Fatalf("SessionKey 应返回 secret.key 的原始内容: %q vs %q", string(key1), fileContent)
	}

	// 返回副本：调用方改写不影响后续签名
	key1[0] = 'X'
	key2, _ := s.SessionKey()
	if string(key2) != fileContent {
		t.Fatalf("SessionKey 应返回副本")
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	key3, err := again.SessionKey()
	if err != nil {
		t.Fatalf("重开后取密钥失败: %v", err)
	}
	if string(key3) != fileContent {
		t.Fatalf("密钥必须跨重启稳定（否则重启即掉登录）: %q vs %q", string(key3), fileContent)
	}
}

func TestSessionKeyRegeneratesWhenCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, secretFileName), "short")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("损坏的 secret.key 不该让 Open 失败: %v", err)
	}
	key, err := s.SessionKey()
	if err != nil {
		t.Fatalf("应已重新生成: %v", err)
	}
	if len(key) < 16 {
		t.Fatalf("新密钥太短: %d", len(key))
	}
	if hasLog(s, "内容过短") == false {
		t.Fatalf("应留下「重新生成」警告: %+v", s.Logs(3))
	}
}

// ---------- 运行历史 ----------

func TestAppendRunAndOrder(t *testing.T) {
	s, dir := openTemp(t)
	base := time.Date(2026, 11, 14, 21, 35, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		r := model.RunRecord{
			ID:        fmt.Sprintf("run-%d", i),
			Mode:      "scheduled",
			StartedAt: base.Add(time.Duration(i) * time.Minute),
			Status:    model.StatusSuccess,
			Logs:      []string{"line"},
		}
		if err := s.AppendRun(r); err != nil {
			t.Fatalf("追加失败: %v", err)
		}
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 3 {
		t.Fatalf("文件应有 3 行，实际 %d", n)
	}
	runs, err := s.Runs(0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("应有 3 条: %d", len(runs))
	}
	if runs[0].ID != "run-2" || runs[2].ID != "run-0" {
		t.Fatalf("应为新的在前: %+v", []string{runs[0].ID, runs[1].ID, runs[2].ID})
	}
	limited, _ := s.Runs(2)
	if len(limited) != 2 || limited[0].ID != "run-2" {
		t.Fatalf("limit 未生效: %+v", limited)
	}
	if _, ok := s.RunByID("run-1"); !ok {
		t.Fatalf("RunByID 应能按 id 找到")
	}
	if _, ok := s.RunByID("不存在"); ok {
		t.Fatalf("不存在的 id 应返回 false")
	}
}

func TestRunRecordDeepCopy(t *testing.T) {
	s, _ := openTemp(t)
	r := model.RunRecord{
		ID:       "r1",
		Location: &model.RunLocation{Lon: 119.2, Lat: 26.0, Label: "single"},
		Logs:     []string{"a"},
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	got, _ := s.RunByID("r1")
	got.Location.Lon = 0
	got.Logs[0] = "改了"

	again, _ := s.RunByID("r1")
	if again.Location.Lon != 119.2 || again.Logs[0] != "a" {
		t.Fatalf("RunByID 应返回深拷贝: %+v", again)
	}
}

func TestAppendRunWithoutID(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.AppendRun(model.RunRecord{Status: model.StatusError}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RunByID(""); ok {
		t.Fatalf("空 id 不该进索引")
	}
	runs, _ := s.Runs(0)
	if len(runs) != 1 {
		t.Fatalf("仍应追加到内存历史: %d", len(runs))
	}
}

// 并发追加 100 条：-race 下必须干净，且一条不丢、索引齐全。
func TestAppendRunConcurrent(t *testing.T) {
	s, dir := openTemp(t)
	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.AppendRun(model.RunRecord{
				ID:        fmt.Sprintf("concurrent-%03d", i),
				Status:    model.StatusSuccess,
				StartedAt: time.Now(),
				Logs:      []string{fmt.Sprintf("log-%d", i)},
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发追加出错: %v", err)
		}
	}
	if ln := countLines(t, filepath.Join(dir, runsFileName)); ln != n {
		t.Fatalf("文件应有 %d 行，实际 %d", n, ln)
	}
	runs, _ := s.Runs(0)
	if len(runs) != n {
		t.Fatalf("内存应有 %d 条，实际 %d", n, len(runs))
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("concurrent-%03d", i)
		if _, ok := s.RunByID(id); !ok {
			t.Fatalf("索引里缺 %s", id)
		}
	}
}

// 启动时行数超限要轮转：整份搬到 runs.jsonl.1，主文件清空。
func TestRunsRotationOnOpen(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, runsOldName), `{"id":"stale"}`+"\n")
	var sb strings.Builder
	for i := 0; i < runsRotateLines+1; i++ {
		fmt.Fprintf(&sb, `{"id":"seed-%04d","status":"success"}`+"\n", i)
	}
	writeFile(t, filepath.Join(dir, runsFileName), sb.String())

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if n := countLines(t, filepath.Join(dir, runsOldName)); n != runsRotateLines+1 {
		t.Fatalf(".1 应收到全部 %d 行，实际 %d", runsRotateLines+1, n)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, runsOldName)), "stale") {
		t.Fatalf("旧的 .1 应被覆盖")
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 0 {
		t.Fatalf("主文件应清空，实际 %d 行", n)
	}
	if runs, _ := s.Runs(0); len(runs) != 0 {
		t.Fatalf("轮转后不应把旧历史读进内存: %d", len(runs))
	}
	if !hasLog(s, "已轮转") {
		t.Fatalf("应留下轮转日志: %+v", s.Logs(3))
	}

	// 轮转后继续追加仍然可用
	if err := s.AppendRun(model.RunRecord{ID: "after"}); err != nil {
		t.Fatalf("轮转后追加失败: %v", err)
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 1 {
		t.Fatalf("轮转后应重新从 1 行开始，实际 %d", n)
	}
}

// 运行期也要轮转：常驻服务不会重启，全靠启动时检查会让文件无限长大。
func TestRunsRotationAtRuntime(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < runsRotateLines; i++ {
		fmt.Fprintf(&sb, `{"id":"seed-%04d","status":"success"}`+"\n", i)
	}
	writeFile(t, filepath.Join(dir, runsFileName), sb.String())

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.Runs(0); len(runs) != runsRotateLines {
		t.Fatalf("加载了 %d 条，期望 %d", len(runs), runsRotateLines)
	}
	if err := s.AppendRun(model.RunRecord{ID: "trigger"}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if len(glob(t, dir, runsOldName)) != 1 {
		t.Fatalf("应触发轮转: %v", glob(t, dir, runsFileName+".1"))
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 0 {
		t.Fatalf("轮转后主文件应为空，实际 %d", n)
	}
	// 内存索引封顶
	if runs, _ := s.Runs(0); len(runs) > runsMemoryCap {
		t.Fatalf("内存历史应封顶 %d，实际 %d", runsMemoryCap, len(runs))
	}
}

func TestRunsSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"a"}` + "\n" + "not json\n" + "\n" + `{"id":"b"}` + "\n"
	writeFile(t, filepath.Join(dir, runsFileName), content)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	runs, _ := s.Runs(0)
	if len(runs) != 2 {
		t.Fatalf("应跳过坏行只读 2 条: %+v", runs)
	}
	if runs[0].ID != "b" {
		t.Fatalf("顺序不对: %+v", runs)
	}
	if !hasLog(s, "跳过一行无法解析") {
		t.Fatalf("应留下跳过警告: %+v", s.Logs(5))
	}
}

// ---------- 日志环 ----------

func TestLogsRingCapacity(t *testing.T) {
	s, _ := openTemp(t)
	for i := 0; i < 1200; i++ {
		s.AppendLog(model.LogEvent{Level: "info", Msg: fmt.Sprintf("msg-%04d", i)})
	}
	all := s.Logs(0)
	if len(all) != logRingCap {
		t.Fatalf("日志环应只剩 %d 条，实际 %d", logRingCap, len(all))
	}
	if all[0].Msg != "msg-1199" {
		t.Fatalf("最新一条应在最前: %q", all[0].Msg)
	}
	if all[len(all)-1].Msg != "msg-0200" {
		t.Fatalf("最旧一条应是第 200 条: %q", all[len(all)-1].Msg)
	}
	if got := s.Logs(3); len(got) != 3 || got[0].Msg != "msg-1199" {
		t.Fatalf("limit 未生效: %+v", got)
	}
}

func TestLogsDefaultsAndLimit(t *testing.T) {
	s, _ := openTemp(t)
	s.AppendLog(model.LogEvent{Msg: "没有时间戳"})
	got := s.Logs(1)[0]
	if got.TS.IsZero() {
		t.Fatalf("零值时间戳应补当前时间")
	}
	if got.Level != "info" {
		t.Fatalf("空级别应补 info: %q", got.Level)
	}
	if n := len(s.Logs(-1)); n < 2 {
		t.Fatalf("负数 limit 应返回全部: %d", n)
	}
	if n := len(s.Logs(0)); n < 2 {
		t.Fatalf("0 也应返回全部: %d", n)
	}
}

func TestLogsConcurrent(t *testing.T) {
	s, _ := openTemp(t)
	base := len(s.Logs(0)) // Open 自己会写"写入默认模板""数据目录就绪"两条
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.AppendLog(model.LogEvent{Msg: fmt.Sprintf("c-%d", i)})
			_ = s.Logs(5)
		}(i)
	}
	wg.Wait()
	if got := s.Logs(0); len(got) != n+base {
		t.Fatalf("应有 %d 条日志，实际 %d", n+base, len(got))
	}
}

// 配置与日志的并发读写：-race 必须干净（HTTP 处理器会同时读配置、写日志）。
func TestConcurrentConfigAndState(t *testing.T) {
	s, _ := openTemp(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := s.Config()
			cfg.Server.Listen = fmt.Sprintf("127.0.0.1:9%03d", i)
			if _, err := s.SaveConfig(cfg, nil); err != nil {
				t.Errorf("保存失败: %v", err)
			}
			_ = s.ConfigVersion()
			s.AppendLog(model.LogEvent{Msg: fmt.Sprintf("并发-%d", i)})
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.Config()
				_ = s.State()
				_ = s.Logs(10)
				_, _ = s.Runs(10)
			}
		}()
	}
	wg.Wait()
	if s.ConfigVersion() < 21 {
		t.Fatalf("版本应累加到至少 21，实际 %d", s.ConfigVersion())
	}
}

// Open("") 用默认数据目录：这里靠 chdir 到临时目录来验证，避免污染包目录。
func TestOpenEmptyDirUsesDefault(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := os.Chdir(old); cerr != nil {
			t.Fatalf("恢复工作目录失败: %v", cerr)
		}
	}()

	s, oerr := Open("  ")
	if oerr != nil {
		t.Fatalf("Open 失败: %v", oerr)
	}
	if want := filepath.Join(dir, "data"); s.Dir != want {
		t.Fatalf("默认数据目录不对: got=%q want=%q", s.Dir, want)
	}
	if len(glob(t, filepath.Join(dir, "data"), configFileName)) != 1 {
		t.Fatalf("默认目录里应写出配置")
	}
}

func TestOpenReportsMkdirFailure(t *testing.T) {
	dir := t.TempDir()
	// 拿一个文件当目录用：MkdirAll 必然失败
	blocker := filepath.Join(dir, "blocker")
	writeFile(t, blocker, "x")
	if _, err := Open(filepath.Join(blocker, "data")); err == nil {
		t.Fatalf("父路径是文件时应报错")
	}
}

func floatPtr(v float64) *float64 { return &v }

// ---------- 落盘失败时的语义 ----------

// 备份失败必须中止保存：没有备份就覆盖用户唯一的配置，是不可接受的失败模式。
func TestSaveConfigBackupFailureAborts(t *testing.T) {
	s, dir := openTemp(t)
	// 把 .bak 占成目录：备份写不进去
	if err := os.Mkdir(filepath.Join(dir, configBakName), 0o700); err != nil {
		t.Fatal(err)
	}
	before := s.ConfigVersion()
	cfg := s.Config()
	cfg.Server.Listen = "127.0.0.1:1234"
	if _, err := s.SaveConfig(cfg, nil); err == nil {
		t.Fatalf("备份失败时应报错")
	} else if !strings.Contains(err.Error(), "备份旧配置失败") {
		t.Fatalf("报错应说明是备份失败: %v", err)
	}
	if s.ConfigVersion() != before {
		t.Fatalf("保存失败时版本不该前进: %d -> %d", before, s.ConfigVersion())
	}
	if s.Config().Server.Listen == "127.0.0.1:1234" {
		t.Fatalf("保存失败时不该更新内存配置")
	}
}

func TestSaveConfigWriteFailure(t *testing.T) {
	s, dir := openTemp(t)
	path := filepath.Join(dir, configFileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfig(s.Config(), nil); err == nil {
		t.Fatalf("目标位置不可写时应报错")
	} else if !strings.Contains(err.Error(), "写入 config.json 失败") {
		t.Fatalf("报错应指出写哪个文件: %v", err)
	}
	if s.ConfigVersion() != 1 {
		t.Fatalf("失败后版本仍是 1: %d", s.ConfigVersion())
	}
}

func TestSaveStateWriteFailure(t *testing.T) {
	s, dir := openTemp(t)
	path := filepath.Join(dir, stateFileName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveState(model.State{LastSuccessDate: "2026-11-14"}); err == nil {
		t.Fatalf("state.json 不可写时应报错")
	}
	if s.State().LastSuccessDate != "" {
		t.Fatalf("失败时不该更新内存状态")
	}
}

func TestAppendRunWriteFailure(t *testing.T) {
	s, dir := openTemp(t)
	if err := os.Mkdir(filepath.Join(dir, runsFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(model.RunRecord{ID: "x"}); err == nil {
		t.Fatalf("runs.jsonl 不是文件时应报错")
	}
	if _, ok := s.RunByID("x"); ok {
		t.Fatalf("写失败时不该进内存索引")
	}
}

// 轮转失败只能记警告，不能假装成功：日志里说"已轮转"但文件没动，会误导排查。
func TestRotateFailureIsLoggedNotFaked(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	// runs.jsonl.1 占成非空目录 → 删不掉也覆盖不了
	oldDir := filepath.Join(dir, runsOldName)
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(oldDir, "占用.txt"), "x")
	var sb strings.Builder
	for i := 0; i < runsRotateLines+5; i++ {
		fmt.Fprintf(&sb, `{"id":"seed-%04d"}`+"\n", i)
	}
	writeFile(t, filepath.Join(dir, runsFileName), sb.String())

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("轮转失败不该让 Open 失败: %v", err)
	}
	if !hasLog(s, "轮转 runs.jsonl 失败") {
		t.Fatalf("应记下轮转失败: %+v", s.Logs(5))
	}
	if hasLog(s, "已轮转") {
		t.Fatalf("轮转失败时不能记「已轮转」")
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != runsRotateLines+5 {
		t.Fatalf("失败后主文件应保持原样，实际 %d 行", n)
	}
}

func TestOpenSecretReadErrorFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, secretFileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatalf("密钥读不出来时应让 Open 失败（否则会话签名不可靠）")
	}
}

func TestOpenAuthReadErrorWarns(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, authFileName)
	writeFile(t, path, `{"password_hash":"h"}`)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("auth.json 读不出来不该阻断启动: %v", err)
	}
	if s.PasswordHash() != "" {
		t.Fatalf("应视为未初始化")
	}
	if !hasLog(s, "视为未初始化") {
		t.Fatalf("应留下警告: %+v", s.Logs(5))
	}
}

// 运行记录的指针字段也要深拷贝：仪表盘会拿到 School/Notify 里的值。
func TestRunDeepCopyWithNestedFields(t *testing.T) {
	s, _ := openTemp(t)
	r := model.RunRecord{
		ID:       "deep",
		Location: &model.RunLocation{Lon: 1, Lat: 2, Label: "single"},
		School:   &model.RunSchool{Campus: "旗山", Matched: true},
		Notify:   &model.RunNotify{Channel: "serverchan", OK: true},
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	got, ok := s.RunByID("deep")
	if !ok {
		t.Fatal("应能查到")
	}
	if got.School == nil || got.School.Campus != "旗山" || got.Notify == nil || !got.Notify.OK {
		t.Fatalf("嵌套字段没带回来: %+v", got)
	}
	got.School.Campus = "改了"
	got.Notify.OK = false
	again, _ := s.RunByID("deep")
	if again.School.Campus != "旗山" || !again.Notify.OK {
		t.Fatalf("嵌套指针应深拷贝: %+v", again)
	}
}
