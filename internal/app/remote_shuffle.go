package app

import "math/rand/v2"

// Track IDs, rather than queue positions, keep edits and duplicate occurrences
// from replaying a song within the same shuffle round. Called with mu held.
func (r *Remote) markShuffleLocked() {
	if !r.saved.Shuffle || len(r.saved.Queue) == 0 {
		return
	}
	id := r.saved.Queue[r.saved.Index]
	for _, seen := range r.saved.ShuffleVisited {
		if seen == id {
			return
		}
	}
	r.saved.ShuffleVisited = append(r.saved.ShuffleVisited, id)
}

func (r *Remote) resetShuffleLocked() {
	r.saved.ShuffleVisited = nil
	r.markShuffleLocked()
}

func (r *Remote) advanceShuffleLocked() bool {
	visited := make(map[string]bool, len(r.saved.ShuffleVisited))
	for _, id := range r.saved.ShuffleVisited {
		visited[id] = true
	}
	candidates := []int{}
	unique := map[string]bool{}
	for i, id := range r.saved.Queue {
		if !visited[id] && !unique[id] {
			candidates = append(candidates, i)
		}
		unique[id] = true
	}
	if len(candidates) == 0 {
		if r.saved.Repeat != "all" {
			return false
		}
		current := r.saved.Queue[r.saved.Index]
		unique = map[string]bool{}
		for i, id := range r.saved.Queue {
			if id != current && !unique[id] {
				candidates = append(candidates, i)
			}
			unique[id] = true
		}
		if len(candidates) == 0 {
			candidates = append(candidates, r.saved.Index)
		}
		r.saved.ShuffleVisited = nil
	}
	r.saved.Index = candidates[rand.IntN(len(candidates))]
	r.markShuffleLocked()
	return true
}
