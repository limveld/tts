package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tts/store"
	"tts/twitch"
)

type fakeAPI struct {
	live        bool
	chatters    []twitch.Chatter
	redemptions []twitch.Redemption
	rewardErr   error
	fulfilled   []string
	ensureCalls int
}

func (f *fakeAPI) SenderID() string { return "broadcaster1" }
func (f *fakeAPI) IsLive(context.Context, string) (bool, error) {
	return f.live, nil
}
func (f *fakeAPI) GetChatters(context.Context, string, string) ([]twitch.Chatter, error) {
	return f.chatters, nil
}
func (f *fakeAPI) EnsureReward(context.Context, string, string, int, string) (string, error) {
	f.ensureCalls++
	if f.rewardErr != nil {
		return "", f.rewardErr
	}
	return "reward1", nil
}
func (f *fakeAPI) GetRedemptions(context.Context, string, string, string) ([]twitch.Redemption, error) {
	return f.redemptions, nil
}
func (f *fakeAPI) FulfillRedemptions(_ context.Context, _, _ string, ids []string) error {
	f.fulfilled = append(f.fulfilled, ids...)
	return nil
}

func testEconomy(t *testing.T, api TwitchAPI) (*Economy, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := EconomyConfig{
		CurrencyName: "marks", AccrualRate: 1, RewardTitle: "Convert to Marks",
		RewardCost: 1000, RewardGrant: 500,
	}
	e := NewEconomy(st, api, cfg, func() string { return "broadcaster1" }, log.New(io.Discard, "", 0))
	return e, st
}

func TestAccrualOnlyWhenLive(t *testing.T) {
	api := &fakeAPI{chatters: []twitch.Chatter{{UserID: "u1", Login: "bob", Display: "Bob"}}}

	// Offline: no accrual.
	e, st := testEconomy(t, api)
	e.accrualTick(context.Background())
	if b, _ := st.Balance("u1"); b != 0 {
		t.Fatalf("offline balance=%d want 0", b)
	}

	// Live: credited, and the user is recorded.
	api.live = true
	e.accrualTick(context.Background())
	if b, _ := st.Balance("u1"); b != 1 {
		t.Fatalf("live balance=%d want 1", b)
	}
	if id, ok, _ := st.ResolveLogin("bob"); !ok || id != "u1" {
		t.Fatalf("user not recorded: id=%q ok=%v", id, ok)
	}
}

func TestConversionCreditsAndFulfills(t *testing.T) {
	api := &fakeAPI{redemptions: []twitch.Redemption{
		{ID: "red1", UserID: "u1", Login: "bob", Display: "Bob"},
	}}
	e, st := testEconomy(t, api)

	e.conversionTick(context.Background())
	if b, _ := st.Balance("u1"); b != 500 {
		t.Fatalf("balance=%d want 500", b)
	}
	if len(api.fulfilled) != 1 || api.fulfilled[0] != "red1" {
		t.Fatalf("fulfilled=%v want [red1]", api.fulfilled)
	}

	// Same redemption again (Twitch re-serving before fulfill propagates): no
	// double credit thanks to the idempotent ref.
	e.conversionTick(context.Background())
	if b, _ := st.Balance("u1"); b != 500 {
		t.Fatalf("balance after re-poll=%d want 500 (idempotent)", b)
	}
}

func TestConversionDisablesOnRewardError(t *testing.T) {
	api := &fakeAPI{rewardErr: errors.New("not an affiliate")}
	e, _ := testEconomy(t, api)

	e.conversionTick(context.Background())
	e.conversionTick(context.Background())
	if !e.convDisabled {
		t.Fatal("conversion should be disabled after a reward error")
	}
	if api.ensureCalls != 1 {
		t.Fatalf("EnsureReward called %d times want 1 (disabled after first failure)", api.ensureCalls)
	}
}

func TestLoadEconomyConfig(t *testing.T) {
	// Missing file: disabled, no error.
	if cfg, enabled, err := LoadEconomyConfig(filepath.Join(t.TempDir(), "none.toml")); err != nil || enabled || cfg.CurrencyName != "" {
		t.Fatalf("missing file: cfg=%+v enabled=%v err=%v want zero/false/nil", cfg, enabled, err)
	}

	// Present but sparse: enabled with defaults filled in.
	p := filepath.Join(t.TempDir(), "points.toml")
	if err := os.WriteFile(p, []byte(`accrual_rate = 2`+"\n"+`tts_cost = 5`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, enabled, err := LoadEconomyConfig(p)
	if err != nil || !enabled {
		t.Fatalf("load: enabled=%v err=%v", enabled, err)
	}
	if cfg.CurrencyName != "marks" || cfg.AccrualInterval != time.Minute || cfg.PollInterval != 30*time.Second {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.AccrualRate != 2 || cfg.TTSCost != 5 || cfg.SFXCost != 1 || cfg.RewardTitle != "Convert to Marks" {
		t.Errorf("values/defaults wrong: %+v", cfg)
	}

	// Bad duration is an error.
	if err := os.WriteFile(p, []byte(`accrual_interval = "nope"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadEconomyConfig(p); err == nil {
		t.Error("expected an error for a bad accrual_interval")
	}
}

func TestEconomyRunStopsOnContext(t *testing.T) {
	api := &fakeAPI{}
	e, _ := testEconomy(t, api)
	e.cfg.AccrualInterval = time.Hour
	e.cfg.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
