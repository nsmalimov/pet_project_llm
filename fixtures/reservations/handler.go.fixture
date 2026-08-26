package reservations

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Handler exposes POST /reserve {"room":"101","day":"2026-03-14T23:30:00-04:00"}.
// It is intentionally untested: the HTTP path is the integration surface a
// proof packet must report as unverified.
type Handler struct{ Store *Store }

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Room string `json:"room"`
		Day  string `json:"day"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	day, err := time.Parse(time.RFC3339, req.Day)
	if err != nil {
		http.Error(w, "day must be RFC3339", http.StatusBadRequest)
		return
	}
	res, err := h.Store.Reserve(req.Room, day)
	if errors.Is(err, ErrDuplicate) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
