package redisocket

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
)

var conn redis.Conn
var APPCLOSE = errors.New("APP_CLOSE")
var Upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type EventHandler func(event string, b []byte) ([]byte, error)

type ReceiveMsgHandler func([]byte) error

type App interface {
	// It client's Producer
	NewClient(w http.ResponseWriter, r *http.Request) (*Client, error)

	//It can notify All subscriber
	Notify(subject string, data []byte) (int, error)

	//A subscriber can cancel all subscriptions
	UnsubscribeAll(c *Client)

	//App start listen. It's blocked
	Listen() error

	//List Redis All Subject
	ListSubject() ([]string, error)

	//Count Redis Subject's subscribers
	NumSubscriber(subject string) (int, error)

	//App Close
	Close()
}

//NewApp It's create a App
func NewApp(p *redis.Pool) (e App, err error) {
	_, err = p.Get().Do("PING")
	if err != nil {
		return
	}
	e = &app{

		rpool:       p,
		psc:         &redis.PubSubConn{p.Get()},
		RWMutex:     new(sync.RWMutex),
		subjects:    make(map[string]map[*Client]bool),
		subscribers: make(map[*Client]map[string]bool),
		closeSign:   make(chan int),
		closeflag:   false,
	}

	return
}
func (e *app) NewClient(w http.ResponseWriter, r *http.Request) (c *Client, err error) {
	ws, err := Upgrader.Upgrade(w, r, nil)
	c = &Client{
		ws:      ws,
		send:    make(chan []byte, 4096),
		RWMutex: new(sync.RWMutex),
		app:     e,
		events:  make(map[string]EventHandler),
	}
	return
}

type app struct {
	psc         *redis.PubSubConn
	rpool       *redis.Pool
	subjects    map[string]map[*Client]bool
	subscribers map[*Client]map[string]bool
	closeSign   chan int
	closeflag   bool
	*sync.RWMutex
}

func (a *app) subscribe(event string, c *Client) (err error) {
	a.Lock()

	defer a.Unlock()
	//observer map
	if _, ok := a.subscribers[c]; !ok {
		events := make(map[string]bool)
		events[event] = true
		a.subscribers[c] = events
	}

	//event map
	if _, ok := a.subjects[event]; !ok {
		clients := make(map[*Client]bool)
		clients[c] = true
		a.subjects[event] = clients
		err = a.psc.Subscribe(event)
		if err != nil {
			return
		}
	}
	return
}
func (a *app) unsubscribe(event string, c *Client) (err error) {
	a.Lock()
	defer a.Unlock()

	//observer map
	if m, ok := a.subscribers[c]; ok {
		delete(m, event)
	}
	//event map
	if m, ok := a.subjects[event]; ok {
		delete(m, c)
		if len(m) == 0 {

			err = a.psc.Unsubscribe(event)
			if err != nil {
				log.Println(err)
				return
			}
			delete(a.subjects, event)
		}
	}

	return
}
func (a *app) UnsubscribeAll(c *Client) {
	if m, ok := a.subscribers[c]; ok {
		for e, _ := range m {
			a.unsubscribe(e, c)
		}
	}
	a.Lock()
	delete(a.subscribers, c)
	a.Unlock()
	return
}
func (a *app) listenRedis() <-chan error {

	errChan := make(chan error, 1)
	go func() {
		for {
			switch v := a.psc.Receive().(type) {
			case redis.Message:
				a.RLock()
				clients := a.subjects[v.Channel]
				a.RUnlock()
				for c, _ := range clients {
					c.trigger(v.Channel, v.Data)
				}

			case error:
				errChan <- v

				break
			}
		}
	}()
	return errChan
}

func (a *app) close() {
	a.closeflag = true
	for c, _ := range a.subscribers {
		c.Close()
	}
}
func (a *app) Listen() error {
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
func (a *app) ListSubject() (subs []string, err error) {
	reply, err := redis.Values(a.rpool.Get().Do("PUBSUB", "CHANNELS"))
	if err != nil {
		return
	}
	if len(reply) == 0 {
		return
	}
	if err = redis.ScanSlice(reply, &subs); err != nil {
		return
	}
	return
}
func (a *app) NumSubscriber(subject string) (c int, err error) {
	reply, err := redis.Values(a.rpool.Get().Do("PUBSUB", "NUMSUB", subject))
	if err != nil {
		return
	}
	if len(reply) == 0 {
		return 0, nil
	}
	var channel string
	var count int
	if _, err = redis.Scan(reply, &channel, &count); err != nil {
		return
	}
	return
}
func (a *app) Close() {
	if !a.closeflag {
		a.closeSign <- 1
		close(a.closeSign)
	}
	return

}

func (e *app) Notify(event string, data []byte) (val int, err error) {

	conn := e.rpool.Get()
	defer conn.Close()
	val, err = redis.Int(conn.Do("PUBLISH", event, data))
	err = e.rpool.Get().Flush()
	return
}
