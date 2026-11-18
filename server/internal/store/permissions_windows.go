//go:build windows

package store

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// hardenDataDir 把数据目录的 ACL 收紧到"只有本账号（外加 SYSTEM / Administrators）"。
//
// 为什么非做不可：Windows 上 os.Chmod 只影响只读位，Go 的 Stat 会如实报告 0666 ——
// 也就是说 writeFile 里那个 0600 在 Windows 上等于没写。而数据目录里同时躺着
// config.json（敏感字段已加密）和 secret.key（那把钥匙）：能读到目录的人就能解密，
// 所以 ACL 才是这台机器上真正的边界。
//
// 用 icacls 而不是 golang.org/x/sys/windows：本项目坚持零第三方依赖，而"去掉继承
// 并重新授权"这件事 icacls 一行就能表达；失败只告警（见 Open 调用处），不因为
// 权限不完美就让服务起不来。
//
// FZU_SKIP_ACL=1 可跳过：数据目录放在被别的账号共享的卷上（备份代理、家庭共享盘）
// 时，去掉继承会顺手把别人挡在外面。
//
// **两步的顺序不能反**（2026-11-18 实测）：`icacls <dir> /inheritance:r
// /grant:r *SID:(OI)(CI)F /T` 会把已存在文件的 DACL 清成空 —— (OI)(CI) 是继承标志，
// icacls 不会把它落到文件对象本身，结果连属主都读不了 secret.key，服务直接自锁。
// 正确做法是先给树里每个对象加**非继承**的显式 ACE，再只对目录本身换成可继承的 ACE。
func hardenDataDir(dir string) error {
	if strings.TrimSpace(os.Getenv("FZU_SKIP_ACL")) != "" {
		return nil
	}
	me, err := user.Current()
	if err != nil {
		return fmt.Errorf("读取当前用户失败: %w", err)
	}
	sid := strings.TrimSpace(me.Uid) // Windows 上 Uid 就是 SID（S-1-5-21-...）
	if sid == "" {
		return fmt.Errorf("当前用户没有可用的 SID")
	}
	// 三个受益者：跑服务的账号、SYSTEM（计划任务/服务要用）、Administrators（备份与排障要用）。
	principals := []string{
		"*" + sid,
		"*S-1-5-18",
		"*S-1-5-32-544",
	}

	// 第一步：整棵树加显式（非继承）全权。已存在的文件不会自动继承，必须逐个给。
	treeArgs := []string{dir}
	for _, p := range principals {
		treeArgs = append(treeArgs, "/grant", p+":F")
	}
	treeArgs = append(treeArgs, "/T", "/C")
	if out, err := runICACLS(treeArgs); err != nil {
		return fmt.Errorf("给数据目录树授权失败: %w（%s）", err, out)
	}

	// 第二步：只改目录本身 —— 去掉从父目录继承来的 ACE（%USERPROFILE% 下默认带
	// Users 可读），换成可继承的全权，之后新建的文件自动带上同样的 ACL。
	dirArgs := []string{dir, "/inheritance:r"}
	for _, p := range principals {
		dirArgs = append(dirArgs, "/grant:r", p+":(OI)(CI)F")
	}
	if out, err := runICACLS(dirArgs); err != nil {
		return fmt.Errorf("收紧数据目录 ACL 失败: %w（%s）", err, out)
	}

	// 第三步：自己验证一遍"还能用"。改坏了 ACL 的代价是服务再也读不到自己的密钥，
	// 与其等到 loadSecret 报一句语焉不详的 Access denied，不如在这里说清楚。
	return verifyDirUsable(dir)
}

// runICACLS 跑一次 icacls，失败时把它的输出带回去（icacls 的报错比 exit code 有信息量）。
func runICACLS(args []string) (string, error) {
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), err
	}
	return "", nil
}

// verifyDirUsable 做一次真实的建/写/读/删，确认收紧之后本进程仍然能操作数据目录。
func verifyDirUsable(dir string) error {
	probe := filepath.Join(dir, ".acl-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("收紧后无法在数据目录写入（请手动检查 ACL 或设置 FZU_SKIP_ACL=1）: %w", err)
	}
	if _, err := os.ReadFile(probe); err != nil {
		_ = os.Remove(probe)
		return fmt.Errorf("收紧后无法读取数据目录里的文件（请手动检查 ACL 或设置 FZU_SKIP_ACL=1）: %w", err)
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("收紧后无法删除数据目录里的文件（请手动检查 ACL 或设置 FZU_SKIP_ACL=1）: %w", err)
	}
	return nil
}
