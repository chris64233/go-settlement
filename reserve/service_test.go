package reserve

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) *Service {
	t.Helper()
	s := NewServiceWithClock(func() time.Time { return t0 })
	if _, err := s.RegisterMerchant("m1", "Acme"); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustRule(t *testing.T, s *Service, rateBP, holdDays int, from time.Time) *ReserveRule {
	t.Helper()
	r, err := s.CreateRule("m1", fmt.Sprintf("rule-%d", rateBP), rateBP, holdDays, from)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustSettlement(t *testing.T, s *Service, id string, amount Money, at time.Time) *ReserveBatch {
	t.Helper()
	_, b, err := s.RegisterSettlement("m1", id, amount, at)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWithholdUsesEffectiveRuleAndFreezesVersion(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0) // 10%, 30 days
	b1 := mustSettlement(t, s, "st1", 10000, t0.AddDate(0, 0, 5))
	if b1.Amount != 1000 || b1.RuleVersion != 1 {
		t.Fatalf("batch1 = %+v", b1)
	}
	if want := t0.AddDate(0, 0, 35); !b1.ReleaseDate.Equal(want) {
		t.Fatalf("release date = %v, want %v", b1.ReleaseDate, want)
	}

	// Rule adjustment: 20%, 60 days. Existing batch must not be recomputed.
	mustRule(t, s, 2000, 60, t0.AddDate(0, 0, 10))
	b2 := mustSettlement(t, s, "st2", 10000, t0.AddDate(0, 0, 15))
	if b2.Amount != 2000 || b2.RuleVersion != 2 {
		t.Fatalf("batch2 = %+v", b2)
	}
	view, err := s.Balance("m1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Batches[0].Amount != 1000 || view.Batches[0].RuleVersion != 1 {
		t.Fatalf("old batch recomputed: %+v", view.Batches[0])
	}
}

func TestRegisterSettlementIdempotent(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	b1 := mustSettlement(t, s, "st1", 10000, t0)
	b2 := mustSettlement(t, s, "st1", 10000, t0)
	if b1.ID != b2.ID {
		t.Fatalf("duplicate settlement created new batch: %s vs %s", b1.ID, b2.ID)
	}
	if _, _, err := s.RegisterSettlement("m1", "st1", 9999, t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestReleaseScanOnlyReleasesUnoccupied(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	mustSettlement(t, s, "st1", 10000, t0) // batch 1000, due t0+30

	// Deduct 400 before release.
	d, err := s.Deduct("m1", "d1", 400, "fraud")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Allocs) != 1 || d.Allocs[0].Amount != 400 {
		t.Fatalf("allocs = %+v", d.Allocs)
	}

	// Not yet due: nothing released.
	run, err := s.ScanReleases("m1", "run1", t0.AddDate(0, 0, 29))
	if err != nil {
		t.Fatal(err)
	}
	if run.Released != 0 {
		t.Fatalf("released early: %d", run.Released)
	}

	// Due: only the unoccupied 600 is released.
	run, err = s.ScanReleases("m1", "run2", t0.AddDate(0, 0, 30))
	if err != nil {
		t.Fatal(err)
	}
	if run.Released != 600 {
		t.Fatalf("released = %d, want 600", run.Released)
	}
	view, _ := s.Balance("m1")
	if view.Available != 600 {
		t.Fatalf("available = %d, want 600", view.Available)
	}
	b := view.Batches[0]
	if b.Deducted != 400 || b.Released != 600 || b.Remaining != 0 {
		t.Fatalf("batch = %+v", b)
	}

	// A second scan must not double-release.
	run, err = s.ScanReleases("m1", "run3", t0.AddDate(0, 0, 31))
	if err != nil {
		t.Fatal(err)
	}
	if run.Released != 0 {
		t.Fatalf("double release: %d", run.Released)
	}
}

func TestReleasedAmountCannotBeReoccupied(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	mustSettlement(t, s, "st1", 10000, t0) // 1000

	if _, err := s.ScanReleases("m1", "run1", t0.AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	// Late deduction: the full 1000 was already released, nothing left.
	if _, err := s.Deduct("m1", "d1", 1, "late"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("expected insufficient reserve, got %v", err)
	}
}

func TestDeductEarliestReleaseFirst(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 0, t0)
	mustSettlement(t, s, "st1", 10000, t0)                  // 1000, due t0
	mustSettlement(t, s, "st2", 10000, t0.AddDate(0, 0, 5)) // 1000, due t0+5

	d, err := s.Deduct("m1", "d1", 1500, "chargeback")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Allocs) != 2 || d.Allocs[0].Amount != 1000 || d.Allocs[1].Amount != 500 {
		t.Fatalf("allocs = %+v", d.Allocs)
	}
	view, _ := s.Balance("m1")
	if view.Batches[0].Remaining != 0 || view.Batches[1].Remaining != 500 {
		t.Fatalf("batches = %+v", view.Batches)
	}
	// Cumulative deduction cannot exceed the original withheld amount.
	if _, err := s.Deduct("m1", "d2", 600, "more"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("expected insufficient, got %v", err)
	}
}

func TestDeductionIdempotency(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	mustSettlement(t, s, "st1", 10000, t0)

	d1, err := s.Deduct("m1", "d1", 300, "fraud")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Deduct("m1", "d1", 300, "fraud")
	if err != nil {
		t.Fatal(err)
	}
	if d1.CreatedAt != d2.CreatedAt || len(d2.Allocs) != 1 {
		t.Fatalf("replay mismatch: %+v", d2)
	}
	view, _ := s.Balance("m1")
	if view.Batches[0].Deducted != 300 {
		t.Fatalf("deducted twice: %+v", view.Batches[0])
	}
	if _, err := s.Deduct("m1", "d1", 301, "fraud"); !errors.Is(err, ErrConflict) {
		t.Fatalf("amount change: %v", err)
	}
	if _, err := s.Deduct("m1", "d1", 300, "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reason change: %v", err)
	}
}

func TestReleaseRunIdempotency(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	mustSettlement(t, s, "st1", 10000, t0)
	asOf := t0.AddDate(0, 0, 30)

	r1, err := s.ScanReleases("m1", "run1", asOf)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.ScanReleases("m1", "run1", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Released != r2.Released || r2.Released != 1000 {
		t.Fatalf("replay = %+v vs %+v", r1, r2)
	}
	if _, err := s.ScanReleases("m1", "run1", asOf.AddDate(0, 0, 1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("asOf change: %v", err)
	}
}

func TestCompensateKeepsOriginalRecord(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	mustSettlement(t, s, "st1", 10000, t0)
	if _, err := s.Deduct("m1", "d1", 400, "fraud"); err != nil {
		t.Fatal(err)
	}

	c, err := s.Compensate("m1", "c1", "d1", 400, "wrong deduction")
	if err != nil {
		t.Fatal(err)
	}
	if c.Amount != 400 {
		t.Fatalf("compensation = %+v", c)
	}
	// Original deduction record unchanged.
	d, _ := s.Deduct("m1", "d1", 400, "fraud")
	if d.Amount != 400 || d.Allocs[0].Amount != 400 {
		t.Fatalf("deduction mutated: %+v", d)
	}
	view, _ := s.Balance("m1")
	if view.Available != 400 {
		t.Fatalf("available = %d, want 400", view.Available)
	}
	if view.Batches[0].Deducted != 400 {
		t.Fatalf("batch deduction rewritten: %+v", view.Batches[0])
	}

	// Idempotent replay and conflict.
	if _, err := s.Compensate("m1", "c1", "d1", 400, "wrong deduction"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compensate("m1", "c1", "d1", 401, "wrong deduction"); !errors.Is(err, ErrConflict) {
		t.Fatalf("amount change: %v", err)
	}
	if _, err := s.Compensate("m1", "c2", "d1", 500, "too much"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("exceeds deduction: %v", err)
	}
	if _, err := s.Compensate("m1", "c3", "nope", 1, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deduction: %v", err)
	}
}

func TestConcurrentScanDeductAndFreeze(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1000, 30, t0)
	for i := 0; i < 20; i++ {
		mustSettlement(t, s, fmt.Sprintf("st%d", i), 10000, t0) // 20 x 1000
	}
	asOf := t0.AddDate(0, 0, 30)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func(i int) { defer wg.Done(); _, _ = s.ScanReleases("m1", fmt.Sprintf("run-%d", i), asOf) }(i)
		go func(i int) { defer wg.Done(); _, _ = s.Deduct("m1", fmt.Sprintf("d-%d", i), 500, "risk") }(i)
		go func(i int) { defer wg.Done(); _, _ = s.Deduct("m1", fmt.Sprintf("f-%d", i), 250, "manual freeze") }(i)
	}
	wg.Wait()

	view, err := s.Balance("m1")
	if err != nil {
		t.Fatal(err)
	}
	var deducted, released, remaining Money
	for _, b := range view.Batches {
		deducted += b.Deducted
		released += b.Released
		remaining += b.Remaining
		if b.Deducted+b.Released > b.Amount {
			t.Fatalf("batch overspent: %+v", b)
		}
	}
	if deducted+released+remaining != 20000 {
		t.Fatalf("total = %d, want 20000", deducted+released+remaining)
	}
	if view.Available != released {
		t.Fatalf("available %d != released %d", view.Available, released)
	}
}

func TestBalanceBreakdown(t *testing.T) {
	s := newTestService(t)
	mustRule(t, s, 1500, 10, t0)
	mustSettlement(t, s, "st1", 20000, t0) // 3000
	if _, err := s.Deduct("m1", "d1", 500, "risk"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScanReleases("m1", "run1", t0.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	view, err := s.Balance("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Batches) != 1 {
		t.Fatalf("batches = %+v", view.Batches)
	}
	b := view.Batches[0]
	if b.Amount != 3000 || b.Deducted != 500 || b.Released != 2500 || b.Remaining != 0 {
		t.Fatalf("batch = %+v", b)
	}
	if view.Available != 2500 {
		t.Fatalf("available = %d", view.Available)
	}
}
