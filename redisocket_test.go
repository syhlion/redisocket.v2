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
	"github.com/sirupsen/logrus"
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

// testServer 起一個 miniredis + Hub + httptest ws server,回傳關閉函式。
func testServer(t *testing.T, prefix string, onUpgrade func(c *Client)) (*Sender, string, func()) {
	_, s, u, c := testServerHub(t, prefix, onUpgrade)
	return s, u, c
}

func testServerHub(t *testing.T, prefix string, onUpgrade func(c *Client)) (*Hub, *Sender, string, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	pool := newTestPool(mr.Addr())
	hub := NewHub(pool, newTestLogger(), false)
	go hub.Listen(prefix)
	time.Sleep(100 * time.Millisecond) // 等 psubscribe + pool 起來

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
		pool.Close()
		mr.Close()
	}
	return hub, NewSender(pool), srv.URL, cleanup
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

// TestPubSubDelivery: client 訂閱頻道 → Sender.Push → client 收到。
func TestPubSubDelivery(t *testing.T) {
	const appKey, channel = "appkey", "mychannel"
	sender, srvURL, cleanup := testServer(t, "test.", func(c *Client) {
		c.On(channel, func(event string, p *Payload) error { return nil })
	})
	defer cleanup()

	conn := dialWS(t, srvURL, appKey, "u1")
	defer conn.Close()
	time.Sleep(150 * time.Millisecond) // 等 On + 訂閱穩定

	if _, err := sender.Push("test.", appKey, channel, []byte("hello")); err != nil {
		t.Fatalf("push: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "hello" {
		t.Fatalf("got %q want hello", string(msg))
	}
}

// TestConcurrentConnAndCount: 並發連線 + 同時讀 CountOnlineUsers,
// 用來讓 pool.users map 的並發讀寫 race 在 -race 下現形。
func TestConcurrentConnAndCount(t *testing.T) {
	hub, _, srvURL, cleanup := testServerHub(t, "race.", nil)
	defer cleanup()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// reader: 不停讀在線數(外部直讀 pool.users,與 join/leave 並發 → 預期 -race 報 race)
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
		c := dialWS(t, srvURL, "appkey", "u")
		conns = append(conns, c)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	for _, c := range conns {
		c.Close()
	}
	wg.Wait()
}
