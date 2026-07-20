package main

import "testing"

func TestDepthTier(t *testing.T) {
	cases := []struct {
		points int64
		tier   int
	}{
		{0, 1}, {999, 1}, {1000, 2}, {1999, 2}, {2000, 3},
		{3999, 3}, {4000, 4}, {5999, 4}, {6000, 5}, {9999, 5}, {10000, 5},
	}
	for _, c := range cases {
		if got := depthTier(c.points); got != c.tier {
			t.Errorf("depthTier(%d) = %d, want %d", c.points, got, c.tier)
		}
	}
}

func broadcasterMsg(text string) ChatMessage {
	m := emsg("host", text, true)
	m.IsBroadcaster = true
	return m
}

func TestSetDepthAndPB(t *testing.T) {
	r, _, _, chat := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	// Set the rating; PB matches the first value.
	r.Handle(broadcasterMsg("!r 4200"))
	if got := r.depthPoints(); got != 4200 {
		t.Fatalf("after !r 4200: points=%d want 4200", got)
	}
	p, ok := ov.last("depth")
	if !ok {
		t.Fatal("no depth push")
	}
	if d := p.data.(depthData); d.Points != 4200 || d.Tier != 4 || d.PB != 4200 {
		t.Fatalf("push=%+v want points=4200 tier=4 pb=4200", d)
	}

	// A lower set leaves the PB untouched.
	r.Handle(broadcasterMsg("!r 3800"))
	if got, pb := r.depthPoints(), r.depthPB(); got != 3800 || pb != 4200 {
		t.Fatalf("after !r 3800: points=%d pb=%d want 3800/4200", got, pb)
	}

	// A new high raises the PB.
	r.Handle(broadcasterMsg("!r 5000"))
	if got, pb := r.depthPoints(), r.depthPB(); got != 5000 || pb != 5000 {
		t.Fatalf("after !r 5000: points=%d pb=%d want 5000/5000", got, pb)
	}

	// !don is an alias for the same set behavior.
	r.Handle(broadcasterMsg("!don 1500"))
	if got, pb := r.depthPoints(), r.depthPB(); got != 1500 || pb != 5000 {
		t.Fatalf("after !don 1500: points=%d pb=%d want 1500/5000", got, pb)
	}

	if len(chat.replies) == 0 {
		t.Error("expected confirmation replies")
	}
}

func TestSetDepthClamps(t *testing.T) {
	r, _, _, _ := econRouter(t)
	r.overlay = &fakeOverlay{}

	r.Handle(broadcasterMsg("!r -50"))
	if got := r.depthPoints(); got != 0 {
		t.Fatalf("negative set: points=%d want 0 (clamped)", got)
	}
	r.Handle(broadcasterMsg("!r 99999"))
	if got, pb := r.depthPoints(), r.depthPB(); got != depthMaxPoints || pb != depthMaxPoints {
		t.Fatalf("overflow set: points=%d pb=%d want %d/%d", got, pb, depthMaxPoints, depthMaxPoints)
	}
}

func TestSetDepthRequiresModOrBroadcaster(t *testing.T) {
	r, _, _, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	r.Handle(emsg("rando", "!r 5000", false)) // not mod/broadcaster
	if got := r.depthPoints(); got != 0 {
		t.Fatalf("non-mod changed depth to %d, want 0", got)
	}
	if _, ok := ov.last("depth"); ok {
		t.Error("non-mod !r should not push depth state")
	}
}
