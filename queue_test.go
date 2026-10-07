package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func newTestQueue(policy ScanPolicy) *Queue {
	return NewQueue(policy, func() time.Time { return testNow })
}

func ins(id, account, amount string, priority int) Instruction {
	return Instruction{
		ExternalID: id,
		Account:    account,
		Amount:     NewMoney(MustDecimal(amount), "CNY"),
		ValueDate:  testNow,
		Priority:   priority,
		Deadline:   testNow.Add(24 * time.Hour),
	}
}

func releasedIDs(t *testing.T, res ScanResult) []string {
	t.Helper()
	var ids []string
	for _, i := range res.Released {
		ids = append(ids, i.ExternalID)
	}
	return ids
}

func TestRegisterIdempotentReplay(t *testing.T) {
	q := newTestQueue(ScanPolicy{})

	first, replayed, err := q.Register(ins("A1", "acct", "100", 5))
	if err != nil || replayed {
		t.Fatalf("first register: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := q.Register(ins("A1", "acct", "100", 5))
	if err != nil {
		t.Fatalf("replay register: %v", err)
	}
	if !replayed {
		t.Fatal("expected replayed=true for identical resubmission")
	}
	if second.Seq != first.Seq || !second.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("replay must return original record, got %+v want %+v", second, first)
	}
}

func TestRegisterConflict(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	if _, _, err := q.Register(ins("A1", "acct", "100", 5)); err != nil {
		t.Fatal(err)
	}
	changed := ins("A1", "acct", "200", 5)
	if _, _, err := q.Register(changed); !errors.Is(err, ErrInstructionConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	changed = ins("A1", "acct", "100", 9)
	if _, _, err := q.Register(changed); !errors.Is(err, ErrInstructionConflict) {
		t.Fatalf("expected conflict on priority change, got %v", err)
	}
}

func TestScanInsufficientFundsStops(t *testing.T) {
	q := newTestQueue(ScanPolicy{StopOnInsufficient: true})
	mustCredit(t, q, "acct", "150")
	mustRegister(t, q, ins("BIG", "acct", "200", 10))
	mustRegister(t, q, ins("SMALL", "acct", "50", 1))

	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := releasedIDs(t, res); len(got) != 0 {
		t.Fatalf("stop policy must not release anything behind the gap, released %v", got)
	}
	pending := q.Pending("acct", testNow)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if pending[0].Instruction.ExternalID != "BIG" || pending[0].Reason != ReasonInsufficientFunds {
		t.Fatalf("BIG should be insufficient, got %+v", pending[0])
	}
	if pending[1].Instruction.ExternalID != "SMALL" || pending[1].Reason != ReasonBlockedByAhead {
		t.Fatalf("SMALL should be blocked by ahead, got %+v", pending[1])
	}
}

func TestScanPrioritySkipOnlyHigherPriority(t *testing.T) {
	q := newTestQueue(ScanPolicy{AllowPrioritySkip: true})
	mustCredit(t, q, "acct", "100")
	mustRegister(t, q, ins("BIG", "acct", "500", 5))
	mustRegister(t, q, ins("LOW", "acct", "60", 1))
	mustRegister(t, q, ins("HIGH", "acct", "80", 9))
	mustRegister(t, q, ins("HIGH2", "acct", "50", 8))

	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := releasedIDs(t, res)
	if len(got) != 1 || got[0] != "HIGH" {
		t.Fatalf("only strictly higher priority instruction may skip, released %v", got)
	}
	pending := q.Pending("acct", testNow)
	reasons := map[string]string{}
	for _, e := range pending {
		reasons[e.Instruction.ExternalID] = e.Reason
	}
	if reasons["LOW"] != ReasonBlockedByAhead {
		t.Fatalf("LOW must not jump the gap, got %q", reasons["LOW"])
	}
	if reasons["HIGH2"] != ReasonInsufficientFunds {
		t.Fatalf("HIGH2 should be insufficient after HIGH consumed balance, got %q", reasons["HIGH2"])
	}
	if bal := q.BalanceOf("acct", "CNY"); bal.Available.String() != "20" {
		t.Fatalf("available should be 20, got %s", bal.Available)
	}
}

func TestScanNoArbitrarySmallFill(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	mustCredit(t, q, "acct", "90")
	mustRegister(t, q, ins("BIG", "acct", "100", 5))
	mustRegister(t, q, ins("SMALL", "acct", "10", 1))

	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := releasedIDs(t, res); len(got) != 0 {
		t.Fatalf("small instruction must not be cherry-picked, released %v", got)
	}
}

func TestConcurrentScansNoDoubleRelease(t *testing.T) {
	q := newTestQueue(ScanPolicy{AllowPrioritySkip: true})
	mustCredit(t, q, "acct", "1000")
	for i := 0; i < 50; i++ {
		mustRegister(t, q, ins(fmt.Sprintf("I%02d", i), "acct", "10", i))
	}

	const scanners = 8
	var wg sync.WaitGroup
	results := make([]ScanResult, scanners)
	for s := 0; s < scanners; s++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := q.Scan("acct", testNow)
			if err != nil {
				t.Error(err)
				return
			}
			results[idx] = res
		}(s)
	}
	wg.Wait()

	seen := map[string]int{}
	total := 0
	for _, res := range results {
		for _, i := range res.Released {
			seen[i.ExternalID]++
			total++
		}
	}
	if total != 50 {
		t.Fatalf("expected 50 total releases across scans, got %d", total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("instruction %s released %d times", id, n)
		}
	}
	if bal := q.BalanceOf("acct", "CNY"); bal.Available.String() != "500" {
		t.Fatalf("available should be 500, got %s", bal.Available)
	}
	if len(q.Pending("acct", testNow)) != 0 {
		t.Fatal("queue should be empty after all releases")
	}
}

func TestConcurrentCreditAndScan(t *testing.T) {
	q := newTestQueue(ScanPolicy{AllowPrioritySkip: true})
	for i := 0; i < 20; i++ {
		mustRegister(t, q, ins(fmt.Sprintf("D%02d", i), "acct", "25", i))
	}
	var wg sync.WaitGroup
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if err := q.Credit("acct", NewMoney(MustDecimal("25"), "CNY")); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := q.Scan("acct", testNow); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	bal := q.BalanceOf("acct", "CNY")
	if bal.Available.Sign() < 0 {
		t.Fatalf("available must never go negative, got %s", bal.Available)
	}
	released := 0
	for i := 0; i < 20; i++ {
		if _, err := q.ReleaseBasis(fmt.Sprintf("D%02d", i)); err == nil {
			released++
		}
	}
	want := (500 - mustInt(t, bal.Available)) / 25
	if released != want {
		t.Fatalf("released %d instructions, balance implies %d", released, want)
	}
}

func TestFreezeReducesAvailable(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	mustCredit(t, q, "acct", "100")
	if err := q.Freeze("acct", NewMoney(MustDecimal("70"), "CNY")); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, q, ins("X", "acct", "50", 5))
	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := releasedIDs(t, res); len(got) != 0 {
		t.Fatalf("frozen funds must not be reused, released %v", got)
	}
	if err := q.Unfreeze("acct", NewMoney(MustDecimal("70"), "CNY")); err != nil {
		t.Fatal(err)
	}
	res, err = q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := releasedIDs(t, res); len(got) != 1 || got[0] != "X" {
		t.Fatalf("after unfreeze X should release, got %v", got)
	}
}

func TestReleaseBasisAndExactAmounts(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	mustCredit(t, q, "acct", "0.30")
	mustRegister(t, q, ins("P", "acct", "0.10", 5))
	mustRegister(t, q, ins("Q", "acct", "0.20", 4))

	if _, err := q.Scan("acct", testNow); err != nil {
		t.Fatal(err)
	}
	rec, err := q.ReleaseBasis("P")
	if err != nil {
		t.Fatal(err)
	}
	if rec.BalanceBefore.String() != "0.3" && rec.BalanceBefore.String() != "3/10" {
		t.Fatalf("unexpected balance before: %s", rec.BalanceBefore)
	}
	after := rec.BalanceBefore.Sub(rec.Amount.Amount)
	if !after.Equal(rec.BalanceAfter) {
		t.Fatalf("release basis inconsistent: %+v", rec)
	}
	if bal := q.BalanceOf("acct", "CNY"); !bal.Available.IsZero() {
		t.Fatalf("0.30 - 0.10 - 0.20 must be exactly zero, got %s", bal.Available)
	}
	if _, err := q.ReleaseBasis("NOPE"); !errors.Is(err, ErrInstructionNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestValueDateNotReached(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	mustCredit(t, q, "acct", "100")
	future := ins("FUT", "acct", "50", 5)
	future.ValueDate = testNow.Add(48 * time.Hour)
	mustRegister(t, q, future)

	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := releasedIDs(t, res); len(got) != 0 {
		t.Fatalf("future value date must not release, got %v", got)
	}
	pending := q.Pending("acct", testNow)
	if pending[0].Reason != ReasonValueDateNotReached {
		t.Fatalf("expected value date reason, got %q", pending[0].Reason)
	}
}

func TestStableOrdering(t *testing.T) {
	q := newTestQueue(ScanPolicy{})
	mustCredit(t, q, "acct", "1000")
	mustRegister(t, q, ins("L1", "acct", "1", 1))
	mustRegister(t, q, ins("H1", "acct", "1", 9))
	mustRegister(t, q, ins("H2", "acct", "1", 9))
	mustRegister(t, q, ins("M1", "acct", "1", 5))

	res, err := q.Scan("acct", testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := releasedIDs(t, res)
	want := []string{"H1", "H2", "M1", "L1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func mustRegister(t *testing.T, q *Queue, i Instruction) {
	t.Helper()
	if _, _, err := q.Register(i); err != nil {
		t.Fatal(err)
	}
}

func mustCredit(t *testing.T, q *Queue, account, amount string) {
	t.Helper()
	if err := q.Credit(account, NewMoney(MustDecimal(amount), "CNY")); err != nil {
		t.Fatal(err)
	}
}

func mustInt(t *testing.T, d Decimal) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(d.String(), "%d", &n); err != nil {
		t.Fatalf("decimal %s not an int", d)
	}
	return n
}
