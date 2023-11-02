package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"
)

var (
	addr     = flag.String("addr", "localhost:0", "tcp service address")
	httpAddr = flag.String("httpaddr", "localhost:0", "http service address")
)

func main() {
	flag.Parse()
	log.SetFlags(0)

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run initializes the chatServer and then
// starts a tcp Server for the passed in address.
func run() error {
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer l.Close()
	log.Printf("tcp listening on %v", l.Addr())

	l2, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return err
	}
	log.Printf("http serving on http://%v", l2.Addr())

	cs := newChatServer()
	s := &http.Server{
		Handler:      cs,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	errc := make(chan error, 2)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				errc <- err
				return
			}

			go cs.subscribe(conn)
		}
	}()

	go func() {
		errc <- s.Serve(l2)
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	select {
	case err := <-errc:
		log.Printf("failed to serve: %v", err)
	case <-sigs:
		log.Println("terminating")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}
