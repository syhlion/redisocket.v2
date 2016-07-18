package redisocket

import (
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type client struct {
	ws     *websocket.Conn
	events map[string]EventHandler
	send   chan []byte
	*sync.RWMutex
	re  ReceiveMsgHandler
	app *app
}

func (c *client) Subscribe(event string, h EventHandler) error {
	c.Lock()
	c.events[event] = h
	c.Unlock()
	return c.app.subscribe(event, c)
}
func (c *client) Unsubscribe(event string) error {
	c.Lock()
	delete(c.events, event)
	c.Unlock()
	return c.app.unsubscribe(event, c)
}

func (c *client) Trigger(event string, data []byte) (err error) {
	c.RLock()
	h, ok := c.events[event]
	c.RUnlock()
	if !ok {
		return errors.New("No Event")
	}
	b, err := h(data)
	if err != nil {
		return
	}
	c.send <- b
	return
}

func (c *client) write(msgType int, data []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(msgType, data)
}

func (c *client) readPump() <-chan error {

	errChan := make(chan error)
	go func() {
		defer func() {
			c.Close()
		}()
		c.ws.SetReadLimit(maxMessageSize)
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		c.ws.SetPongHandler(func(string) error { c.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
		for {
			msgType, data, err := c.ws.ReadMessage()
			if err != nil {
				errChan <- err
				close(errChan)
				return
			}
			if msgType != websocket.TextMessage {
				continue
			}

			err = c.re(data)
			if err != nil {
				errChan <- err
				return
			}
		}
	}()
	return errChan

}
func (c *client) Close() {
	c.app.UnsubscribeAll(c)
	c.ws.Close()
	return
}

func (c *client) Listen(re ReceiveMsgHandler) (err error) {
	c.re = re
	writeErr := c.writePump()
	readErr := c.readPump()
	select {
	case e := <-writeErr:
		return e
	case e := <-readErr:
		return e
	}
}

func (c *client) writePump() <-chan error {
	errChan := make(chan error)
	go func() {
		t := time.NewTicker(pingPeriod)
		defer func() {
			c.Close()
			t.Stop()
		}()
		for {
			select {
			case msg, ok := <-c.send:
				if !ok {
					errChan <- c.write(websocket.CloseMessage, []byte{})
					close(errChan)
					return
				}

				if err := c.write(websocket.TextMessage, msg); err != nil {
					errChan <- err
					close(errChan)
					return
				}

			case <-t.C:
				if err := c.write(websocket.PingMessage, []byte{}); err != nil {
					errChan <- err
					close(errChan)
					return
				}

			}
		}
	}()
	return errChan

}
