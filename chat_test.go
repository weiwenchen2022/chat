package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func assertSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertMessage(t *testing.T, expMsg, msg string, err error) {
	t.Helper()
	if err != nil || expMsg != msg {
		t.Fatalf("expected %q, nil, but got %q, %v", expMsg, msg, err)
	}
}

func Test_chatServer_simple(t *testing.T) {
	t.Parallel()

	// This is a simple echo test with a single client.
	// The client sends a message and ensures it receives
	// it on its connection.
	addr, url, closeFn := setupTest(t)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cl1, err := newTCPClient(ctx, addr)
	assertSuccess(t, err)
	defer cl1.Close()

	cl2, err := newWsClient(ctx, url)
	assertSuccess(t, err)
	defer cl2.Close()

	// skip hello msg
	_, err = cl1.nextMessage()
	assertSuccess(t, err)

	// skip hello msg
	_, err = cl2.nextMessage()
	assertSuccess(t, err)

	{
		expMsg := randString(512)
		err = cl1.publish(ctx, expMsg+"\n")
		assertSuccess(t, err)

		msg, err := cl2.nextMessage()
		assertMessage(t, expMsg, msg, err)

		msg, err = cl1.nextMessage()
		assertMessage(t, expMsg, msg, err)
	}

	{
		expMsg := randString(512)
		err = cl2.publish(ctx, expMsg+"\n")
		assertSuccess(t, err)

		msg, err := cl1.nextMessage()
		assertMessage(t, expMsg, msg, err)

		msg, err = cl2.nextMessage()
		assertMessage(t, expMsg, msg, err)
	}
}

func Test_chatServer_concurrency(t *testing.T) {
	t.Parallel()

	// This test is a complex concurrency test.
	// 10 clients are started that send 128 different
	// messages of max 128 bytes concurrently.
	//
	// The test verifies that every message is seen by ever client
	// and no errors occur anywhere.
	const (
		nmessages      = 128
		maxMessageSize = 128
		nclients       = 16
	)

	addr, url, closeFn := setupTest(t)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clients := make([]client, nclients)
	clientMsgs := make([]map[string]struct{}, nclients)
	for i := range clients {
		var (
			cl  client
			err error
		)
		switch rand.Intn(2) {
		case 0:
			cl, err = newTCPClient(ctx, addr)
		default:
			cl, err = newWsClient(ctx, url)
		}
		assertSuccess(t, err)
		defer cl.Close()

		clients[i] = cl
		clientMsgs[i] = randMessages(nmessages, maxMessageSize)
	}

	allMessages := make(map[string]struct{})
	for _, msgs := range clientMsgs {
		for m := range msgs {
			allMessages[m] = struct{}{}
		}
	}

	var wg sync.WaitGroup
	for i, cl := range clients {
		i := i
		cl := cl

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cl.publishMsgs(ctx, clientMsgs[i])
			if err != nil {
				t.Errorf("client %d failed to publish all messages: %v", i, err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cl.nextMessage(); err != nil {
				t.Errorf("client %d failed to receive hello message: %v", i, err)
			}

			err := testAllMessagesReceived(cl, nclients*nmessages, allMessages)
			if err != nil {
				t.Errorf("client %d failed to receive all messages: %v", i, err)
			}
		}()
	}

	wg.Wait()
}

// setupTest sets up chatServer that can be used
// via the returned addr and url.
//
// Defer closeFn to ensure everything is cleaned up at
// the end of the test.
//
// chatServer logs will be logged via t.Logf.
func setupTest(t *testing.T) (addr, url string, closeFn func()) {
	cs := newChatServer()
	cs.logf = t.Logf

	// To ensure tests run quickly under even -race.
	cs.subscriberMessageBuffer = 4096

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("failed to listen tcp :0: %v", err)
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("failed to accept: %v", err)
				}
				return
			}
			go cs.subscribe(conn)
		}
	}()

	s := httptest.NewServer(cs)
	return l.Addr().String(), s.URL, func() {
		l.Close()
		s.Close()
	}
}

// testAllMessagesReceived ensures that after n reads, all msgs in msgs
// have been read.
func testAllMessagesReceived(cl client, n int, msgs map[string]struct{}) error {
	msgs = cloneMessages(msgs)

	for i := 0; i < n; i++ {
		msg, err := cl.nextMessage()
		if err != nil {
			return err
		}
		delete(msgs, msg)
	}

	if len(msgs) != 0 {
		return fmt.Errorf("did not receive all expected messages: %q", msgs)
	}
	return nil
}

func cloneMessages(msgs map[string]struct{}) map[string]struct{} {
	return maps.Clone(msgs)
}

func randMessages(n, maxMessageLength int) map[string]struct{} {
	msgs := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		for {
			m := randString(rand.Intn(maxMessageLength))
			if _, ok := msgs[m]; !ok {
				msgs[m] = struct{}{}
				break
			}
		}
	}
	return msgs
}

type client interface {
	Close() error

	publish(context.Context, string) error
	publishMsgs(context.Context, map[string]struct{}) error

	nextMessage() (string, error)
}

var (
	_ client = (*tcpClient)(nil)
	_ client = (*wsClient)(nil)
)

type tcpClient struct {
	addr string

	r *bufio.Reader
	w *bufio.Writer
	c io.Closer
}

func newTCPClient(ctx context.Context, addr string) (client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	cl := &tcpClient{
		addr: addr,

		r: bufio.NewReader(c),
		w: bufio.NewWriter(c),
		c: c,
	}
	return cl, nil
}

func (cl *tcpClient) publish(ctx context.Context, msg string) (err error) {
	defer func() {
		if err != nil {
			cl.c.Close()
		}
	}()

	_, err = cl.w.WriteString(msg)
	if err == nil {
		err = cl.w.Flush()
	}
	return err
}

func (cl *tcpClient) publishMsgs(ctx context.Context, msgs map[string]struct{}) error {
	for m := range msgs {
		err := cl.publish(ctx, m+"\n")
		if err != nil {
			return err
		}
	}
	return nil
}

func (cl *tcpClient) nextMessage() (string, error) {
	s, err := cl.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	s = strings.Trim(s, "\n")
	if i := strings.IndexByte(s, '>'); i >= 0 {
		s = s[i+2:]
	}
	return s, nil
}

func (cl *tcpClient) Close() error {
	return cl.c.Close()
}

type wsClient struct {
	url string
	c   *websocket.Conn
}

func newWsClient(ctx context.Context, url string) (client, error) {
	c, _, err := websocket.Dial(ctx, url+"/subscribe", nil)
	if err != nil {
		return nil, err
	}

	cl := &wsClient{
		url: url,
		c:   c,
	}
	return cl, nil
}

func (cl *wsClient) publish(ctx context.Context, msg string) (err error) {
	defer func() {
		if err != nil {
			cl.c.Close(websocket.StatusInternalError, "publish failed")
		}
	}()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cl.url+"/publish", strings.NewReader(msg))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if http.StatusAccepted != resp.StatusCode {
		return fmt.Errorf("publish request failed: %v", resp.StatusCode)
	}
	return nil
}

func (cl *wsClient) publishMsgs(ctx context.Context, msgs map[string]struct{}) error {
	for m := range msgs {
		err := cl.publish(ctx, m+"\n")
		if err != nil {
			return err
		}
	}
	return nil
}

func (cl *wsClient) nextMessage() (string, error) {
	typ, b, err := cl.c.Read(context.Background())
	if err != nil {
		return "", err
	}

	if websocket.MessageText != typ {
		cl.c.Close(websocket.StatusUnsupportedData, "expected text message")
		return "", fmt.Errorf("expected text message but got %v", typ)
	}

	s := strings.Trim(string(b), "\n")
	if i := strings.IndexByte(s, '>'); i >= 0 {
		s = s[i+2:]
	}
	return s, nil
}

func (cl *wsClient) Close() error {
	return cl.c.Close(websocket.StatusNormalClosure, "")
}

// randString generates a random string with length n.
func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		switch rand.Intn(2) {
		case 0:
			b[i] = 'a' + byte(rand.Intn(26))
		default:
			b[i] = 'A' + byte(rand.Intn(26))
		}
	}
	return string(b)
}
