package desktop

import "sync"

func (s *Server) subscribePermissions() (<-chan struct{}, func()) {
	s.mu.Lock()
	id := s.nextSubID
	s.nextSubID++
	updates := make(chan struct{}, 1)
	s.permissionSubs[id] = updates
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if existing, ok := s.permissionSubs[id]; ok {
				delete(s.permissionSubs, id)
				close(existing)
			}
			s.mu.Unlock()
		})
	}
	return updates, cancel
}

func (s *Server) notifyPermissionSubscribers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, subscriber := range s.permissionSubs {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}
