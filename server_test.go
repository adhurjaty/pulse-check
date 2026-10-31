package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testPin = "correct horse battery staple"

type testEnv struct {
	t     *testing.T
	srv   *httptest.Server
	store *Store
	api   *Server
}

func newTestEnv(t *testing.T, pin string) *testEnv {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	must(t, err)
	api := NewServer(store, pin)
	api.Heartbeat = 50 * time.Millisecond
	api.Coalesce = 10 * time.Millisecond
	api.FailDelay = 0
	srv := httptest.NewServer(routes(api))
	t.Cleanup(srv.Close)
	return &testEnv{t, srv, store, api}
}

// do sends a request and returns the status and body. A non-nil body is
// sent as-is if it is a string or []byte, and as JSON otherwise.
func (e *testEnv) do(method, path, pin string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		must(e.t, err)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	must(e.t, err)
	if pin != "" {
		req.Header.Set("X-Host-Pin", pin)
	}
	resp, err := http.DefaultClient.Do(req)
	must(e.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must(e.t, err)
	return resp.StatusCode, data
}

func (e *testEnv) expect(wantStatus int, method, path, pin string, body any) []byte {
	e.t.Helper()
	status, data := e.do(method, path, pin, body)
	if status != wantStatus {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, data)
	}
	return data
}

func (e *testEnv) createRoom() Snapshot {
	e.t.Helper()
	data, err := os.ReadFile("sample-questions.json")
	must(e.t, err)
	var snap Snapshot
	must(e.t, json.Unmarshal(e.expect(http.StatusCreated, "POST", "/api/rooms", testPin, data), &snap))
	return snap
}

func (e *testEnv) control(code, action string, from Snapshot) Snapshot {
	e.t.Helper()
	var snap Snapshot
	body := map[string]any{"action": action, "phase": from.Phase, "index": from.Index}
	must(e.t, json.Unmarshal(e.expect(http.StatusOK, "POST", "/api/rooms/"+code+"/control", testPin, body), &snap))
	return snap
}

func (e *testEnv) vote(code, voter string, question, choice int) {
	e.t.Helper()
	e.expect(http.StatusOK, "POST", "/api/rooms/"+code+"/vote", "",
		map[string]any{"voter": voter, "question": question, "choice": choice})
}

// stream is a test client for an event stream.
type stream struct {
	snaps  chan Snapshot
	lines  chan string
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *testEnv) openStream(code, voter string) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	u := e.srv.URL + "/api/rooms/" + code + "/events"
	if voter != "" {
		u += "?voter=" + voter
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	must(e.t, err)
	resp, err := http.DefaultClient.Do(req)
	must(e.t, err)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("events: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		e.t.Fatalf("events: content type %q", ct)
	}
	s := &stream{snaps: make(chan Snapshot, 1000), lines: make(chan string, 1000), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			s.lines <- line
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var snap Snapshot
				if err := json.Unmarshal([]byte(data), &snap); err == nil {
					s.snaps <- snap
				}
			}
		}
	}()
	e.t.Cleanup(cancel)
	return s
}

// waitFor reads snapshots until one matches, or fails after a timeout.
func (s *stream) waitFor(t *testing.T, what string, match func(Snapshot) bool) Snapshot {
	t.Helper()
	timeout := time.After(2 * time.Second)
	var last Snapshot
	for {
		select {
		case snap := <-s.snaps:
			if match(snap) {
				return snap
			}
			last = snap
		case <-timeout:
			t.Fatalf("timed out waiting for %s; last snapshot: %+v", what, last)
		}
	}
}

// A full session over HTTP, watched through event streams.
func TestFullSessionOverHTTP(t *testing.T) {
	e := newTestEnv(t, testPin)
	room := e.createRoom()
	if room.Phase != PhaseLobby || room.Total != 6 || !reflect.DeepEqual(room.Choices, defaultChoices) {
		t.Fatalf("unexpected new room %+v", room)
	}
	code := room.Code

	host := e.openStream(code, "")
	alice := e.openStream(strings.ToLower(code), "alice") // codes are case-insensitive
	host.waitFor(t, "initial snapshot", func(s Snapshot) bool { return s.Phase == PhaseLobby })

	for _, v := range []string{"alice", "bob", "carol", "alice"} {
		e.expect(http.StatusOK, "POST", "/api/rooms/"+code+"/join", "", map[string]string{"voter": v})
	}
	host.waitFor(t, "3 participants", func(s Snapshot) bool { return s.Participants == 3 })

	pos := e.control(code, ActionAdvance, room)
	alice.waitFor(t, "question 1", func(s Snapshot) bool {
		return s.Phase == PhaseVoting && s.Index == 0 && s.Question != nil && strings.HasPrefix(s.Question.Text, "A SaaS company")
	})

	e.vote(code, "alice", 0, 0)
	e.vote(code, "bob", 0, 3)
	e.vote(code, "carol", 0, 3)
	e.vote(code, "alice", 0, 1) // changes her mind
	snap := host.waitFor(t, "3 votes", func(s Snapshot) bool { return s.VotesCast == 3 })
	if snap.Counts != nil {
		t.Fatalf("host saw counts while voting was open: %v", snap.Counts)
	}
	alice.waitFor(t, "alice's changed vote", func(s Snapshot) bool { return s.MyVote != nil && *s.MyVote == 1 })

	pos = e.control(code, ActionAdvance, pos)
	host.waitFor(t, "results 1", func(s Snapshot) bool {
		return s.Phase == PhaseResults && reflect.DeepEqual(s.Counts, []int{0, 1, 0, 2})
	})
	alice.waitFor(t, "results on alice's phone", func(s Snapshot) bool {
		return s.Phase == PhaseResults && s.MyVote != nil && *s.MyVote == 1
	})

	for q := 1; q < 6; q++ {
		pos = e.control(code, ActionAdvance, pos) // voting q
		e.vote(code, "alice", q, 2)
		pos = e.control(code, ActionAdvance, pos) // results q
	}
	pos = e.control(code, ActionAdvance, pos)
	snap = host.waitFor(t, "summary", func(s Snapshot) bool { return s.Phase == PhaseDone })
	if len(snap.Summary) != 6 || !reflect.DeepEqual(snap.Summary[5].Counts, []int{0, 0, 1, 0}) {
		t.Fatalf("unexpected summary %+v", snap.Summary)
	}

	// GET returns the same snapshot the stream sends.
	var got Snapshot
	must(t, json.Unmarshal(e.expect(http.StatusOK, "GET", "/api/rooms/"+code+"?voter=alice", "", nil), &got))
	if got.Phase != PhaseDone || len(got.Summary) != 6 {
		t.Fatalf("GET snapshot: %+v", got)
	}

	// CSV export.
	status, data := e.do("GET", "/api/rooms/"+code+"/results.csv", testPin, nil)
	if status != http.StatusOK {
		t.Fatalf("csv: status %d", status)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	must(t, err)
	wantHeader := append(append([]string{"#", "Question"}, defaultChoices...), "Total")
	if len(rows) != 7 || !reflect.DeepEqual(rows[0], wantHeader) {
		t.Fatalf("csv header/rows: %q", rows)
	}
	if r := rows[1]; r[0] != "1" || !strings.HasPrefix(r[1], "A SaaS company") || !reflect.DeepEqual(r[2:], []string{"0", "1", "0", "2", "3"}) {
		t.Fatalf("csv row 1: %q", r)
	}
	if r := rows[6]; !reflect.DeepEqual(r[2:], []string{"0", "0", "1", "0", "1"}) {
		t.Fatalf("csv row 6: %q", r)
	}
}

func TestHostAuth(t *testing.T) {
	e := newTestEnv(t, testPin)
	code := e.createRoom().Code
	lobby := map[string]any{"action": "advance", "phase": "lobby", "index": 0}

	for _, pin := range []string{"", "wrong", testPin + "x", strings.ToUpper(testPin)} {
		e.expect(http.StatusUnauthorized, "POST", "/api/host/check", pin, nil)
		e.expect(http.StatusUnauthorized, "POST", "/api/rooms", pin, `{"title":"T","questions":[{"text":"q"}]}`)
		e.expect(http.StatusUnauthorized, "POST", "/api/rooms/"+code+"/control", pin, lobby)
		e.expect(http.StatusUnauthorized, "GET", "/api/rooms/"+code+"/results.csv", pin, nil)
	}
	e.expect(http.StatusNoContent, "POST", "/api/host/check", testPin, nil)
	// A PIN in the query string is not accepted.
	e.expect(http.StatusUnauthorized, "GET", "/api/rooms/"+code+"/results.csv?pin="+url.QueryEscape(testPin), "", nil)
	// Nothing changed.
	if pos := e.store.Get(code).Position(); pos != (Position{PhaseLobby, 0}) {
		t.Fatalf("unauthorized control moved room to %+v", pos)
	}
}

func TestHostRoutesDisabledWithoutPin(t *testing.T) {
	e := newTestEnv(t, "")
	data := e.expect(http.StatusServiceUnavailable, "POST", "/api/rooms", "", `{"title":"T","questions":[{"text":"q"}]}`)
	if !strings.Contains(string(data), "HOST_PIN is not set") {
		t.Fatalf("unexpected body %s", data)
	}
	e.expect(http.StatusServiceUnavailable, "POST", "/api/host/check", "anything", nil)
}

func TestCreateRejectsBadFiles(t *testing.T) {
	e := newTestEnv(t, testPin)
	data := e.expect(http.StatusUnprocessableEntity, "POST", "/api/rooms", testPin,
		`{"title": "", "questions": [{"txt": "q"}]}`)
	var body struct {
		Error    string   `json:"error"`
		Problems []string `json:"problems"`
	}
	must(t, json.Unmarshal(data, &body))
	want := []string{"title: must not be blank", `questions[0]: unknown field "txt"`, "questions[0].text: is required"}
	if body.Error != "invalid question file" || !reflect.DeepEqual(body.Problems, want) {
		t.Fatalf("got %+v", body)
	}

	huge := `{"title": "T", "questions": [{"text": "` + strings.Repeat("x", 300<<10) + `"}]}`
	e.expect(http.StatusRequestEntityTooLarge, "POST", "/api/rooms", testPin, huge)
}

func TestAPIErrors(t *testing.T) {
	e := newTestEnv(t, testPin)
	room := e.createRoom()
	code := room.Code
	base := "/api/rooms/" + code

	e.expect(http.StatusNotFound, "GET", "/api/rooms/ZZZZ", "", nil)
	e.expect(http.StatusNotFound, "GET", "/api/rooms/not-a-code", "", nil)
	e.expect(http.StatusNotFound, "POST", "/api/rooms/ZZZZ/join", "", map[string]string{"voter": "v1"})
	e.expect(http.StatusOK, "GET", "/api/rooms/"+strings.ToLower(code), "", nil)
	e.expect(http.StatusBadRequest, "GET", base+"?voter=bad%20id", "", nil)
	e.expect(http.StatusBadRequest, "GET", base+"/events?voter=bad%20id", "", nil)

	// Malformed bodies.
	e.expect(http.StatusBadRequest, "POST", base+"/join", "", `{`)
	e.expect(http.StatusBadRequest, "POST", base+"/join", "", `{"voter": "v1", "name": "x"}`)
	e.expect(http.StatusBadRequest, "POST", base+"/join", "", `{"voter": "v1"} {}`)
	e.expect(http.StatusBadRequest, "POST", base+"/join", "", `{"voter": "has spaces"}`)
	e.expect(http.StatusBadRequest, "POST", base+"/join", "", `{"voter": "`+strings.Repeat("a", 5000)+`"}`)

	// Voting rules.
	e.expect(http.StatusConflict, "POST", base+"/vote", "", map[string]any{"voter": "v1", "question": 0, "choice": 0})
	e.expect(http.StatusBadRequest, "POST", base+"/vote", "", map[string]any{"voter": "v1", "question": 0})
	pos := e.control(code, ActionAdvance, room)
	e.expect(http.StatusBadRequest, "POST", base+"/vote", "", map[string]any{"voter": "v1", "question": 0, "choice": 4})
	e.expect(http.StatusConflict, "POST", base+"/vote", "", map[string]any{"voter": "v1", "question": 1, "choice": 0})

	// Controls.
	e.expect(http.StatusBadRequest, "POST", base+"/control", testPin, map[string]any{"action": "jump", "phase": pos.Phase, "index": pos.Index})
	e.expect(http.StatusBadRequest, "POST", base+"/control", testPin, map[string]any{"action": "advance"})
	e.expect(http.StatusConflict, "POST", base+"/control", testPin, map[string]any{"action": "back", "phase": "lobby", "index": 0})

	// A double press: the second request carries a position the room has left.
	e.control(code, ActionAdvance, pos)
	e.expect(http.StatusConflict, "POST", base+"/control", testPin, map[string]any{"action": "advance", "phase": pos.Phase, "index": pos.Index})
	if got := e.store.Get(code).Position(); got != (Position{PhaseResults, 0}) {
		t.Fatalf("double press moved room to %+v", got)
	}
}

// Idle streams get heartbeats, and closed streams are unsubscribed.
func TestEventStreamHeartbeatAndCleanup(t *testing.T) {
	e := newTestEnv(t, testPin)
	code := e.createRoom().Code
	s := e.openStream(code, "v1")

	sawRetry, sawPing := false, false
	timeout := time.After(2 * time.Second)
	for !sawPing {
		select {
		case line := <-s.lines:
			sawRetry = sawRetry || line == "retry: 2000"
			sawPing = line == ": ping"
		case <-timeout:
			t.Fatal("no heartbeat received")
		}
	}
	if !sawRetry {
		t.Fatal("stream did not set a retry interval")
	}
	if n := e.store.Subscribers(code); n != 1 {
		t.Fatalf("subscribers = %d, want 1", n)
	}
	s.cancel()
	waitUntil(t, "subscriber removed", func() bool { return e.store.Subscribers(code) == 0 })
}

// Open streams end when the server shuts down (main relies on this).
func TestEventStreamEndsOnShutdown(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	must(t, err)
	api := NewServer(store, testPin)
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewUnstartedServer(routes(api))
	srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	srv.Start()
	defer srv.Close()
	e := &testEnv{t, srv, store, api}

	s := e.openStream(e.createRoom().Code, "")
	<-s.snaps
	cancel()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after shutdown")
	}
}

// A burst of votes reaches each client as a few updates, not one per vote.
func TestEventStreamCoalescesBursts(t *testing.T) {
	e := newTestEnv(t, testPin)
	e.api.Coalesce = 200 * time.Millisecond
	room := e.createRoom()
	e.control(room.Code, ActionAdvance, room)
	host := e.openStream(room.Code, "")
	host.waitFor(t, "voting", func(s Snapshot) bool { return s.Phase == PhaseVoting })

	for i := range 50 {
		e.vote(room.Code, fmt.Sprintf("v%d", i), 0, i%4)
	}
	host.waitFor(t, "50 votes", func(s Snapshot) bool { return s.VotesCast == 50 })
	time.Sleep(300 * time.Millisecond)
	updates := 0
	for len(host.snaps) > 0 {
		<-host.snaps
	}
	// Count lines already consumed by waitFor via the lines channel.
	for len(host.lines) > 0 {
		if strings.HasPrefix(<-host.lines, "data: ") {
			updates++
		}
	}
	if updates > 10 {
		t.Fatalf("got %d updates for 50 votes; expected them to be coalesced", updates)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
