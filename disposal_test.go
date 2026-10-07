package settlement

import (
	"errors"
	"sync"
	"testing"
)

func newStoreWithParticipant(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterParticipant("p1", 100, 200, 50); err != nil {
		t.Fatal(err)
	}
	s.FundMutualFund(1000)
	return s
}

func TestWaterfallOrder(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Usages) != 4 {
		t.Fatalf("expected 4 layer usages, got %d", len(d.Usages))
	}
	want := []struct {
		layer  Layer
		amount int64
	}{
		{LayerCash, 100},
		{LayerCollateral, 200},
		{LayerFundShare, 50},
		{LayerMutualFund, 150},
	}
	for i, w := range want {
		if d.Usages[i].Layer != w.layer || d.Usages[i].Amount != w.amount {
			t.Errorf("usage %d: got %v/%d, want %v/%d", i, d.Usages[i].Layer, d.Usages[i].Amount, w.layer, w.amount)
		}
	}
	if !d.Completed() || d.RemainingGap != 0 {
		t.Errorf("expected completed disposal, gap=%d", d.RemainingGap)
	}
	cash, col, share, err := s.RemainingResources("p1")
	if err != nil {
		t.Fatal(err)
	}
	if cash != 0 || col != 0 || share != 0 {
		t.Errorf("remaining resources = %d/%d/%d, want 0/0/0", cash, col, share)
	}
	alloc, pool := s.MutualFundAllocation()
	if alloc["d1"] != 150 || pool != 850 {
		t.Errorf("mutual fund alloc=%v pool=%d, want 150/850", alloc, pool)
	}
}

func TestPartialLayersSkipEmpty(t *testing.T) {
	s := NewStore()
	if err := s.RegisterParticipant("p1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	s.FundMutualFund(100)
	if err := s.RegisterPosition("pos1", "p1", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Usages) != 1 || d.Usages[0].Layer != LayerMutualFund || d.Usages[0].Amount != 60 {
		t.Fatalf("expected single mutual fund usage of 60, got %+v", d.Usages)
	}
}

func TestRemainingGapNotCompleted(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 5000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	// 100+200+50+1000 = 1350，缺口 3650
	if d.Completed() {
		t.Error("disposal must not be marked completed with remaining gap")
	}
	if d.RemainingGap != 3650 {
		t.Errorf("remaining gap = %d, want 3650", d.RemainingGap)
	}
}

func TestFreezeBlocksOtherDisposalAndResourceChange(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPosition("pos2", "p1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d2", "pos2"); !errors.Is(err, ErrResourceFrozen) {
		t.Errorf("expected ErrResourceFrozen, got %v", err)
	}
	if _, err := s.InitiateDisposal("d3", "pos1"); !errors.Is(err, ErrPositionNotOpen) {
		t.Errorf("expected ErrPositionNotOpen, got %v", err)
	}
	if err := s.TopUpCash("p1", 50); !errors.Is(err, ErrResourceFrozen) {
		t.Errorf("expected ErrResourceFrozen on top-up, got %v", err)
	}
	if err := s.RevalueCollateral("p1", -10); !errors.Is(err, ErrResourceFrozen) {
		t.Errorf("expected ErrResourceFrozen on revalue, got %v", err)
	}
}

func TestResourceChangeBeforeExecuteConflicts(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterParticipant("p2", 10, 10, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPosition("pos1", "p1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	// 其他参与方的担保品估值变化推进全局估值版本，使旧计划冲突
	if err := s.RevalueCollateral("p2", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecuteDisposal("d1"); !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
	// 冲突后处置未执行，余额与台账均未变化
	d, _ := s.GetDisposal("d1")
	if d.Executed || len(d.Usages) != 0 {
		t.Error("conflicted disposal must not consume any layer")
	}
	cash, col, share, _ := s.RemainingResources("p1")
	if cash != 100 || col != 200 || share != 50 {
		t.Errorf("resources changed after conflict: %d/%d/%d", cash, col, share)
	}
}

func TestLateResourceChangeAfterExecute(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 150); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	usagesBefore := len(d.Usages)
	// 处置先完成，迟到的补缴只作用于剩余余额，不能改写使用明细
	if err := s.TopUpCash("p1", 500); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.GetDisposal("d1")
	if len(d2.Usages) != usagesBefore {
		t.Error("late resource change rewrote usage ledger")
	}
	cash, _, _, _ := s.RemainingResources("p1")
	if cash != 500 {
		t.Errorf("cash = %d, want 500", cash)
	}
}

func TestIdempotentInitiateAndConflict(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 100); err != nil {
		t.Fatal(err)
	}
	d1, err := s.InitiateDisposal("d1", "pos1")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.InitiateDisposal("d1", "pos1")
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Error("idempotent initiate should return the original disposal")
	}
	// 规则版本变化后相同处置号返回冲突
	s.SetRuleVersion(2)
	if _, err := s.InitiateDisposal("d1", "pos1"); !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict after rule version change, got %v", err)
	}
}

func TestIdempotentExecute(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d1, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || len(d2.Usages) != 1 {
		t.Error("repeated execute must return original result without double deduction")
	}
	cash, _, _, _ := s.RemainingResources("p1")
	if cash != 0 {
		t.Errorf("cash = %d, want 0 (deducted exactly once)", cash)
	}
}

func TestTraceability(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 250); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	d, err := s.ExecuteDisposal("d1")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range d.Usages {
		if u.Position.PositionID != "pos1" || u.Position.Version != 1 {
			t.Errorf("usage not traceable to frozen position: %+v", u.Position)
		}
		if u.Resources.CashVersion != 1 || u.Resources.ValuationVersion != 1 {
			t.Errorf("usage not traceable to frozen resource versions: %+v", u.Resources)
		}
	}
}

func TestConcurrentExecuteConsistent(t *testing.T) {
	s := newStoreWithParticipant(t)
	if err := s.RegisterPosition("pos1", "p1", 350); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateDisposal("d1", "pos1"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ExecuteDisposal("d1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	d, _ := s.GetDisposal("d1")
	var total int64
	for _, u := range d.Usages {
		total += u.Amount
	}
	if total != 350 {
		t.Errorf("ledger total = %d, want 350", total)
	}
	cash, col, share, _ := s.RemainingResources("p1")
	_, pool := s.MutualFundAllocation()
	if cash+col+share != 0 || pool != 1000 {
		t.Errorf("balances inconsistent with ledger: %d/%d/%d pool=%d", cash, col, share, pool)
	}
}
