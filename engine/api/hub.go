package api

// hub.go —— SSE 订阅/广播中心（M3-W1）。
//
// 语义：
//   - 每订阅一个 buffered chan（容量 64）；Broadcast 非阻塞投递，
//     满则踢线（close）——慢消费者不拖累全局，重连由客户端负责。
//   - 连接上限 maxSubs（超出 ErrTooMany），防本机进程耗尽资源。

import (
	"encoding/json"
	"fmt"
	"sync"
)

const (
	subBuf         = 64
	defaultMaxSubs = 8
)

// ErrTooManySubs SSE 连接数达上限（→503）。
var ErrTooManySubs = fmt.Errorf("sse: too many subscribers")

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
	max  int
}

func newHub(max int) *hub {
	if max <= 0 {
		max = defaultMaxSubs
	}
	return &hub{subs: map[chan []byte]struct{}{}, max: max}
}

// count 当前订阅数（测试/观测）。
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// subscribe 注册订阅；返回的 chan 关闭即代表被踢（应终止响应）。
func (h *hub) subscribe() (chan []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.max {
		return nil, ErrTooManySubs
	}
	ch := make(chan []byte, subBuf)
	h.subs[ch] = struct{}{}
	return ch, nil
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch) // 幂等：只关注册中的；handler 收到 closed 即退出
	}
}

// broadcast 组帧并投递全部订阅者；写不进（满）= 踢线。
func (h *hub) broadcast(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	frame := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, b)

	h.mu.Lock()
	var kick []chan []byte
	for ch := range h.subs {
		select {
		case ch <- frame:
		default:
			kick = append(kick, ch)
		}
	}
	for _, ch := range kick {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// sendTo 单订阅者直发（满 = 踢）。用于新连接的首帧 status。
func (h *hub) sendTo(ch chan []byte, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	frame := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, b)
	select {
	case ch <- frame:
	default:
		h.unsubscribe(ch)
	}
}
