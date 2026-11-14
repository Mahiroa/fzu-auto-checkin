package store

// 持久化层的对抗性验收用例（verify-agent）。
//
// 既有 store_test.go 已经覆盖了"正常路径 + 语法损坏"；这里补的是**对抗性**输入：
// 类型不匹配的 JSON、Windows 换行、字段缺失的"半份配置"、以及轮转之后
// "GET /runs 还能不能看到最近记录"这条契约（§7 §19）。
//
// 全部确定性、离线、不依赖时钟。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
)

// reviewRunID 造一个稳定可比较的 id。
func reviewRunID(i int) string { return fmt.Sprintf("review-%04d", i) }

// TestReviewRotationKeepsRecentRuns 覆盖 §7「超过 2000 行轮转」之后
// 「GET /runs 仍返回最近记录」这条可见行为（GET /runs 只是 store.Runs 的薄包装）。
func TestReviewRotationKeepsRecentRuns(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("首次 Open 失败：%v", err)
	}
	// 预置正好 2000 行：Open 会全部读进内存（等于 runsRotateLines，不触发轮转）。
	var sb strings.Builder
	for i := 0; i < runsRotateLines; i++ {
		fmt.Fprintf(&sb, `{"id":"%s","status":"success"}`+"\n", reviewRunID(i))
	}
	writeFile(t, filepath.Join(dir, runsFileName), sb.String())

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	if runs, _ := s.Runs(0); len(runs) != runsRotateLines {
		t.Fatalf("应读入 %d 条，实际 %d", runsRotateLines, len(runs))
	}
	if len(glob(t, dir, runsOldName)) != 0 {
		t.Fatalf("正好 %d 行不该触发轮转", runsRotateLines)
	}

	// 第 2001 行触发运行期轮转。
	if err := s.AppendRun(model.RunRecord{ID: "trigger", Status: model.StatusSuccess}); err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if len(glob(t, dir, runsOldName)) != 1 {
		t.Fatalf("应触发轮转（缺少 %s）", runsOldName)
	}
	if n := countLines(t, filepath.Join(dir, runsOldName)); n != runsRotateLines+1 {
		t.Fatalf(".1 应收到全部 %d 行，实际 %d", runsRotateLines+1, n)
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 0 {
		t.Fatalf("轮转后主文件应为空，实际 %d 行", n)
	}

	// 关键断言：轮转**不能**把内存里的历史清掉，否则仪表盘会突然"历史消失"。
	recent, err := s.Runs(5)
	if err != nil {
		t.Fatalf("Runs 失败：%v", err)
	}
	if len(recent) != 5 {
		t.Fatalf("轮转后 Runs(5) 应仍返回 5 条，实际 %d", len(recent))
	}
	if recent[0].ID != "trigger" {
		t.Fatalf("最新的应是 trigger，实际 %q", recent[0].ID)
	}
	want := []string{"trigger", reviewRunID(runsRotateLines - 1), reviewRunID(runsRotateLines - 2)}
	for i, id := range want {
		if recent[i].ID != id {
			t.Fatalf("轮转后 Runs(5) 第 %d 条期望 %q，实际 %q", i, id, recent[i].ID)
		}
	}
	if _, ok := s.RunByID("trigger"); !ok {
		t.Fatalf("轮转后 RunByID(trigger) 应仍可查")
	}
	// 内存封顶到 runsMemoryCap：被挤出去的最旧一条索引也要一起删掉。
	if _, ok := s.RunByID(reviewRunID(0)); ok {
		t.Fatalf("%s 已被内存上限挤出，索引里不该还在", reviewRunID(0))
	}
	if _, ok := s.RunByID(reviewRunID(1)); !ok {
		t.Fatalf("%s 应仍在索引里", reviewRunID(1))
	}

	// 轮转后继续追加：文件从 1 行重新开始，顺序依旧新的在前。
	if err := s.AppendRun(model.RunRecord{ID: "after", Status: model.StatusSuccess}); err != nil {
		t.Fatalf("轮转后追加失败：%v", err)
	}
	if n := countLines(t, filepath.Join(dir, runsFileName)); n != 1 {
		t.Fatalf("轮转后应重新从 1 行开始，实际 %d", n)
	}
	if latest, _ := s.Runs(1); len(latest) != 1 || latest[0].ID != "after" {
		t.Fatalf("最近一条应为 after，实际 %+v", latest)
	}
}

// TestReviewCorruptConfigTypeMismatchRecovers 覆盖"JSON 语法合法但字段类型错"的损坏：
// 与语法错误一样必须降级启动（§7 崩溃恢复），且留下 .corrupt 证据。
func TestReviewCorruptConfigTypeMismatchRecovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("首次 Open 失败：%v", err)
	}
	// version 被手写成了字符串；checkin 被写成了数组。两者都会让 Unmarshal 失败。
	writeFile(t, filepath.Join(dir, configFileName), `{"version":"seven","checkin":[]}`)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("类型不匹配的 config.json 不该让 Open 失败：%v", err)
	}
	if got := s.Config().Checkin.Schedule.WindowStart; got != config.DefaultWindowStart {
		t.Fatalf("应回落到默认配置，实际 window_start=%q", got)
	}
	if s.ConfigVersion() != 1 {
		t.Fatalf("应回落到默认版本 1，实际 %d", s.ConfigVersion())
	}
	corrupt := glob(t, dir, configFileName+".corrupt-*")
	if len(corrupt) != 1 {
		t.Fatalf("应留下一个 .corrupt-* 备份，实际 %v", corrupt)
	}
	if !strings.Contains(readFile(t, corrupt[0]), "seven") {
		t.Fatalf("备份里应是损坏原文")
	}
	var rebuilt model.Config
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, configFileName))), &rebuilt); err != nil {
		t.Fatalf("重建的 config.json 不可解析：%v", err)
	}
	// 重建后必须能正常保存（否则服务虽然起来了却永远存不了配置）。
	if _, err := s.SaveConfig(s.Config(), nil); err != nil {
		t.Fatalf("恢复后保存配置失败：%v", err)
	}
}

// TestReviewRunsCRLFIsTolerated 覆盖 Windows 上最常见的输入：用记事本/别的工具
// 看过一眼 runs.jsonl，行尾就变成 \r\n。加载必须照常（不能整份丢掉）。
func TestReviewRunsCRLFIsTolerated(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("首次 Open 失败：%v", err)
	}
	var sb strings.Builder
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&sb, `{"id":"%s","status":"success"}`+"\r\n", reviewRunID(i))
	}
	writeFile(t, filepath.Join(dir, runsFileName), sb.String())

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	runs, err := s.Runs(0)
	if err != nil {
		t.Fatalf("Runs 失败：%v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("CRLF 文件应读入 3 条，实际 %d", len(runs))
	}
	if runs[0].ID != reviewRunID(2) {
		t.Fatalf("顺序应为新的在前，实际首条 %q", runs[0].ID)
	}
	if hasLog(s, "跳过一行无法解析") {
		t.Fatalf("CRLF 不该被当成坏行")
	}
}

// TestReviewMinimalConfigFileIsUsable 记录一个"配置文件存在但缺字段"的现状：
// Open 不做 Normalize，直接把半份配置交给上层（默认值只在 PUT/Normalize 时补齐）。
// 这里只断言"不 panic、能继续用"，语义差异写进报告的「契约偏差」。
func TestReviewMinimalConfigFileIsUsable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("首次 Open 失败：%v", err)
	}
	writeFile(t, filepath.Join(dir, configFileName), `{"version":3}`)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("半份配置不该让 Open 失败：%v", err)
	}
	if s.ConfigVersion() != 3 {
		t.Fatalf("版本应保留 3，实际 %d", s.ConfigVersion())
	}
	got := s.Config()
	if got.Checkin.Schedule.WindowStart == "" {
		t.Logf("现状：config.json 只写了 version 时，store 不做 Normalize，" +
			"回显的 window_start/times 全是空（契约 §6.2 的默认值只在 PUT 时补齐）")
	}
	if got.Server.Listen != "" {
		t.Fatalf("半份配置的 listen 应为空（由 server.New 兜默认值），实际 %q", got.Server.Listen)
	}
	// 保存一次即可得到完整配置（PUT /config 会走 Normalize）。
	if _, err := s.SaveConfig(config.DefaultConfig(), nil); err != nil {
		t.Fatalf("保存默认配置失败：%v", err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	if again.Config().Checkin.Schedule.WindowStart != config.DefaultWindowStart {
		t.Fatalf("保存默认配置后应能读回默认窗口")
	}
	if again.ConfigVersion() != 4 {
		t.Fatalf("版本应递增到 4，实际 %d", again.ConfigVersion())
	}
	// 半份配置也没有产生 .corrupt（它是合法 JSON，不该被隔离）。
	if files := glob(t, dir, configFileName+".corrupt-*"); len(files) != 0 {
		t.Fatalf("合法但字段缺失的配置不该被隔离：%v", files)
	}
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err != nil {
		t.Fatalf("config.json 应存在：%v", err)
	}
}
