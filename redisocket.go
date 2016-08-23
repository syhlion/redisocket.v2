package redisocket

import (
	"errors"
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

type EventHandler func([]byte) ([]byte, error)

type ReceiveMsgHandler func([]byte) error

//Subscriber
type Subscriber interface {
	//A Subscriber can subscribe subject
	Subscribe(event string, h EventHandler) error

	//A Subscriber can  unsubscribe subject
	Unsubscribe(event string) error

	//Clients start listen. It's blocked
	Listen(ReceiveMsgHandler) error

	//Close clients connection
	Close()

	//Subscriber can trigger event
	Trigger(event string, data []byte) (err error)
}

type App interface {
	// It client's Producer
	NewClient(w http.ResponseWriter, r *http.Request) (Subscriber, error)

	//It can notify All subscriber
	Notify(subject string, data []byte) (int, error)

	//A subscriber can cancel all subscriptions
	UnsubscribeAll(c Subscriber)

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
func NewApp(p *redis.Pool) App {
	e := &app{

		rpool:       p,
		psc:         &redis.PubSubConn{p.Get()},
		RWMutex:     new(sync.RWMutex),
		subjects:    make(map[string]map[Subscriber]bool),
		subscribers: make(map[Subscriber]map[string]bool),
		closeSign:   make(chan int),
	}

	return e
}
func (e *app) NewClient(w http.ResponseWriter, r *http.Request) (c Subscriber, err error) {
	ws, err := Upgrader.Upgrade(w, r, nil)
	c = &client{
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
	subjects    map[string]map[Subscriber]bool
	subscribers map[Subscriber]map[string]bool
	closeSign   chan int
	*sync.RWMutex
}

func (a *app) subscribe(event string, c Subscriber) (err error) {
	a.Lock()

	defer a.Unlock()
	//observer map
	if m, ok := a.subscribers[c]; ok {
		m[event] = true
	} else {
		events := make(map[string]bool)
		events[event] = true
		a.subscribers[c] = events
	}

	//event map
	if m, ok := a.subjects[event]; ok {
		m[c] = true
	} else {
		clients := make(map[Subscriber]bool)
		clients[c] = true
		a.subjects[event] = clients
		err = a.psc.Subscribe(event)
		if err != nil {
			return
		}
	}
	return
}
func (a *app) unsubscribe(event string, c Subscriber) (err error) {
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
				return
			}
		}
	}

	return
}
func (a *app) UnsubscribeAll(c Subscriber) {
	a.Lock()
	conn := a.rpool.Get()
	defer func() {
		a.Unlock()
		conn.Close()
	}()
	if m, ok := a.subscribers[c]; ok {
		for e, _ := range m {
			delete(a.subjects[e], c)
			if len(a.subjects[e]) == 0 {
				conn.Do("UNSUBSCRIBE", e)
			}
		}
		delete(a.subscribers, c)
	}
	return
}
func (a *app) listenRedis() <-chan error {

	errChan := make(chan error)
	go func() {
		for {
			switch v := a.psc.Receive().(type) {
			case redis.Message:
				a.RLock()
				clients := a.subjects[v.Channel]
				a.RUnlock()
				for c, _ := range clients {
					c.Trigger(v.Channel, v.Data)
				}

			case error:
				errChan <- v

				for c, _ := range a.subscribers {
					a.UnsubscribeAll(c)
					c.Close()
				}
				break
			}
		}
	}()
	return errChan
}
func (a *app) Listen() error {
	redisErr := a.listenRedis()
	select {
	case e := <-redisErr:
		close(a.closeSign)
		return e
	case <-a.closeSign:
		close(a.closeSign)
		for c, _ := range a.subscribers {
			a.UnsubscribeAll(c)
			c.Close()
		}
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
	a.closeSign <- 1
	return

}

func (e *app) Notify(event string, data []byte) (val int, err error) {

	conn := e.rpool.Get()
	defer conn.Close()
	val, err = redis.Int(conn.Do("PUBLISH", event, data))
	err = e.rpool.Get().Flush()
	return
}
