// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"time"
)

// MessageInputHeld tells Coordination's leader that an agent is held at an
// input state the factory does not know, so that it brings the hold to the
// user.
const MessageInputHeld = "input.held"

// InputHold is an agent held at an input state its provider version's
// allow-list does not name (03 rules 24 and 25). Screen is what the agent's
// terminal showed, Job the job its work belongs to, zero for none.
type InputHold struct {
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	Job        int64  `json:"job,omitempty"`
	Provider   string `json:"provider"`
	Version    string `json:"version"`
	Screen     string `json:"screen"`
	// Waiting is the dispatch whose nudge waits, zero for none.
	Waiting int64 `json:"waiting,omitempty"`
}

// InputHoldLifted is the end of a hold: the agent is at a known input state
// again, or the user took its input.
type InputHoldLifted struct {
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	Why        string `json:"why"`
}

// ModalAnswered is the safe default answer the daemon typed at a modal on
// the allow-list.
type ModalAnswered struct {
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	Modal      string `json:"modal"`
	Provider   string `json:"provider"`
	Version    string `json:"version"`
	Keys       string `json:"keys"`
}

// InputHeld journals that agent is held, and, when there is a
// recipient, sends the recipient the hold in a message, whose dispatch it
// returns. Nothing is typed, and nothing the agent waits on is cancelled: its
// messages stay as dispatches that wait.
func (s *Store) InputHeld(h InputHold, recipient string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	if _, err := appendEntry(tx, at, KindInputHeld, h); err != nil {
		return Dispatch{}, err
	}
	var told Dispatch
	if recipient != "" {
		body := fmt.Sprintf("%s is held at an input state %s %s does not know, and nothing has been typed at it. Its work stops there, and the messages for it wait, kept, until it is past it: the factory never cancels or retries it. Tell the user, and keep independent work going. The user can look with `asmai attach %s`, and take the agent's input with `asmai take %s`.\nIt shows:\n%s", h.Agent, h.Provider, h.Version, h.Agent, h.Agent, indent(h.Screen))
		told, err = createMessage(tx, messageRef{}, h.Job, Daemon, recipient, MessageInputHeld, body, at)
		if err != nil {
			return Dispatch{}, err
		}
	}
	return told, tx.Commit()
}

// InputHoldLifted journals that a hold ended.
func (s *Store) InputHoldLifted(l InputHoldLifted, at time.Time) error {
	return s.journal(KindInputHoldLifted, l, at)
}

// ModalAnswered journals a modal answered from the allow-list.
func (s *Store) ModalAnswered(a ModalAnswered, at time.Time) error {
	return s.journal(KindModalAnswered, a, at)
}

func (s *Store) journal(kind string, data any, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := appendEntry(tx, at, kind, data); err != nil {
		return err
	}
	return tx.Commit()
}
