package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustImport(t *testing.T, s *Store, serial string, amount int64) *Receipt {
	t.Helper()
	r, err := s.ImportReceipt(serial, "ACME Corp", "CNY", amount, time.Now(), "payment")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return r
}

func mustBalance(t *testing.T, s *Store, receiptID string) int64 {
	t.Helper()
	b, err := s.Balance(receiptID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func TestImportIdempotentAndConflict(t *testing.T) {
	s := NewStore()
	r1 := mustImport(t, s, "SN-1", 10000)

	r2, err := s.ImportReceipt("SN-1", "ACME Corp", "CNY", 10000, time.Now(), "payment")
	if err != nil {
		t.Fatalf("duplicate import should return original: %v", err)
	}
	if r2.ID != r1.ID {
		t.Fatalf("expected same receipt, got %s vs %s", r2.ID, r1.ID)
	}

	if _, err := s.ImportReceipt("SN-1", "ACME Corp", "CNY", 9999, time.Now(), "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("amount change should conflict, got %v", err)
	}
	if _, err := s.ImportReceipt("SN-1", "ACME Corp", "USD", 10000, time.Now(), "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("currency change should conflict, got %v", err)
	}
	if _, err := s.ImportReceipt("SN-1", "Other", "CNY", 10000, time.Now(), "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("payer change should conflict, got %v", err)
	}
}

func TestClaimSplitAndAtomicity(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s, "SN-2", 10000)

	c1, err := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "orderA", Amount: 4000}, {Payee: "orderB", Amount: 3000}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if c1.Status != ClaimPending {
		t.Fatalf("expected pending, got %s", c1.Status)
	}
	if got := mustBalance(t, s, r.ID); got != 3000 {
		t.Fatalf("balance after pending claim = %d, want 3000", got)
	}

	r, _ = s.GetReceipt(r.ID)
	if _, err := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "orderC", Amount: 3000}}); err != nil {
		t.Fatalf("second claim: %v", err)
	}

	r, _ = s.GetReceipt(r.ID)
	before := len(s.ClaimProgress(r.ID))
	failed, err := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "orderD", Amount: 100}, {Payee: "orderE", Amount: 100}})
	if !errors.Is(err, ErrOverAllocation) {
		t.Fatalf("expected over-allocation, got %v", err)
	}
	if failed.Status != ClaimFailed || failed.Reason == "" {
		t.Fatalf("failed claim should keep reason, got %+v", failed)
	}
	if got := len(s.ClaimProgress(r.ID)); got != before+1 {
		t.Fatalf("failed claim should be recorded")
	}
	if got := mustBalance(t, s, r.ID); got != 0 {
		t.Fatalf("failed claim must not affect balance, got %d", got)
	}
	if got := len(s.Ledger("SN-2")); got != 0 {
		t.Fatalf("failed claim must not create ledger entries, got %d", got)
	}
}

func TestConfirmWithdrawAndVersionConflict(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s, "SN-3", 5000)

	c1, _ := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "A", Amount: 2000}})
	r, _ = s.GetReceipt(r.ID)
	c2, _ := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "B", Amount: 2000}})

	if err := s.ConfirmClaim(c1.ID, c1.ReceiptVersion); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale confirm should fail, got %v", err)
	}
	r, _ = s.GetReceipt(r.ID)
	if err := s.ConfirmClaim(c1.ID, r.Version); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := len(s.Ledger("SN-3")); got != 1 {
		t.Fatalf("confirm should create 1 ledger entry, got %d", got)
	}

	r, _ = s.GetReceipt(r.ID)
	if err := s.WithdrawClaim(c2.ID, r.Version); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := mustBalance(t, s, r.ID); got != 3000 {
		t.Fatalf("balance after withdraw = %d, want 3000", got)
	}
	r, _ = s.GetReceipt(r.ID)
	if err := s.WithdrawClaim(c2.ID, r.Version); !errors.Is(err, ErrClaimNotPending) {
		t.Fatalf("double withdraw should fail, got %v", err)
	}
	if err := s.WithdrawClaim(c1.ID, r.Version); !errors.Is(err, ErrClaimNotPending) {
		t.Fatalf("withdraw confirmed claim should fail, got %v", err)
	}
}

func TestReversal(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s, "SN-4", 8000)
	c, _ := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "A", Amount: 5000}})
	r, _ = s.GetReceipt(r.ID)
	if err := s.ConfirmClaim(c.ID, r.Version); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	rev, err := s.ReverseClaim(c.ID, 2000, "rev-key-1")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if got := mustBalance(t, s, r.ID); got != 5000 {
		t.Fatalf("balance after reversal = %d, want 5000", got)
	}

	rev2, err := s.ReverseClaim(c.ID, 2000, "rev-key-1")
	if err != nil || rev2.ID != rev.ID {
		t.Fatalf("idempotent reversal should return original, got %v %v", rev2, err)
	}
	if got := mustBalance(t, s, r.ID); got != 5000 {
		t.Fatalf("duplicate reversal must not re-add balance, got %d", got)
	}

	if _, err := s.ReverseClaim(c.ID, 4000, "rev-key-2"); !errors.Is(err, ErrOverReversal) {
		t.Fatalf("over reversal should fail, got %v", err)
	}
	if _, err := s.ReverseClaim(c.ID, 3000, "rev-key-3"); err != nil {
		t.Fatalf("full reversal: %v", err)
	}
	if got := mustBalance(t, s, r.ID); got != 8000 {
		t.Fatalf("balance after full reversal = %d, want 8000", got)
	}

	r, _ = s.GetReceipt(r.ID)
	if _, err := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "B", Amount: 8000}}); err != nil {
		t.Fatalf("re-claim after reversal: %v", err)
	}

	entries := s.Ledger("SN-4")
	var outs, ins int64
	for _, e := range entries {
		if e.BankSerialNo != "SN-4" {
			t.Fatalf("ledger entry not traceable to serial: %+v", e)
		}
		switch e.Type {
		case LedgerClaimOut:
			outs += e.Amount
		case LedgerReversalIn:
			ins += e.Amount
		}
	}
	if outs != 5000 || ins != 5000 {
		t.Fatalf("ledger totals out=%d in=%d, want 5000/5000", outs, ins)
	}
	if got := len(s.Reversals(r.ID)); got != 2 {
		t.Fatalf("expected 2 reversal records, got %d", got)
	}
}

func TestConcurrentClaimsNoOverAllocation(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s, "SN-5", 10000)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				cur, err := s.GetReceipt(r.ID)
				if err != nil {
					return
				}
				_, err = s.SubmitClaim(r.ID, cur.Version, []Allocation{{Payee: fmt.Sprintf("P%d", i), Amount: 1000}})
				if errors.Is(err, ErrVersionConflict) {
					continue
				}
				if err == nil {
					mu.Lock()
					succeeded += 1000
					mu.Unlock()
				}
				return
			}
		}(i)
	}
	wg.Wait()
	if succeeded > 10000 {
		t.Fatalf("over-allocated: %d", succeeded)
	}
	if got := mustBalance(t, s, r.ID); got != 10000-succeeded {
		t.Fatalf("balance %d inconsistent with allocated %d", got, succeeded)
	}
}

func TestQueries(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s, "SN-6", 6000)
	c, _ := s.SubmitClaim(r.ID, r.Version, []Allocation{{Payee: "A", Amount: 2000}, {Payee: "B", Amount: 1000}})
	r, _ = s.GetReceipt(r.ID)
	_ = s.ConfirmClaim(c.ID, r.Version)
	_, _ = s.ReverseClaim(c.ID, 500, "k1")

	if got := len(s.ClaimProgress(r.ID)); got != 1 {
		t.Fatalf("progress count = %d, want 1", got)
	}
	if got := len(s.Allocations(r.ID)); got != 2 {
		t.Fatalf("allocations count = %d, want 2", got)
	}
	if got := len(s.Reversals(r.ID)); got != 1 {
		t.Fatalf("reversals count = %d, want 1", got)
	}
	entries := s.Ledger("SN-6")
	if len(entries) != 3 {
		t.Fatalf("ledger entries = %d, want 3", len(entries))
	}
	if got := mustBalance(t, s, r.ID); got != 3500 {
		t.Fatalf("balance = %d, want 3500", got)
	}
}
