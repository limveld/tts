package sfxlib

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sfx.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSingleAndMultiClip(t *testing.T) {
	path := writeTOML(t, `
[sounds.Gonnacome]
file = "gc.mp3"
url  = "https://example.com/gc.mp3"

[[sounds.airhorn.clips]]
file = "a1.mp3"
url  = "https://example.com/a1.mp3"
[[sounds.airhorn.clips]]
file = "a2.mp3"
`)
	lib, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(lib) != 2 {
		t.Fatalf("got %d sounds, want 2: %v", len(lib), lib)
	}

	// Name is lowercased; the single file/url form normalises to one clip.
	gc, ok := lib["gonnacome"]
	if !ok {
		t.Fatalf("missing gonnacome; got keys %v", keys(lib))
	}
	if len(gc) != 1 || gc[0].File != "gc.mp3" || gc[0].URL != "https://example.com/gc.mp3" {
		t.Errorf("gonnacome=%+v", gc)
	}

	// The multi-clip form keeps its list, pairing each file with its own url.
	air := lib["airhorn"]
	if len(air) != 2 || air[0].File != "a1.mp3" || air[1].File != "a2.mp3" {
		t.Errorf("airhorn=%+v", air)
	}
}

func TestLoadRejectsEmptySound(t *testing.T) {
	path := writeTOML(t, `
[sounds.broken]
url = "https://example.com/x.mp3"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a sound with no file/clips")
	}
}

func TestLoadVolumeAndTrim(t *testing.T) {
	path := writeTOML(t, `
[sounds.quiet]
file = "q.mp3"
volume = 30
start = 2.5
end = 6

[[sounds.multi.clips]]
file = "m1.mp3"
volume = 80
[[sounds.multi.clips]]
file = "m2.mp3"
`)
	lib, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	q := lib["quiet"][0]
	if q.Volume == nil || *q.Volume != 30 || q.Start != 2.5 || q.End != 6 {
		t.Errorf("quiet=%+v (volume=%v)", q, q.Volume)
	}
	m := lib["multi"]
	if m[0].Volume == nil || *m[0].Volume != 80 {
		t.Errorf("multi[0] volume=%v want 80", m[0].Volume)
	}
	if m[1].Volume != nil {
		t.Errorf("multi[1] volume=%v want nil (default full)", m[1].Volume)
	}
}

func TestLoadRejectsBadVolumeAndTrim(t *testing.T) {
	for _, body := range []string{
		"[sounds.a]\nfile = \"a.mp3\"\nvolume = 150\n", // volume out of range
		"[sounds.a]\nfile = \"a.mp3\"\nstart = 5\nend = 3\n", // end <= start
	} {
		if _, err := Load(writeTOML(t, body)); err == nil {
			t.Errorf("expected an error for %q", body)
		}
	}
}

func keys(m map[string][]Clip) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
