package redisocket

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/gorilla/websocket"
)

type WebsocketOptional struct {
	WriteWait      time.Duration
	PongWait       time.Duration
	PingPeriod     time.Duration
	MaxMessageSize int64
	Upgrader       websocket.Upgrader
}

var (
	DefaultWebsocketOptional = WebsocketOptional{
		WriteWait:      10 * time.Second,
		PongWait:       60 * time.Second,
		PingPeriod:     (60 * time.Second * 9) / 10,
		MaxMessageSize: 512,
		Upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
)

var APPCLOSE = errors.New("APP_CLOSE")

type EventHandler func(event string, b []byte) ([]byte, error)

type ReceiveMsgHandler func([]byte) error

//NewApp It's create a Hub
func NewHub(p *redis.Pool) (e *Hub) {

	return &Hub{

		Config:      DefaultWebsocketOptional,
		rpool:       p,
		psc:         &redis.PubSubConn{p.Get()},
		RWMutex:     new(sync.RWMutex),
		subjects:    make(map[string]map[*Client]bool),
		subscribers: make(map[*Client]map[string]bool),
		closeSign:   make(chan int),
		closeflag:   false,
	}

}
func (e *Hub) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (c *Client, err error) {
	ws, err := e.Config.Upgrader.Upgrade(w, r, responseHeader)
	c = &Client{
		ws:      ws,
		send:    make(chan []byte, 4096),
		RWMutex: new(sync.RWMutex),
		hub:     e,
		events:  make(map[string]EventHandler),
	}
	return
}

type Hub struct {
	Config      WebsocketOptional
	psc         *redis.PubSubConn
	rpool       *redis.Pool
	subjects    map[string]map[*Client]bool
	subscribers map[*Client]map[string]bool
	closeSign   chan int
	closeflag   bool
	*sync.RWMutex
}

func (a *Hub) Ping() (err error) {
	_, err = a.rpool.Get().Do("PING")
	if err != nil {
		return
	}
	return
}

func (a *Hub) register(event string, c *Client) (err error) {
	a.Lock()

	defer a.Unlock()
	//observer map
	if m, ok := a.subscribers[c]; !ok {
		events := make(map[string]bool)
		events[event] = true
		a.subscribers[c] = events
	} else {
		m[event] = true
	}

	//event map
	if _, ok := a.subjects[event]; !ok {
		clients := make(map[*Client]bool)
		clients[c] = true
		a.subjects[event] = clients
	}
	return
}

func (a *Hub) unregister(event string, c *Client) (err error) {
	a.Lock()
	defer a.Unlock()

	//observer map
	if m, ok := a.subscribers[c]; ok {
		delete(m, event)
		if len(m) == 0 {
			delete(a.subscribers, c)
		}
	}
	//event map
	if m, ok := a.subjects[event]; ok {
		delete(m, c)
		if len(m) == 0 {
			/*
				err = a.psc.Unsubscribe(event)
				if err != nil {
					log.Println(err)
					return
				}*/
			delete(a.subjects, event)
		}
	}

	return
}

func (a *Hub) UnregisterAll(c *Client) {
	if m, ok := a.subscribers[c]; ok {
		for e, _ := range m {
			a.unregister(e, c)
		}
	}
	a.Lock()
	delete(a.subscribers, c)
	a.Unlock()
	return
}
func (a *Hub) listenRedis() <-chan error {

	errChan := make(chan error, 1)
	go func() {
		for {
			switch v := a.psc.Receive().(type) {
			case redis.PMessage:
				a.RLock()
				clients, ok := a.subjects[v.Channel]
				a.RUnlock()
				if !ok {
					continue
				}
				for c, _ := range clients {
					c.Trigger(v.Channel, v.Data)
				}

			case error:
				errChan <- v

				break
			}
		}
	}()
	return errChan
}

func (a *Hub) close() {
	a.closeflag = true
	for c, _ := range a.subscribers {
		c.Close()
	}
}
func (a *Hub) Listen() error {
	a.psc.PSubscribe("*")
	redisErr := a.listenRedis()
	select {
	case e := <-redisErr:
		a.close()
		return e
	case <-a.closeSign:
		a.close()
		return APPCLOSE

	}
}
func (a *Hub) Close() {
	if !a.closeflag {
		a.closeSign <- 1
		close(a.closeSign)
	}
	return

}

func (e *Hub) Publish(event string, data []byte) (val int, err error) {

	conn := e.rpool.Get()
	defer conn.Close()
	val, err = redis.Int(conn.Do("PUBLISH", event, data))
	err = e.rpool.Get().Flush()
	return
}
