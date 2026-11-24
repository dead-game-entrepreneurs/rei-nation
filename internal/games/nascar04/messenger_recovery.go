package nascar04

import (
	"context"
	"time"
)

const (
	messengerAuthGrace     = 15 * time.Second
	messengerRecoveryGrace = 30 * time.Second
)

func (m *messenger) monitorConnections(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			m.recoverConnections(now)
		}
	}
}

func (m *messenger) recoverConnections(now time.Time) {
	m.lobby.mu.Lock()
	defer m.lobby.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	// A socket answering PING can still be waiting for AUTH. Expire it
	// independently of the lobby so peers sharing an address stay isolated.
	for conn, since := range m.pendingAuth {
		if now.Sub(since) >= messengerAuthGrace {
			delete(m.pendingAuth, conn)
			conn.CloseWithReason("Messenger sign-in timed out")
			m.lobby.opts.Log.Info("NASCAR Messenger sign-in timed out", "messenger_connection", conn.ID())
		}
	}
	for _, p := range m.lobby.online {
		if p.lkey == "" || p.race != nil || p.messengerLostAt.IsZero() || now.Sub(p.messengerLostAt) < messengerRecoveryGrace {
			continue
		}
		p.messengerLostAt = time.Time{}
		p.lkey = ""
		p.client.CloseWithReason("Messenger recovery timer")
		m.lobby.opts.Log.Info("NASCAR lobby disconnected to recover Messenger", "lobby_connection", p.client.ID())
	}
}
