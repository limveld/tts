package store

// The names in-flight rounds are stored under in game_rounds.
//
// This list lives here, rather than in the bot that writes it, because two
// programs have to agree on it being complete: the bot saves a round under one
// of these names, and cmd/store-migrate carries in-flight rounds across at
// cutover. It used to be spelled out separately in both, with a comment in the
// migrate tool asking that they be kept in step — and they drifted the first time
// a fourth game was added, which nothing caught, because the two lists are in
// different packages and neither could see the other.
//
// A round left behind at cutover is escrowed marks left behind with it.
const (
	GameGamble      = "gamble"
	GameWordle      = "wordle"
	GameConnections = "connections"
	GameMaze        = "maze"
)

// Games is every game that can have an in-flight round, in no particular order.
// Anything that has to sweep all rounds should range over this rather than write
// its own list.
var Games = []string{GameGamble, GameWordle, GameConnections, GameMaze}
