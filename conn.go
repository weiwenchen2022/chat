package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"time"

	"nhooyr.io/websocket"
)

type Conn interface {
	CloseNow() error
	Close(int, string) error

	Read(context.Context) ([]byte, error)

	Write(context.Context, []byte) error
}

var (
	_ Conn = (*wsConn)(nil)
	_ Conn = (*netConn)(nil)
)

type wsConn struct {
	c *websocket.Conn
}

func newWsConn(c *websocket.Conn) Conn {
	return &wsConn{c: c}
}

func (wc *wsConn) CloseNow() error {
	return wc.c.CloseNow()
}

func (wc *wsConn) Close(code int, reason string) error {
	return wc.c.Close(websocket.StatusCode(code), reason)
}

var ErrUnsupportedData = errors.New("expect text messsage")

func (wc *wsConn) Read(ctx context.Context) ([]byte, error) {
	typ, msg, err := wc.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if websocket.MessageText != typ {
		return msg, ErrUnsupportedData
	}
	return msg, nil
}

func (wc *wsConn) Write(ctx context.Context, p []byte) error {
	return wc.c.Write(ctx, websocket.MessageText, p)
}

type netConn struct {
	r *bufio.Reader
	c net.Conn
}

func newNetConn(c net.Conn) Conn {
	return &netConn{
		r: bufio.NewReader(c),
		c: c,
	}
}

func (nc *netConn) CloseNow() error {
	return nc.c.Close()
}

func (nc *netConn) Close(int, string) error {
	return errors.New("netConn not implements Close")
}

func (nc *netConn) Read(ctx context.Context) (b []byte, err error) {
	stopc := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		nc.c.SetReadDeadline(time.Now())
		close(stopc)
	})
	defer func() {
		if !stop() {
			// The AfterFunc was started.
			// Wait for it to complete, and reset the Conn's deadline.
			<-stopc
			nc.c.SetReadDeadline(time.Time{})
			err = ctx.Err()
		}
	}()
	return nc.r.ReadBytes('\n')
}

func (nc *netConn) Write(ctx context.Context, b []byte) (err error) {
	stopc := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		nc.c.SetWriteDeadline(time.Now())
		close(stopc)
	})
	defer func() {
		if !stop() {
			// The AfterFunc was started.
			// Wait for it to complete, and reset the Conn's deadline.
			<-stopc
			nc.c.SetWriteDeadline(time.Time{})
			err = ctx.Err()
		}
	}()
	_, err = nc.c.Write(b)
	return err
}
