//go:build !windows

package store

import "os"

// hardenDataDir 在类 Unix 上把数据目录钉成 0700。
//
// 目录里是加密后的 config.json 与 secret.key：文件权限由 writeFile 逐个设成 0600，
// 这里补上目录本身——没有目录的执行位，别人连"文件名"都列不出来。
// 已经存在的目录也要重设一次：历史版本创建它时可能带着当时的 umask。
func hardenDataDir(dir string) error {
	return os.Chmod(dir, 0o700)
}
