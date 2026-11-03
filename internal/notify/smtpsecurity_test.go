package notify

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

type countSMTPConn struct {
	net.Conn
	read int
}

func (c *countSMTPConn) Read(p []byte) (int, error) { n, e := c.Conn.Read(p); c.read += n; return n, e }

func TestSMTPGreetingReadIsBounded(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	counted := &countSMTPConn{Conn: client}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer peer.Close()
		_, _ = peer.Write([]byte("220 " + strings.Repeat("x", maxSMTPReplyBytes*4)))
	}()
	d := &smtpDriver{o: SMTPOptions{Addr: "mail.invalid:25", Timeout: time.Second, Dial: func(context.Context, string) (net.Conn, error) { return counted, nil }}}
	err := d.Deliver(context.Background(), &Message{To: []string{"user@example.invalid"}})
	if err == nil || counted.read > maxSMTPReplyBytes {
		t.Fatalf("err=%T consumed=%d", err, counted.read)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP peer was not closed")
	}
}
