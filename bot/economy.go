package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"tts/store"
	"tts/twitch"
)

// EconomyConfig holds the loyalty-points ("marks") settings, loaded from
// points.toml. A zero/absent config leaves the economy disabled (marks aren't
// charged and no accrual/conversion runs).
type EconomyConfig struct {
	CurrencyName    string
	AccrualInterval time.Duration
	AccrualRate     int64
	TTSCost         int64
	SFXCost         int64
	RewardTitle     string
	RewardCost      int
	RewardGrant     int64
	RewardPrompt    string
	PollInterval    time.Duration
	// Games:
	GambleMinBet   int64
	GambleDuration time.Duration // how long an open !g round accepts joins
	WordleReward   int64         // marks awarded to a Wordle solver
}

// LoadEconomyConfig parses points.toml. A missing file is not an error — it just
// leaves the economy disabled (enabled=false), so tts/sfx stay free. Absent
// fields fall back to sensible defaults.
func LoadEconomyConfig(path string) (cfg EconomyConfig, enabled bool, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return EconomyConfig{}, false, nil
	}
	var doc struct {
		CurrencyName    string `toml:"currency_name"`
		AccrualInterval string `toml:"accrual_interval"`
		AccrualRate     int64  `toml:"accrual_rate"`
		TTSCost         int64  `toml:"tts_cost"`
		SFXCost         int64  `toml:"sfx_cost"`
		RewardTitle     string `toml:"reward_title"`
		RewardCost      int    `toml:"reward_cost"`
		RewardGrant     int64  `toml:"reward_grant"`
		RewardPrompt    string `toml:"reward_prompt"`
		PollInterval    string `toml:"poll_interval"`
		GambleMinBet    int64  `toml:"gamble_min_bet"`
		GambleDuration  string `toml:"gamble_duration"`
		WordleReward    int64  `toml:"wordle_reward"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return EconomyConfig{}, false, err
	}

	accrual, err := durationOr(doc.AccrualInterval, time.Minute)
	if err != nil {
		return EconomyConfig{}, false, fmt.Errorf("accrual_interval: %w", err)
	}
	poll, err := durationOr(doc.PollInterval, 30*time.Second)
	if err != nil {
		return EconomyConfig{}, false, fmt.Errorf("poll_interval: %w", err)
	}
	gambleDur, err := durationOr(doc.GambleDuration, 60*time.Second)
	if err != nil {
		return EconomyConfig{}, false, fmt.Errorf("gamble_duration: %w", err)
	}

	cfg = EconomyConfig{
		CurrencyName:    orString(doc.CurrencyName, "marks"),
		AccrualInterval: accrual,
		AccrualRate:     orInt64(doc.AccrualRate, 1),
		TTSCost:         orInt64(doc.TTSCost, 1),
		SFXCost:         orInt64(doc.SFXCost, 1),
		RewardTitle:     orString(doc.RewardTitle, "Convert to Marks"),
		RewardCost:      doc.RewardCost,
		RewardGrant:     doc.RewardGrant,
		RewardPrompt:    doc.RewardPrompt,
		PollInterval:    poll,
		GambleMinBet:    orInt64(doc.GambleMinBet, 10),
		GambleDuration:  gambleDur,
		WordleReward:    orInt64(doc.WordleReward, 100),
	}
	return cfg, true, nil
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 1m, 30s)", s)
	}
	return d, nil
}

func orString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orInt64(n, def int64) int64 {
	if n == 0 {
		return def
	}
	return n
}

// TwitchAPI is the slice of the Twitch client the economy runner needs (an
// interface so tests can fake it). *twitch.Client satisfies it.
type TwitchAPI interface {
	SenderID() string
	IsLive(ctx context.Context, broadcasterID string) (bool, error)
	GetChatters(ctx context.Context, broadcasterID, moderatorID string) ([]twitch.Chatter, error)
	EnsureReward(ctx context.Context, broadcasterID, title string, cost int, prompt string) (string, error)
	GetRedemptions(ctx context.Context, broadcasterID, rewardID, status string) ([]twitch.Redemption, error)
	FulfillRedemptions(ctx context.Context, broadcasterID, rewardID string, ids []string) error
}

// Economy runs the two earning loops: watch-time accrual (live-gated Get
// Chatters) and Channel-Point→marks conversion (poll a bot-managed reward).
// roomID returns the channel's broadcaster id (the numeric room-id from chat
// tags), "" until a message has been seen.
type Economy struct {
	store  *store.Store
	api    TwitchAPI
	cfg    EconomyConfig
	roomID func() string
	logger *log.Logger

	rewardID     string // resolved lazily once a broadcaster id is known
	convDisabled bool   // set if the channel can't have channel points
}

func NewEconomy(st *store.Store, api TwitchAPI, cfg EconomyConfig, roomID func() string, logger *log.Logger) *Economy {
	return &Economy{store: st, api: api, cfg: cfg, roomID: roomID, logger: logger}
}

// Run drives the accrual and conversion tickers until ctx is canceled.
func (e *Economy) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loops := []struct {
		every time.Duration
		tick  func(context.Context)
	}{
		{e.cfg.AccrualInterval, e.accrualTick},
		{e.cfg.PollInterval, e.conversionTick},
	}
	for _, l := range loops {
		if l.every <= 0 {
			continue
		}
		wg.Add(1)
		go func(every time.Duration, tick func(context.Context)) {
			defer wg.Done()
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					tick(ctx)
				}
			}
		}(l.every, l.tick)
	}
	wg.Wait()
}

// accrualTick credits every present viewer AccrualRate marks, but only while the
// stream is live (so points don't farm 24/7 while the service runs offline).
func (e *Economy) accrualTick(ctx context.Context) {
	broadcaster := e.roomID()
	moderator := e.api.SenderID()
	if broadcaster == "" || moderator == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	live, err := e.api.IsLive(ctx, broadcaster)
	if err != nil {
		e.logger.Printf("accrual: is-live: %v", err)
		return
	}
	if !live {
		return
	}
	chatters, err := e.api.GetChatters(ctx, broadcaster, moderator)
	if err != nil {
		e.logger.Printf("accrual: get-chatters: %v", err)
		return
	}
	for _, ch := range chatters {
		if err := e.store.UpsertUser(ch.UserID, ch.Login, ch.Display); err != nil {
			e.logger.Printf("accrual: upsert %s: %v", ch.Login, err)
			continue
		}
		if _, err := e.store.Credit(ch.UserID, e.cfg.AccrualRate, "accrual", ""); err != nil {
			e.logger.Printf("accrual: credit %s: %v", ch.Login, err)
		}
	}
	if len(chatters) > 0 {
		e.logger.Printf("accrual: +%d %s to %d viewers", e.cfg.AccrualRate, e.cfg.CurrencyName, len(chatters))
	}
}

// conversionTick credits marks for any new "Convert to Marks" redemptions and
// marks them fulfilled. Crediting is idempotent on the redemption id, so a crash
// between crediting and fulfilling never double-credits.
func (e *Economy) conversionTick(ctx context.Context) {
	if e.convDisabled {
		return
	}
	broadcaster := e.roomID()
	if broadcaster == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if e.rewardID == "" {
		id, err := e.api.EnsureReward(ctx, broadcaster, e.cfg.RewardTitle, e.cfg.RewardCost, e.cfg.RewardPrompt)
		if err != nil {
			e.logger.Printf("conversion disabled (reward unavailable — channel affiliate?): %v", err)
			e.convDisabled = true
			return
		}
		e.rewardID = id
		e.logger.Printf("conversion: reward %q ready (%d pts -> %d %s)", e.cfg.RewardTitle, e.cfg.RewardCost, e.cfg.RewardGrant, e.cfg.CurrencyName)
	}

	reds, err := e.api.GetRedemptions(ctx, broadcaster, e.rewardID, "UNFULFILLED")
	if err != nil {
		e.logger.Printf("conversion: get-redemptions: %v", err)
		return
	}
	var done []string
	for _, r := range reds {
		if err := e.store.UpsertUser(r.UserID, r.Login, r.Display); err != nil {
			e.logger.Printf("conversion: upsert %s: %v", r.Login, err)
			continue
		}
		if _, err := e.store.Credit(r.UserID, e.cfg.RewardGrant, "convert", r.ID); err != nil {
			e.logger.Printf("conversion: credit %s: %v", r.Login, err)
			continue
		}
		done = append(done, r.ID)
	}
	// Fulfill in batches of 50 (Twitch's per-call limit).
	for len(done) > 0 {
		n := min(50, len(done))
		if err := e.api.FulfillRedemptions(ctx, broadcaster, e.rewardID, done[:n]); err != nil {
			e.logger.Printf("conversion: fulfill: %v", err)
			return
		}
		done = done[n:]
	}
	if len(reds) > 0 {
		e.logger.Printf("conversion: credited %d redemptions", len(reds))
	}
}
