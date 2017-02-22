package redisocket

import (
	"time"

	"github.com/garyburd/redigo/redis"
)

type eventPayload struct {
	payload *Payload
	event   string
}

type Pool struct {
	users     map[*Client]bool
	broadcast chan *eventPayload
	join      chan *Client
	leave     chan *Client
	rpool     *redis.Pool
}

func (h *Pool) Run() {
	t := time.NewTicker(1 * time.Minute)
	defer func() {
		t.Stop()
	}()
	for {
		select {
		case p := <-h.broadcast:
			for u, _ := range h.users {
				u.Trigger(p.event, p.payload)
			}

		case u := <-h.join:
			h.users[u] = true

		case u := <-h.leave:
			if _, ok := h.users[u]; ok {
				close(u.send)
				delete(h.users, u)
			}
		case <-t.C:
			conn := h.rpool.Get()
			conn.Send("MULTI")
			for u, _ := range h.users {
				if u.uid != "" {
					conn.Send("SADD", a.ChannelPrefix+"online", u.uid)
					conn.Send("EXPIRE", a.ChannelPrefix+"online", 2*60)
				}
				for e, _ := range u.events {
					conn.Send("SADD", a.ChannelPrefix+"channels", key)
					conn.Send("EXPIRE", a.ChannelPrefix+"channels", 2*60)
				}
			}
			conn.Do("EXEC")
			conn.Close()
		}

	}
}
func (a *Pool) Broadcast(event string, p *Payload) {
	a.broadcast <- &eventPayload{p, event}
}
func (a *Pool) Join(c *Client) {
	a.join <- c
}
func (a *Pool) Leave(c *Client) {
	a.leave <- c
}
