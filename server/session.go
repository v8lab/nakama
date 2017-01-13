// Copyright 2017 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/satori/go.uuid"
	"github.com/uber-go/zap"
	"sync"
)

type session struct {
	sync.Mutex
	logger     zap.Logger
	config     Config
	id         uuid.UUID
	userID     uuid.UUID
	stopped    bool
	conn       *websocket.Conn
	pingTicker *time.Ticker
	unregister func(s *session)
}

// NewSession creates a new session which encapsulates a socket connection
func NewSession(logger zap.Logger, config Config, userID uuid.UUID, websocketConn *websocket.Conn, unregister func(s *session)) *session {
	sessionID := uuid.NewV4()
	sessionLogger := logger.With(zap.String("uid", userID.String()), zap.String("session", sessionID.String()))

	return &session{
		logger:     sessionLogger,
		config:     config,
		id:         sessionID,
		userID:     userID,
		conn:       websocketConn,
		stopped:    false,
		pingTicker: time.NewTicker(time.Duration(config.GetTransport().PingPeriodMs) * time.Millisecond),
		unregister: unregister,
	}
}

func (s *session) Consume(processRequest func(logger zap.Logger, session *session, envelope *Envelope)) {
	defer s.Close()
	s.conn.SetReadLimit(s.config.GetTransport().MaxMessageSizeBytes)
	s.conn.SetReadDeadline(time.Now().Add(time.Duration(s.config.GetTransport().PongWaitMs) * time.Millisecond))
	s.conn.SetPongHandler(func(string) error {
		s.conn.SetReadDeadline(time.Now().Add(time.Duration(s.config.GetTransport().PongWaitMs) * time.Millisecond))
		return nil
	})

	// Send an initial ping immediately, then at intervals.
	s.pingNow()
	go s.pingPeriodically()

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				s.logger.Warn("Error reading message from client", zap.Object("error", err.Error()))
			}
			break
		}

		s.logger.Debug("Read message from socket", zap.Object("message", data))
		request := &Envelope{}
		err = proto.Unmarshal(data, request)
		if err != nil {
			s.logger.Warn("Received malformed payload", zap.Object("data", data))
			s.Send(&Envelope{CollationId: request.CollationId, Payload: &Envelope_Error{&Error{Reason: "Unrecognized message"}}})
		} else {
			//TODO(mofirouz, zyro) Add session-global context here
			//to cancel in-progress operations when the session is closed
			requestLogger := s.logger.With(zap.String("cid", request.CollationId))
			processRequest(requestLogger, s, request)
		}
	}
}

func (s *session) pingPeriodically() {
	for range s.pingTicker.C {
		if !s.pingNow() {
			// If ping fails the session will be stopped, clean up the loop.
			return
		}
	}
}

func (s *session) pingNow() bool {
	// Websocket ping.
	s.conn.SetWriteDeadline(time.Now().Add(time.Duration(s.config.GetTransport().WriteWaitMs) * time.Millisecond))
	err := s.conn.WriteMessage(websocket.PingMessage, []byte{})
	if err != nil {
		s.logger.Warn("Could not send ping. Closing channel", zap.String("remoteAddress", s.conn.RemoteAddr().String()), zap.Error(err))
		s.Close()
		return false
	}

	// Server heartbeat.
	err = s.Send(&Envelope{Payload: &Envelope_Heartbeat{&Heartbeat{Timestamp: time.Now().UTC().Unix()}}})
	if err != nil {
		s.logger.Warn("Could not send heartbeat", zap.String("remoteAddress", s.conn.RemoteAddr().String()), zap.Error(err))
	}

	return true
}

func (s *session) Send(response proto.Message) error {
	payload, err := proto.Marshal(response)

	if err != nil {
		s.logger.Warn("Could not marshall Response to byte[]", zap.Error(err))
		return err
	}

	return s.SendBytes(payload)
}

func (s *session) SendBytes(payload []byte) error {
	s.conn.SetWriteDeadline(time.Now().Add(time.Duration(s.config.GetTransport().WriteWaitMs) * time.Millisecond))
	return s.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (s *session) Close() {
	s.Lock()
	if s.stopped {
		return
	}
	s.stopped = true
	s.Unlock()

	s.logger.Warn("Closing client channel.", zap.String("remoteAddress", s.conn.RemoteAddr().String()))

	s.unregister(s)
	s.pingTicker.Stop()
	s.conn.Close()
}
