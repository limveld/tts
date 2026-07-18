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

func TestDonRelativeAndAbsolute(t *testing.T) {
	r, _, _, chat := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	broadcaster := func(text string) ChatMessage {
		m := emsg("host", text, true)
		m.IsBroadcaster = true
		return m
	}

	// Absolute set.
	r.Handle(broadcaster("!don 2500"))
	if got := r.depthPoints(); got != 2500 {
		t.Fatalf("after set: points=%d want 2500", got)
	}
	p, ok := ov.last("depth")
	if !ok || p.data.(depthData).Points != 2500 || p.data.(depthData).Tier != 3 {
		t.Fatalf("push=%+v want points=2500 tier=3", p.data)
	}

	// Relative add.
	r.Handle(broadcaster("!don +2000"))
	if got := r.depthPoints(); got != 4500 {
		t.Fatalf("after +2000: points=%d want 4500", got)
	}

	// Relative subtract.
	r.Handle(broadcaster("!don -1000"))
	if got := r.depthPoints(); got != 3500 {
		t.Fatalf("after -1000: points=%d want 3500", got)
	}

	// Clamp at 0.
	r.Handle(broadcaster("!don -99999"))
	if got := r.depthPoints(); got != 0 {
		t.Fatalf("after big subtract: points=%d want 0 (clamped)", got)
	}

	// Clamp at max.
	r.Handle(broadcaster("!don 99999"))
	if got := r.depthPoints(); got != depthMaxPoints {
		t.Fatalf("after big set: points=%d want %d (clamped)", got, depthMaxPoints)
	}

	if len(chat.replies) == 0 {
		t.Error("expected confirmation replies")
	}
}

func TestDonRequiresModOrBroadcaster(t *testing.T) {
	r, _, _, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	r.Handle(emsg("rando", "!don 5000", false)) // not mod/broadcaster
	if got := r.depthPoints(); got != 0 {
		t.Fatalf("non-mod changed depth to %d, want 0", got)
	}
	if _, ok := ov.last("depth"); ok {
		t.Error("non-mod !don should not push depth state")
	}
}
