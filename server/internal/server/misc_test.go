package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestSanitizeErrorMasksEverySecret 直接压脱敏逻辑：它同时保护日志、SSE、
// runs.jsonl 与推送正文，是"不许泄漏凭据"这条硬约束的唯一实现点。
func TestSanitizeErrorMasksEverySecret(t *testing.T) {
	env := newTestEnv(t)
	cfg := baseConfig()
	cfg.User.Token = "token-very-secret-1234"
	cfg.Notify.Type = "wecom"
	cfg.Notify.ServerChanKey = "SCTsecret9876"
	cfg.Notify.PushPlusToken = "pushplus-token-55"
	cfg.Notify.BarkURL = "https://api.day.app/barkdevicekey"
	cfg.Notify.WecomWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=wecomkey"

	raw := "上游返回 401：token=" + cfg.User.Token +
		" password=" + cfg.User.Password +
		" sct=" + cfg.Notify.ServerChanKey +
		" pp=" + cfg.Notify.PushPlusToken +
		" bark=" + cfg.Notify.BarkURL +
		" wecom=" + cfg.Notify.WecomWebhook

	got := env.srv.sanitizeError(raw, cfg)
	for _, secret := range []string{
		cfg.User.Token, cfg.User.Password, cfg.Notify.ServerChanKey,
		cfg.Notify.PushPlusToken, cfg.Notify.BarkURL, cfg.Notify.WecomWebhook,
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("脱敏后仍含明文 %q：%s", secret, got)
		}
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("脱敏结果应保留 *** 痕迹：%s", got)
	}
	if !strings.Contains(got, "上游返回 401") {
		t.Fatalf("脱敏不该吃掉错误本身的语义：%s", got)
	}

	// 超长原文截断，且不能切出半个汉字。
	long := strings.Repeat("错", maxErrorTextLen)
	truncated := env.srv.sanitizeError(long, cfg)
	if len(truncated) > maxErrorTextLen+32 {
		t.Fatalf("超长错误没有被截断：%d 字节", len(truncated))
	}
	if !strings.Contains(truncated, "已截断") {
		t.Fatalf("截断后应有提示：%q", truncated[maxInt(0, len(truncated)-20):])
	}
	if !utf8.ValidString(truncated) {
		t.Fatalf("截断结果不是合法 UTF-8：%q", truncated)
	}

	// 极短的"秘密"不该做替换（否则错误原文里的状态码/ID 会被打碎）。
	short := baseConfig()
	short.User.Password = "1234"
	if out := env.srv.sanitizeError("错误码 1234 表示参数缺失", short); !strings.Contains(out, "1234") {
		t.Fatalf("4 字符的短值不该被当成秘密替换：%q", out)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestDebugLevelKeepsStreamFlushable 验证"请求日志中间件包一层 ResponseWriter
// 之后 SSE 仍然能刷新"——这正是 statusRecorder 必须实现 Flush/Unwrap 的理由，
// 也是那种只在 debug 级别才会炸的隐性 bug。
func TestDebugLevelKeepsStreamFlushable(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.srv.SetLogLevel("debug")
	c := env.authedClient()

	env.srv.logf("info", "", "debug 模式下的快照日志")
	stream := env.openStream(c)

	frame := stream.next(t, 3*time.Second)
	if frame.name != eventLog {
		t.Fatalf("debug 模式下 SSE 第一帧仍应是 event: log，实际 %q", frame.name)
	}
	stream.close()

	// 请求日志确实写进了内存环（说明中间件走了带包装的分支）。
	waitValue(t, 2*time.Second, "请求日志出现", func() bool {
		for _, event := range env.store.Logs(200) {
			if event.Level == "debug" && strings.Contains(event.Msg, "GET /api/v1/") {
				return true
			}
		}
		return false
	})

	// 包装器仍要能被 ResponseController 穿透（SSE 清写超时依赖它）。
	rec := &statusRecorder{ResponseWriter: &nopResponseWriter{}, status: http.StatusOK}
	if rec.Unwrap() == nil {
		t.Fatalf("statusRecorder.Unwrap 不该返回 nil")
	}
	rec.Flush() // 底层不支持 Flusher 时也不能 panic
}

// nopResponseWriter 是最小的 ResponseWriter，用来验证包装器的兜底分支。
type nopResponseWriter struct{ header http.Header }

func (w *nopResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *nopResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nopResponseWriter) WriteHeader(int)             {}

// TestStoreAccessorAndErrorString 覆盖两个纯胶水方法（main 与日志会用）。
func TestStoreAccessorAndErrorString(t *testing.T) {
	env := newTestEnv(t)
	if env.srv.Store() != env.store {
		t.Fatalf("Store() 应返回装配时的同一个 store")
	}
	err := newError(http.StatusBadRequest, codeConfigInvalid, "配置不对", "细节1")
	if !strings.Contains(err.Error(), codeConfigInvalid) || !strings.Contains(err.Error(), "配置不对") {
		t.Fatalf("apiError.Error() 内容不符: %q", err.Error())
	}
	var nilErr *apiError
	if nilErr.Error() != "" {
		t.Fatalf("nil apiError 的 Error() 应为空串")
	}
}
