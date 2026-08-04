package main

// Board-game arbiter: Wordle and Connections are both center-stage board games
// that share the same overlay region and the same chat's attention, so only one
// runs at a time. The Router holds a single boardKind token guarded by boardMu;
// each game claims the stage on start and releases it when its finished board is
// cleared. A mod !skipgame force-ends whichever is live.
//
// Lock discipline: claimBoard/releaseBoard fully lock and unlock boardMu and
// never nest it inside a per-game mutex (wordleMu / connMu), so there is no lock
// ordering between them.

type boardKind string

const (
	boardNone        boardKind = ""
	boardWordle      boardKind = "wordle"
	boardConnections boardKind = "connections"
)

// claimBoard reserves the stage for kind. ok is false when a *different* board
// game already holds it (live names which one, for the refusal message);
// re-claiming for the same kind succeeds so a game's own "already running" check
// can handle that case.
func (r *Router) claimBoard(kind boardKind) (ok bool, live boardKind) {
	r.boardMu.Lock()
	defer r.boardMu.Unlock()
	if r.board != boardNone && r.board != kind {
		return false, r.board
	}
	r.board = kind
	return true, kind
}

// releaseBoard frees the stage, but only if kind still holds it (a superseding
// game won't have its release clobber the new owner).
func (r *Router) releaseBoard(kind boardKind) {
	r.boardMu.Lock()
	defer r.boardMu.Unlock()
	if r.board == kind {
		r.board = boardNone
	}
}

// liveBoard reports which board game currently owns the stage.
func (r *Router) liveBoard() boardKind {
	r.boardMu.Lock()
	defer r.boardMu.Unlock()
	return r.board
}

// skipGame (mod-only !skipgame) force-ends whichever board game is live, clearing
// the stage for a new one.
func (r *Router) skipGame(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	switch r.liveBoard() {
	case boardWordle:
		r.forceEndWordle()
	case boardConnections:
		r.forceEndConnections()
	default:
		r.reply(m, "no game is running.")
	}
}

// boardBusyMsg is the refusal sent when someone tries to start a board game while
// the other one owns the stage.
func boardBusyMsg(live boardKind) string {
	switch live {
	case boardConnections:
		return "🧩 a Connections round is going — finish it (!group) or a mod can !skipgame."
	case boardWordle:
		return "🟩 a Wordle round is going — !guess it, or a mod can !skipgame."
	default:
		return "another game is running — a mod can !skipgame."
	}
}
