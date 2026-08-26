// Package reservations is a small in-memory booking store used by the HTTP
// handler in handler.go. A room can be reserved at most once per calendar
// day (UTC).
package reservations

import (
	"errors"
	"sync"
	"time"
)

var ErrDuplicate = errors.New("room already reserved for that day")

type Reservation struct {
	ID   int
	Room string
	Day  time.Time
}

type Store struct {
	mu    sync.Mutex
	next  int
	byKey map[string]Reservation
}

func NewStore() *Store {
	return &Store{byKey: map[string]Reservation{}}
}

// dayKey identifies a (room, calendar day) slot.
func dayKey(room string, day time.Time) string {
	return room + "/" + day.Format("2006-01-02")
}

// Reserve books room for the calendar day containing day. It returns
// ErrDuplicate when that slot is already taken.
func (s *Store) Reserve(room string, day time.Time) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := dayKey(room, day)
	if _, taken := s.byKey[key]; taken {
		return Reservation{}, ErrDuplicate
	}
	s.next++
	r := Reservation{ID: s.next, Room: room, Day: day}
	s.byKey[key] = r
	return r, nil
}

// Count returns the number of reservations held.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}
