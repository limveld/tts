package mazeview

import "testing"

// TestSeatWrapsPastThePalette: max_seats is configurable and the palette is five
// long, so seat 5 has to come back round rather than panic. This lives here
// rather than in the bot because the palette does — the test needs to know how
// long it is, and exporting that just to be asserted on would be the tail wagging
// the dog.
func TestSeatWrapsPastThePalette(t *testing.T) {
	for _, n := range []int{0, 4, 5, 11, -1} {
		hex, emoji := Seat(n)
		if hex == "" || emoji == "" {
			t.Errorf("seat %d has no colour: %q/%q", n, hex, emoji)
		}
	}
	h0, e0 := Seat(0)
	if h, e := Seat(len(seats)); h != h0 || e != e0 {
		t.Errorf("seat %d gave %q/%q, want it wrapped back to %q/%q", len(seats), h, e, h0, e0)
	}
}

// TestSeatColoursAreDistinct: two runners the same colour is two players who
// cannot tell which square is theirs, and chat names the colour as the way to
// find yourself.
func TestSeatColoursAreDistinct(t *testing.T) {
	seenHex, seenEmoji := map[string]bool{}, map[string]bool{}
	for i := range seats {
		hex, emoji := Seat(i)
		if seenHex[hex] {
			t.Errorf("seat %d repeats colour %s", i, hex)
		}
		if seenEmoji[emoji] {
			t.Errorf("seat %d repeats emoji %s", i, emoji)
		}
		seenHex[hex], seenEmoji[emoji] = true, true
	}
}
