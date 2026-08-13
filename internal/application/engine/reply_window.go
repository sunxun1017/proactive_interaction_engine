package engine

import (
	"sync/atomic"
	"time"
)

// ReplyAcceptanceWindow is the immutable application view used by input
// adapters to decide whether speech may become a canonical user reply.
type ReplyAcceptanceWindow struct {
	SubjectID string
	OpenedAt  time.Time
	Deadline  time.Time
}

type replyAcceptanceWindowStore struct {
	current atomic.Pointer[ReplyAcceptanceWindow]
}

func (s *replyAcceptanceWindowStore) publish(window ReplyAcceptanceWindow) {
	value := window
	s.current.Store(&value)
}

func (s *replyAcceptanceWindowStore) clear() {
	s.current.Store(nil)
}

func (s *replyAcceptanceWindowStore) load() (ReplyAcceptanceWindow, bool) {
	value := s.current.Load()
	if value == nil {
		return ReplyAcceptanceWindow{}, false
	}
	return *value, true
}

// CurrentReplyAcceptanceWindow returns a value copy safe for concurrent input
// adapter queries. Engine mutations remain owned by its single writer.
func (e *Engine) CurrentReplyAcceptanceWindow() (ReplyAcceptanceWindow, bool) {
	return e.replyWindow.load()
}
