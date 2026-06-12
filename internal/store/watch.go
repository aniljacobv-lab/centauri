package store

import "github.com/proxima360/centauri/internal/model"

// Subscribe registers a watcher that receives every event committed
// after this call. Agents subscribe instead of polling. Delivery is
// best-effort: if the buffer fills, messages are dropped (the log is
// the truth; the channel is a wake-up signal). The channel is closed on
// Unsubscribe or Close.
func (s *Store) Subscribe(buffer int) (int, <-chan *model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if buffer <= 0 {
		buffer = 256
	}
	s.nextSub++
	id := s.nextSub
	ch := make(chan *model.Event, buffer)
	if s.closed {
		close(ch) // a watcher on a closed store sees immediate EOF
		return id, ch
	}
	s.subs[id] = ch
	return id, ch
}

// Unsubscribe removes a watcher and closes its channel.
func (s *Store) Unsubscribe(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
}
