// +build benchmark

package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kensomanpow/nano"
	"github.com/kensomanpow/nano/benchmark/io"
	"github.com/kensomanpow/nano/benchmark/testdata"
	"github.com/kensomanpow/nano/component"
	"github.com/kensomanpow/nano/serialize/protobuf"
	"github.com/kensomanpow/nano/session"
)

const (
	addr = "127.0.0.1:13250" // local address
	conc = 200           // concurrent client count
)

var	metrics int32

//
type TestHandler struct {
	component.Base
}

func main() {
	atomic.StoreInt32(&metrics, 0)
	runtime.GOMAXPROCS(runtime.NumCPU())
	go server()

	// wait server startup
	time.Sleep(1 * time.Second)
	for i := 0; i < conc; i++ {
		go client()
	}

	log.SetFlags(log.LstdFlags | log.Llongfile)

	sg := make(chan os.Signal)
	signal.Notify(sg, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL)

	<-sg

}

func (h *TestHandler) AfterInit() {
	ticker := time.NewTicker(time.Second)

	// metrics output ticker
	go func() {
		for range ticker.C {
			println("QPS", atomic.LoadInt32(&metrics))
			atomic.StoreInt32(&metrics, 0)
		}
	}()
}

func (h *TestHandler) Ping(s *session.Session, data *testdata.Ping) error {
	return s.Push("pong", &testdata.Pong{Content: data.Content})
}

func server() {
	nano.Register(&TestHandler{})
	nano.SetSerializer(protobuf.NewSerializer())
	nano.SetLogger(log.New(os.Stdout, "", log.LstdFlags|log.Llongfile))

	nano.Listen(addr)
}

func client() {
	c := io.NewConnector()
	
	if err := c.Start(addr); err != nil {
		panic(err)
	}

	chReady := make(chan struct{})
	c.OnConnected(func() {
		chReady <- struct{}{}
	})

	c.On("pong", func(data interface{}) {
		atomic.AddInt32(&metrics, 1)
	})

	<-chReady
	for {
		c.Notify("TestHandler.Ping", &testdata.Ping{})
		time.Sleep(2 * time.Millisecond)
	}
}
