package storetest

import "testing"

func testSettings(t *testing.T, s Store) {
	if _, ok, err := s.GetSetting("charge_mode"); err != nil || ok {
		t.Fatalf("absent setting: ok=%v err=%v want false/nil", ok, err)
	}
	if err := s.SetSetting("charge_mode", "free"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.GetSetting("charge_mode"); err != nil || !ok || v != "free" {
		t.Fatalf("get: v=%q ok=%v err=%v", v, ok, err)
	}
	// Overwrite.
	if err := s.SetSetting("charge_mode", "paid"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.GetSetting("charge_mode"); v != "paid" {
		t.Fatalf("after overwrite v=%q want paid", v)
	}
	// Keys are independent.
	if err := s.SetSetting("depth_points", "1234"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.GetSetting("charge_mode"); v != "paid" {
		t.Errorf("charge_mode=%q clobbered by a second key", v)
	}
	// An empty value is a real value, distinct from absence.
	if err := s.SetSetting("depth_pb", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.GetSetting("depth_pb"); err != nil || !ok || v != "" {
		t.Errorf("empty value: v=%q ok=%v err=%v want \"\"/true/nil", v, ok, err)
	}
}
