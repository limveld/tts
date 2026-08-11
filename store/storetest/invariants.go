package storetest

import (
	"database/sql"
	"math/rand"
	"testing"
)

// The invariant suite is the one place the conformance tests are allowed to look
// behind the contract. Every other case here drives the Store interface and
// nothing else, because a case that reaches for a column is testing an
// implementation rather than a behavior.
//
// These have to. accounts.balance is a materialized total that no interface
// method exposes, and there is no way to assert "these two representations of
// the same number match" through an API that only ever returns one of them.
// Likewise there is no API for "make this history go away", which is what
// retention does and what the ref-idempotency case has to simulate.

// NewWithDB builds an isolated store and hands back a handle to the same
// database. Only the invariant suite gets one.
type NewWithDB func(t *testing.T) (Store, *sql.DB)

// RunInvariants executes the cases that check storage-level invariants rather
// than behavior.
func RunInvariants(t *testing.T, newStore NewWithDB) {
	cases := []struct {
		name string
		body func(*testing.T, Store, *sql.DB)
	}{
		{"MaterializedBalanceMatchesLedger", testMaterializedBalanceMatchesLedger},
		{"EveryLedgerUserHasAnAccountRow", testEveryLedgerUserHasAnAccountRow},
		{"CreditRefSurvivesItsLedgerRow", testCreditRefSurvivesItsLedgerRow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, db := newStore(t)
			c.body(t, s, db)
		})
	}
}

// testMaterializedBalanceMatchesLedger is the gate for switching the read paths
// over to accounts.balance. It drives a deliberately messy workload through the
// public API — every money path, including the ones that write a different
// number than they were asked to — and then checks the two representations
// agree for every user.
//
// The seed is fixed: a failure has to be reproducible, and "it passed on the
// third run" is not a property anyone can act on.
func testMaterializedBalanceMatchesLedger(t *testing.T, s Store, db *sql.DB) {
	const users = 50
	rng := rand.New(rand.NewSource(20260811))
	id := func(n int) string { return "u" + string(rune('a'+n%26)) + string(rune('0'+n/26)) }

	for i := 0; i < users; i++ {
		if err := s.UpsertUser(id(i), "login"+id(i), "Display "+id(i)); err != nil {
			t.Fatalf("UpsertUser: %v", err)
		}
	}

	for step := 0; step < 600; step++ {
		u := id(rng.Intn(users))
		switch rng.Intn(6) {
		case 0: // plain accrual
			if _, err := s.Credit(u, int64(1+rng.Intn(50)), "accrual", ""); err != nil {
				t.Fatalf("Credit: %v", err)
			}
		case 1: // redemption, sometimes a repeat of one already applied
			ref := "redeem-" + id(rng.Intn(20))
			if _, err := s.Credit(u, int64(1+rng.Intn(100)), "redemption", ref); err != nil {
				t.Fatalf("Credit(ref): %v", err)
			}
		case 2: // admin mint
			if _, err := s.Grant(u, int64(1+rng.Intn(200)), "grant"); err != nil {
				t.Fatalf("Grant: %v", err)
			}
		case 3: // claw-back, frequently past zero so the clamp fires
			if _, err := s.Grant(u, -int64(1+rng.Intn(400)), "clawback"); err != nil {
				t.Fatalf("Grant(negative): %v", err)
			}
		case 4: // spend, frequently more than they have so it is refused
			if _, err := s.Spend(u, int64(1+rng.Intn(300)), "spend"); err != nil {
				t.Fatalf("Spend: %v", err)
			}
		case 5: // transfer, including the occasional self-transfer
			if _, err := s.Transfer(u, id(rng.Intn(users)), int64(1+rng.Intn(150)), "give"); err != nil {
				t.Fatalf("Transfer: %v", err)
			}
		}
	}

	assertBalancesMatchLedger(t, db)
}

// testEveryLedgerUserHasAnAccountRow pins the other half of the invariant. A
// user with ledger rows and no accounts row would read as a zero balance the
// moment the read paths switch over — money that silently vanishes rather than
// money that visibly disagrees.
func testEveryLedgerUserHasAnAccountRow(t *testing.T, s Store, db *sql.DB) {
	if _, err := s.Credit("solo", 100, "accrual", ""); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if _, err := s.Grant("granted", 50, "grant"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var missing int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT user_id FROM ledger) l
		 WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.user_id = l.user_id)`).Scan(&missing); err != nil {
		t.Fatalf("query: %v", err)
	}
	if missing != 0 {
		t.Errorf("%d users have ledger rows but no accounts row", missing)
	}
	assertBalancesMatchLedger(t, db)
}

// Idempotency is a property of the redemption id, not of the ledger row it
// produced. Retention drops old ledger rows, so if the guarantee still lived on
// the ledger — as the ledger_ref unique index — every aged-out redemption would
// silently re-arm and could be credited a second time.
//
// The delete here stands in for a dropped partition. It is the one case in this
// suite that reaches past the contract, because there is no API for "make this
// history go away" and the bug is invisible without it.
func testCreditRefSurvivesItsLedgerRow(t *testing.T, s Store, db *sql.DB) {
	if err := s.UpsertUser("u1", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	credited, err := s.Credit("u1", 250, "redemption", "redeem-1")
	if err != nil || !credited {
		t.Fatalf("first credit: credited=%v err=%v want true/nil", credited, err)
	}
	before, err := s.Balance("u1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`DELETE FROM ledger WHERE ref = 'redeem-1'`); err != nil {
		t.Fatalf("aging out the ledger row: %v", err)
	}

	credited, err = s.Credit("u1", 250, "redemption", "redeem-1")
	if err != nil {
		t.Fatal(err)
	}
	if credited {
		t.Error("a redemption id was credited twice after its ledger row aged out")
	}
	after, err := s.Balance("u1")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("balance moved on a repeat redemption: %d -> %d", before, after)
	}
}

// assertBalancesMatchLedger reports every user whose materialized balance
// disagrees with their ledger. It reports all of them rather than failing on the
// first, because the pattern across users is what tells you which write path is
// wrong.
//
// The query is deliberately symmetric: a FROM accounts LEFT JOIN ledger would
// miss a ledger user with no accounts row entirely, which is the more dangerous
// direction.
func assertBalancesMatchLedger(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(
		`SELECT u.user_id,
		        COALESCE((SELECT balance FROM accounts WHERE user_id = u.user_id), 0),
		        COALESCE((SELECT SUM(delta) FROM ledger WHERE user_id = u.user_id), 0)
		   FROM (SELECT user_id FROM accounts UNION SELECT user_id FROM ledger) u
		  ORDER BY u.user_id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var checked, bad int
	for rows.Next() {
		var userID string
		var balance, summed int64
		if err := rows.Scan(&userID, &balance, &summed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		checked++
		if balance != summed {
			bad++
			t.Errorf("%s: accounts.balance=%d SUM(ledger)=%d (off by %d)",
				userID, balance, summed, balance-summed)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if checked == 0 {
		t.Fatal("no users checked — the workload wrote nothing, so this proved nothing")
	}
	if bad > 0 {
		t.Logf("%d of %d users disagree", bad, checked)
	}
}
