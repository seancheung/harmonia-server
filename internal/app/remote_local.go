package app

// OwnTone can reject pause when playback is already stopped. Confirm its state
// before transferring output, and recheck after a failed pause for natural ends.
func (r *Remote) pauseForLocal() error {
	inactive := func(state map[string]any) bool {
		return state["state"] == "stop" || state["state"] == "pause"
	}
	state, err := r.call("GET", "player", nil)
	if err != nil {
		return err
	}
	if inactive(state) {
		return nil
	}
	_, err = r.call("PUT", "player/pause", nil)
	if err != nil {
		state, checkErr := r.call("GET", "player", nil)
		if checkErr == nil && inactive(state) {
			return nil
		}
	}
	return err
}
