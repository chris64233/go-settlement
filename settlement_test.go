package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func sampleReceipt() Receipt {
	return Receipt{
		SerialNo:   "BNK-20261008-001",
		Payer:      "ACME Corp",
		Currency:   "CNY",
		Amount:     100_00,
		ReceivedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC),
		Memo:       "货款",
	}
}

func mustImport(t *testing.T, s *Store) Receipt {
	t.Helper()
	r, err := s.ImportReceipt(sampleReceipt())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return *r
}

func balanceOf(t *testing.T, s *Store, serial string) Balance {
	t.Helper()
	b, err := s.GetBalance(serial)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func TestImportReceiptIdempotentAndConflict(t *testing.T) {
	s := NewStore()
	first := mustImport(t, s)

	again, err := s.ImportReceipt(sampleReceipt())
	if err != nil {
		t.Fatalf("duplicate import should return original: %v", err)
	}
	if again.SerialNo != first.SerialNo || again.Amount != first.Amount {
		t.Fatalf("expected original receipt, got %+v", again)
	}

	dup := sampleReceipt()
	dup.Amount = 200_00
	if _, err := s.ImportReceipt(dup); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("amount change should conflict, got %v", err)
	}
	dup = sampleReceipt()
	dup.Currency = "USD"
	if _, err := s.ImportReceipt(dup); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("currency change should conflict, got %v", err)
	}
	dup = sampleReceipt()
	dup.Payer = "Other"
	if _, err := s.ImportReceipt(dup); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("payer change should conflict, got %v", err)
	}
}

func TestClaimSplitConfirmAndBalance(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)

	// 第一次认领：拆分到两个收款对象。
	c1, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{
		{Payee: "订单A", Amount: 30_00},
		{Payee: "订单B", Amount: 20_00},
	})
	if err != nil {
		t.Fatalf("submit C1: %v", err)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Pending != 50_00 || b.Available != 50_00 {
		t.Fatalf("after submit: %+v", b)
	}
	if _, err := s.ConfirmClaim(c1.ID); err != nil {
		t.Fatalf("confirm C1: %v", err)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Confirmed != 50_00 || b.Pending != 0 {
		t.Fatalf("after confirm: %+v", b)
	}

	// 第二次认领剩余部分。
	b := balanceOf(t, s, r.SerialNo)
	c2, err := s.SubmitClaim("C2", r.SerialNo, b.Version, []Allocation{{Payee: "订单C", Amount: 50_00}})
	if err != nil {
		t.Fatalf("submit C2: %v", err)
	}
	if _, err := s.ConfirmClaim(c2.ID); err != nil {
		t.Fatalf("confirm C2: %v", err)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Confirmed != 100_00 || b.Available != 0 {
		t.Fatalf("fully claimed: %+v", b)
	}

	// 台账可追溯银行流水。
	ledger := s.ListLedger(r.SerialNo)
	if len(ledger) != 3 {
		t.Fatalf("expected 3 ledger entries, got %d", len(ledger))
	}
	var sum int64
	for _, e := range ledger {
		if e.SerialNo != r.SerialNo || e.Type != LedgerClaim {
			t.Fatalf("bad entry: %+v", e)
		}
		sum += e.Amount
	}
	if sum != 100_00 {
		t.Fatalf("ledger sum = %d", sum)
	}
}

func TestClaimOverAllocationRejected(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 100_01}}); !errors.Is(err, ErrOverAllocation) {
		t.Fatalf("expected over-allocation, got %v", err)
	}
	// 处理中金额也占用额度。
	if _, err := s.SubmitClaim("C2", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 60_00}}); err != nil {
		t.Fatalf("submit C2: %v", err)
	}
	b := balanceOf(t, s, r.SerialNo)
	if _, err := s.SubmitClaim("C3", r.SerialNo, b.Version, []Allocation{{Payee: "B", Amount: 40_01}}); !errors.Is(err, ErrOverAllocation) {
		t.Fatalf("pending should count toward limit, got %v", err)
	}
}

func TestStaleVersionRejected(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 10_00}}); err != nil {
		t.Fatalf("submit C1: %v", err)
	}
	// 版本已前进，旧版本的新申请被拒绝。
	if _, err := s.SubmitClaim("C2", r.SerialNo, 0, []Allocation{{Payee: "B", Amount: 10_00}}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected stale version, got %v", err)
	}
}

func TestStaleClaimConfirmFailsWithReason(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	c1, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 10_00}})
	if err != nil {
		t.Fatalf("submit C1: %v", err)
	}
	// 另一操作员提交了更新的认领，使 C1 锁定版本过期。
	b := balanceOf(t, s, r.SerialNo)
	if _, err := s.SubmitClaim("C2", r.SerialNo, b.Version, []Allocation{{Payee: "B", Amount: 10_00}}); err != nil {
		t.Fatalf("submit C2: %v", err)
	}
	if _, err := s.ConfirmClaim(c1.ID); err == nil {
		t.Fatal("stale claim confirm should fail")
	}
	got, _ := s.GetClaim("C1")
	if got.Status != ClaimFailed || got.Reason == "" {
		t.Fatalf("failed claim should keep reason: %+v", got)
	}
	// 失败申请不得生成台账，占用金额被释放。
	if n := len(s.ListLedger(r.SerialNo)); n != 0 {
		t.Fatalf("failed claim must not produce ledger, got %d", n)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Pending != 10_00 {
		t.Fatalf("pending should only include C2: %+v", b)
	}
}

func TestWithdrawReleasesPending(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 40_00}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.WithdrawClaim("C1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	b := balanceOf(t, s, r.SerialNo)
	if b.Pending != 0 || b.Available != 100_00 {
		t.Fatalf("withdraw should release pending: %+v", b)
	}
	if _, err := s.ConfirmClaim("C1"); !errors.Is(err, ErrClaimNotPending) {
		t.Fatalf("withdrawn claim cannot confirm: %v", err)
	}
	if _, err := s.WithdrawClaim("C1"); !errors.Is(err, ErrClaimNotPending) {
		t.Fatalf("double withdraw rejected: %v", err)
	}
}

func TestReversal(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 60_00}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ConfirmClaim("C1"); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// 部分冲正，恢复暂收余额。
	if _, err := s.Reverse("R1", "C1", 25_00, "归属错误"); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	b := balanceOf(t, s, r.SerialNo)
	if b.Confirmed != 35_00 || b.Available != 65_00 {
		t.Fatalf("after reversal: %+v", b)
	}

	// 重复冲正号返回原记录，不重复加回余额。
	again, err := s.Reverse("R1", "C1", 25_00, "归属错误")
	if err != nil || again.ID != "R1" {
		t.Fatalf("duplicate reversal should return original: %v %+v", err, again)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Confirmed != 35_00 {
		t.Fatalf("duplicate reversal must not restore twice: %+v", b)
	}

	// 冲正金额不能超过认领剩余。
	if _, err := s.Reverse("R2", "C1", 35_01, "超额"); !errors.Is(err, ErrReversalExceeded) {
		t.Fatalf("expected reversal exceeded, got %v", err)
	}
	if _, err := s.Reverse("R2", "C1", 35_00, "剩余冲正"); err != nil {
		t.Fatalf("reverse remaining: %v", err)
	}
	if b := balanceOf(t, s, r.SerialNo); b.Confirmed != 0 || b.Available != 100_00 {
		t.Fatalf("fully reversed: %+v", b)
	}

	revs := s.ListReversals(r.SerialNo)
	if len(revs) != 2 {
		t.Fatalf("expected 2 reversals, got %d", len(revs))
	}
	// 冲正后余额恢复，可由新的认领处理。
	b = balanceOf(t, s, r.SerialNo)
	if _, err := s.SubmitClaim("C2", r.SerialNo, b.Version, []Allocation{{Payee: "B", Amount: 100_00}}); err != nil {
		t.Fatalf("re-claim after reversal: %v", err)
	}
	// 台账包含正负两条线。
	var sum int64
	for _, e := range s.ListLedger(r.SerialNo) {
		sum += e.Amount
	}
	if sum != 0 {
		t.Fatalf("ledger net should be 0 after full reversal, got %d", sum)
	}
}

func TestReverseRequiresConfirmedClaim(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{{Payee: "A", Amount: 10_00}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Reverse("R1", "C1", 10_00, "未确认"); !errors.Is(err, ErrClaimNotConfirmed) {
		t.Fatalf("pending claim cannot be reversed: %v", err)
	}
}

func TestConcurrentClaimsNeverOverAllocate(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("C%d", i)
			// 每个操作员基于自己观察到的版本反复尝试认领 10 元。
			for attempt := 0; attempt < 50; attempt++ {
				b, err := s.GetBalance(r.SerialNo)
				if err != nil {
					return
				}
				_, err = s.SubmitClaim(id, r.SerialNo, b.Version, []Allocation{{Payee: "A", Amount: 10_00}})
				if errors.Is(err, ErrStaleVersion) {
					continue
				}
				if err != nil {
					return // 额度不足等，停止
				}
				if _, err := s.ConfirmClaim(id); err != nil {
					return
				}
				return
			}
		}(i)
	}
	wg.Wait()

	b := balanceOf(t, s, r.SerialNo)
	if b.Confirmed > 100_00 || b.Confirmed+b.Pending > 100_00 {
		t.Fatalf("over-allocated: %+v", b)
	}
	if b.Confirmed != 100_00 {
		t.Fatalf("expected full allocation, got %+v", b)
	}
	var sum int64
	for _, e := range s.ListLedger(r.SerialNo) {
		sum += e.Amount
	}
	if sum != b.Confirmed {
		t.Fatalf("ledger sum %d != confirmed %d", sum, b.Confirmed)
	}
}

func TestQueriesTraceableToSerial(t *testing.T) {
	s := NewStore()
	r := mustImport(t, s)
	if _, err := s.SubmitClaim("C1", r.SerialNo, 0, []Allocation{
		{Payee: "订单A", Amount: 30_00},
		{Payee: "订单B", Amount: 20_00},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ConfirmClaim("C1"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := s.Reverse("R1", "C1", 5_00, "部分错误"); err != nil {
		t.Fatalf("reverse: %v", err)
	}

	claims := s.ListClaims(r.SerialNo)
	if len(claims) != 1 || len(claims[0].Items) != 2 || claims[0].Reversed != 5_00 {
		t.Fatalf("claims: %+v", claims)
	}
	revs := s.ListReversals(r.SerialNo)
	if len(revs) != 1 || revs[0].ClaimID != "C1" || revs[0].SerialNo != r.SerialNo {
		t.Fatalf("reversals: %+v", revs)
	}
	ledger := s.ListLedger(r.SerialNo)
	if len(ledger) != 3 {
		t.Fatalf("ledger: %+v", ledger)
	}
	for _, e := range ledger {
		if e.SerialNo != r.SerialNo {
			t.Fatalf("entry not traceable to serial: %+v", e)
		}
	}
}
