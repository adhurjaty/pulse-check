package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

const (
	maxQuestionFileBytes = 256 << 10
	maxRequestBytes      = 4 << 10
)

// Server is the HTTP API.
type Server struct {
	store   *Store
	hostPin string

	// Heartbeat is how often an idle event stream sends a comment, so
	// proxies and phones don't drop the connection.
	Heartbeat time.Duration
	// Coalesce is how long an event stream waits after a change before
	// sending, so a burst of votes becomes one update.
	Coalesce time.Duration
	// FailDelay slows down wrong host PIN attempts.
	FailDelay time.Duration
}

func NewServer(store *Store, hostPin string) *Server {
	return &Server{
		store:     store,
		hostPin:   hostPin,
		Heartbeat: 15 * time.Second,
		Coalesce:  100 * time.Millisecond,
		FailDelay: time.Second,
	}
}

// Register adds the API routes to mux.
//
//	POST /api/host/check              host  check the host PIN
//	POST /api/rooms                   host  create a room; body is a question file
//	GET  /api/rooms/{code}            any   current snapshot (?voter=ID)
//	GET  /api/rooms/{code}/events     any   SSE stream of snapshots (?voter=ID)
//	POST /api/rooms/{code}/join       any   {"voter"}
//	POST /api/rooms/{code}/vote       any   {"voter", "question", "choice"}
//	POST /api/rooms/{code}/control    host  {"action": "advance"|"back", "phase", "index"}
//	GET  /api/rooms/{code}/results.csv host  results for every question
//
// Host routes need the X-Host-Pin header.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/host/check", s.host(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/rooms", s.host(s.handleCreate))
	mux.HandleFunc("GET /api/rooms/{code}", s.withRoom(s.handleGet))
	mux.HandleFunc("GET /api/rooms/{code}/events", s.withRoom(s.handleEvents))
	mux.HandleFunc("POST /api/rooms/{code}/join", s.withRoom(s.handleJoin))
	mux.HandleFunc("POST /api/rooms/{code}/vote", s.withRoom(s.handleVote))
	mux.HandleFunc("POST /api/rooms/{code}/control", s.host(s.withRoom(s.handleControl)))
	mux.HandleFunc("GET /api/rooms/{code}/results.csv", s.host(s.withRoom(s.handleCSV)))
}

// host rejects requests without the right X-Host-Pin. If no PIN is
// configured, host routes are disabled rather than left open.
func (s *Server) host(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hostPin == "" {
			writeError(w, http.StatusServiceUnavailable, "host controls are disabled because HOST_PIN is not set")
			return
		}
		pin := r.Header.Get("X-Host-Pin")
		if subtle.ConstantTimeCompare([]byte(pin), []byte(s.hostPin)) != 1 {
			time.Sleep(s.FailDelay)
			writeError(w, http.StatusUnauthorized, "wrong host PIN")
			return
		}
		h(w, r)
	}
}

type roomHandler func(w http.ResponseWriter, r *http.Request, room *Room)

func (s *Server) withRoom(h roomHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code, ok := NormalizeCode(r.PathValue("code"))
		var room *Room
		if ok {
			room = s.store.Get(code)
		}
		if room == nil {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		h(w, r, room)
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxQuestionFileBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("question file is larger than %d KB", maxQuestionFileBytes>>10))
			return
		}
		writeError(w, http.StatusBadRequest, "couldn't read request body")
		return
	}
	set, err := ParseQuestionSet(data)
	if err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":    "invalid question file",
				"problems": verr.Problems,
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	room := s.store.Create(set)
	snap := room.Snapshot("")
	log.Printf("room %s created: %q, %d questions", snap.Code, snap.Title, snap.Total)
	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, room *Room) {
	voter, ok := voterParam(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, room.Snapshot(voter))
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request, room *Room) {
	var req struct {
		Voter string `json:"voter"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	isNew, err := room.Join(req.Voter)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	if isNew {
		s.store.Changed(room.Code())
	}
	writeJSON(w, http.StatusOK, room.Snapshot(req.Voter))
}

func (s *Server) handleVote(w http.ResponseWriter, r *http.Request, room *Room) {
	var req struct {
		Voter    string `json:"voter"`
		Question *int   `json:"question"`
		Choice   *int   `json:"choice"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Question == nil || req.Choice == nil {
		writeError(w, http.StatusBadRequest, `"question" and "choice" are required`)
		return
	}
	if err := room.Vote(req.Voter, *req.Question, *req.Choice); err != nil {
		writeRoomError(w, err)
		return
	}
	snap := room.Snapshot(req.Voter)
	s.store.Changed(snap.Code)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request, room *Room) {
	var req struct {
		Action string `json:"action"`
		Phase  string `json:"phase"`
		Index  *int   `json:"index"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Phase == "" || req.Index == nil {
		writeError(w, http.StatusBadRequest, `"phase" and "index" (the room's current position) are required`)
		return
	}
	if err := room.Step(req.Action, Position{req.Phase, *req.Index}); err != nil {
		writeRoomError(w, err)
		return
	}
	snap := room.Snapshot("")
	s.store.Changed(snap.Code)
	log.Printf("room %s: %s -> %s %d", snap.Code, req.Action, snap.Phase, snap.Index)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleCSV(w http.ResponseWriter, r *http.Request, room *Room) {
	_, choices, results := room.Results()
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	header := append([]string{"#", "Question"}, choices...)
	cw.Write(append(header, "Total"))
	for i, q := range results {
		row := []string{strconv.Itoa(i + 1), q.Text}
		total := 0
		for _, c := range q.Counts {
			row = append(row, strconv.Itoa(c))
			total += c
		}
		cw.Write(append(row, strconv.Itoa(total)))
	}
	cw.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pulse-check-%s.csv"`, room.Code()))
	w.Write(buf.Bytes())
}

// handleEvents streams the room's snapshot as Server-Sent Events: once on
// connect, then after every change, with heartbeat "ping" events in between.
// The browser's EventSource reconnects on its own if the connection drops.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, room *Room) {
	voter, ok := voterParam(w, r)
	if !ok {
		return
	}
	code := room.Code()
	// Subscribe before sending the first snapshot, so no change is missed.
	sub, unsubscribe := s.store.Subscribe(code, voter)
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	send := func() error {
		data, err := json.Marshal(room.Snapshot(voter))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		return rc.Flush()
	}

	// Tell the browser to retry quickly after a dropped connection.
	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}
	if err := send(); err != nil {
		return
	}

	heartbeat := time.NewTicker(s.Heartbeat)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.notify:
			if s.Coalesce > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.Coalesce):
				}
				// Drop a signal that arrived while waiting; this send covers it.
				select {
				case <-sub.notify:
				default:
				}
			}
			if err := send(); err != nil {
				return
			}
		case <-heartbeat.C:
			// A named event (not a comment) so the page can see it and
			// detect a connection that has silently died.
			if _, err := io.WriteString(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func voterParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	voter := r.URL.Query().Get("voter")
	if voter != "" && !validVoterID(voter) {
		writeError(w, http.StatusBadRequest, ErrInvalidVoter.Error())
		return "", false
	}
	return voter, true
}

// decodeBody reads a small JSON request body strictly: unknown fields and
// trailing data are rejected.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid request body: unexpected data after JSON")
		return false
	}
	return true
}

func writeRoomError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrInvalidVoter), errors.Is(err, ErrInvalidChoice), errors.Is(err, ErrUnknownAction):
		status = http.StatusBadRequest
	case errors.Is(err, ErrVotingClosed), errors.Is(err, ErrWrongQuestion),
		errors.Is(err, ErrStale), errors.Is(err, ErrBadTransition):
		status = http.StatusConflict
	}
	writeError(w, status, err.Error())
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
