package settlement

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newSucceededTransfer(t *testing.T, s *Service, id, merchant string, amount int64) {
	t.Helper()
	if _, err := s.CreateTransfer(id, merchant, amount, "CNY"); err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if _, err := s.ConfirmTransferSuccess(id, "SR-"+id); err != nil {
		t.Fatalf("confirm success: %v", err)
	}
}

func TestPartialReturns(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	now := time.Now()

	if _, err := s.ConfirmReturn("BR1", "T1", 400, "CNY", "账户关闭", now); err != nil {
		t.Fatalf("first return: %v", err)
	}
	if _, err := s.ConfirmReturn("BR2", "T1", 600, "CNY", "账户关闭", now); err != nil {
		t.Fatalf("second return: %v", err)
	}

	acct, _ := s.GetAccount("M1")
	if acct.Available != 1000 {
		t.Fatalf("available = %d, want 1000", acct.Available)
	}
	led := s.Ledger("M1")
	if len(led) != 2 {
		t.Fatalf("ledger entries = %d, want 2", len(led))
	}

	// 原转账仍为成功，成功回执保留
	view, err := s.GetTransferView("T1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Transfer.Status != TransferStatusSuccess || view.Transfer.SuccessReceipt != "SR-T1" {
		t.Fatalf("original transfer mutated: %+v", view.Transfer)
	}
	if view.RemainingAmount != 0 || view.TotalReturned != 1000 {
		t.Fatalf("remaining=%d total=%d", view.RemainingAmount, view.TotalReturned)
	}
	if len(view.Returns) != 2 {
		t.Fatalf("returns = %d, want 2", len(view.Returns))
	}
}

func TestReturnRequiresSuccessAndMatchingCurrency(t *testing.T) {
	s := NewService()
	if _, err := s.CreateTransfer("T1", "M1", 1000, "CNY"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.ConfirmReturn("BR1", "T1", 100, "CNY", "r", now); !errors.Is(err, ErrTransferNotSucceeded) {
		t.Fatalf("want ErrTransferNotSucceeded, got %v", err)
	}
	newSucceededTransfer(t, s, "T2", "M1", 1000)
	if _, err := s.ConfirmReturn("BR2", "T2", 100, "USD", "r", now); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("want ErrCurrencyMismatch, got %v", err)
	}
	if _, err := s.ConfirmReturn("BR3", "NOPE", 100, "CNY", "r", now); !errors.Is(err, ErrTransferNotFound) {
		t.Fatalf("want ErrTransferNotFound, got %v", err)
	}
}

func TestConcurrentReturnsNeverOverCredit(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	now := time.Now()

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			no := string(rune('A'+i)) + "-RET"
			_, err := s.ConfirmReturn(no, "T1", 100, "CNY", "r", now)
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			} else if !errors.Is(err, ErrReturnAmountExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want 10", succeeded)
	}
	acct, _ := s.GetAccount("M1")
	if acct.Available != 1000 {
		t.Fatalf("available = %d, want 1000", acct.Available)
	}
	if got := len(s.Ledger("M1")); got != 10 {
		t.Fatalf("ledger entries = %d, want 10", got)
	}
}

func TestConcurrentReturnFreezeWithdrawAtomic(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	s.OpenAccount("M1", "CNY")
	now := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			s.ConfirmReturn(string(rune('a'+i))+"-R", "T1", 20, "CNY", "r", now)
		}(i)
		go func() { defer wg.Done(); s.Freeze("M1", 30) }()
		go func() { defer wg.Done(); s.Withdraw("M1", 10) }()
	}
	wg.Wait()

	acct, _ := s.GetAccount("M1")
	if acct.Available < 0 || acct.Frozen < 0 {
		t.Fatalf("negative balance: %+v", acct)
	}
	// 台账入账总额必须等于 退汇入账 - 无遗漏（冲正未发生）
	var credited int64
	for _, e := range s.Ledger("M1") {
		credited += e.Amount
	}
	view, _ := s.GetTransferView("T1")
	if credited != view.TotalReturned {
		t.Fatalf("ledger credited %d != total returned %d", credited, view.TotalReturned)
	}
}

func TestIdempotentDuplicateReceipt(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	now := time.Now()

	first, err := s.ConfirmReturn("BR1", "T1", 500, "CNY", "账户关闭", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ConfirmReturn("BR1", "T1", 500, "CNY", "账户关闭", now)
	if err != nil {
		t.Fatalf("duplicate should be idempotent: %v", err)
	}
	if first.LedgerID != second.LedgerID {
		t.Fatalf("duplicate produced different result")
	}
	acct, _ := s.GetAccount("M1")
	if acct.Available != 500 {
		t.Fatalf("available = %d, want 500 (credited once)", acct.Available)
	}
	if got := len(s.Ledger("M1")); got != 1 {
		t.Fatalf("ledger entries = %d, want 1", got)
	}
}

func TestConflictingReceipt(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	newSucceededTransfer(t, s, "T2", "M1", 1000)
	now := time.Now()
	if _, err := s.ConfirmReturn("BR1", "T1", 500, "CNY", "r", now); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		transferID string
		amount     int64
		currency   string
	}{
		{"different transfer", "T2", 500, "CNY"},
		{"different amount", "T1", 600, "CNY"},
		{"different currency", "T1", 500, "USD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.ConfirmReturn("BR1", c.transferID, c.amount, c.currency, "r", now)
			if !errors.Is(err, ErrReturnConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
		})
	}
}

func TestReversal(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	now := time.Now()
	if _, err := s.ConfirmReturn("BR1", "T1", 500, "CNY", "r", now); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReverseReturn("BR1", 200, "错误退汇"); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if _, err := s.ReverseReturn("BR1", 200, "错误退汇"); err != nil {
		t.Fatalf("reversal 2: %v", err)
	}
	// 累计冲正不能超过已入账 500
	if _, err := s.ReverseReturn("BR1", 200, "x"); !errors.Is(err, ErrReversalAmountExceed) {
		t.Fatalf("want ErrReversalAmountExceed, got %v", err)
	}

	acct, _ := s.GetAccount("M1")
	if acct.Available != 100 {
		t.Fatalf("available = %d, want 100", acct.Available)
	}
	led := s.Ledger("M1")
	if len(led) != 3 {
		t.Fatalf("ledger = %d, want 3 (1 credit + 2 debit)", len(led))
	}
	var sum int64
	for _, e := range led {
		sum += e.Amount
	}
	if sum != 100 {
		t.Fatalf("ledger sum = %d, want 100", sum)
	}

	view, _ := s.GetTransferView("T1")
	if len(view.Returns) != 1 || len(view.Returns[0].Reversals) != 2 {
		t.Fatalf("view reversals not linked: %+v", view.Returns)
	}
	if view.Returns[0].Receipt.ReversedAmt != 400 {
		t.Fatalf("reversed amt = %d, want 400", view.Returns[0].Receipt.ReversedAmt)
	}
}

func TestReversalInsufficientBalance(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	now := time.Now()
	if _, err := s.ConfirmReturn("BR1", "T1", 500, "CNY", "r", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Withdraw("M1", 400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReverseReturn("BR1", 200, "x"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	// 失败不能产生台账
	if got := len(s.Ledger("M1")); got != 1 {
		t.Fatalf("ledger = %d, want 1", got)
	}
}

func TestSuccessReceiptImmutable(t *testing.T) {
	s := NewService()
	newSucceededTransfer(t, s, "T1", "M1", 1000)
	if _, err := s.ConfirmTransferSuccess("T1", "OTHER"); err == nil {
		t.Fatal("expected error when overwriting success receipt")
	}
	if tr, err := s.ConfirmTransferSuccess("T1", "SR-T1"); err != nil || tr.Status != TransferStatusSuccess {
		t.Fatalf("idempotent confirm failed: %v", err)
	}
}
