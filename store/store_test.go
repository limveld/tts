package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCommandCRUD(t *testing.T) {
	s := openTemp(t)

	created, err := s.Add(Command{Name: "discord", Response: "join $user", Cooldown: 5})
	if err != nil || !created {
		t.Fatalf("Add discord: created=%v err=%v", created, err)
	}
	// Duplicate add: not created, no error.
	if created, err := s.Add(Command{Name: "discord", Response: "other"}); err != nil || created {
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

	if _, err := s.Add(Command{Name: "socials", Response: "x"}); err != nil {
		t.Fatal(err)
	}
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "discord" || names[1] != "socials" {
		t.Errorf("List=%v want [discord socials]", names)
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
