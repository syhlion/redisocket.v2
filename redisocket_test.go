package redisocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"go.uber.org/goleak"
)

func newTestPool(addr string) *redis.Pool {
	return &redis.Pool{
		MaxIdle: 8,
		Dial:    func() (redis.Conn, error) { return redis.Dial("tcp", addr) },
	}
}

func newTestLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel) // keep test output quiet
	return l
}

// startEmbeddedNATS 在 process 內起一個真的 nats-server(隨機埠),免外部依賴。
func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	return ns
}

// backend 是「一個 bus 後端 + presence 用的 redis pool + 前綴 + 清理」。
// presence 仍走 redis(Phase C 才抽離),故兩後端都帶 miniredis pool。
type backend struct {
	broker Broker
	// presence 非 nil 時直接注入(NATS-native);否則用 presencePool 建 redisPresence。
	presence     Presence
	presencePool *redis.Pool
	prefix       string
	cleanup      func()
}

// backends:同一套行為測試會對這裡每個後端各跑一次,斷言必須一致。
var backends = map[string]func(t *testing.T) backend{
	"redis": func(t *testing.T) backend {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		pool := newTestPool(mr.Addr())
		return backend{
			broker:       newRedisBroker(pool),
			presencePool: pool,
			prefix:       "test.",
			cleanup:      func() { pool.Close(); mr.Close() },
		}
	},
	"nats": func(t *testing.T) backend {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		pool := newTestPool(mr.Addr())
		ns := startEmbeddedNATS(t)
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("nats connect: %v", err)
		}
		return backend{
			broker:       newNATSBroker(nc),
			presencePool: pool,
			prefix:       "gusher.",
			cleanup:      func() { nc.Close(); ns.Shutdown(); pool.Close(); mr.Close() },
		}
	},
	// nats-native:bus 與 presence 都走 NATS,完全無 Redis。
	"nats-native": func(t *testing.T) backend {
		ns := startEmbeddedNATS(t)
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("nats connect: %v", err)
		}
		presence, err := newMemoryPresence(nc, "gusher.")
		if err != nil {
			t.Fatalf("memory presence: %v", err)
		}
		return backend{
			broker:   newNATSBroker(nc),
			presence: presence,
			prefix:   "gusher.",
			cleanup:  func() { presence.Close(); nc.Close(); ns.Shutdown() },
		}
	},
}

// testServerHub 用給定後端起 Hub + httptest ws server。
func testServerHub(t *testing.T, be backend, onUpgrade func(c *Client)) (*Hub, string, func()) {
	t.Helper()
	var hub *Hub
	if be.presence != nil {
		hub = NewHubWithBrokerAndPresence(be.broker, be.presence, newTestLogger(), false)
	} else {
		hub = NewHubWithBroker(be.broker, be.presencePool, newTestLogger(), false)
	}
	go hub.Listen(be.prefix)
	time.Sleep(100 * time.Millisecond) // 等 subscribe + pool 起來

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appKey := r.URL.Query().Get("app_key")
		uid := r.URL.Query().Get("uid")
		auth := &Auth{AppKey: appKey, UserId: uid, Channels: []string{"*"}}
		c, err := hub.Upgrade(w, r, nil, uid, appKey, auth)
		if err != nil {
			return
		}
		if onUpgrade != nil {
			onUpgrade(c)
		}
		c.Listen(func(b []byte) ([]byte, error) { return nil, nil })
	}))

	cleanup := func() {
		srv.Close()
		hub.Close()
		be.cleanup()
	}
	return hub, srv.URL, cleanup
}

func dialWS(t *testing.T, srvURL, appKey, uid string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/?app_key=" + appKey + "&uid=" + uid
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func noopHandler(event string, p *Payload) error { return nil }

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[string]bool, len(got))
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

// TestMemoryPresenceCrossNode: 兩個節點各持本機成員,驗證查詢透過 NATS
// request/reply 跨節點聚合(union)。
func TestMemoryPresenceCrossNode(t *testing.T) {
	ns := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc1, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc1.Close()
	nc2, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc2.Close()
	p1, err := newMemoryPresence(nc1, "gusher.")
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := newMemoryPresence(nc2, "gusher.")
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	p1.Touch("gusher.", "app", "userA", []string{"room1"})
	p2.Touch("gusher.", "app", "userB", []string{"room1", "room2"})
	time.Sleep(50 * time.Millisecond) // 等兩節點的 responder 訂閱就緒

	if online, _ := p1.Online("gusher.", "app"); !sameSet(online, []string{"userA", "userB"}) {
		t.Fatalf("Online = %v, want {userA,userB}", online)
	}
	if r1, _ := p2.OnlineByChannel("gusher.", "app", "room1"); !sameSet(r1, []string{"userA", "userB"}) {
		t.Fatalf("OnlineByChannel(room1) = %v, want {userA,userB}", r1)
	}
	if r2, _ := p1.OnlineByChannel("gusher.", "app", "room2"); !sameSet(r2, []string{"userB"}) {
		t.Fatalf("OnlineByChannel(room2) = %v, want {userB}", r2)
	}
	if chs, _ := p1.Channels("gusher.", "app", "*"); !sameSet(chs, []string{"room1", "room2"}) {
		t.Fatalf("Channels = %v, want {room1,room2}", chs)
	}
}

// TestPubSubDelivery: client 訂閱頻道 → broker.Publish → client 收到。兩後端各跑一次。
func TestPubSubDelivery(t *testing.T) {
	for name, factory := range backends {
		t.Run(name, func(t *testing.T) {
			be := factory(t)
			const appKey, channel = "appkey", "mychannel"
			_, srvURL, cleanup := testServerHub(t, be, func(c *Client) {
				c.On(channel, noopHandler)
			})
			defer cleanup()

			conn := dialWS(t, srvURL, appKey, "u1")
			defer conn.Close()
			time.Sleep(150 * time.Millisecond) // 等 On + 訂閱穩定

			if _, err := be.broker.Publish(be.prefix, appKey, channel, []byte("hello")); err != nil {
				t.Fatalf("publish: %v", err)
			}

			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(msg) != "hello" {
				t.Fatalf("got %q want hello", string(msg))
			}
		})
	}
}

// TestDottedChannel: 頻道名含 "." 與特殊字元,確保 NATS subject/header 映射 roundtrip 不壞。
func TestDottedChannel(t *testing.T) {
	for name, factory := range backends {
		t.Run(name, func(t *testing.T) {
			be := factory(t)
			const appKey, channel = "appkey", "room.5.user@x"
			_, srvURL, cleanup := testServerHub(t, be, func(c *Client) {
				c.On(channel, noopHandler)
			})
			defer cleanup()

			conn := dialWS(t, srvURL, appKey, "u1")
			defer conn.Close()
			time.Sleep(150 * time.Millisecond)

			if _, err := be.broker.Publish(be.prefix, appKey, channel, []byte("dotted")); err != nil {
				t.Fatalf("publish: %v", err)
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(msg) != "dotted" {
				t.Fatalf("got %q want dotted", string(msg))
			}
		})
	}
}

// TestNoGoroutineLeak: 連線後 graceful shutdown,驗證引擎所有背景 goroutine
// (pool.run / message workers / statistic / broker 訂閱 / dispatchLoop / client)都退出。
// 用 redis backend(engine goroutine 生命週期與後端無關;避開內嵌 nats server 自身 goroutine)。
func TestNoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	be := backends["redis"](t)
	_, srvURL, cleanup := testServerHub(t, be, func(c *Client) {
		c.On("ch", noopHandler)
	})
	conn := dialWS(t, srvURL, "appkey", "u1")
	time.Sleep(150 * time.Millisecond)
	conn.Close()
	cleanup() // srv.Close + hub.Close(graceful) + pool/miniredis close
	// 給 redis 訂閱輪詢(250ms)足夠時間注意到 done 後退出
	time.Sleep(700 * time.Millisecond)
}

// TestClientEventsConcurrent: 並發 On/Off churn,與 writePump ping-tick 讀 events
// 並發,守住 c.events 的 map race 修復(連線層,與 backend 無關,用 redis fixture)。
func TestClientEventsConcurrent(t *testing.T) {
	be := backends["redis"](t)
	hub := NewHubWithBroker(be.broker, be.presencePool, newTestLogger(), false)
	hub.Config.PingPeriod = 5 * time.Millisecond // 讓 ping 分支頻繁讀 events
	go hub.Listen(be.prefix)
	time.Sleep(50 * time.Millisecond)

	captured := make(chan *Client, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := &Auth{AppKey: "a", UserId: "u", Channels: []string{"*"}}
		c, err := hub.Upgrade(w, r, nil, "u", "a", auth)
		if err != nil {
			return
		}
		c.On("seed", noopHandler) // 保持非空,避免 timeout 斷線
		captured <- c
		c.Listen(func(b []byte) ([]byte, error) { return nil, nil })
	}))
	defer func() { srv.Close(); hub.Close(); be.cleanup() }()

	conn := dialWS(t, srv.URL, "a", "u")
	defer conn.Close()
	c := <-captured

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			c.On("x", noopHandler)
			c.Off("x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("timeout")
	}
}

// TestConcurrentConnAndCount: 並發連線 + 同時讀 CountOnlineUsers(pool.users map 並發)。
func TestConcurrentConnAndCount(t *testing.T) {
	for name, factory := range backends {
		t.Run(name, func(t *testing.T) {
			be := factory(t)
			hub, srvURL, cleanup := testServerHub(t, be, nil)
			defer cleanup()

			var wg sync.WaitGroup
			stop := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						_ = hub.CountOnlineUsers()
					}
				}
			}()

			conns := make([]*websocket.Conn, 0, 20)
			for i := 0; i < 20; i++ {
				conns = append(conns, dialWS(t, srvURL, "appkey", "u"))
			}
			time.Sleep(200 * time.Millisecond)
			close(stop)
			for _, c := range conns {
				c.Close()
			}
			wg.Wait()
		})
	}
}
