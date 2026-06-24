package redisocket

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/websocket"
	jsoniter "github.com/json-iterator/go"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

//User client interface
type User interface {
	Trigger(event string, p *Payload) (err error)
	Close()
}

//Payload reciev from redis
type Payload struct {
	Len            int
	Data           []byte
	PrepareMessage *websocket.PreparedMessage
	IsPrepare      bool
	Event          string
}

//WebsocketOptional  init websocket hub config
type WebsocketOptional struct {
	ScanInterval   time.Duration
	WriteWait      time.Duration
	PongWait       time.Duration
	PingPeriod     time.Duration
	MaxMessageSize int64
	Upgrader       websocket.Upgrader
}
type socketPayload struct {
	Sid  string      `json:"sid"`
	Data interface{} `json:"data"`
}
type userPayload struct {
	Uid  string      `json:"uid"`
	Data interface{} `json:"data"`
}
type reloadChannelPayload struct {
	Uid      string   `json:"uid"`
	Channels []string `json:"data"`
}
type addChannelPayload struct {
	Uid     string `json:"uid"`
	Channel string `json:"data"`
}

var (
	//DefaultWebsocketOptional default config
	DefaultWebsocketOptional = WebsocketOptional{
		ScanInterval:   30 * time.Second,
		WriteWait:      10 * time.Second,
		PongWait:       60 * time.Second,
		PingPeriod:     (60 * time.Second * 9) / 10,
		MaxMessageSize: 512,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
)

//EventHandler event handler
type EventHandler func(event string, payload *Payload) error

//ReceiveMsgHandler client receive msg
type ReceiveMsgHandler func([]byte) ([]byte, error)

//NewSender return sender  send to hub
func NewSender(m *redis.Pool) (e *Sender) {

	return &Sender{
		redisManager: m,
	}
}

//Sender struct
type Sender struct {
	redisManager *redis.Pool
}

//BatchData push batch data struct
type BatchData struct {
	Event string
	Data  []byte
}

//GetChannels get all sub channels
func (s *Sender) GetChannels(channelPrefix string, appKey string, pattern string) (channels []string, err error) {
	keyPrefix := fmt.Sprintf("%s%s@channels:", channelPrefix, appKey)
	conn := s.redisManager.Get()
	defer conn.Close()
	tmp, err := redis.Strings(conn.Do("keys", keyPrefix+pattern))
	channels = make([]string, 0)
	for _, v := range tmp {
		channel := strings.Replace(v, keyPrefix, "", -1)
		if channel == "" {
			continue
		}
		channels = append(channels, channel)
	}

	return
}

//GetOnlineByChannel get all online user by  channel
func (s *Sender) GetOnlineByChannel(channelPrefix string, appKey string, channel string) (online []string, err error) {
	memberKey := fmt.Sprintf("%s%s@channels:%s", channelPrefix, appKey, channel)
	conn := s.redisManager.Get()
	defer conn.Close()
	nt := time.Now().Unix()
	dt := nt - 120
	online, err = redis.Strings(conn.Do("ZRANGEBYSCORE", memberKey, dt, nt))
	return
}

//GetOnline get all online user
func (s *Sender) GetOnline(channelPrefix string, appKey string) (online []string, err error) {
	memberKey := fmt.Sprintf("%s%s@online", channelPrefix, appKey)
	conn := s.redisManager.Get()
	defer conn.Close()
	nt := time.Now().Unix()
	dt := nt - 120
	online, err = redis.Strings(conn.Do("ZRANGEBYSCORE", memberKey, dt, nt))
	return
}

//PushBatch push batch data
func (s *Sender) PushBatch(channelPrefix, appKey string, data []BatchData) {
	conn := s.redisManager.Get()
	defer conn.Close()
	for _, d := range data {
		conn.Do("PUBLISH", channelPrefix+appKey+"@"+d.Event, d.Data)
	}
	return
}

//PushToSid  push to user socket id
func (s *Sender) PushToSid(channelPrefix, appKey string, uid string, data interface{}) (val int, err error) {
	conn := s.redisManager.Get()
	defer conn.Close()
	u := socketPayload{
		Sid:  uid,
		Data: data,
	}
	d, err := json.Marshal(u)
	if err != nil {
		return
	}
	val, err = redis.Int(conn.Do("PUBLISH", channelPrefix+appKey+"@"+"#GUSHERFUNC-TOSID#", d))
	return
}

//PushTo  push to user socket
func (s *Sender) PushToUid(channelPrefix, appKey string, uid string, data interface{}) (val int, err error) {
	conn := s.redisManager.Get()
	defer conn.Close()
	u := userPayload{
		Uid:  uid,
		Data: data,
	}
	d, err := json.Marshal(u)
	if err != nil {
		return
	}
	val, err = redis.Int(conn.Do("PUBLISH", channelPrefix+appKey+"@"+"#GUSHERFUNC-TOUID#", d))
	return
}

//ReloadChannel  reload user channel list
func (s *Sender) ReloadChannel(channelPrefix, appKey string, uid string, channels []string) (val int, err error) {
	conn := s.redisManager.Get()
	defer conn.Close()
	u := reloadChannelPayload{
		Uid:      uid,
		Channels: channels,
	}
	d, err := json.Marshal(u)
	if err != nil {
		return
	}
	val, err = redis.Int(conn.Do("PUBLISH", channelPrefix+appKey+"@"+"#GUSHERFUNC-RELOADCHANEL#", d))
	return
}

//AddChannel  append channel to user channel list
func (s *Sender) AddChannel(channelPrefix, appKey string, uid string, channel string) (val int, err error) {
	conn := s.redisManager.Get()
	defer conn.Close()
	u := addChannelPayload{
		Uid:     uid,
		Channel: channel,
	}
	d, err := json.Marshal(u)
	if err != nil {
		return
	}
	val, err = redis.Int(conn.Do("PUBLISH", channelPrefix+appKey+"@"+"#GUSHERFUNC-ADDCHANEL#", d))
	return
}

//Push push single data
func (s *Sender) Push(channelPrefix, appKey string, event string, data []byte) (val int, err error) {
	conn := s.redisManager.Get()
	defer conn.Close()
	val, err = redis.Int(conn.Do("PUBLISH", channelPrefix+appKey+"@"+event, data))
	return
}

//NewHub It's create a Hub
func NewHub(m *redis.Pool, log *logrus.Logger, debug bool) (e *Hub) {

	stat := &Statistic{
		inMemChannel:  make(chan int, 8192),
		outMemChannel: make(chan int, 8192),
		inMsgChannel:  make(chan int, 8192),
		outMsgChannel: make(chan int, 8192),
		l:             log,
	}
	go stat.Run()
	pool := &pool{
		stat:               stat,
		users:              make(map[*Client]bool),
		broadcastChan:      make(chan *eventPayload, 4096),
		joinChan:           make(chan *Client),
		leaveChan:          make(chan *Client),
		kickSidChan:        make(chan string),
		kickUidChan:        make(chan string),
		uPayloadChan:       make(chan *uPayload, 4096),
		uReloadChannelChan: make(chan *uReloadChannelPayload, 4096),
		uAddChannelChan:    make(chan *uAddChannelPayload, 4096),
		sPayloadChan:       make(chan *sPayload, 4096),
		shutdownChan:       make(chan int, 1),
		rpool:              m,
	}
	mq := &messageQuene{
		freeBufferChan: make(chan *buffer, 8192),
		serveChan:      make(chan *buffer, 8192),
		pool:           pool,
	}
	mq.run()

	return &Hub{

		messageQuene: mq,
		Config:       DefaultWebsocketOptional,
		redisManager: m,
		broker:       newRedisBroker(m),
		pool:         pool,
		debug:        debug,
		closeSign:    make(chan int, 1),
		log:          log,
	}

}

//Upgrade gorilla websocket wrap upgrade method
func (e *Hub) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header, uid string, prefix string, auth *Auth) (c *Client, err error) {
	ws, err := e.Config.Upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	sid := uuid.NewV1()
	c = &Client{
		prefix:  prefix,
		uid:     uid,
		sid:     sid.String(),
		ws:      ws,
		send:    make(chan *Payload, 256),
		RWMutex: new(sync.RWMutex),
		hub:     e,
		events:  make(map[string]EventHandler),
		auth:    auth,
	}
	e.join(c)
	return
}

//Hub client hub
type Hub struct {
	ChannelPrefix string
	messageQuene  *messageQuene
	Config        WebsocketOptional
	broker        Broker
	redisManager  *redis.Pool
	*pool
	debug     bool
	log       *logrus.Logger
	closeSign chan int
}

//Ping ping redis server
func (e *Hub) Ping() (err error) {
	_, err = e.redisManager.Get().Do("PING")
	if err != nil {
		return
	}
	return
}
func (e *Hub) logger(format string, v ...interface{}) {
	if e.debug {
		e.log.Infof(format, v...)
	}
}

//CountOnlineUsers return online user total
func (e *Hub) CountOnlineUsers() (i int) {
	return e.pool.onlineCount()
}
// dispatchLoop 消費 broker 收到的事件,做控制事件分派或頻道廣播。
func (e *Hub) dispatchLoop(msgs <-chan BrokerEvent) {
	for ev := range msgs {
		e.handleEvent(ev.Event, ev.Data)
	}
}

// handleEvent 處理單一 bus 事件:#GUSHERFUNC-*# 為控制事件,其餘為一般頻道廣播。
func (e *Hub) handleEvent(channel string, data []byte) {
	switch channel {
	case "#GUSHERFUNC-TOUID#":
		up := &userPayload{}
		if err := json.Unmarshal(data, up); err != nil {
			return
		}
		b, err := json.Marshal(up.Data)
		if err != nil {
			return
		}
		e.toUid(up.Uid, b)
	case "#GUSHERFUNC-TOSID#":
		up := &socketPayload{}
		if err := json.Unmarshal(data, up); err != nil {
			return
		}
		b, err := json.Marshal(up.Data)
		if err != nil {
			return
		}
		e.toSid(up.Sid, b)
	case "#GUSHERFUNC-RELOADCHANEL#":
		up := &reloadChannelPayload{}
		if err := json.Unmarshal(data, up); err != nil {
			return
		}
		e.reloadUidChannels(up.Uid, up.Channels)
	case "#GUSHERFUNC-ADDCHANEL#":
		up := &addChannelPayload{}
		if err := json.Unmarshal(data, up); err != nil {
			return
		}
		e.addUidChannels(up.Uid, up.Channel)
	default:
		pMsg, err := websocket.NewPreparedMessage(websocket.TextMessage, data)
		if err != nil {
			return
		}
		p := &Payload{
			Len:            len(data),
			PrepareMessage: pMsg,
			IsPrepare:      true,
		}
		e.broadcast(channel, p)
	}
}

//Listen hub start
//it's block method
func (e *Hub) Listen(channelPrefix string) error {
	e.pool.channelPrefix = channelPrefix
	e.ChannelPrefix = channelPrefix
	msgs, busErr := e.broker.Subscribe(channelPrefix)
	go e.dispatchLoop(msgs)
	e.pool.scanInterval = e.Config.ScanInterval
	poolErr := e.pool.run()
	select {
	case er := <-busErr:
		e.pool.shutdown()
		return er
	case er := <-poolErr:
		return er
	case <-e.closeSign:
		e.pool.shutdown()
		return nil
	}
}

//Close close hub & close every client
//
// 註:目前不關閉 broker 的訂閱 goroutine(沿用舊行為,goroutine 會洩漏)。
// 乾淨停止 broker(避免與 Receive 並發 race)留待「graceful shutdown」階段,
// 屆時 Broker 改為可停止的訂閱迴圈(NATS 實作會一開始就支援)。
func (e *Hub) Close() {
	e.closeSign <- 1
	return

}
