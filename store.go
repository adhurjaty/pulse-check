package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store holds every room, tells subscribers when a room changes, and saves
// all rooms to a single JSON file shortly after any change.
type Store struct {
	path string

	mu    sync.Mutex
	rooms map[string]*Room
	subs  map[string]map[*subscriber]struct{} // room code -> subscribers
	dirty bool

	flushMu sync.Mutex // serializes writes to the file
}

type subscriber struct {
	voter  string
	notify chan struct{} // buffered (1): a pending signal means "send a fresh snapshot"
}

// OpenStore loads saved rooms from path, if the file exists.
//
// A file that can't be parsed is moved aside (to path.corrupt-<time>) and
// the store starts empty, so a bad file can't stop the server from
// starting. A single damaged room is skipped, and the original file is
// kept the same way.
func OpenStore(path string) (*Store, error) {
	st := &Store{path: path, rooms: map[string]*Room{}, subs: map[string]map[*subscriber]struct{}{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		st.moveAside(fmt.Sprintf("can't parse %s: %v", path, err))
		return st, nil
	}
	skipped := false
	for code, msg := range raw {
		r := &Room{}
		if err := json.Unmarshal(msg, r); err != nil {
			log.Printf("store: skipping room %s: %v", code, err)
			skipped = true
			continue
		}
		st.rooms[r.s.Code] = r
	}
	if skipped {
		st.keepCopy("some rooms could not be loaded")
	}
	log.Printf("store: loaded %d room(s) from %s", len(st.rooms), path)
	return st, nil
}

func (st *Store) moveAside(reason string) {
	dest := fmt.Sprintf("%s.corrupt-%s", st.path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(st.path, dest); err != nil {
		log.Printf("store: %s, and moving it aside failed: %v", reason, err)
		return
	}
	log.Printf("store: %s; moved it to %s and started empty", reason, dest)
}

func (st *Store) keepCopy(reason string) {
	dest := fmt.Sprintf("%s.corrupt-%s", st.path, time.Now().UTC().Format("20060102T150405Z"))
	data, err := os.ReadFile(st.path)
	if err == nil {
		err = os.WriteFile(dest, data, 0o644)
	}
	if err != nil {
		log.Printf("store: %s, and saving a copy failed: %v", reason, err)
		return
	}
	log.Printf("store: %s; kept the original file as %s", reason, dest)
}

// Create makes a room with a new unique code.
func (st *Store) Create(set QuestionSet) *Room {
	st.mu.Lock()
	defer st.mu.Unlock()
	code := GenerateCode(func(c string) bool { _, taken := st.rooms[c]; return taken })
	r := NewRoom(code, set)
	st.rooms[code] = r
	st.dirty = true
	return r
}

// Get returns the room with this code, or nil.
func (st *Store) Get(code string) *Room {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.rooms[code]
}

// Changed records that a room was modified: it will be saved, and every
// subscriber to it is told to send a fresh snapshot.
func (st *Store) Changed(code string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dirty = true
	for s := range st.subs[code] {
		select {
		case s.notify <- struct{}{}:
		default: // a signal is already pending; one snapshot covers both changes
		}
	}
}

// Subscribe registers for change signals on a room. Call the returned
// function to unsubscribe.
func (st *Store) Subscribe(code, voter string) (*subscriber, func()) {
	s := &subscriber{voter: voter, notify: make(chan struct{}, 1)}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.subs[code] == nil {
		st.subs[code] = map[*subscriber]struct{}{}
	}
	st.subs[code][s] = struct{}{}
	return s, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		delete(st.subs[code], s)
		if len(st.subs[code]) == 0 {
			delete(st.subs, code)
		}
	}
}

// Subscribers returns how many clients are connected to a room.
func (st *Store) Subscribers(code string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.subs[code])
}

// Flush writes all rooms to disk if anything changed since the last write.
// The file is replaced atomically, so a crash mid-write can't corrupt it.
func (st *Store) Flush() error {
	st.flushMu.Lock()
	defer st.flushMu.Unlock()

	st.mu.Lock()
	if !st.dirty {
		st.mu.Unlock()
		return nil
	}
	st.dirty = false
	rooms := make(map[string]*Room, len(st.rooms))
	for code, r := range st.rooms {
		rooms[code] = r
	}
	st.mu.Unlock()

	err := writeFileAtomic(st.path, rooms)
	if err != nil {
		st.mu.Lock()
		st.dirty = true // try again next time
		st.mu.Unlock()
	}
	return err
}

func writeFileAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Run saves changes every interval until ctx is cancelled. Call Flush
// once more after Run returns to save the final state.
func (st *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Flush(); err != nil {
				log.Printf("store: save failed: %v", err)
			}
		}
	}
}
