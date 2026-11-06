package extensions

import (
	"crypto/rand"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

// StartSession opens an active-conversation lifecycle, closing its predecessor
// first. Empty IDs get a process-local identity for non-persisted sessions.
func (m *Manager) StartSession(id, cwd, reason string) {
	m.StartSessionWithAlias(id, "", cwd, reason)
}

// StartSessionWithAlias opens a lifecycle with an optional readable session alias.
// The alias is metadata, not a replacement for the conversation ID.
func (m *Manager) StartSessionWithAlias(id, alias, cwd, reason string) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.stopping {
		return
	}
	if cwd == "" {
		cwd = m.cwd
	}
	if id == "" && m.sessionID != "" && !m.sessionClosed && cwd == m.sessionCWD {
		return
	}
	if id != "" && id == m.sessionID && cwd == m.sessionCWD && !m.sessionClosed {
		return
	}
	if m.sessionID != "" && !m.sessionClosed {
		if reason == "" {
			reason = "session_switch"
		}
		m.emitEventLocked(extproto.EventFromHost{Event: "session_end", Reason: reason})
	}
	if id == "" {
		id = rand.Text()
	}
	m.sessionID, m.sessionAlias, m.sessionCWD, m.sessionClosed = id, alias, cwd, false
	m.emitEventLocked(extproto.EventFromHost{Event: "session_start"})
}

// SessionContext returns the active conversation identity and working directory.
func (m *Manager) SessionContext() (string, string) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.sessionID, m.sessionCWD
}

// EndSession emits at most one end notification per active lifecycle. It is
// observational: it does not close the persisted session or stop subprocesses.
func (m *Manager) EndSession(reason string) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.endSessionLocked(reason)
}

func (m *Manager) stopLifecycle() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.endSessionLocked("shutdown")
	m.stopping = true
}

func (m *Manager) endSessionLocked(reason string) {
	if m.sessionID == "" || m.sessionClosed {
		return
	}
	m.emitEventLocked(extproto.EventFromHost{Event: "session_end", Reason: reason})
	m.sessionClosed = true
}
