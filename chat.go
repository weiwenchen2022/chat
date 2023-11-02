package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const writeWait = 5 * time.Second

// chatServer enables broadcasting to a set of subscribers.
type chatServer struct {
	// subscriberMessageBuffer controls the max number
	// of messages that can be queued for a subscriber
	// before it is kicked.
	//
	// Defaults to 16.
	subscriberMessageBuffer int

	// logf controls where logs are sent.
	// Defaults to log.Printf.
	logf func(string, ...any)

	// serveMux routes the various endpoints to the appropriate handler.
	serveMux http.ServeMux

	subscribersMu sync.Mutex
	subscribers   map[*subscriber]struct{}

	userID int
}

// newChatServer constructs a chatServer with the defaults.
func newChatServer() *chatServer {
	cs := &chatServer{
		subscriberMessageBuffer: 16,
		logf:                    log.Printf,
		subscribers:             make(map[*subscriber]struct{}),
	}
	cs.serveMux.Handle("/", http.FileServer(http.Dir(".")))
	cs.serveMux.HandleFunc("/subscribe", cs.subscribeHandler)
	cs.serveMux.HandleFunc("/publish", cs.publishHandler)

	return cs
}

func (cs *chatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cs.serveMux.ServeHTTP(w, r)
}

// subscriber represents a subscriber.
// Messages are sent on the msgs channel and if the client
// cannot keep up with the messages, closeSlow is called.
type subscriber struct {
	nick      string
	msgs      chan []byte
	closeSlow func()
}

// subscribeHandler accepts the WebSocket connection and then subscribes
// it to all future messages.
func (cs *chatServer) subscribeHandler(w http.ResponseWriter, r *http.Request) {
	err := cs.subscribeWs(r.Context(), w, r)
	if errors.Is(err, context.Canceled) {
		return
	}

	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return
	}
	if err != nil {
		cs.logf("%v", err)
		return
	}
}

// publishHandler reads the request body with a limit of 8192 bytes and then publishes
// the received message.
func (cs *chatServer) publishHandler(w http.ResponseWriter, r *http.Request) {
	if http.MethodPost != r.Method {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	body := http.MaxBytesReader(w, r.Body, 8192)
	msg, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	cs.publish(msg)

	w.WriteHeader(http.StatusAccepted)
}

// subscribeWs subscribes the given WebSocket to all broadcast messages.
// It creates a subscriber with a buffered msgs chan to give some room to slower
// connections and then registers the subscriber. It then listens for all messages
// and writes them to the WebSocket. If the context is cancelled or
// an error occurs, it returns and deletes the subscription.
//
// It uses CloseRead to keep reading from the connection to process control
// messages and cancel the context if the connection drops.
func (cs *chatServer) subscribeWs(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var (
		mu     sync.Mutex
		c      Conn
		closed bool
	)
	s := &subscriber{
		msgs: make(chan []byte, cs.subscriberMessageBuffer),
		closeSlow: func() {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			if c != nil {
				c.Close(int(websocket.StatusPolicyViolation), "connection too slow to keep up with messages")
			}
		},
	}
	cs.addSubscriber(s)
	defer cs.deleteSubscriber(s)

	c2, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}

	mu.Lock()
	if closed {
		mu.Unlock()
		return net.ErrClosed
	}

	c = newWsConn(c2)
	mu.Unlock()
	defer c.CloseNow()

	ctx = cs.reader(ctx, c, s)

	err = writeTimeout(ctx, c, welcomeMsg, writeWait)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-s.msgs:
			err := writeTimeout(ctx, c, msg, writeWait)
			if err != nil {
				return err
			}
		}
	}
}

func (cs *chatServer) reader(ctx context.Context, c Conn, s *subscriber) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer c.CloseNow()
		defer cancel()

		for {
			msg, err := c.Read(ctx)
			if err != nil {
				if errors.Is(err, ErrUnsupportedData) {
					c.Close(int(websocket.StatusUnsupportedData), ErrUnsupportedData.Error())
				}
				return
			}

			if msg[0] == '/' {
				msg = bytes.Trim(msg, "\n ")
				arg := strings.Split(string(msg), " ")
				switch arg[0] {
				case "/nick":
					s.nick = arg[1]
				}
				continue
			}

			msg = []byte(fmt.Sprintf("%s> %s", s.nick, msg))
			cs.publish(msg)
		}
	}()
	return ctx
}

var welcomeMsg = []byte("Welcome to Simple Chat! Use /nick <nick> to set your nick.\n")

// subscribe subscribes the given net.Conn to all broadcast messages.
// It creates a subscriber with a buffered msgs chan to give some room to slower
// connections and then registers the subscriber. It then listens for all messages
// and writes them to the net.Conn. If the context is cancelled or
// an error occurs, it returns and deletes the subscription.
func (cs *chatServer) subscribe(c2 net.Conn) error {
	var (
		ctx    = context.Background()
		mu     sync.Mutex
		c      Conn
		closed bool
	)

	s := &subscriber{
		msgs: make(chan []byte, cs.subscriberMessageBuffer),
		closeSlow: func() {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			if c != nil {
				c.CloseNow()
			}
		},
	}
	cs.addSubscriber(s)
	defer cs.deleteSubscriber(s)

	mu.Lock()
	if closed {
		mu.Unlock()
		return net.ErrClosed
	}
	c = newNetConn(c2)
	mu.Unlock()
	defer c.CloseNow()

	ctx = cs.reader(ctx, c, s)

	err := writeTimeout(ctx, c, welcomeMsg, writeWait)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-s.msgs:
			err := writeTimeout(ctx, c, msg, writeWait)
			if err != nil {
				return err
			}
		}
	}
}

// publish publishes the msg to all subscribers.
// It never blocks and so messages to slow subscribers
// are dropped.
func (cs *chatServer) publish(msg []byte) {
	cs.subscribersMu.Lock()
	defer cs.subscribersMu.Unlock()

	for s := range cs.subscribers {
		select {
		case s.msgs <- msg:
		default:
			go s.closeSlow()
		}
	}
}

// addSubscriber registers a subscriber.
func (cs *chatServer) addSubscriber(s *subscriber) {
	cs.subscribersMu.Lock()
	cs.userID++
	s.nick = fmt.Sprintf("user%d", cs.userID)
	cs.subscribers[s] = struct{}{}
	cs.subscribersMu.Unlock()
}

// deleteSubscriber deletes the given subscriber.
func (cs *chatServer) deleteSubscriber(s *subscriber) {
	cs.subscribersMu.Lock()
	delete(cs.subscribers, s)
	cs.subscribersMu.Unlock()
}

func writeTimeout(ctx context.Context, c Conn, msg []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Write(ctx, msg)
}
