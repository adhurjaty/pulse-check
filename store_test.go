package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func corruptCopies(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	must(t, err)
	return matches
}

// Rooms, votes and positions survive a restart.
func TestStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := OpenStore(path)
	must(t, err)
	r := st.Create(testSet(3))
	mustStep(t, r, ActionAdvance)
	must(t, r.Vote("v1", 0, 2))
	must(t, r.Vote("v2", 0, 0))
	st.Changed(r.Code())
	other := st.Create(testSet(1))
	must(t, st.Flush())

	reopened, err := OpenStore(path)
	must(t, err)
	for _, want := range []*Room{r, other} {
		got := reopened.Get(want.Code())
		if got == nil {
			t.Fatalf("room %s missing after restart", want.Code())
		}
		for _, voter := range []string{"", "v1", "v2"} {
			if a, b := want.Snapshot(voter), got.Snapshot(voter); !reflect.DeepEqual(a, b) {
				t.Fatalf("room %s for %q differs after restart:\n%+v\n%+v", want.Code(), voter, a, b)
			}
		}
	}
	// New codes don't collide with loaded rooms, and the loaded rooms keep working.
	if c := reopened.Create(testSet(1)).Code(); c == r.Code() || c == other.Code() {
		t.Fatalf("reused code %s", c)
	}
	must(t, reopened.Get(r.Code()).Vote("v3", 0, 1))
}

func TestStoreFlushOnlyWhenChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := OpenStore(path)
	must(t, err)
	must(t, st.Flush())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("clean store wrote a file (stat err = %v)", err)
	}
}

func TestStoreRunSavesPeriodically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := OpenStore(path)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.Run(ctx, 10*time.Millisecond); close(done) }()
	code := st.Create(testSet(1)).Code()
	waitUntil(t, "periodic save", func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(data), code)
	})
	cancel()
	<-done
}

// An unparseable file is moved aside and the server starts empty.
func TestStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	must(t, os.WriteFile(path, []byte("{not json"), 0o644))
	st, err := OpenStore(path)
	must(t, err)
	if len(st.rooms) != 0 {
		t.Fatalf("loaded %d rooms from a corrupt file", len(st.rooms))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("corrupt file was left in place")
	}
	if copies := corruptCopies(t, dir); len(copies) != 1 {
		t.Fatalf("corrupt copies: %v", copies)
	}
}

// One damaged room is skipped; the others load, and the original is kept.
func TestStoreSkipsDamagedRoom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := OpenStore(path)
	must(t, err)
	good := st.Create(testSet(2)).Code()
	must(t, st.Flush())

	data, err := os.ReadFile(path)
	must(t, err)
	damaged := strings.Replace(string(data), "{", `{"BADD": {"code": "BADD", "phase": "nonsense"}, `, 1)
	must(t, os.WriteFile(path, []byte(damaged), 0o644))

	reopened, err := OpenStore(path)
	must(t, err)
	if reopened.Get(good) == nil || reopened.Get("BADD") != nil || len(reopened.rooms) != 1 {
		t.Fatalf("rooms after load: %v", reopened.rooms)
	}
	if copies := corruptCopies(t, dir); len(copies) != 1 {
		t.Fatalf("corrupt copies: %v", copies)
	}
}

// A failed save is retried on the next flush instead of being forgotten.
func TestStoreRetriesFailedSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "state.json")
	st, err := OpenStore(path)
	must(t, err)
	code := st.Create(testSet(1)).Code()
	must(t, os.RemoveAll(dir))
	if err := st.Flush(); err == nil {
		t.Fatal("expected flush to fail with the directory missing")
	}
	must(t, os.MkdirAll(dir, 0o755))
	must(t, st.Flush())
	data, err := os.ReadFile(path)
	must(t, err)
	if !strings.Contains(string(data), code) {
		t.Fatal("retried save is missing the room")
	}
}
