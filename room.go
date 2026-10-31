package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	PhaseLobby   = "lobby"
	PhaseVoting  = "voting"
	PhaseResults = "results"
	PhaseDone    = "done"

	ActionAdvance = "advance"
	ActionBack    = "back"
)

var (
	ErrInvalidVoter   = errors.New("invalid voter ID")
	ErrVotingClosed   = errors.New("voting is closed")
	ErrWrongQuestion  = errors.New("that question is no longer open for voting")
	ErrInvalidChoice  = errors.New("invalid choice")
	ErrStale          = errors.New("the room has already moved on")
	ErrBadTransition  = errors.New("that action isn't available right now")
	ErrUnknownAction  = errors.New("unknown action")
	ErrCorruptedState = errors.New("saved room state is invalid")
)

// Position is where a room is in its sequence:
// lobby -> voting(0) -> results(0) -> voting(1) -> ... -> results(n-1) -> done.
type Position struct {
	Phase string `json:"phase"`
	Index int    `json:"index"`
}

// Room is one live session. It is safe for concurrent use.
type Room struct {
	mu sync.Mutex
	s  roomState
}

// roomState is everything that is saved to disk.
type roomState struct {
	Code         string               `json:"code"`
	CreatedAt    time.Time            `json:"createdAt"`
	Set          QuestionSet          `json:"questionSet"`
	Phase        string               `json:"phase"`
	Index        int                  `json:"index"`
	Participants map[string]time.Time `json:"participants"` // voter ID -> first seen
	Votes        []map[string]int     `json:"votes"`        // per question: voter ID -> choice index
}

func NewRoom(code string, set QuestionSet) *Room {
	r := &Room{s: roomState{
		Code:         code,
		CreatedAt:    time.Now().UTC(),
		Set:          set,
		Phase:        PhaseLobby,
		Participants: map[string]time.Time{},
		Votes:        make([]map[string]int, len(set.Questions)),
	}}
	for i := range r.s.Votes {
		r.s.Votes[i] = map[string]int{}
	}
	return r
}

func (r *Room) Position() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Position{r.s.Phase, r.s.Index}
}

// Join registers a voter. It reports whether the voter was new.
func (r *Room) Join(voter string) (bool, error) {
	if !validVoterID(voter) {
		return false, ErrInvalidVoter
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.joinLocked(voter), nil
}

func (r *Room) joinLocked(voter string) bool {
	if _, ok := r.s.Participants[voter]; ok {
		return false
	}
	r.s.Participants[voter] = time.Now().UTC()
	return true
}

// Vote records or replaces a voter's choice for the open question.
func (r *Room) Vote(voter string, question, choice int) error {
	if !validVoterID(voter) {
		return ErrInvalidVoter
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.s.Phase != PhaseVoting {
		return ErrVotingClosed
	}
	if question != r.s.Index {
		return ErrWrongQuestion
	}
	if choice < 0 || choice >= len(r.s.Set.Choices) {
		return ErrInvalidChoice
	}
	r.joinLocked(voter)
	r.s.Votes[question][voter] = choice
	return nil
}

// Step moves the room one position forward ("advance") or back ("back").
// from must match the room's current position, so a repeated request (such
// as a double-pressed clicker) is rejected with ErrStale instead of
// skipping ahead.
func (r *Room) Step(action string, from Position) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if from != (Position{r.s.Phase, r.s.Index}) {
		return ErrStale
	}
	n := len(r.s.Set.Questions)
	switch action {
	case ActionAdvance:
		switch r.s.Phase {
		case PhaseLobby:
			r.s.Phase, r.s.Index = PhaseVoting, 0
		case PhaseVoting:
			r.s.Phase = PhaseResults
		case PhaseResults:
			if r.s.Index+1 < n {
				r.s.Phase, r.s.Index = PhaseVoting, r.s.Index+1
			} else {
				r.s.Phase, r.s.Index = PhaseDone, 0
			}
		default:
			return ErrBadTransition
		}
	case ActionBack:
		switch r.s.Phase {
		case PhaseVoting:
			if r.s.Index == 0 {
				r.s.Phase = PhaseLobby
			} else {
				r.s.Phase, r.s.Index = PhaseResults, r.s.Index-1
			}
		case PhaseResults:
			r.s.Phase = PhaseVoting // reopen voting; existing votes are kept
		case PhaseDone:
			r.s.Phase, r.s.Index = PhaseResults, n-1
		default:
			return ErrBadTransition
		}
	default:
		return ErrUnknownAction
	}
	return nil
}

type QuestionResult struct {
	Text   string `json:"text"`
	Counts []int  `json:"counts"`
}

// Snapshot is what a client receives. It never contains other voters' IDs
// or choices, and it has no per-choice counts while voting is open.
type Snapshot struct {
	Code         string           `json:"code"`
	Title        string           `json:"title"`
	Phase        string           `json:"phase"`
	Index        int              `json:"index"`
	Total        int              `json:"total"`
	Choices      []string         `json:"choices"`
	Participants int              `json:"participants"`
	Question     *Question        `json:"question,omitempty"`
	VotesCast    int              `json:"votesCast"`
	Counts       []int            `json:"counts,omitempty"`
	MyVote       *int             `json:"myVote,omitempty"`
	Summary      []QuestionResult `json:"summary,omitempty"`
}

// Snapshot returns the room as seen by voter (or by the host, if voter is "").
func (r *Room) Snapshot(voter string) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		Code:         r.s.Code,
		Title:        r.s.Set.Title,
		Phase:        r.s.Phase,
		Index:        r.s.Index,
		Total:        len(r.s.Set.Questions),
		Choices:      append([]string(nil), r.s.Set.Choices...),
		Participants: len(r.s.Participants),
	}
	switch r.s.Phase {
	case PhaseVoting, PhaseResults:
		q := r.s.Set.Questions[r.s.Index]
		s.Question = &q
		s.VotesCast = len(r.s.Votes[r.s.Index])
		if v, ok := r.s.Votes[r.s.Index][voter]; ok && voter != "" {
			s.MyVote = &v
		}
		if r.s.Phase == PhaseResults {
			s.Counts = r.countsLocked(r.s.Index)
		}
	case PhaseDone:
		for i, q := range r.s.Set.Questions {
			s.Summary = append(s.Summary, QuestionResult{Text: q.Text, Counts: r.countsLocked(i)})
		}
	}
	return s
}

func (r *Room) countsLocked(i int) []int {
	c := make([]int, len(r.s.Set.Choices))
	for _, v := range r.s.Votes[i] {
		c[v]++
	}
	return c
}

func (r *Room) MarshalJSON() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.Marshal(r.s)
}

// UnmarshalJSON loads a saved room and checks that it is internally
// consistent, so a damaged file can't cause a panic later.
func (r *Room) UnmarshalJSON(data []byte) error {
	var s roomState
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n := len(s.Set.Questions)
	switch {
	case !validCode(s.Code):
		return fmt.Errorf("%w: bad code %q", ErrCorruptedState, s.Code)
	case n == 0 || len(s.Set.Choices) < minChoices:
		return fmt.Errorf("%w: room %s has no questions or choices", ErrCorruptedState, s.Code)
	case s.Phase != PhaseLobby && s.Phase != PhaseVoting && s.Phase != PhaseResults && s.Phase != PhaseDone:
		return fmt.Errorf("%w: room %s has unknown phase %q", ErrCorruptedState, s.Code, s.Phase)
	case s.Index < 0 || s.Index >= n:
		return fmt.Errorf("%w: room %s has question index %d of %d", ErrCorruptedState, s.Code, s.Index, n)
	case len(s.Votes) != n:
		return fmt.Errorf("%w: room %s has votes for %d questions, expected %d", ErrCorruptedState, s.Code, len(s.Votes), n)
	}
	if s.Participants == nil {
		s.Participants = map[string]time.Time{}
	}
	for i, votes := range s.Votes {
		if votes == nil {
			s.Votes[i] = map[string]int{}
		}
		for voter, c := range votes {
			if c < 0 || c >= len(s.Set.Choices) {
				return fmt.Errorf("%w: room %s has an out-of-range vote", ErrCorruptedState, s.Code)
			}
			if _, ok := s.Participants[voter]; !ok {
				s.Participants[voter] = s.CreatedAt
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s = s
	return nil
}

// Room codes are 4 letters, without I or O, which are easy to misread.
const (
	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	codeLength   = 4
)

// GenerateCode returns a random room code for which taken returns false.
func GenerateCode(taken func(string) bool) string {
	max := big.NewInt(int64(len(codeAlphabet)))
	for {
		b := make([]byte, codeLength)
		for i := range b {
			n, err := rand.Int(rand.Reader, max)
			if err != nil {
				panic(err) // crypto/rand never fails on supported platforms
			}
			b[i] = codeAlphabet[n.Int64()]
		}
		if code := string(b); !taken(code) {
			return code
		}
	}
}

// NormalizeCode upper-cases and trims user input, and reports whether the
// result is a well-formed room code.
func NormalizeCode(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	return s, validCode(s)
}

func validCode(s string) bool {
	if len(s) != codeLength {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(codeAlphabet, c) {
			return false
		}
	}
	return true
}

// validVoterID accepts 1-64 characters of [A-Za-z0-9_-], which covers the
// UUIDs that browsers generate.
func validVoterID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
