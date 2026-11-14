package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// SSE 常量（§9.2 / §G）。
const (
	eventLog = "log"
	eventRun = "run"

	// maxSSEClients 是并发连接上限。每个连接占一个 goroutine + 一条 TCP，
	// 无上限就等于把 fd 交到前端手里；16 个足够（仪表盘 + 日志页 + 几个标签页）。
	maxSSEClients = 16
	// sseClientBuffer 是每个连接的发送缓冲。慢消费者超过它就丢帧而不是阻塞广播：
	// 一次广播要走完全部连接，任何一个卡住的浏览器都会把签到运行本身拖慢。
	sseClientBuffer = 32
	// ssePingInterval 是保活间隔。反向代理常在 60s 空闲后掐连接，20s 留足余量。
	ssePingInterval = 20 * time.Second
	// sseSnapshotLogs 是连上后补发的历史条数，够看出"刚发生了什么"。
	sseSnapshotLogs = 50
)

// sseEvent 是一条待发送的 SSE 帧。
type sseEvent struct {
	name string
	data []byte
}

// sseClient 是一条连接的发送队列。
type sseClient struct {
	events chan sseEvent
}

// sseHub 是日志/运行事件的扇出点。
//
// 广播路径上不允许任何阻塞操作：它被 flow 的每一条日志调用，一旦这里等待慢客户端，
// 一次签到就会因为某个断网的浏览器而变慢。所以只做"非阻塞投递 + 失败计数"。
type sseHub struct {
	mu       sync.Mutex
	clients  map[*sseClient]struct{}
	max      int
	dropped  uint64
	lastDrop time.Time
	sent     uint64
}

func newSSEHub(max int) *sseHub {
	if max <= 0 {
		max = maxSSEClients
	}
	return &sseHub{clients: make(map[*sseClient]struct{}, max), max: max}
}

// subscribe 注册一个连接；超出上限返回 false（由 handler 回 503 sse_limit）。
func (h *sseHub) subscribe() (*sseClient, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.max {
		return nil, false
	}
	c := &sseClient{events: make(chan sseEvent, sseClientBuffer)}
	h.clients[c] = struct{}{}
	return c, true
}

func (h *sseHub) unsubscribe(c *sseClient) {
	if c == nil {
		return
	}
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// broadcast 把一条事件发给所有连接。payload 序列化一次后共享给所有连接
// （每条连接各自 marshal 是纯浪费，而且大日志环下会变成 O(连接数) 的分配）。
func (h *sseHub) broadcast(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		// 序列化失败是内部错误，但绝不能让它冒泡到签到流程：日志发不出去
		// 不该影响打卡结论。这里静默丢弃这一帧。
		return
	}
	ev := sseEvent{name: name, data: data}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent++
	for c := range h.clients {
		select {
		case c.events <- ev:
		default:
			// 慢消费者：丢掉这一条并计数。丢弃是有损的，但"日志少一行"远好于
			// "签到被日志拖住"，而且丢弃数量可在 /health 的诊断里看到。
			h.dropped++
			h.lastDrop = time.Now()
		}
	}
}

// stats 返回 (当前连接数, 累计丢弃数)，只用于诊断。
func (h *sseHub) stats() (int, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients), h.dropped
}

// count 返回当前连接数（测试用）。
func (h *sseHub) count() int {
	n, _ := h.stats()
	return n
}

// handleLogStream 是 GET /api/v1/logs/stream（§9.2 / §G）。
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, newError(http.StatusInternalServerError, codeInternal,
			"当前连接不支持流式输出（缺少 http.Flusher）"))
		return
	}
	client, ok := s.hub.subscribe()
	if !ok {
		writeError(w, newError(http.StatusServiceUnavailable, codeSSELimit,
			fmt.Sprintf("实时日志连接数已达上限 %d，请关闭多余的标签页后重试", maxSSEClients)))
		return
	}
	defer s.hub.unsubscribe(client)

	// 必须解除本连接的写超时：http.Server.WriteTimeout=30s 会在 30 秒后掐断
	// 任何还没写完的响应，而 SSE 的正常生命周期是"几个小时"。SetWriteDeadline
	// 传零值 = 取消截止时间。httptest 的 ResponseWriter 不支持时返回
	// ErrNotSupported，忽略即可（测试里也没有 30s 的硬超时）。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	// Connection 在 HTTP/2 里是非法头，但这套服务默认是 HTTP/1.1 明文；
	// 写上它是为了让 nginx 之类的反代不要把连接当成可复用的普通响应缓冲起来。
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 先补发内存环快照：新打开的页面不该从"下一条日志"开始，否则它看不到
	// 刚刚失败的那次运行。store.Logs 返回的新的在前，这里反序成时间正序，
	// 前端才能按顺序 append。
	snapshot := s.store.Logs(sseSnapshotLogs)
	for i := len(snapshot) - 1; i >= 0; i-- {
		if err := writeSSEFrame(w, eventLog, snapshot[i]); err != nil {
			return
		}
	}
	flusher.Flush()

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			// 客户端断开（关标签页/断网）。这里必须返回，让 defer 注销连接，
			// 否则 hub 会一直往一个没人读的 channel 里丢帧，直到上限被占满。
			return
		case ev := <-client.events:
			if _, err := writeSSERaw(w, ev.name, ev.data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			// 注释帧（以 ':' 开头）不会触发任何事件监听器，只用来把连接撑住。
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEFrame 把任意值序列化成一帧。
func writeSSEFrame(w io.Writer, name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = writeSSERaw(w, name, data)
	return err
}

// writeSSERaw 写一帧 event/data。data 必须是单行（JSON 序列化天然满足）。
func writeSSERaw(w io.Writer, name string, data []byte) (int, error) {
	var buf bytes.Buffer
	buf.Grow(len(data) + len(name) + 16)
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\n")
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return w.Write(buf.Bytes())
}
