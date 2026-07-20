package main

import (
	"fmt"
	"strconv"
	"strings"
)

// The Deep-of-Night depth rating. The bot owns the value (persisted in the
// store, no localStorage) and pushes it to the overlay, which renders a depth
// icon + the current rating and an all-time personal best in the bottom-right
// corner. Broadcaster/mods set the rating directly with "!r N" / "!don N"; the PB
// auto-rises whenever a new rating beats it.

const (
	depthSettingKey = "depth_points"
	depthPBKey      = "depth_pb"
	depthMaxPoints  = 10000 // matches the prototype's cap
)

// depthData is the overlay render state for the depth widget. The overlay maps
// points -> tier icon from the same thresholds; tier is included for convenience.
type depthData struct {
	Points int64 `json:"points"`
	Tier   int   `json:"tier"`
	PB     int64 `json:"pb"`
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

// setDepth handles "!r N" / "!don N" (broadcaster/mods only): set the rating to
// an exact value, clamped to [0, depthMaxPoints]. The PB auto-rises to match a
// new high.
func (r *Router) setDepth(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	arg := strings.TrimSpace(rest)
	next, err := strconv.ParseInt(arg, 10, 64)
	if arg == "" || err != nil {
		r.reply(m, "usage: !r <rating> (e.g. !r 4200)")
		return
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

	pb := r.depthPB()
	if next > pb {
		pb = next
		if err := r.store.SetSetting(depthPBKey, strconv.FormatInt(pb, 10)); err != nil {
			r.logger.Printf("depth pb persist: %v", err)
		}
	}

	r.pushDepth(next)
	r.reply(m, fmt.Sprintf("Depth: %d (rank %d). PB %d.", next, depthTier(next), pb))
}

// depthPoints reads the persisted depth total (0 when unset).
func (r *Router) depthPoints() int64 { return r.depthSetting(depthSettingKey) }

// depthPB reads the persisted all-time personal best (0 when unset).
func (r *Router) depthPB() int64 { return r.depthSetting(depthPBKey) }

func (r *Router) depthSetting(key string) int64 {
	if r.store == nil {
		return 0
	}
	v, ok, err := r.store.GetSetting(key)
	if err != nil {
		r.logger.Printf("depth read %s: %v", key, err)
		return 0
	}
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// pushDepth sends the current depth state (rating + PB) to the overlay (no-op
// without one).
func (r *Router) pushDepth(points int64) {
	if r.overlay == nil {
		return
	}
	r.overlay.Push("depth", depthData{Points: points, Tier: depthTier(points), PB: r.depthPB()})
}
