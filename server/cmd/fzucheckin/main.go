// Command fzucheckin 是福州大学晚点名自动签到的常驻服务入口。
//
// 它只做四件事：解析参数 → 打开数据目录 → 装配 server → 等信号后优雅退出。
// 任何"吞掉错误继续启动"的写法都在这里被明确禁止：起不来就该带着原因退出，
// 而不是留下一个看起来在跑、实际上永远不会签到的进程（那种失败最难被发现）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"fzucheckin/internal/config"
	"fzucheckin/internal/model"
	"fzucheckin/internal/server"
	"fzucheckin/internal/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	listen := flag.String("listen", envOr("FZU_LISTEN", ""),
		"监听地址，例如 127.0.0.1:8787；不填则用配置里的 server.listen（默认 "+config.DefaultListen+"）")
	dataDir := flag.String("data", envOr("FZU_DATA_DIR", config.DefaultDataDir),
		"数据目录（config.json / state.json / runs.jsonl / secret.key / auth.json）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	// 只有确实把服务藏在可信反向代理后面时才打开：开着意味着限速身份取自
	// X-Forwarded-For，而那是客户端能随手写的头——可伪造的头不能当身份。
	trustProxy := flag.Bool("trust-proxy", envBool("FZU_TRUST_PROXY"),
		"采信 X-Forwarded-For（仅当服务前有可信反向代理时开启）")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"用法：fzucheckin [-listen 127.0.0.1:8787] [-data ./data] [-trust-proxy] [-version]\n"+
				"环境变量：FZU_LISTEN / FZU_DATA_DIR / FZU_LOG_LEVEL(debug|info|warn|error) / FZU_TRUST_PROXY=1\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("fzucheckin version %s\n", server.Version)
		return 0
	}

	// 先开数据目录：它是唯一真源（配置/状态/历史/会话密钥），
	// 打不开就没有任何继续的意义。
	st, err := store.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：数据目录不可用：%v\n", err)
		return 1
	}

	srv, err := server.New(server.Deps{Store: st, TrustProxy: *trustProxy})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：服务装配失败：%v\n", err)
		return 1
	}
	// 命令行参数的优先级高于配置：配置文件是"默认值"，-listen 是"这次怎么跑"。
	if strings.TrimSpace(*listen) != "" {
		srv.SetListen(*listen)
	}
	if level := strings.TrimSpace(os.Getenv("FZU_LOG_LEVEL")); level != "" {
		srv.SetLogLevel(level)
	}

	// 收到 SIGINT/SIGTERM 后 ctx 被取消，server.Run 负责停调度器、关 HTTP、
	// 等进行中的签到最多 60 秒，然后返回。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// data_dir 只能由启动参数决定：config.json 本身就躺在数据目录里，读它之前必须
	// 先知道目录（鸡生蛋）。这里把解析后的真实目录写回配置，让界面显示的是真值，
	// 而不是一个"改了也不生效"的假字段。
	syncDataDir(st, *dataDir)

	printBanner(srv, st)

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "服务异常退出：%v\n", err)
		return 1
	}
	return 0
}

// printBanner 打印"我起来了、去哪里点、数据在哪"。
// stdout 而不是 stderr：systemd/计划任务把它当正常输出收进日志，
// 而用户第一次运行最需要的就是那个可点开的 URL。
func printBanner(srv *server.Server, st *store.Store) {
	addr := srv.Addr()
	fmt.Printf("fzu-auto-checkin v%s\n", server.Version)
	fmt.Printf("  管理界面  %s\n", urlOf(addr))
	fmt.Printf("  监听地址  %s\n", addr)
	fmt.Printf("  数据目录  %s\n", st.Dir)
	fmt.Printf("  首次启动请打开上面的地址设置管理密码；Ctrl+C 退出\n")
}

// urlOf 把监听地址变成可点开的 URL（":8787" / "0.0.0.0:8787" → 127.0.0.1）。
func urlOf(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// envOr 优先取环境变量，空则用默认值。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envBool 把 "1/true/yes/on" 视为真（与 CHECKIN_FORCE 的严格 "1" 不同：
// 这里没有"手滑值"的风险，宽松解析更符合习惯）。
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// syncDataDir 把实际数据目录写回配置，避免界面上那个字段成为"承诺了做不到的事"。
func syncDataDir(st *store.Store, dataDir string) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return
	}
	if samePath(st.Config().Server.DataDir, abs) {
		return
	}
	if _, err := st.UpdateConfig(func(cfg *model.Config) []string {
		cfg.Server.DataDir = abs
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "提示：写回 data_dir 失败（不影响运行）：%v\n", err)
	}
}

// samePath 在 Windows 上大小写不敏感，直接比字符串会把同一个目录判成不同。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
