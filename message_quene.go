package redisocket

// defaultMessageWorkers 是處理 inbound 訊息的 worker goroutine 數。
// TODO(Phase E/F):改為可設定 + 可停止(graceful shutdown)。
const defaultMessageWorkers = 1024

type messageQuene struct {
	serveChan      chan *buffer
	freeBufferChan chan *buffer
	pool           *pool
}

func (m *messageQuene) worker() {
	for b := range m.serveChan {
		m.serve(b)
	}
}
func (m *messageQuene) run() {
	for i := 0; i < defaultMessageWorkers; i++ {
		go m.worker()
	}
}

func (m *messageQuene) serve(buffer *buffer) {
	receiveMsg, err := buffer.client.re(buffer.buffer.Bytes())
	if err == nil {
		byteCount := len(receiveMsg)
		if byteCount > 0 {
			m.pool.toSid(buffer.client.sid, receiveMsg)
		}
	} else {
		m.pool.kickSid(buffer.client.sid)
	}
	buffer.reset(nil)
	select {
	case m.freeBufferChan <- buffer:
	default:
	}
	return
}
