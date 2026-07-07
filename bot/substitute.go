package main

import (
	"math/rand"
	"strconv"
	"strings"
)

// subCtx carries the values a custom-command response can interpolate.
type subCtx struct {
	User  string   // caller's display name
	Args  []string // whitespace-split args after the command
	Rest  string   // raw args string
	Count int      // this command's use count (after this use)
	rnd   *rand.Rand
}

// substitute expands the v1 variables in a custom-command response:
//
//	$user    caller display name        $touser  first arg, else $user
//	$args    all args (raw)             $1..$9   positional arg (empty if absent)
//	$count   command use count          $random  a random number 1-100 (each occurrence)
func substitute(resp string, c subCtx) string {
	// Positional $1..$9 first (so $args/$user replacements can't split them).
	for i := 1; i <= 9; i++ {
		v := ""
		if i-1 < len(c.Args) {
			v = c.Args[i-1]
		}
		resp = strings.ReplaceAll(resp, "$"+strconv.Itoa(i), v)
	}

	touser := c.User
	if len(c.Args) > 0 {
		touser = c.Args[0]
	}
	// $touser before $user so "$touser" isn't matched as "$user"+"...".
	resp = strings.NewReplacer(
		"$touser", touser,
		"$user", c.User,
		"$args", c.Rest,
		"$count", strconv.Itoa(c.Count),
	).Replace(resp)

	// $random: a fresh number per occurrence.
	for strings.Contains(resp, "$random") {
		n := 1
		if c.rnd != nil {
			n = c.rnd.Intn(100) + 1
		}
		resp = strings.Replace(resp, "$random", strconv.Itoa(n), 1)
	}
	return resp
}
