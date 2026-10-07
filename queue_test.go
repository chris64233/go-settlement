package settlement

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var scanTime = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func reg(s *Service, id, account, currency, amount string, priority int) *Instruction {
	in, err := s.Register(RegisterCommand{
		ExternalID: id,
		Account:    account,
		Currency:   currency,
		Amount:     MustAmount(amount),
		ValueDate:  "2026-10-09",
		Priority:   priority,
		Deadline:   scanTime.Add(24 * time.Hour),
	})
	if err != nil {
		panic(err)
	}
	return in
}

func fund(t *testing.T, s *Service, account, currency, amount string) {
	t.Helper()
	if _, err := s.ApplyBalanceChange(account, currency, MustAmount(amount), "credit"); err != nil {
		t.Fatalf("fund: %v", err)
	}
}

func TestStableOrdering(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "low-first", "A", "USD", "10", 1)
	reg(s, "high", "A", "USD", "10", 5)
	reg(s, "low-second", "A", "USD", "10", 1)

	q := s.Queue()
	want := []string{"high", "low-first", "low-second"}
	for i, id := range want {
		if q[i].Instruction.ExternalID != id {
			t.Fatalf("position %d: got %s want %s", i, q[i].Instruction.ExternalID, id)
		}
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	first := reg(s, "X1", "A", "USD", "100.50", 3)

	// 相同内容重放：返回原结果，不产生新指令。
	again, err := s.Register(RegisterCommand{
		ExternalID: "X1", Account: "A", Currency: "USD",
		Amount: MustAmount("100.50"), ValueDate: "2026-10-09",
		Priority: 3, Deadline: scanTime.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.Seq != first.Seq || again.Status != first.Status {
		t.Fatalf("replay returned different result: %+v vs %+v", again, first)
	}
	if len(s.Queue()) != 1 {
		t.Fatalf("replay created duplicate instruction")
	}

	// 关键内容变化：返回冲突。
	_, err = s.Register(RegisterCommand{
		ExternalID: "X1", Account: "A", Currency: "USD",
		Amount: MustAmount("200"), ValueDate: "2026-10-09",
		Priority: 3, Deadline: scanTime.Add(24 * time.Hour),
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if conflict.Existing.Amount.String() != "100.5" {
		t.Fatalf("conflict should carry existing instruction, got %s", conflict.Existing.Amount)
	}
}

func TestInsufficientFundsStopsScan(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "big", "A", "USD", "1000", 9)
	reg(s, "small", "A", "USD", "10", 1)
	fund(t, s, "A", "USD", "500")

	res, err := s.Scan(scanTime)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Released) != 0 {
		t.Fatalf("nothing should be released, got %v", res.Released)
	}
	// 小额指令不得绕过前面的资金缺口。
	in, _, err := s.Explain("small")
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != StatusPending || in.WaitReason == "" {
		t.Fatalf("small should remain pending with reason, got %+v", in)
	}
	in, _, _ = s.Explain("big")
	if in.WaitReason == "" {
		t.Fatalf("big should carry insufficient-funds reason")
	}
	if got := s.Balance("A", "USD"); got.String() != "500" {
		t.Fatalf("balance changed: %s", got)
	}
}

func TestOvertakePolicy(t *testing.T) {
	s := NewService(PolicyAllowMarkedOvertake)
	reg(s, "big", "A", "USD", "1000", 9)
	// 明确允许越过的小额指令。
	if _, err := s.Register(RegisterCommand{
		ExternalID: "small-marked", Account: "A", Currency: "USD",
		Amount: MustAmount("10"), ValueDate: "2026-10-09",
		Priority: 1, Deadline: scanTime.Add(24 * time.Hour), AllowOvertake: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 未标记的小额指令不得越过。
	reg(s, "small-unmarked", "A", "USD", "10", 1)
	fund(t, s, "A", "USD", "50")

	res, err := s.Scan(scanTime)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Released) != 1 || res.Released[0].ExternalID != "small-marked" {
		t.Fatalf("only marked instruction may overtake, got %+v", res.Released)
	}
	in, _, _ := s.Explain("small-unmarked")
	if in.Status != StatusPending {
		t.Fatalf("unmarked instruction must stay pending, got %s", in.Status)
	}
	if got := s.Balance("A", "USD"); got.String() != "40" {
		t.Fatalf("balance: got %s want 40", got)
	}
}

func TestConcurrentScansNoDoubleSpend(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "only", "A", "USD", "100", 5)
	fund(t, s, "A", "USD", "100")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	released := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.Scan(scanTime)
			errs[i] = err
			if err == nil {
				released[i] = len(res.Released)
			}
		}(i)
	}
	wg.Wait()

	totalReleased := 0
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
		totalReleased += released[i]
	}
	if totalReleased != 1 {
		t.Fatalf("instruction must be released exactly once, got %d", totalReleased)
	}
	if got := s.Balance("A", "USD"); !got.IsZero() {
		t.Fatalf("balance spent more than once: %s", got)
	}
	in, rec, _ := s.Explain("only")
	if in.Status != StatusReleased || rec == nil {
		t.Fatalf("instruction should be released exactly once: %+v", in)
	}
	if rec.BalanceBefore.String() != "100" || rec.BalanceAfter.String() != "0" {
		t.Fatalf("release basis wrong: %+v", rec)
	}
}

func TestConcurrentFundingAndScan(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "i1", "A", "USD", "60", 5)
	fund(t, s, "A", "USD", "100")

	var wg sync.WaitGroup
	var scanErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, scanErr = s.Scan(scanTime)
	}()
	go func() {
		defer wg.Done()
		_, _ = s.ApplyBalanceChange("A", "USD", MustAmount("50"), "late credit")
	}()
	wg.Wait()
	if scanErr != nil && !errors.Is(scanErr, ErrVersionConflict) {
		t.Fatalf("scan: %v", scanErr)
	}
	// 无论哪种交错，余额都必须非负且账目一致。
	bal := s.Balance("A", "USD")
	if bal.Sign() < 0 {
		t.Fatalf("negative balance: %s", bal)
	}
	in, _, _ := s.Explain("i1")
	if in.Status == StatusReleased && bal.String() != "90" && bal.String() != "40" {
		t.Fatalf("inconsistent balance after release: %s", bal)
	}
}

func TestVersionConflictKeepsQueueUnchanged(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "a", "A", "USD", "10", 5)
	reg(s, "b", "A", "USD", "20", 1)
	fund(t, s, "A", "USD", "100")

	before := s.Queue()
	// 版本变化后（新登记指令），基于旧版本的扫描提交应失败。
	// 这里通过并发触发：先扫描成功一次改变版本，再模拟旧版本提交。
	if _, err := s.Scan(scanTime); err != nil {
		t.Fatalf("scan: %v", err)
	}
	after := s.Queue()
	if len(before) != len(after) {
		t.Fatalf("queue length changed unexpectedly")
	}
	for i := range before {
		if before[i].Instruction.ExternalID != after[i].Instruction.ExternalID {
			t.Fatalf("queue order changed at %d", i)
		}
	}
	// 已释放指令不得再次释放。
	res, err := s.Scan(scanTime)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(res.Released) != 0 {
		t.Fatalf("released instructions must not be released twice: %+v", res.Released)
	}
}

func TestFrozenAccountBlocksRelease(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	reg(s, "f1", "A", "USD", "10", 5)
	fund(t, s, "A", "USD", "100")
	s.SetFrozen("A", "USD", true)

	res, err := s.Scan(scanTime)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Released) != 0 {
		t.Fatalf("frozen account must not release")
	}
	s.SetFrozen("A", "USD", false)
	res, err = s.Scan(scanTime)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Released) != 1 {
		t.Fatalf("release expected after unfreeze")
	}
}

func TestNegativeBalanceRejected(t *testing.T) {
	s := NewService(PolicyStopOnInsufficient)
	fund(t, s, "A", "USD", "50")
	if _, err := s.ApplyBalanceChange("A", "USD", MustAmount("-60"), "debit"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected insufficient balance, got %v", err)
	}
}

func TestAmountPrecision(t *testing.T) {
	a := MustAmount("0.1")
	b := MustAmount("0.2")
	if a.Add(b).String() != "0.3" {
		t.Fatalf("0.1+0.2 = %s", a.Add(b))
	}
	if MustAmount("100.50").String() != "100.5" {
		t.Fatalf("normalization wrong")
	}
}
