package app

import (
	"context"
	"testing"
)

func TestCancelledScanPreservesLibrary(t *testing.T) {
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Path: t.TempDir()}}
		st.Tracks = []Track{{ID: "t", SourceID: "s", Path: "missing.flac", Favorite: true, PlayCount: 7, Cover: "cover.jpg", HasCover: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.scanner.mu.Lock()
	a.scanner.cancel = cancel
	a.scanner.status = ScanStatus{Running: true}
	a.scanner.mu.Unlock()
	if res := request(t, a, "DELETE", "/api/scan", nil); res.Code != 202 {
		t.Fatal(res.Code)
	}
	if !a.scanner.Status().Stopping {
		t.Fatal("missing stopping status")
	}
	a.scanner.run(ctx, false)
	status := a.scanner.Status()
	if status.Running || status.Stopping || !status.Cancelled || len(status.Errors) > 0 {
		t.Fatalf("bad status: %+v", status)
	}
	a.scanner.refreshCovers(ctx)
	track := a.store.Read().Tracks[0]
	if track.Missing || !track.Favorite || track.PlayCount != 7 || track.Cover != "cover.jpg" {
		t.Fatal("cancelled scan changed saved data")
	}
	if !a.scanner.Start(false) {
		t.Fatal("cannot restart after cancellation")
	}
	a.scanner.wg.Wait()
	if a.scanner.Status().Cancelled {
		t.Fatal("cancelled flag not reset")
	}
}
