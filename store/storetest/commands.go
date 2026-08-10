package storetest

import (
	"testing"

	"tts/store"
)

func testCommandCRUD(t *testing.T, s Store) {
	created, err := s.Add(store.Command{Name: "discord", Response: "join $user", Cooldown: 5})
	if err != nil || !created {
		t.Fatalf("Add discord: created=%v err=%v", created, err)
	}
	// Duplicate add: not created, no error.
	if created, err := s.Add(store.Command{Name: "discord", Response: "other"}); err != nil || created {
		t.Fatalf("Add duplicate: created=%v err=%v want false/nil", created, err)
	}

	c, ok, err := s.Get("discord")
	if err != nil || !ok || c.Response != "join $user" || c.Cooldown != 5 || c.MinRole != "everyone" {
		t.Fatalf("Get discord: %+v ok=%v err=%v", c, ok, err)
	}
	if _, ok, _ := s.Get("nope"); ok {
		t.Error("Get nope: want ok=false")
	}

	if err := s.IncCount("discord"); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := s.Get("discord"); c.Count != 1 {
		t.Errorf("count=%d want 1", c.Count)
	}

	// Edit preserves count.
	if found, err := s.SetResponse("discord", "new"); err != nil || !found {
		t.Fatalf("SetResponse: found=%v err=%v", found, err)
	}
	if c, _, _ := s.Get("discord"); c.Response != "new" || c.Count != 1 {
		t.Errorf("after edit: %+v want response=new count=1", c)
	}
	if found, _ := s.SetResponse("nope", "x"); found {
		t.Error("SetResponse nope: want found=false")
	}

	if found, err := s.Delete("discord"); err != nil || !found {
		t.Fatalf("Delete: found=%v err=%v", found, err)
	}
	if _, ok, _ := s.Get("discord"); ok {
		t.Error("discord present after delete")
	}
	if found, _ := s.Delete("nope"); found {
		t.Error("Delete nope: want found=false")
	}
}

// List is what !commands prints, so the two backends must agree on the order.
// The names here are chosen to diverge under a locale-aware collation: an
// en_US.UTF-8 cluster ignores the underscore and interleaves the words, while
// SQLite's byte comparison sorts "_" (0x5f) after every uppercase letter and
// before every lowercase one. COLLATE "C" is what makes them match.
func testCommandListSorted(t *testing.T, s Store) {
	for _, n := range []string{"socials", "Discord", "so_cool", "schedule", "SOCIALS"} {
		if _, err := s.Add(store.Command{Name: n, Response: "x"}); err != nil {
			t.Fatalf("Add %q: %v", n, err)
		}
	}
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Discord", "SOCIALS", "schedule", "so_cool", "socials"}
	if len(names) != len(want) {
		t.Fatalf("List=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("List=%v want %v (byte order, not locale order)", names, want)
		}
	}
}
