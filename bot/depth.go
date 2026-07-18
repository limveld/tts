package main

import (
	"fmt"
	"strconv"
	"strings"
)

// The Deep-of-Night depth rating. The bot owns the value (persisted in the
// store, no localStorage) and pushes it to the overlay, which renders a depth
// icon + the points number in the bottom-right corner. Broadcaster/mods adjust
// it with "!don +N" / "!don -N" (relative) or "!don N" (set an absolute value).

const (
	depthSettingKey = "depth_points"
	depthMaxPoints  = 10000 // matches the prototype's cap
)

// depthData is the overlay render state for the depth widget. The overlay maps
// points -> tier icon from the same thresholds; tier is included for convenience.
type depthData struct {
	Points int64 `json:"points"`
	Tier   int   `json:"tier"`
}

// depthTier maps a points total to a depth rank (1-5), using the prototype's
// thresholds (0/1000/2000/4000/6000).
func depthTier(points int64) int {
	switch {
	case points >= 6000:
		return 5
	case points >= 4000:
		return 4
	case points >= 2000:
		return 3
	case points >= 1000:
		return 2
	default:
		return 1
	}
}

// don handles "!don +N" / "!don -N" / "!don N" (broadcaster/mods only).
func (r *Router) don(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	arg := strings.TrimSpace(rest)
	n, err := strconv.ParseInt(arg, 10, 64)
	if arg == "" || err != nil {
		r.reply(m, "usage: !don +N / -N (or !don N to set an absolute value).")
		return
	}
	// A leading sign means a relative adjustment; a bare number sets the value.
	next := n
	if strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "-") {
		next = r.depthPoints() + n
	}
	if next < 0 {
		next = 0
	}
	if next > depthMaxPoints {
		next = depthMaxPoints
	}
	if err := r.store.SetSetting(depthSettingKey, strconv.FormatInt(next, 10)); err != nil {
		r.logger.Printf("depth persist: %v", err)
		return
	}
	r.pushDepth(next)
	r.reply(m, fmt.Sprintf("Depth: %s (rank %d).", comma(next), depthTier(next)))
}

// depthPoints reads the persisted depth total (0 when unset).
func (r *Router) depthPoints() int64 {
	if r.store == nil {
		return 0
	}
	v, ok, err := r.store.GetSetting(depthSettingKey)
	if err != nil {
		r.logger.Printf("depth read: %v", err)
		return 0
	}
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// pushDepth sends the current depth state to the overlay (no-op without one).
func (r *Router) pushDepth(points int64) {
	if r.overlay == nil {
		return
	}
	r.overlay.Push("depth", depthData{Points: points, Tier: depthTier(points)})
}
