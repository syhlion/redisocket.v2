package redisocket

import (
	"errors"
	"sync/atomic"
	"time"
)

type eventPayload struct {
	payload *Payload
	event   string
}

// pool 用
type uPayload struct {
	uid  string
	data []byte
}
type uReloadChannelPayload struct {
	uid      string
	channels []string
}
type uAddChannelPayload struct {
	uid     string
	channel string
}
type sPayload struct {
	sid  string
	data []byte
}

type pool struct {
	users              map[*Client]bool
	broadcastChan      chan *eventPayload
	joinChan           chan *Client
	leaveChan          chan *Client
	shutdownChan       chan int
	kickUidChan        chan string
	kickSidChan        chan string
	uPayloadChan       chan *uPayload
	uReloadChannelChan chan *uReloadChannelPayload
	uAddChannelChan    chan *uAddChannelPayload
	sPayloadChan       chan *sPayload
	presence           Presence
	channelPrefix      string
	scanInterval       time.Duration
	msgTotal           int64
	msgByteSum         int64
	stat               *Statistic
	// userCount 為 users map 大小的 atomic 鏡像,供 pool goroutine 外安全讀取
	// (CountOnlineUsers)。只在 pool goroutine 內 join/leave 時更新。
	userCount int64
}

// onlineCount 回傳目前在線連線數,可在 pool goroutine 外安全呼叫。
func (h *pool) onlineCount() int {
	return int(atomic.LoadInt64(&h.userCount))
}

func (h *pool) run() <-chan error {
	errChan := make(chan error, 1)
	go func() {
		t := time.NewTicker(h.scanInterval)
		defer func() {
			t.Stop()
			err := errors.New("pool close")
			errChan <- err
		}()
		for {
			select {
			case p := <-h.broadcastChan:
				for u := range h.users {
					u.Trigger(p.event, p.payload)
				}
			case <-h.shutdownChan:
				for u := range h.users {
					u.Close()
				}
			case n := <-h.kickSidChan:
				for u := range h.users {
					if u.sid == n {
						u.Close()
					}
				}
			case n := <-h.uReloadChannelChan:
				for u := range h.users {
					if u.uid == n.uid {

						u.SetChannels(n.channels)

					}
				}
			case n := <-h.uAddChannelChan:
				for u := range h.users {
					if u.uid == n.uid {

						u.AddChannel(n.channel)

					}
				}
			case s := <-h.kickUidChan:
				for u := range h.users {
					if u.uid == s {
						u.Close()
					}
				}
			case n := <-h.uPayloadChan:
				for u := range h.users {
					if u.uid == n.uid {

						u.Send(n.data)
					}
				}
			case n := <-h.sPayloadChan:
				for u := range h.users {
					if u.sid == n.sid {
						u.Send(n.data)
					}
				}
			case u := <-h.joinChan:
				h.stat.AddMem()
				h.users[u] = true
				atomic.AddInt64(&h.userCount, 1)
			case u := <-h.leaveChan:
				if _, ok := h.users[u]; ok {
					h.stat.SubMem()
					close(u.send)
					delete(h.users, u)
					atomic.AddInt64(&h.userCount, -1)
				}
			case <-t.C:
				h.syncOnline()

			}

		}
	}()
	return errChan
}
func (h *pool) toUid(uid string, d []byte) {
	u := &uPayload{uid: uid, data: d}
	h.uPayloadChan <- u
}
func (h *pool) reloadUidChannels(uid string, channels []string) {
	u := &uReloadChannelPayload{uid: uid, channels: channels}
	h.uReloadChannelChan <- u
}
func (h *pool) addUidChannels(uid string, channel string) {
	u := &uAddChannelPayload{uid: uid, channel: channel}
	h.uAddChannelChan <- u
}
func (h *pool) toSid(sid string, d []byte) {
	u := &sPayload{sid: sid, data: d}
	h.sPayloadChan <- u
}
func (h *pool) shutdown() {
	h.shutdownChan <- 1
}
func (h *pool) kickUid(uid string) {
	h.kickUidChan <- uid
}
func (h *pool) kickSid(sid string) {
	h.kickSidChan <- sid
}
func (h *pool) syncOnline() (err error) {
	// 在 pool goroutine 內快照所有成員(讀 u.events 需 u.RLock),再交給 presence 批次處理。
	members := make([]PresenceMember, 0, len(h.users))
	for u := range h.users {
		u.RLock()
		chs := make([]string, 0, len(u.events))
		for e := range u.events {
			chs = append(chs, e)
		}
		u.RUnlock()
		members = append(members, PresenceMember{AppKey: u.prefix, Uid: u.uid, Channels: chs})
	}
	return h.presence.Sync(h.channelPrefix, members)
}
func (h *pool) broadcast(event string, p *Payload) {
	h.broadcastChan <- &eventPayload{p, event}
}
func (h *pool) join(c *Client) {
	h.joinChan <- c
}
func (h *pool) leave(c *Client) {
	h.leaveChan <- c
}
