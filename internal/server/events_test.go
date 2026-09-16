package server

import (
	"sort"
	"testing"
	"time"
)

// 等某个主题出现，最多等 timeout。
func waitTopic(t *testing.T, sub *subscriber, topic string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-sub.wake:
			for _, got := range sub.drain() {
				if got == topic {
					return true
				}
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false
}

func TestHubNotifyReachesSubscriber(t *testing.T) {
	h := newEventHub()
	sub, cancel := h.subscribe()
	defer cancel()

	if h.count() != 1 {
		t.Fatalf("订阅后 count = %d，期望 1", h.count())
	}

	h.notify(topicFiles)

	if !waitTopic(t, sub, topicFiles, time.Second) {
		t.Fatal("notify 之后一秒内没收到 files 主题")
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := newEventHub()
	sub, cancel := h.subscribe()
	cancel()

	if h.count() != 0 {
		t.Fatalf("注销后 count = %d，期望 0", h.count())
	}

	h.notify(topicFiles)

	select {
	case <-sub.wake:
		t.Fatal("注销后仍然收到了唤醒")
	case <-time.After(100 * time.Millisecond):
		// 预期：什么也收不到
	}
}

// 这是整个广播中心最要紧的一条性质：**一个不读消息的订阅者不能阻塞 notify**。
//
// 前端卡住 / 网络断开时，它的通道就没人消费。如果 notify 用阻塞发送，
// 上传流程会在这里被拖死——表现是"某个手机断开连接之后，所有人都传不了文件"，
// 而且很难联想到是推送机制引起的。所以 notify 必须是非阻塞的。
func TestHubNotifyNeverBlocks(t *testing.T) {
	h := newEventHub()
	_, cancel := h.subscribe() // 故意不读
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.notify(topicFiles)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify 被不读消息的订阅者阻塞了")
	}
}

// 同一个主题通知多次会被合并成一次。
//
// 通知是"有事发生"的信号而不是数据，积压多次没有意义——前端收到任何一次
// 都会重新拉一遍完整列表。合并掉可以避免一次传多个文件时把前端刷爆。
func TestHubSameTopicCoalesces(t *testing.T) {
	h := newEventHub()
	sub, cancel := h.subscribe()
	defer cancel()

	h.notify(topicFiles)
	h.notify(topicFiles)
	h.notify(topicFiles)

	select {
	case <-sub.wake:
	case <-time.After(time.Second):
		t.Fatal("没收到唤醒")
	}
	got := sub.drain()
	if len(got) != 1 || got[0] != topicFiles {
		t.Fatalf("三次同主题通知 drain 出 %v，期望只有 [files]", got)
	}

	// 不该再有第二次唤醒
	select {
	case <-sub.wake:
		t.Fatal("同主题被合并后仍然产生了第二次唤醒")
	case <-time.After(100 * time.Millisecond):
	}
}

// **不同主题绝不能被合并掉。**
//
// 这条是引入主题机制的原因本身：早先的版本用 `chan struct{}` 当信号，
// 缓冲只有 1，files 和 texts 同时变化时会合成一个信号，
// 前端只刷新其中一个，另一个要等下次变更才更新——偶发、难复现。
func TestHubDifferentTopicsNotMerged(t *testing.T) {
	h := newEventHub()
	sub, cancel := h.subscribe()
	defer cancel()

	// 连着来，制造"同一次唤醒里带了两个主题"的情形
	h.notify(topicFiles)
	h.notify(topicTexts)

	var got []string
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case <-sub.wake:
			got = append(got, sub.drain()...)
		case <-time.After(20 * time.Millisecond):
		}
	}

	sort.Strings(got)
	if len(got) != 2 || got[0] != topicFiles || got[1] != topicTexts {
		t.Fatalf("drain 出 %v，期望 files 和 texts 都在", got)
	}
}

// 多订阅者各自收到，且互不影响。
func TestHubFansOutToAllSubscribers(t *testing.T) {
	h := newEventHub()
	const n = 5
	subs := make([]*subscriber, 0, n)
	for i := 0; i < n; i++ {
		sub, cancel := h.subscribe()
		defer cancel()
		subs = append(subs, sub)
	}

	if h.count() != n {
		t.Fatalf("count = %d，期望 %d", h.count(), n)
	}

	h.notify(topicTexts)

	for i, sub := range subs {
		if !waitTopic(t, sub, topicTexts, time.Second) {
			t.Fatalf("第 %d 个订阅者没收到通知", i)
		}
	}
}

// 并发订阅 / 注销 / 通知不能触发 data race 或 panic。
// （注意：本机没有 C 编译器，`-race` 跑不了，这条只是裸压一遍。）
func TestHubConcurrentUse(t *testing.T) {
	h := newEventHub()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			sub, cancel := h.subscribe()
			h.notify(topicFiles)
			_ = sub.drain()
			cancel()
		}
	}()

	for i := 0; i < 200; i++ {
		h.notify(topicTexts)
		h.count()
	}
	<-done
}
