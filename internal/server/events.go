package server

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// 通知主题。前端按主题决定要重新拉什么。
const (
	topicFiles = "files" // 文件列表变了
	topicTexts = "texts" // 共享文本 / 设备备注变了
)

// subscriber 是一个 SSE 订阅者。
//
// 设计要点：**按主题记一个 dirty 集合，而不是往通道里塞主题字符串**。
// 早先的版本是 `chan struct{}`（只有"有事发生"一个信号），加第二个主题时
// 就不能这么写了：通道缓冲是 1，files 和 texts 同时变化时会合并成一个信号，
// 前端只刷新其中一个，另一个要等到下次变更才更新——偶发、难复现。
// 记 dirty 集合既能按主题去重（同一主题变十次也只通知一次），
// 又不会把不同主题合并掉。
type subscriber struct {
	mu    sync.Mutex
	dirty map[string]bool
	// wake 带 1 个缓冲：它只表示"有 dirty 待取"，重复唤醒没有意义。
	wake chan struct{}
}

func newSubscriber() *subscriber {
	return &subscriber{dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func (sub *subscriber) mark(topic string) {
	sub.mu.Lock()
	sub.dirty[topic] = true
	sub.mu.Unlock()

	// 非阻塞：槽位已被占用说明已经欠着一次唤醒了，再多也没用
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

// drain 取走并清空当前积压的主题。
func (sub *subscriber) drain() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(sub.dirty))
	for t := range sub.dirty {
		out = append(out, t)
	}
	sub.dirty = make(map[string]bool, len(out))
	return out
}

// eventHub 是变更通知的广播中心。
type eventHub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[*subscriber]struct{})}
}

// subscribe 注册一个订阅者，返回它和注销函数。
func (h *eventHub) subscribe() (*subscriber, func()) {
	sub := newSubscriber()
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub, func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}
}

// notify 通知所有订阅者"topic 变了"。
//
// 全程非阻塞：订阅者（前端）处理不过来时只是把主题记进它的 dirty 集合，
// 不会在这里等。这是刻意的——通知只是"去重新拉列表"的提示，
// 而如果在这里阻塞，一个卡住不读的客户端就能把上传流程整个拖住。
func (h *eventHub) notify(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.mark(topic)
	}
}

func (h *eventHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// sseHeartbeat 是心跳间隔。
//
// 没有心跳的话，连接会被中间设备（路由器 NAT 表、反向代理）当成空闲连接掐掉，
// 而 TCP 层面断开后前端可能要过很久才发现——表现为"用一会儿就不自动刷新了"，
// 且刷新页面就恢复正常，很难查。定期发一行注释把连接焐热。
const sseHeartbeat = 25 * time.Second

// handleEvents 是 SSE 长连接：把变更推给浏览器。
//
// 用 SSE 而不是 WebSocket：这里只需要服务端单向通知，SSE 走的是普通 HTTP，
// 不需要握手升级、不需要额外依赖，断线后前端自己重连即可。
// 用 fetch + ReadableStream 而不是 EventSource：EventSource 没法带自定义请求头，
// 而本项目的鉴权走 X-Admin-Token 头，开访问口令时 EventSource 根本连不上。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前服务不支持流式响应", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx 默认会缓冲上游响应，缓冲住 SSE 就等于永不推送。
	// 这个头是 Nginx 认的约定，让它对这条响应放行。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub, cancel := s.events.subscribe()
	defer cancel()

	// 先发一个 ready，让前端确认通道真的通了（否则只能靠"一直没消息"来判断，
	// 而"没消息"和"连上了但恰好没变更"区分不开）。
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// 客户端断开（关页面、切网络）。这是正常结束，不是错误。
			return
		case <-sub.wake:
			// 先取再写：写的过程中新来的通知会重新置脏并再次唤醒，
			// 不会因为"正在写"而丢掉。
			for _, topic := range sub.drain() {
				if _, err := io.WriteString(w, "event: "+topic+"\ndata: {}\n\n"); err != nil {
					return
				}
			}
			flusher.Flush()
		case <-ticker.C:
			// 以注释行做心跳，前端会忽略它
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
