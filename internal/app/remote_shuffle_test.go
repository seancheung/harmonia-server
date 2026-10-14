package app

import (
	"encoding/json"
	"testing"
)

func TestRemoteShuffleRounds(t *testing.T) {
	for _, manual := range []bool{false, true} {
		r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b", "c", "a"}, Shuffle: true, Repeat: "all"}}
		r.resetShuffleLocked()
		last := ""
		for round := 0; round < 100; round++ {
			seen := map[string]bool{}
			for i := 0; i < 3; i++ {
				id := r.saved.Queue[r.saved.Index]
				if seen[id] || id == last {
					t.Fatalf("manual=%v round=%d repeated %s", manual, round, id)
				}
				seen[id] = true
				last = id
				if !r.advanceLocked(manual) {
					t.Fatal("repeat all stopped")
				}
			}
		}
	}
}

func TestRemoteShuffleEditsAndRestore(t *testing.T) {
	r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b", "c"}, Shuffle: true, Repeat: "off"}}
	r.resetShuffleLocked()
	if !r.advanceLocked(false) {
		t.Fatal("stopped early")
	}
	second := r.saved.Queue[r.saved.Index]
	// Reorder, remove the unplayed song, and append a new one.
	r.saved.Queue = []string{second, "a", "d"}
	r.saved.Index = 0
	b, err := json.Marshal(r.saved)
	if err != nil {
		t.Fatal(err)
	}
	restored := &Remote{}
	if err := json.Unmarshal(b, &restored.saved); err != nil {
		t.Fatal(err)
	}
	restored.markShuffleLocked()
	if !restored.advanceLocked(true) || restored.saved.Queue[restored.saved.Index] != "d" {
		t.Fatal("lost round progress")
	}
	if restored.advanceLocked(false) {
		t.Fatal("replayed a completed round without repeat")
	}
}

func TestRemoteShuffleSingleAndModeChanges(t *testing.T) {
	r := &Remote{saved: RemoteSaved{Queue: []string{"a", "a"}, Shuffle: true, Repeat: "off"}}
	r.resetShuffleLocked()
	if r.advanceLocked(false) {
		t.Fatal("duplicate queue entry replayed")
	}
	r.saved.Repeat = "all"
	if !r.advanceLocked(false) {
		t.Fatal("single song should loop")
	}
	r.saved.Queue = []string{"a", "b"}
	r.saved.Repeat = "single"
	if !r.advanceLocked(false) || r.saved.Index != 0 {
		t.Fatal("single repeat should retain song")
	}
	if !r.advanceLocked(true) || r.saved.Index != 1 {
		t.Fatal("manual next should bypass single repeat")
	}
}

func TestRemotePlayedRoundInSequentialMode(t *testing.T) {
	r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b", "c"}, Index: 1, Repeat: "all"}}
	r.resetShuffleLocked()
	if len(r.saved.ShuffleVisited) != 1 || r.saved.ShuffleVisited[0] != "b" {
		t.Fatal("marked unplayed prefix")
	}
	r.advanceLocked(false)
	if len(r.saved.ShuffleVisited) != 2 {
		t.Fatal("lost played track")
	}
	r.advanceLocked(false)
	if len(r.saved.ShuffleVisited) != 1 || r.saved.ShuffleVisited[0] != "a" {
		t.Fatal("did not reset at wrap")
	}
}

func TestRemoteStatusIncludesPlayedIDsWithoutQueue(t *testing.T) {
	a := seedBrowse(t)
	a.remote.mu.Lock()
	a.remote.saved.Queue = []string{"000", "001"}
	a.remote.saved.Index = 1
	a.remote.saved.ShuffleVisited = []string{"000", "001"}
	a.remote.mu.Unlock()
	var first struct{ QueueVersion string }
	res := request(t, a, "GET", "/api/remote", nil)
	if err := json.Unmarshal(res.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	res = request(t, a, "GET", "/api/remote?queueVersion="+first.QueueVersion, nil)
	var status struct {
		Queue     []Track
		PlayedIDs []string `json:"playedIds"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Queue != nil || len(status.PlayedIDs) != 2 || status.PlayedIDs[0] != "000" {
		t.Fatal(res.Body.String())
	}
}
