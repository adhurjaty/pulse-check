package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func testSet(n int) QuestionSet {
	set := QuestionSet{Title: "Test", Choices: append([]string(nil), defaultChoices...)}
	for i := range n {
		set.Questions = append(set.Questions, Question{Text: fmt.Sprintf("Q%d", i+1)})
	}
	return set
}

func mustStep(t *testing.T, r *Room, action string) {
	t.Helper()
	if err := r.Step(action, r.Position()); err != nil {
		t.Fatalf("%s from %+v: %v", action, r.Position(), err)
	}
}

// B1: new rooms start in the lobby with a well-formed code.
func TestNewRoom(t *testing.T) {
	r := NewRoom("ABCD", testSet(3))
	if got := r.Position(); got != (Position{PhaseLobby, 0}) {
		t.Fatalf("position = %+v", got)
	}
	s := r.Snapshot("")
	if s.Code != "ABCD" || s.Total != 3 || s.Participants != 0 || s.Question != nil {
		t.Fatalf("unexpected snapshot %+v", s)
	}
}

// B1: codes are 4 letters, never I or O, and never reuse a taken code.
func TestGenerateCode(t *testing.T) {
	taken := map[string]bool{}
	for range 10000 {
		code := GenerateCode(func(c string) bool { return taken[c] })
		if !validCode(code) || strings.ContainsAny(code, "IO") {
			t.Fatalf("bad code %q", code)
		}
		if taken[code] {
			t.Fatalf("duplicate code %q", code)
		}
		taken[code] = true
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]bool{
		" abcd ": true, "ABCD": true, "ABC": false, "ABCDE": false, "ABCI": false, "AB1D": false, "": false,
	} {
		code, ok := NormalizeCode(in)
		if ok != want {
			t.Errorf("NormalizeCode(%q) ok = %v, want %v", in, ok, want)
		}
		if ok && code != "ABCD" {
			t.Errorf("NormalizeCode(%q) = %q", in, code)
		}
	}
}

// B2, B3: advance walks the whole sequence, back walks it in reverse, and
// both stop at the ends without changing anything.
func TestSequence(t *testing.T) {
	r := NewRoom("ABCD", testSet(3))
	want := []Position{
		{PhaseLobby, 0},
		{PhaseVoting, 0}, {PhaseResults, 0},
		{PhaseVoting, 1}, {PhaseResults, 1},
		{PhaseVoting, 2}, {PhaseResults, 2},
		{PhaseDone, 0},
	}
	for i := 1; i < len(want); i++ {
		mustStep(t, r, ActionAdvance)
		if got := r.Position(); got != want[i] {
			t.Fatalf("advance %d: got %+v, want %+v", i, got, want[i])
		}
	}
	if err := r.Step(ActionAdvance, r.Position()); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("advance at done: got %v", err)
	}
	if got := r.Position(); got != want[len(want)-1] {
		t.Fatalf("advance at done changed position to %+v", got)
	}
	for i := len(want) - 2; i >= 0; i-- {
		mustStep(t, r, ActionBack)
		if got := r.Position(); got != want[i] {
			t.Fatalf("back to %d: got %+v, want %+v", i, got, want[i])
		}
	}
	if err := r.Step(ActionBack, r.Position()); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("back at lobby: got %v", err)
	}
	if err := r.Step("jump", r.Position()); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("unknown action: got %v", err)
	}
	if got := r.Position(); got != want[0] {
		t.Fatalf("rejected actions changed position to %+v", got)
	}
}

// B4: a repeated request from a position the room has left is rejected.
func TestDoublePressIsRejected(t *testing.T) {
	r := NewRoom("ABCD", testSet(3))
	mustStep(t, r, ActionAdvance) // voting 0
	from := r.Position()
	if err := r.Step(ActionAdvance, from); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(ActionAdvance, from); !errors.Is(err, ErrStale) {
		t.Fatalf("second press: got %v, want ErrStale", err)
	}
	if got := r.Position(); got != (Position{PhaseResults, 0}) {
		t.Fatalf("double press moved room to %+v", got)
	}
}

// B5: a single-question room goes straight from its results to done.
func TestSingleQuestion(t *testing.T) {
	r := NewRoom("ABCD", testSet(1))
	for _, want := range []Position{{PhaseVoting, 0}, {PhaseResults, 0}, {PhaseDone, 0}} {
		mustStep(t, r, ActionAdvance)
		if got := r.Position(); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	mustStep(t, r, ActionBack)
	if got := r.Position(); got != (Position{PhaseResults, 0}) {
		t.Fatalf("back from done: %+v", got)
	}
}

// C1: votes only count while the current question is open.
func TestVoteRejections(t *testing.T) {
	r := NewRoom("ABCD", testSet(2))
	if err := r.Vote("v1", 0, 0); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("vote in lobby: %v", err)
	}
	mustStep(t, r, ActionAdvance) // voting 0
	for _, tc := range []struct {
		voter            string
		question, choice int
		want             error
	}{
		{"", 0, 0, ErrInvalidVoter},
		{strings.Repeat("a", 65), 0, 0, ErrInvalidVoter},
		{"bad id!", 0, 0, ErrInvalidVoter},
		{"v1", 1, 0, ErrWrongQuestion},
		{"v1", 0, -1, ErrInvalidChoice},
		{"v1", 0, 4, ErrInvalidChoice},
	} {
		if err := r.Vote(tc.voter, tc.question, tc.choice); !errors.Is(err, tc.want) {
			t.Errorf("Vote(%q, %d, %d) = %v, want %v", tc.voter, tc.question, tc.choice, err, tc.want)
		}
	}
	mustStep(t, r, ActionAdvance) // results 0
	if err := r.Vote("v1", 0, 0); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("vote after results: %v", err)
	}
	mustStep(t, r, ActionAdvance) // voting 1
	if err := r.Vote("v1", 0, 0); !errors.Is(err, ErrWrongQuestion) {
		t.Fatalf("late vote for previous question: %v", err)
	}
	if s := r.Snapshot(""); s.Participants != 0 {
		t.Fatalf("rejected votes registered %d participants", s.Participants)
	}
}

// C2-C5: re-votes replace, reopening keeps votes, questions are independent,
// voting registers the voter, and joining twice counts once.
func TestVoting(t *testing.T) {
	r := NewRoom("ABCD", testSet(2))
	if isNew, err := r.Join("v1"); err != nil || !isNew {
		t.Fatalf("first join: %v %v", isNew, err)
	}
	if isNew, err := r.Join("v1"); err != nil || isNew {
		t.Fatalf("second join: %v %v", isNew, err)
	}
	if _, err := r.Join("not valid"); !errors.Is(err, ErrInvalidVoter) {
		t.Fatalf("join with bad ID: %v", err)
	}

	mustStep(t, r, ActionAdvance) // voting 0
	must(t, r.Vote("v1", 0, 0))
	must(t, r.Vote("v1", 0, 3)) // changes their mind
	must(t, r.Vote("v2", 0, 3)) // never joined explicitly
	must(t, r.Vote("v3", 0, 1))
	if s := r.Snapshot(""); s.VotesCast != 3 || s.Participants != 3 {
		t.Fatalf("votesCast=%d participants=%d, want 3 and 3", s.VotesCast, s.Participants)
	}

	mustStep(t, r, ActionAdvance) // results 0
	if got := r.Snapshot("").Counts; !reflect.DeepEqual(got, []int{0, 1, 0, 2}) {
		t.Fatalf("counts = %v", got)
	}

	mustStep(t, r, ActionBack) // reopen voting 0
	if s := r.Snapshot("v1"); s.VotesCast != 3 || s.MyVote == nil || *s.MyVote != 3 {
		t.Fatalf("reopen lost votes: %+v", s)
	}
	must(t, r.Vote("v3", 0, 2))
	mustStep(t, r, ActionAdvance) // results 0
	if got := r.Snapshot("").Counts; !reflect.DeepEqual(got, []int{0, 0, 1, 2}) {
		t.Fatalf("counts after reopen = %v", got)
	}

	mustStep(t, r, ActionAdvance) // voting 1
	if s := r.Snapshot("v1"); s.VotesCast != 0 || s.MyVote != nil {
		t.Fatalf("question 2 inherited votes: %+v", s)
	}
	must(t, r.Vote("v1", 1, 0))
	mustStep(t, r, ActionAdvance) // results 1
	mustStep(t, r, ActionAdvance) // done
	s := r.Snapshot("")
	want := []QuestionResult{{"Q1", []int{0, 0, 1, 2}}, {"Q2", []int{1, 0, 0, 0}}}
	if !reflect.DeepEqual(s.Summary, want) {
		t.Fatalf("summary = %+v", s.Summary)
	}
}

// C6: concurrent votes are all counted, under the race detector.
func TestConcurrentVotes(t *testing.T) {
	r := NewRoom("ABCD", testSet(1))
	mustStep(t, r, ActionAdvance)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			if err := r.Vote(fmt.Sprintf("voter-%d", i), 0, i%4); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() { r.Snapshot(fmt.Sprintf("voter-%d", i)) })
	}
	wg.Wait()
	mustStep(t, r, ActionAdvance)
	s := r.Snapshot("")
	if s.VotesCast != 200 || s.Participants != 200 || !reflect.DeepEqual(s.Counts, []int{50, 50, 50, 50}) {
		t.Fatalf("votesCast=%d participants=%d counts=%v", s.VotesCast, s.Participants, s.Counts)
	}
}

// D1-D4: what clients can see in each phase.
func TestSnapshotVisibility(t *testing.T) {
	r := NewRoom("ABCD", testSet(2))
	mustStep(t, r, ActionAdvance) // voting 0
	must(t, r.Vote("alice-id", 0, 0))
	must(t, r.Vote("bob-id", 0, 3))

	// D1: no counts while voting; only progress and your own vote.
	s := r.Snapshot("alice-id")
	if s.Counts != nil {
		t.Fatalf("counts visible during voting: %v", s.Counts)
	}
	if s.VotesCast != 2 || s.MyVote == nil || *s.MyVote != 0 || s.Question.Text != "Q1" {
		t.Fatalf("unexpected voting snapshot %+v", s)
	}
	raw := mustJSON(t, s)
	if strings.Contains(raw, "counts") {
		t.Fatalf("serialized snapshot mentions counts during voting: %s", raw)
	}

	// D4: neither the host nor another student sees anyone's ID or choice.
	for _, viewer := range []string{"", "alice-id", "carol-id"} {
		raw := mustJSON(t, r.Snapshot(viewer))
		for _, id := range []string{"alice-id", "bob-id"} {
			if strings.Contains(raw, id) {
				t.Fatalf("snapshot for %q leaks voter ID %q: %s", viewer, id, raw)
			}
		}
	}
	if s := r.Snapshot("carol-id"); s.MyVote != nil {
		t.Fatalf("non-voter got myVote %d", *s.MyVote)
	}
	if s := r.Snapshot(""); s.MyVote != nil {
		t.Fatal("host snapshot has myVote")
	}

	// D2: results show counts that add up to the votes cast.
	mustStep(t, r, ActionAdvance)
	s = r.Snapshot("bob-id")
	sum := 0
	for _, c := range s.Counts {
		sum += c
	}
	if sum != s.VotesCast || !reflect.DeepEqual(s.Counts, []int{1, 0, 0, 1}) || *s.MyVote != 3 {
		t.Fatalf("unexpected results snapshot %+v", s)
	}

	// D3: done has a summary for every question, including unanswered ones.
	mustStep(t, r, ActionAdvance)
	mustStep(t, r, ActionAdvance)
	mustStep(t, r, ActionAdvance)
	s = r.Snapshot("")
	if s.Phase != PhaseDone || len(s.Summary) != 2 || s.Question != nil ||
		!reflect.DeepEqual(s.Summary[1].Counts, []int{0, 0, 0, 0}) {
		t.Fatalf("unexpected done snapshot %+v", s)
	}
}

// D5: a room saved to JSON and loaded back is identical.
func TestRoomJSONRoundTrip(t *testing.T) {
	r := NewRoom("ABCD", testSet(3))
	must(t, r.Step(ActionAdvance, r.Position()))
	must(t, r.Vote("v1", 0, 2))
	must(t, r.Vote("v2", 0, 1))
	mustStep(t, r, ActionAdvance)
	mustStep(t, r, ActionAdvance) // voting 1
	must(t, r.Vote("v1", 1, 3))
	_, _ = r.Join("v3")

	first, err := json.Marshal(r)
	must(t, err)
	var loaded Room
	must(t, json.Unmarshal(first, &loaded))
	second, err := json.Marshal(&loaded)
	must(t, err)
	if string(first) != string(second) {
		t.Fatalf("round trip changed state:\n%s\n%s", first, second)
	}
	for _, viewer := range []string{"", "v1", "v2", "v3"} {
		if a, b := r.Snapshot(viewer), loaded.Snapshot(viewer); !reflect.DeepEqual(a, b) {
			t.Fatalf("snapshot for %q differs after reload:\n%+v\n%+v", viewer, a, b)
		}
	}
	// The loaded room keeps working.
	must(t, loaded.Vote("v2", 1, 0))
}

// A damaged save file is rejected instead of causing a panic later.
func TestRoomUnmarshalRejectsBadState(t *testing.T) {
	r := NewRoom("ABCD", testSet(2))
	mustStep(t, r, ActionAdvance)
	must(t, r.Vote("v1", 0, 1))
	good := mustJSON(t, r)

	for name, corrupt := range map[string]func(map[string]any){
		"bad code":       func(m map[string]any) { m["code"] = "abc" },
		"no questions":   func(m map[string]any) { m["questionSet"].(map[string]any)["questions"] = []any{} },
		"unknown phase":  func(m map[string]any) { m["phase"] = "paused" },
		"index too high": func(m map[string]any) { m["index"] = 2 },
		"votes length":   func(m map[string]any) { m["votes"] = []any{} },
		"vote out of range": func(m map[string]any) {
			m["votes"] = []any{map[string]any{"v1": 9}, map[string]any{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			must(t, json.Unmarshal([]byte(good), &m))
			corrupt(m)
			var loaded Room
			if err := json.Unmarshal([]byte(mustJSON(t, m)), &loaded); !errors.Is(err, ErrCorruptedState) {
				t.Fatalf("got %v, want ErrCorruptedState", err)
			}
		})
	}

	// Missing maps are filled in, and voters are restored as participants.
	var loaded Room
	must(t, json.Unmarshal([]byte(`{"code":"ABCD","questionSet":{"title":"T","choices":["a","b"],"questions":[{"text":"q"}]},
		"phase":"voting","index":0,"votes":[{"v1":1}]}`), &loaded))
	if s := loaded.Snapshot("v1"); s.Participants != 1 || s.VotesCast != 1 {
		t.Fatalf("unexpected snapshot %+v", s)
	}
	if _, err := loaded.Join("v2"); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}
