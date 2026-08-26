package reservations

import (
	"errors"
	"testing"
	"time"
)

func TestReserveRejectsExactDuplicate(t *testing.T) {
	s := NewStore()
	day := time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)
	if _, err := s.Reserve("101", day); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := s.Reserve("101", day); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second reservation: want ErrDuplicate, got %v", err)
	}
}

func TestReserveDifferentDaysAndRooms(t *testing.T) {
	s := NewStore()
	d1 := time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)
	d2 := d1.Add(24 * time.Hour)
	for _, c := range []struct {
		room string
		day  time.Time
	}{{"101", d1}, {"101", d2}, {"102", d1}} {
		if _, err := s.Reserve(c.room, c.day); err != nil {
			t.Fatalf("reserve %s %s: %v", c.room, c.day, err)
		}
	}
	if got := s.Count(); got != 3 {
		t.Fatalf("count: want 3, got %d", got)
	}
}

// Clients send the day in their own zone. 2026-03-14T23:30 in New York
// (UTC-4) is 2026-03-15T03:30 UTC — the same UTC calendar day as a booking
// made at 2026-03-15T10:00 UTC, and must be rejected as a duplicate.
func TestReserveRejectsSameUTCDayAcrossTimezones(t *testing.T) {
	s := NewStore()
	ny := time.FixedZone("America/New_York", -4*60*60)
	local := time.Date(2026, 3, 14, 23, 30, 0, 0, ny)
	utc := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	if _, err := s.Reserve("101", local); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := s.Reserve("101", utc); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same UTC day from another zone: want ErrDuplicate, got %v (count=%d)", err, s.Count())
	}
}
