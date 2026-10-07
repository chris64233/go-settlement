package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newSuccessTransfer(t *testing.T, amount int64) *Service {
	t.Helper()
	s := NewService()
	if err := s.OpenAccount("biz1", "CNY"); err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("biz1", amount*2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTransfer("T1", "biz1", amount, "CNY"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmTransferSuccess("T1", "BANK-RCPT-1"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPartialReturns(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	now := time.Now()
	if err := s.SubmitReturn("R1", "T1", 300, "CNY", "账户关闭", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitReturn("R2", "T1", 400, "CNY", "账户关闭", now); err != nil {
		t.Fatal(err)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 1000+300+400 {
		t.Fatalf("available = %d", acc.Available)
	}
	d, err := s.GetTransferDetail("T1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Transfer.Status != TransferStatusSuccess || d.Transfer.BankReceiptNo != "BANK-RCPT-1" {
		t.Fatalf("original success receipt must be preserved: %+v", d.Transfer)
	}
	if len(d.Returns) != 2 || d.Remaining != 300 {
		t.Fatalf("returns=%d remaining=%d", len(d.Returns), d.Remaining)
	}
	var returnLedgers int
	for _, e := range d.Ledger {
		if e.Type == LedgerReturnIn {
			returnLedgers++
		}
	}
	if returnLedgers != 2 {
		t.Fatalf("each return needs its own ledger entry, got %d", returnLedgers)
	}
}

func TestReturnRequiresSuccessAndMatchingInfo(t *testing.T) {
	s := NewService()
	_ = s.OpenAccount("biz1", "CNY")
	_ = s.Deposit("biz1", 1000)
	_ = s.CreateTransfer("T1", "biz1", 500, "CNY")
	if err := s.SubmitReturn("R1", "T1", 100, "CNY", "x", time.Now()); !errors.Is(err, ErrTransferNotSuccess) {
		t.Fatalf("pending transfer must reject return: %v", err)
	}
	_ = s.ConfirmTransferSuccess("T1", "B1")
	if err := s.SubmitReturn("R1", "T1", 100, "USD", "x", time.Now()); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("currency mismatch: %v", err)
	}
	if err := s.SubmitReturn("R1", "T9", 100, "CNY", "x", time.Now()); !errors.Is(err, ErrTransferNotFound) {
		t.Fatalf("unknown transfer: %v", err)
	}
}

func TestReturnExceeded(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	_ = s.SubmitReturn("R1", "T1", 800, "CNY", "x", time.Now())
	if err := s.SubmitReturn("R2", "T1", 300, "CNY", "x", time.Now()); !errors.Is(err, ErrReturnExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 1800 {
		t.Fatalf("failed return must not credit balance: %d", acc.Available)
	}
}

func TestConcurrentReturnsNoOverCredit(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	var wg sync.WaitGroup
	var okCount int64
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.SubmitReturn(fmt.Sprintf("R%d", i), "T1", 100, "CNY", "x", time.Now())
			if err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			} else if !errors.Is(err, ErrReturnExceeded) {
				t.Errorf("unexpected err: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if okCount != 10 {
		t.Fatalf("exactly 10 returns of 100 should succeed, got %d", okCount)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 2000 {
		t.Fatalf("available = %d", acc.Available)
	}
	d, _ := s.GetTransferDetail("T1")
	if d.Remaining != 0 || d.Transfer.ReturnedAmount != 1000 {
		t.Fatalf("returned=%d remaining=%d", d.Transfer.ReturnedAmount, d.Remaining)
	}
}

func TestDuplicateReturnIdempotentAndConflict(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	now := time.Now()
	if err := s.SubmitReturn("R1", "T1", 300, "CNY", "账户关闭", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitReturn("R1", "T1", 300, "CNY", "账户关闭", now); err != nil {
		t.Fatalf("identical retry must return first result: %v", err)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 1300 {
		t.Fatalf("duplicate must not double credit: %d", acc.Available)
	}
	if err := s.SubmitReturn("R1", "T1", 400, "CNY", "x", now); !errors.Is(err, ErrReturnConflict) {
		t.Fatalf("amount change must conflict: %v", err)
	}
	if err := s.SubmitReturn("R1", "T1", 300, "USD", "x", now); !errors.Is(err, ErrReturnConflict) {
		t.Fatalf("currency change must conflict: %v", err)
	}
	_ = s.CreateTransfer("T2", "biz1", 100, "CNY")
	_ = s.ConfirmTransferSuccess("T2", "B2")
	if err := s.SubmitReturn("R1", "T2", 300, "CNY", "x", now); !errors.Is(err, ErrReturnConflict) {
		t.Fatalf("transfer change must conflict: %v", err)
	}
	d, _ := s.GetTransferDetail("T1")
	if len(d.Returns) != 1 {
		t.Fatalf("only one return receipt expected, got %d", len(d.Returns))
	}
}

func TestReversal(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	_ = s.SubmitReturn("R1", "T1", 600, "CNY", "误退", time.Now())
	if err := s.ReverseReturn("REV1", "R1", 200, "冲正"); err != nil {
		t.Fatal(err)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 1000+600-200 {
		t.Fatalf("available = %d", acc.Available)
	}
	// 幂等
	if err := s.ReverseReturn("REV1", "R1", 200, "冲正"); err != nil {
		t.Fatalf("reversal retry must be idempotent: %v", err)
	}
	// 冲突
	if err := s.ReverseReturn("REV1", "R1", 300, "x"); !errors.Is(err, ErrReversalConflict) {
		t.Fatalf("reversal conflict: %v", err)
	}
	// 累计冲正不能超过已入账金额
	if err := s.ReverseReturn("REV2", "R1", 500, "x"); !errors.Is(err, ErrReversalExceeded) {
		t.Fatalf("reversal exceeded: %v", err)
	}
	if err := s.ReverseReturn("REV3", "R9", 100, "x"); !errors.Is(err, ErrReturnNotFound) {
		t.Fatalf("unknown return: %v", err)
	}
	d, _ := s.GetTransferDetail("T1")
	if len(d.Reversals) != 1 || d.Reversals[0].BankReturnNo != "R1" {
		t.Fatalf("reversal association broken: %+v", d.Reversals)
	}
	if d.Returns[0].CreditedAmount() != 400 {
		t.Fatalf("credited = %d", d.Returns[0].CreditedAmount())
	}
	if d.Remaining != 600 { // 1000 - (600-200)
		t.Fatalf("remaining after reversal = %d", d.Remaining)
	}
}

func TestConcurrentReturnFreezeWithdrawAtomic(t *testing.T) {
	s := newSuccessTransfer(t, 1000) // available 1000 after transfer out
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.SubmitReturn(fmt.Sprintf("R%d", i), "T1", 100, "CNY", "x", time.Now())
		}(i)
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Freeze("biz1", 50)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Withdraw("biz1", 50)
		}()
	}
	wg.Wait()
	acc, _ := s.GetAccount("biz1")
	// 余额永不为负，台账与余额一致
	if acc.Available < 0 || acc.Frozen < 0 {
		t.Fatalf("negative balance: %+v", acc)
	}
	d, _ := s.GetTransferDetail("T1")
	var ledgerSum int64
	for _, e := range d.Ledger {
		switch e.Type {
		case LedgerReturnIn:
			ledgerSum += e.Amount
		}
	}
	if ledgerSum != d.Transfer.ReturnedAmount {
		t.Fatalf("ledger sum %d != returned %d", ledgerSum, d.Transfer.ReturnedAmount)
	}
}

func TestFailedWithdrawDoesNotAffectReturn(t *testing.T) {
	s := newSuccessTransfer(t, 1000)
	if err := s.Withdraw("biz1", 5000); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected insufficient, got %v", err)
	}
	if err := s.SubmitReturn("R1", "T1", 1000, "CNY", "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	acc, _ := s.GetAccount("biz1")
	if acc.Available != 2000 {
		t.Fatalf("available = %d", acc.Available)
	}
}
