package app

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type LibrarySnapshot struct {
	State
	SyncCursor string `json:"syncCursor"`
}

type LibraryChanges struct {
	SyncCursor string   `json:"syncCursor"`
	Reset      bool     `json:"reset"`
	Tracks     []Track  `json:"tracks"`
	Removed    []string `json:"removed"`
	Metadata   *State   `json:"metadata,omitempty"`
}

func libraryMetadata(state State) State {
	state.Tracks = nil
	state.Sessions = nil
	return state
}

// Called under mu after a successful commit. Track versions coalesce repeated edits.
func (s *Store) recordLibraryChanges(next State, compareTracks bool, changed []string) {
	metadataChanged := !reflect.DeepEqual(libraryMetadata(s.state), libraryMetadata(next))
	if compareTracks {
		ids := make(map[string]bool, len(next.Tracks))
		for _, track := range next.Tracks {
			ids[track.ID] = true
			index, exists := s.trackIndex[track.ID]
			if !exists || !reflect.DeepEqual(s.state.Tracks[index], track) {
				changed = append(changed, track.ID)
			}
		}
		for _, track := range s.state.Tracks {
			if !ids[track.ID] {
				changed = append(changed, track.ID)
			}
		}
	}
	if !metadataChanged && len(changed) == 0 {
		return
	}
	s.syncRevision++
	if metadataChanged {
		s.syncMetadataRevision = s.syncRevision
	}
	for _, id := range changed {
		s.syncTracks[id] = s.syncRevision
	}
	// Bound removed-ID retention. A new epoch explicitly resets older clients.
	if len(s.syncTracks) > len(next.Tracks)+10000 {
		s.syncEpoch = newID()
		s.syncTracks = make(map[string]uint64)
	}
}

func (s *Store) syncCursor() string { return fmt.Sprintf("%s:%d", s.syncEpoch, s.syncRevision) }

func (s *Store) LibrarySnapshot() LibrarySnapshot {
	s.mu.RLock()
	state, cursor := s.state, s.syncCursor()
	s.mu.RUnlock()
	state.Sessions = nil
	state.Tracks = append([]Track{}, state.Tracks...)
	for i := range state.Tracks {
		state.Tracks[i].Lyrics = ""
		state.Tracks[i].Cover = ""
	}
	return LibrarySnapshot{State: clone(state), SyncCursor: cursor}
}

func (s *Store) LibraryChanges(cursor string) LibraryChanges {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := LibraryChanges{SyncCursor: s.syncCursor(), Tracks: []Track{}, Removed: []string{}}
	epoch, revision, ok := strings.Cut(cursor, ":")
	since, err := strconv.ParseUint(revision, 10, 64)
	if !ok || err != nil || epoch != s.syncEpoch || since > s.syncRevision {
		result.Reset = true
		return result
	}
	if since == s.syncRevision {
		return result
	}
	ids := []string{}
	for id, revision := range s.syncTracks {
		if revision > since {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if index, exists := s.trackIndex[id]; exists {
			track := s.state.Tracks[index]
			track.Lyrics = ""
			track.Cover = ""
			result.Tracks = append(result.Tracks, clone(track))
		} else {
			result.Removed = append(result.Removed, id)
		}
	}
	if s.syncMetadataRevision > since {
		metadata := clone(libraryMetadata(s.state))
		result.Metadata = &metadata
	}
	return result
}
