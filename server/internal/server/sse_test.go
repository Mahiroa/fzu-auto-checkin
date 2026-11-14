package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"fzucheckin/internal/model"
)

// TestSSESnapshotThenLiveEvents 覆盖验收清单第 9 条的前半：
// Content-Type 正确、先收到快照帧、event: log 格式合法、运行结束推 event: run。
func TestSSESnapshotThenLiveEvents(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	env.setNow(fixedDay(21, 40))
	c := env.authedClient()

	// 连接之前先造一条日志，它必须出现在快照帧里。
	env.srv.logf("info", "", "连接前的日志-%s", "snapshot")
	time.Sleep(10 * time.Millisecond)

	stream := env.openStream(c)
	if ct := stream.resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type 期望 text/event-stream，实际 %q", ct)
	}
	if cc := stream.resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("SSE Cache-Control 期望 no-cache，实际 %q", cc)
	}
	if xab := stream.resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Fatalf("SSE X-Accel-Buffering 期望 no，实际 %q", xab)
	}

	// 快照第一帧必须是合法的 event: log。
	frame := stream.next(t, 3*time.Second)
	if frame.name != eventLog {
		t.Fatalf("第一帧期望 event: log，实际 %q（data=%s）", frame.name, frame.data)
	}
	var event model.LogEvent
	if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
		t.Fatalf("SSE data 不是合法 LogEvent JSON: %v（%s）", err, frame.data)
	}
	if event.TS.IsZero() || event.Level == "" || event.Msg == "" {
		t.Fatalf("快照日志字段缺失: %+v", event)
	}

	// 触发一次运行，最终应收到 event: run。
	runID := env.acceptRun(c, map[string]any{"mode": "force"})
	var runFrame sseFrame
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if frame.name == "" {
			frame = stream.next(t, 3*time.Second)
			continue
		}
		if frame.name == eventRun {
			runFrame = frame
			break
		}
		frame = sseFrame{}
	}
	if runFrame.name != eventRun {
		t.Fatalf("没有收到 event: run（run_id=%s）", runID)
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Slot   string `json:"slot"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(runFrame.data), &payload); err != nil {
		t.Fatalf("event: run 的 data 不是合法 JSON: %v（%s）", err, runFrame.data)
	}
	if payload.ID != runID || payload.Status != model.StatusSuccess {
		t.Fatalf("event: run 内容不符: %+v（期望 id=%s）", payload, runID)
	}
	env.waitRun(runID, 5*time.Second)
}

// TestSSEConnectionLimit 覆盖验收清单第 9 条的后半：第 17 个连接 503。
func TestSSEConnectionLimit(t *testing.T) {
	env := newTestEnv(t)
	env.saveConfig(baseConfig())
	c := env.authedClient()

	streams := make([]*sseStream, 0, maxSSEClients)
	for i := 0; i < maxSSEClients; i++ {
		streams = append(streams, env.openStream(c))
	}
	env.waitHubClients(maxSSEClients, 3*time.Second)

	res := env.do(c, http.MethodGet, apiPrefix+"/logs/stream", nil)
	env.expectStatus(res, http.StatusServiceUnavailable, "第 17 条 SSE 连接")
	if code := errCode(t, res.body); code != codeSSELimit {
		t.Fatalf("错误码期望 %s，实际 %s", codeSSELimit, code)
	}

	// 断开全部连接后计数必须回落（否则 fd 会随着刷新页面缓慢泄漏）。
	for _, stream := range streams {
		stream.close()
	}
	env.waitHubClients(0, 5*time.Second)

	// 回落之后可以重新连上。
	again := env.openStream(c)
	if frame := again.next(t, 3*time.Second); frame.name != eventLog {
		t.Fatalf("重连后期望先收到快照帧，实际 %q", frame.name)
	}
	again.close()
	env.waitHubClients(0, 5*time.Second)
}
