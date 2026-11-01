// Command loadtest runs a full Pulse Check session against a server with
// many simulated students, and checks that every student sees every step
// promptly and that every vote is counted exactly once.
//
//	go run ./cmd/loadtest -url https://pulse.example.com -n 150 -pin-file ~/.config/pulse-check/host-pin
//
// Each student has its own connection (like separate phones), joins,
// keeps a live event stream open, and answers each question after a
// random delay. Some change their answer once. A few drop their stream
// mid-session and reconnect. The host side creates the room, waits for
// everyone, and steps through the session.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type snapshot struct {
	Code         string   `json:"code"`
	Phase        string   `json:"phase"`
	Index        int      `json:"index"`
	Total        int      `json:"total"`
	Choices      []string `json:"choices"`
	Participants int      `json:"participants"`
	VotesCast    int      `json:"votesCast"`
	Counts       []int    `json:"counts"`
	MyVote       *int     `json:"myVote"`
}

type position struct {
	Phase string
	Index int
}

func (p position) String() string {
	if p.Phase == "lobby" || p.Phase == "done" {
		return p.Phase
	}
	return fmt.Sprintf("%s %d", p.Phase, p.Index+1)
}

var (
	baseURL   string
	errorsMu  sync.Mutex
	errorList []string
	httpCalls atomic.Int64
)

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	errorsMu.Lock()
	errorList = append(errorList, msg)
	n := len(errorList)
	errorsMu.Unlock()
	if n <= 20 {
		log.Print("ERROR ", msg)
	}
}

func newClient() *http.Client {
	// A transport per student: separate TCP/TLS connections, like phones.
	return &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4}}
}

func call(ctx context.Context, c *http.Client, method, path string, body any, pin string, out any) (int, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if pin != "" {
		req.Header.Set("X-Host-Pin", pin)
	}
	httpCalls.Add(1)
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s %s: %v", method, path, err)
		}
	}
	return res.StatusCode, nil
}

// --- Students ------------------------------------------------------------

type student struct {
	n      int
	id     string
	client *http.Client
	code   string
	rng    *mrand.Rand

	mu      sync.Mutex
	seen    map[position]time.Time // first time each position arrived
	final   map[int]int            // question -> last choice the server accepted
	pending map[int]bool           // questions with a vote scheduled or sent

	dropAt     int // question index at which to drop the stream (-1: never)
	reconnects int
	offline    atomic.Bool // deliberately disconnected right now
}

func newStudent(n int, code string, dropAt int) *student {
	b := make([]byte, 12)
	rand.Read(b)
	return &student{
		n: n, id: "load-" + hex.EncodeToString(b), client: newClient(), code: code,
		rng:  mrand.New(mrand.NewPCG(uint64(n), 7)),
		seen: map[position]time.Time{}, final: map[int]int{}, pending: map[int]bool{},
		dropAt: dropAt,
	}
}

func (s *student) run(ctx context.Context, think time.Duration, done chan<- struct{}) {
	defer close(done)
	if _, err := call(ctx, s.client, "POST", "/api/rooms/"+s.code+"/join", map[string]string{"voter": s.id}, "", nil); err != nil {
		fail("student %d join: %v", s.n, err)
		return
	}
	for ctx.Err() == nil {
		finished, dropped := s.stream(ctx, think)
		if finished {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if dropped {
			// Simulated network loss: stay away a few seconds.
			s.offline.Store(true)
			s.reconnects++
			time.Sleep(3 * time.Second)
			s.offline.Store(false)
		} else {
			fail("student %d: event stream ended unexpectedly; reconnecting", s.n)
			s.reconnects++
			time.Sleep(2 * time.Second)
		}
	}
}

// stream reads the event stream until the session is done (finished), the
// student deliberately drops it (dropped), or it breaks.
func (s *student) stream(ctx context.Context, think time.Duration) (finished, dropped bool) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", baseURL+"/api/rooms/"+s.code+"/events?voter="+s.id, nil)
	req.Header.Set("Accept", "text/event-stream")
	httpCalls.Add(1)
	res, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			fail("student %d: open stream: %v", s.n, err)
		}
		return false, false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		fail("student %d: stream status %d", s.n, res.StatusCode)
		return false, false
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	event, data := "", ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event == "" && data != "" {
				var snap snapshot
				if err := json.Unmarshal([]byte(data), &snap); err != nil {
					fail("student %d: bad snapshot: %v", s.n, err)
				} else {
					if s.onSnapshot(ctx, snap, think) {
						return true, false
					}
					if snap.Phase == "voting" && snap.Index == s.dropAt && s.voted(snap.Index) {
						s.dropAt = -1
						return false, true
					}
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimPrefix(line[5:], " ")
		}
	}
	return false, false
}

func (s *student) voted(q int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.final[q]
	return ok
}

func (s *student) onSnapshot(ctx context.Context, snap snapshot, think time.Duration) bool {
	now := time.Now()
	pos := position{snap.Phase, snap.Index}
	s.mu.Lock()
	if _, ok := s.seen[pos]; !ok {
		s.seen[pos] = now
	}
	schedule := snap.Phase == "voting" && !s.pending[snap.Index]
	if schedule {
		s.pending[snap.Index] = true
	}
	s.mu.Unlock()
	if schedule {
		go s.answer(ctx, snap.Index, len(snap.Choices), think)
	}
	return snap.Phase == "done"
}

// answer votes after a random delay; 20% of students later change their mind.
func (s *student) answer(ctx context.Context, q, nChoices int, think time.Duration) {
	s.mu.Lock()
	delay := time.Duration(s.rng.Int64N(int64(think)))
	choice := s.rng.IntN(nChoices)
	change := s.rng.IntN(5) == 0
	second := (choice + 1 + s.rng.IntN(nChoices-1)) % nChoices
	s.mu.Unlock()

	if !s.sendVote(ctx, q, choice, delay) {
		return
	}
	if change {
		s.sendVote(ctx, q, second, time.Duration(s.rng.Int64N(int64(think/2)+1)))
	}
}

func (s *student) sendVote(ctx context.Context, q, choice int, delay time.Duration) bool {
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return false
	}
	var snap snapshot
	body := struct {
		Voter    string `json:"voter"`
		Question int    `json:"question"`
		Choice   int    `json:"choice"`
	}{s.id, q, choice}
	status, err := call(ctx, s.client, "POST", "/api/rooms/"+s.code+"/vote", body, "", &snap)
	if err != nil {
		if status == http.StatusConflict {
			return false // voting already closed: the host moved on
		}
		fail("student %d vote q%d: %v", s.n, q+1, err)
		return false
	}
	if snap.MyVote == nil || *snap.MyVote != choice {
		fail("student %d vote q%d: response myVote=%v, want %d", s.n, q+1, snap.MyVote, choice)
	}
	s.mu.Lock()
	s.final[q] = choice
	s.mu.Unlock()
	return true
}

// --- Host ----------------------------------------------------------------

type stepResult struct {
	pos       position
	latencies []time.Duration
	recovered []time.Duration // students who were offline when the host pressed
	missing   int
}

func main() {
	log.SetFlags(log.Ltime)
	url := flag.String("url", "http://localhost:8080", "server base URL")
	n := flag.Int("n", 150, "number of simulated students")
	pinFile := flag.String("pin-file", "", "file holding the host PIN (or set HOST_PIN)")
	questions := flag.String("questions", "sample-questions.json", "question file")
	think := flag.Duration("think", 8*time.Second, "students answer within this long of a question opening")
	ramp := flag.Duration("ramp", 10*time.Second, "spread student joins over this long")
	drops := flag.Int("drops", 10, "students that drop and reconnect their stream during question 2")
	pause := flag.Duration("results-pause", 2*time.Second, "time spent on each results screen")
	flag.Parse()
	baseURL = strings.TrimRight(*url, "/")

	pin := os.Getenv("HOST_PIN")
	if *pinFile != "" {
		data, err := os.ReadFile(expandHome(*pinFile))
		if err != nil {
			log.Fatal(err)
		}
		pin = strings.TrimSpace(string(data))
	}
	if pin == "" {
		log.Fatal("no host PIN: use -pin-file or HOST_PIN")
	}
	qfile, err := os.ReadFile(*questions)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	host := newClient()

	var room snapshot
	if _, err := call(ctx, host, "POST", "/api/rooms", qfile, pin, &room); err != nil {
		log.Fatalf("create room: %v", err)
	}
	log.Printf("room %s: %d questions, %d choices; %d students joining over %s", room.Code, room.Total, len(room.Choices), *n, *ramp)
	start := time.Now()

	students := make([]*student, *n)
	dones := make([]chan struct{}, *n)
	for i := range students {
		dropAt := -1
		if i < *drops && room.Total > 1 {
			dropAt = 1
		}
		students[i] = newStudent(i, room.Code, dropAt)
		dones[i] = make(chan struct{})
		go students[i].run(ctx, *think, dones[i])
		time.Sleep(*ramp / time.Duration(*n))
	}

	get := func() snapshot {
		var s snapshot
		if _, err := call(ctx, host, "GET", "/api/rooms/"+room.Code, nil, "", &s); err != nil {
			fail("host get: %v", err)
		}
		return s
	}
	waitFor := func(what string, timeout time.Duration, ok func(snapshot) bool) snapshot {
		deadline := time.Now().Add(timeout)
		for {
			s := get()
			if ok(s) {
				return s
			}
			if time.Now().After(deadline) {
				fail("timed out after %s waiting for %s (participants %d, votes %d)", timeout, what, s.Participants, s.VotesCast)
				return s
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	waitFor("everyone to join", 30*time.Second, func(s snapshot) bool { return s.Participants == *n })
	log.Printf("all %d joined in %s", *n, time.Since(start).Round(time.Millisecond))

	var steps []stepResult
	advance := func(from position) position {
		sent := time.Now()
		wasOffline := make([]bool, len(students))
		for i, st := range students {
			wasOffline[i] = st.offline.Load()
		}
		var s snapshot
		body := map[string]any{"action": "advance", "phase": from.Phase, "index": from.Index}
		if _, err := call(ctx, host, "POST", "/api/rooms/"+room.Code+"/control", body, pin, &s); err != nil {
			log.Fatalf("advance from %s: %v", from, err)
		}
		to := position{s.Phase, s.Index}
		// A double press must be rejected, not skip ahead.
		if status, _ := call(ctx, host, "POST", "/api/rooms/"+room.Code+"/control", body, pin, nil); status != http.StatusConflict {
			fail("repeated advance from %s: status %d, want 409", from, status)
		}
		// Wait for every student to see the new position.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if countSeen(students, to) == *n {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		r := stepResult{pos: to}
		for i, st := range students {
			st.mu.Lock()
			t, ok := st.seen[to]
			st.mu.Unlock()
			if ok && wasOffline[i] {
				r.recovered = append(r.recovered, max(0, t.Sub(sent)))
			} else if ok {
				r.latencies = append(r.latencies, max(0, t.Sub(sent)))
			} else {
				r.missing++
			}
		}
		steps = append(steps, r)
		p50, p95, mx := stats(r.latencies)
		line := fmt.Sprintf("→ %-10s seen by %d/%d  p50 %s  p95 %s  max %s", to, len(r.latencies)+len(r.recovered), *n, p50, p95, mx)
		if len(r.recovered) > 0 {
			_, _, rmax := stats(r.recovered)
			line += fmt.Sprintf("  (+%d offline at the press, caught up on reconnect by %s)", len(r.recovered), rmax)
		}
		log.Print(line)
		if r.missing > 0 {
			fail("%d students never saw %s", r.missing, to)
		}
		return to
	}

	pos := position{"lobby", 0}
	for q := 0; q < room.Total; q++ {
		pos = advance(pos) // open voting
		qStart := time.Now()
		// Wait until everyone has voted and any changed answers are in.
		waitFor(fmt.Sprintf("all votes on question %d", q+1), *think*3+15*time.Second, func(s snapshot) bool {
			return s.VotesCast == *n && time.Since(qStart) > *think*3/2
		})
		pos = advance(pos) // show results
		res := get()
		want := make([]int, len(room.Choices))
		for _, st := range students {
			st.mu.Lock()
			if c, ok := st.final[q]; ok {
				want[c]++
			}
			st.mu.Unlock()
		}
		if !slices.Equal(res.Counts, want) {
			fail("question %d counts %v, want %v", q+1, res.Counts, want)
		} else {
			log.Printf("  question %d counts %v match every student's final answer", q+1, res.Counts)
		}
		time.Sleep(*pause)
	}
	pos = advance(pos) // done
	if pos.Phase != "done" {
		fail("expected done, got %s", pos)
	}

	// Final CSV must agree with the students' answers too.
	checkCSV(ctx, host, room.Code, pin, students, room.Total, len(room.Choices))

	for _, d := range dones {
		select {
		case <-d:
		case <-time.After(10 * time.Second):
			fail("a student never saw the session finish")
		}
	}

	// Summary.
	var all, recovered []time.Duration
	missing := 0
	for _, r := range steps {
		all = append(all, r.latencies...)
		recovered = append(recovered, r.recovered...)
		missing += r.missing
	}
	reconnects := 0
	for _, st := range students {
		reconnects += st.reconnects
	}
	p50, p95, mx := stats(all)
	fmt.Println()
	fmt.Printf("Session %s: %d students, %d steps, %s total, %d HTTP requests\n", room.Code, *n, len(steps), time.Since(start).Round(time.Second), httpCalls.Load())
	fmt.Printf("Step latency (host press → student receives): p50 %s, p95 %s, max %s; %d missed\n", p50, p95, mx, missing)
	fmt.Printf("Stream reconnects: %d (%d planned); %d updates delivered on reconnect\n", reconnects, min(*drops, *n), len(recovered))
	if len(errorList) > 0 {
		fmt.Printf("FAIL: %d errors\n", len(errorList))
		os.Exit(1)
	}
	if mx > 2*time.Second {
		fmt.Printf("FAIL: slowest update took %s (limit 2s)\n", mx)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func countSeen(students []*student, p position) int {
	n := 0
	for _, st := range students {
		st.mu.Lock()
		if _, ok := st.seen[p]; ok {
			n++
		}
		st.mu.Unlock()
	}
	return n
}

func checkCSV(ctx context.Context, c *http.Client, code, pin string, students []*student, total, nChoices int) {
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/rooms/"+code+"/results.csv", nil)
	req.Header.Set("X-Host-Pin", pin)
	res, err := c.Do(req)
	if err != nil {
		fail("csv: %v", err)
		return
	}
	defer res.Body.Close()
	rows, err := csv.NewReader(res.Body).ReadAll()
	if err != nil || len(rows) != total+1 {
		fail("csv: %d rows, err %v", len(rows), err)
		return
	}
	for q := 0; q < total; q++ {
		want := make([]int, nChoices)
		for _, st := range students {
			if ch, ok := st.final[q]; ok {
				want[ch]++
			}
		}
		row := rows[q+1]
		got := make([]int, 0, nChoices)
		for _, cell := range row[len(row)-nChoices-1 : len(row)-1] {
			v, _ := strconv.Atoi(cell)
			got = append(got, v)
		}
		if !slices.Equal(got, want) {
			fail("csv question %d: %v, want %v (row %v)", q+1, got, want, row)
		}
	}
	log.Printf("CSV matches for all %d questions", total)
}

func stats(d []time.Duration) (p50, p95, mx time.Duration) {
	if len(d) == 0 {
		return
	}
	s := slices.Clone(d)
	slices.Sort(s)
	r := func(x time.Duration) time.Duration { return x.Round(time.Millisecond) }
	return r(s[len(s)/2]), r(s[len(s)*95/100]), r(s[len(s)-1])
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

