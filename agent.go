// Copyright (c) nano Author. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package nano

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/showmei5320/nano/internal/codec"
	"github.com/showmei5320/nano/internal/message"
	"github.com/showmei5320/nano/internal/packet"
	"github.com/showmei5320/nano/session"
)

const (
	agentWriteBacklog = 16
)

var (
	// ErrBrokenPipe represents the low-level connection has broken.
	ErrBrokenPipe = errors.New("broken low-level pipe")
	// ErrBufferExceed indicates that the current session buffer is full and
	// can not receive more data.
	ErrBufferExceed = errors.New("session send buffer exceed")
	// AgentGroup 裝所有的Agent
	AgentGroup = NewGroup("agents")

	AgentGroupLock = sync.RWMutex{}
)

type (
	// Agent corresponding a user, used for store raw conn information
	agent struct {
		// regular agent member
		session *session.Session    // session
		conn    net.Conn            // low-level conn fd
		lastMid uint                // last message id
		state   int32               // current agent state
		chDie   chan struct{}       // wait for close
		chSend  chan pendingMessage // push message queue
		lastAt  int64               // last heartbeat unix time stamp
		decoder *codec.Decoder      // binary decoder

		srv reflect.Value // cached session reflect.Value
	}

	pendingMessage struct {
		typ     message.Type // message type
		route   string       // message route(push)
		mid     uint         // response message id(response)
		payload interface{}  // payload
		kick    bool
	}

	writePacket struct {
		data []byte
		kick bool
	}
)

// Create new agent instance
func newAgent(conn net.Conn) *agent {
	a := &agent{
		conn:    conn,
		state:   statusStart,
		chDie:   make(chan struct{}),
		lastAt:  time.Now().Unix(),
		chSend:  make(chan pendingMessage, agentWriteBacklog),
		decoder: codec.NewDecoder(),
	}

	// binding session
	s := session.New(a)
	a.session = s
	a.srv = reflect.ValueOf(s)

	AgentGroup.Add(s)

	return a
}

func (a *agent) send(m pendingMessage) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = ErrBrokenPipe
		}
	}()
	a.chSend <- m
	return
}

func (a *agent) MID() uint {
	return a.lastMid
}

// Push, implementation for session.NetworkEntity interface
func (a *agent) Push(route string, v interface{}) error {
	if a.status() == statusClosed {
		return ErrBrokenPipe
	}

	if len(a.chSend) >= agentWriteBacklog {
		return ErrBufferExceed
	}

	if env.debug {
		switch d := v.(type) {
		case []byte:
			logger.Println(fmt.Sprintf("Type=Push, ID=%d, UID=%d, Route=%s, Data=%dbytes",
				a.session.ID(), a.session.UID(), route, len(d)))
		default:
			logger.Println(fmt.Sprintf("Type=Push, ID=%d, UID=%d, Route=%s, Data=%+v",
				a.session.ID(), a.session.UID(), route, v))
		}
	}

	// a.chSend <- pendingMessage{typ: message.Push, route: route, payload: v, kick: false}
	// return nil
	return a.send(pendingMessage{typ: message.Push, route: route, payload: v, kick: false})
}

func (a *agent) Kick(v interface{}) error {
	if a.status() == statusClosed {
		return ErrBrokenPipe
	}

	if len(a.chSend) >= agentWriteBacklog {
		return ErrBufferExceed
	}

	// a.chSend <- pendingMessage{typ: message.Push, route: "error", payload: v, kick: true}
	// return nil
	return a.send(pendingMessage{typ: message.Push, route: "error", payload: v, kick: true})
}

// Response, implementation for session.NetworkEntity interface
// Response message to session
func (a *agent) Response(v interface{}) error {
	return a.ResponseMID(a.lastMid, v)
}

// Response, implementation for session.NetworkEntity interface
// Response message to session
func (a *agent) ResponseMID(mid uint, v interface{}) error {
	if a.status() == statusClosed {
		return ErrBrokenPipe
	}

	if mid <= 0 {
		return ErrSessionOnNotify
	}

	if len(a.chSend) >= agentWriteBacklog {
		return ErrBufferExceed
	}

	if env.debug {
		switch d := v.(type) {
		case []byte:
			logger.Println(fmt.Sprintf("Type=Response, ID=%d, UID=%d, MID=%d, Data=%dbytes",
				a.session.ID(), a.session.UID(), mid, len(d)))
		default:
			logger.Println(fmt.Sprintf("Type=Response, ID=%d, UID=%d, MID=%d, Data=%+v",
				a.session.ID(), a.session.UID(), mid, v))
		}
	}

	a.chSend <- pendingMessage{typ: message.Response, mid: mid, payload: v, kick: false}
	return nil
}

// Close, implementation for session.NetworkEntity interface
// Close closes the agent, clean inner state and close low-level connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (a *agent) Close() error {
	AgentGroup.Leave(a.session)
	if a.status() == statusClosed {
		return ErrCloseClosedSession
	}
	a.setStatus(statusClosed)

	if env.debug {
		logger.Println(fmt.Sprintf("Session closed, ID=%d, UID=%d, IP=%s",
			a.session.ID(), a.session.UID(), a.conn.RemoteAddr()))
	}

	// prevent closing closed channel
	select {
	case <-a.chDie:
		// expect
	default:
		close(a.chDie)
		if a.session.UID() != 0 {
			handler.chCloseSession <- a.session
		}
	}

	return a.conn.Close()
}

// RemoteAddr, implementation for session.NetworkEntity interface
// returns the remote network address.
func (a *agent) RemoteAddr() net.Addr {
	return a.conn.RemoteAddr()
}

// String, implementation for Stringer interface
func (a *agent) String() string {
	return fmt.Sprintf("Remote=%s, LastTime=%d", a.conn.RemoteAddr().String(), a.lastAt)
}

func (a *agent) status() int32 {
	return atomic.LoadInt32(&a.state)
}

func (a *agent) setStatus(state int32) {
	atomic.StoreInt32(&a.state, state)
}

func (a *agent) write() {
	ticker := time.NewTicker(env.heartbeat)
	chWrite := make(chan writePacket, agentWriteBacklog)
	// clean func
	defer func() {
		ticker.Stop()
		// close(a.chSend)
		// close(chWrite)
		a.Close()
		if env.debug {
			logger.Println(fmt.Sprintf("Session write goroutine exit, SessionID=%d, UID=%d", a.session.ID(), a.session.UID()))
		}
	}()

	for {
		select {
		case <-ticker.C:
			deadline := time.Now().Add(-2 * env.heartbeat).Unix()
			if a.lastAt < deadline {
				logger.Println(fmt.Sprintf("Session heartbeat timeout, LastTime=%d, Deadline=%d", a.lastAt, deadline))
				return
			}
			chWrite <- writePacket{
				data: hbd,
				kick: false,
			}

		case writePacket := <-chWrite:
			// close agent while low-level conn broken
			_, err := a.conn.Write(writePacket.data)

			if err != nil {
				logger.Println(err.Error())
				return
			}

			if writePacket.kick {
				return
			}

		case data := <-a.chSend:
			payload, err := serializeOrRaw(data.payload)
			if err != nil {
				logger.Println(err.Error())
				break
			}

			if len(Pipeline.Outbound.handlers) > 0 {
				for _, h := range Pipeline.Outbound.handlers {
					payload, err = h(a.session, payload)
					if err != nil {
						logger.Println("broken pipeline", err.Error())
						break
					}
				}
			}

			// construct message and encode
			m := &message.Message{
				Type:  data.typ,
				Data:  payload,
				Route: data.route,
				ID:    data.mid,
			}
			em, err := m.Encode()
			if err != nil {
				logger.Println(err.Error())
				break
			}

			// packet encode
			p, err := codec.Encode(packet.Data, em)
			if err != nil {
				logger.Println(err)
				break
			}
			chWrite <- writePacket{
				data: p,
				kick: data.kick,
			}

		case <-a.chDie: // agent closed signal
			return

		case <-env.die: // application quit
			return
		}
	}
}
