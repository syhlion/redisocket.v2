package redisocket

import "bytes"

type Buffer struct {
	buffer *bytes.Buffer
	client *Client
}

func (b *Buffer) Reset() {
	b.buffer.Reset()
	b.client = nil
}
