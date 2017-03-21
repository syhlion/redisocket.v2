package redisocket

import "bytes"

type buffer struct {
	buffer *bytes.Buffer
	client *Client
}

func (b *buffer) Reset(c *Client) {
	b.buffer.Reset()
	b.client = c
}
