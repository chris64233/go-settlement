package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func setup(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	if err := s.RegisterMerchant("m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReserveRule("m1", 1000, 30, base); err != nil { // 10%, 持有 30 天
		t.Fatal(err)
	}
	return s
}

func TestRegisterSettlementUsesEffectiveRule(t *testing.T) {
	s := setup(t)

	b1, err := s.RegisterSettlement("st1", "m1", 100000, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b1.Withheld != 10000 || b1.RuleVersion != 1 {
		t.Fatalf("unexpected batch: %+v", b1)
	}
	if want := base.Add(31 * 24 * time.Hour); !b1.ReleaseDate.Equal(want) {
		t.Fatalf("release date = %v, want %v", b1.ReleaseDate, want)
	}

	// 规则调整：比例升为 20%，持有 60 天。
	if _, err := s.SetReserveRule("m1", 2000, 60, base.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	b2, err := s.RegisterSettlement("st2", "m1", 100000, base.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b2.Withheld != 20000 || b2.RuleVersion != 2 {
		t.Fatalf("unexpected batch: %+v", b2)
	}

	// 已形成的批次不被重算。
	again, err := s.RegisterSettlement("st1", "m1", 100000, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.Withheld != 10000 || again.RuleVersion != 1 || again.ID != b1.ID {
		t.Fatalf("existing batch recomputed: %+v", again)
	}
}

func TestDeductEarliestExpiryFirst(t *testing.T) {
	s := setup(t)
	if _, err := s.RegisterSettlement("st1", "m1", 100000, base); err != nil { // 到期早
		t.Fatal(err)
	}
	if _, err := s.RegisterSettlement("st2", "m1", 100000, base.Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	d, err := s.Deduct("D1", "m1", 15000, "chargeback")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Allocations) != 2 ||
		d.Allocations[0] != (BatchAllocation{BatchID: "RB-st1", Amount: 10000}) ||
		d.Allocations[1] != (BatchAllocation{BatchID: "RB-st2", Amount: 5000}) {
		t.Fatalf("unexpected allocations: %+v", d.Allocations)
	}

	bal, err := s.Balance("m1")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Batches[0].Occupied != 10000 || bal.Batches[0].Remaining != 0 {
		t.Fatalf("batch1: %+v", bal.Batches[0])
	}
	if bal.Batches[1].Occupied != 5000 || bal.Batches[1].Remaining != 5000 {
		t.Fatalf("batch2: %+v", bal.Batches[1])
	}
}

func TestReleaseSkipsOccupiedAndFrozen(t *testing.T) {
	s := setup(t)
	if _, err := s.RegisterSettlement("st1", "m1", 100000, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deduct("D1", "m1", 3000, "risk"); err != nil {
		t.Fatal(err)
	}
	if err := s.Freeze("RB-st1", 2000); err != nil {
		t.Fatal(err)
	}

	// 到期前不释放。
	r0, err := s.ReleaseDue("R0", base.Add(29*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r0.Total != 0 {
		t.Fatalf("released before due: %+v", r0)
	}

	// 到期后只释放未被占用和冻结的部分：10000-3000-2000=5000。
	r1, err := s.ReleaseDue("R1", base.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Total != 5000 || len(r1.Entries) != 1 {
		t.Fatalf("unexpected release: %+v", r1)
	}

	// 解冻后再次扫描，释放剩余 2000；已扣除的 3000 不会被重复释放。
	if err := s.Unfreeze("RB-st1", 2000); err != nil {
		t.Fatal(err)
	}
	r2, err := s.ReleaseDue("R2", base.Add(31*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Total != 2000 {
		t.Fatalf("unexpected release: %+v", r2)
	}

	bal, _ := s.Balance("m1")
	if bal.AvailableBalance != 7000 {
		t.Fatalf("available = %d, want 7000", bal.AvailableBalance)
	}
	b := bal.Batches[0]
	if b.Occupied != 3000 || b.Released != 7000 || b.Remaining != 0 {
		t.Fatalf("batch: %+v", b)
	}
}

func TestLateDeductionCannotTouchReleased(t *testing.T) {
	s := setup(t)
	if _, err := s.RegisterSettlement("st1", "m1", 100000, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseDue("R1", base.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 全部释放后，迟到的扣款无保证金可占用。
	if _, err := s.Deduct("D1", "m1", 1000, "late"); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
}

func TestIdempotency(t *testing.T) {
	s := setup(t)
	if _, err := s.RegisterSettlement("st1", "m1", 100000, base); err != nil {
		t.Fatal(err)
	}

	d1, err := s.Deduct("D1", "m1", 3000, "risk")
	if err != nil {
		t.Fatal(err)
	}
	// 同号同内容返回原结果。
	d1again, err := s.Deduct("D1", "m1", 3000, "risk")
	if err != nil {
		t.Fatal(err)
	}
	if d1again.Amount != d1.Amount || len(d1again.Allocations) != 1 {
		t.Fatalf("replay mismatch: %+v", d1again)
	}
	// 金额或原因变化返回冲突。
	if _, err := s.Deduct("D1", "m1", 4000, "risk"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := s.Deduct("D1", "m1", 3000, "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	// 释放批次号幂等：同号返回原结果，不重复入账。
	r1, err := s.ReleaseDue("R1", base.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r1again, err := s.ReleaseDue("R1", base.Add(40*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r1again.Total != r1.Total {
		t.Fatalf("release replay mismatch: %d vs %d", r1again.Total, r1.Total)
	}
	bal, _ := s.Balance("m1")
	if bal.AvailableBalance != r1.Total {
		t.Fatalf("double release: available = %d", bal.AvailableBalance)
	}
}

func TestCompensate(t *testing.T) {
	s := setup(t)
	if _, err := s.RegisterSettlement("st1", "m1", 100000, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deduct("D1", "m1", 6000, "risk"); err != nil {
		t.Fatal(err)
	}

	c, err := s.Compensate("C1", "D1", 2000, "wrong amount")
	if err != nil {
		t.Fatal(err)
	}
	if c.Amount != 2000 {
		t.Fatalf("compensation: %+v", c)
	}

	// 原扣款记录保持不变。
	d, err := s.DeductionOf("D1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Amount != 6000 || d.Allocations[0].Amount != 6000 || d.Compensated != 2000 {
		t.Fatalf("deduction mutated: %+v", d)
	}

	bal, _ := s.Balance("m1")
	if bal.AvailableBalance != 2000 || bal.Batches[0].Occupied != 4000 {
		t.Fatalf("balance: %+v", bal)
	}

	// 补回号幂等，超量补回被拒绝。
	if _, err := s.Compensate("C1", "D1", 2000, "wrong amount"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compensate("C1", "D1", 3000, "wrong amount"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := s.Compensate("C2", "D1", 5000, "too much"); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
}

func TestConcurrentReleaseDeductFreeze(t *testing.T) {
	s := setup(t)
	for i := 0; i < 10; i++ {
		if _, err := s.RegisterSettlement(fmt.Sprintf("st%d", i), "m1", 100000, base); err != nil {
			t.Fatal(err)
		}
	}
	due := base.Add(30 * 24 * time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.ReleaseDue(fmt.Sprintf("R%d", i), due)
			_, _ = s.Deduct(fmt.Sprintf("D%d", i), "m1", 5000, "risk")
			_ = s.Freeze("RB-st0", 100)
			_ = s.Unfreeze("RB-st0", 100)
		}(i)
	}
	wg.Wait()

	bal, err := s.Balance("m1")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bal.Batches {
		if b.Occupied < 0 || b.Released < 0 || b.Frozen < 0 {
			t.Fatalf("negative component: %+v", b)
		}
		if b.Occupied+b.Released+b.Frozen > b.Withheld {
			t.Fatalf("over-used batch: %+v", b)
		}
		if b.Remaining != b.Withheld-b.Occupied-b.Released-b.Frozen {
			t.Fatalf("inconsistent batch: %+v", b)
		}
	}
	// 释放总额 + 占用总额不得超过暂扣总额。
	var released, occupied, withheld Money
	for _, b := range bal.Batches {
		released += b.Released
		occupied += b.Occupied
		withheld += b.Withheld
	}
	if released+occupied > withheld {
		t.Fatalf("released %d + occupied %d > withheld %d", released, occupied, withheld)
	}
	if bal.AvailableBalance != released {
		t.Fatalf("available %d != released %d", bal.AvailableBalance, released)
	}
}
