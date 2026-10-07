package settlement

import (
	"errors"
	"sync"
	"testing"
)

func newEngineWithParticipant(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine()
	if err := e.RegisterParticipant("P1", 100, 50); err != nil {
		t.Fatal(err)
	}
	if err := e.AddCollateral("P1", "C1", 80); err != nil {
		t.Fatal(err)
	}
	if err := e.AddCollateral("P1", "C2", 40); err != nil {
		t.Fatal(err)
	}
	if err := e.ContributeMutualFund(1000); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWaterfallOrderAndRemainingGap(t *testing.T) {
	e := newEngineWithParticipant(t)
	// Deficit 300: cash 100 + collateral 120 + fund share 50 = 270, mutual fund 30.
	if err := e.RegisterPosition("POS1", "P1", 300); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	d, err := e.ExecuteDisposal("D1")
	if err != nil {
		t.Fatal(err)
	}
	if d.RemainingGap != 0 {
		t.Fatalf("remaining gap = %d, want 0", d.RemainingGap)
	}
	wantLayers := []Layer{LayerCash, LayerCollateral, LayerCollateral, LayerFundShare, LayerMutualFund}
	wantAmounts := []int64{100, 80, 40, 50, 30}
	if len(d.Usage) != len(wantLayers) {
		t.Fatalf("usage records = %d, want %d", len(d.Usage), len(wantLayers))
	}
	for i, u := range d.Usage {
		if u.Layer != wantLayers[i] || u.Amount != wantAmounts[i] {
			t.Fatalf("usage[%d] = %v/%d, want %v/%d", i, u.Layer, u.Amount, wantLayers[i], wantAmounts[i])
		}
		if u.PositionID != "POS1" || u.PositionVersion != 1 || u.RulesVersion != "v1" {
			t.Fatalf("usage[%d] not traceable to frozen versions: %+v", i, u)
		}
	}
	r, err := e.RemainingResources("P1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Cash != 0 || r.FundShare != 0 || r.Collaterals["C1"] != 0 || r.Collaterals["C2"] != 0 {
		t.Fatalf("participant resources not fully consumed: %+v", r)
	}
	if r.MutualFund != 970 {
		t.Fatalf("mutual fund = %d, want 970", r.MutualFund)
	}
	if got := e.MutualFundAllocations()["D1"]; got != 30 {
		t.Fatalf("mutual fund allocation = %d, want 30", got)
	}
}

func TestInsufficientResourcesLeavesGap(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RegisterPosition("POS1", "P1", 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	d, err := e.ExecuteDisposal("D1")
	if err != nil {
		t.Fatal(err)
	}
	if d.RemainingGap != 2000-100-120-50-1000 {
		t.Fatalf("remaining gap = %d", d.RemainingGap)
	}
	r, _ := e.RemainingResources("P1")
	if r.Cash != 0 || r.FundShare != 0 || r.MutualFund != 0 {
		t.Fatalf("resources should be exhausted: %+v", r)
	}
}

func TestPlanRejectsRevokedAndOccupiedResources(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RevokeCollateral("P1", "C2"); err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterPosition("POS1", "P1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); !errors.Is(err, ErrCollateralRevoked) {
		t.Fatalf("err = %v, want ErrCollateralRevoked", err)
	}

	e2 := newEngineWithParticipant(t)
	if err := e2.RegisterPosition("POS1", "P1", 10); err != nil {
		t.Fatal(err)
	}
	if err := e2.RegisterPosition("POS2", "P1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.PlanDisposal("D2", "POS2"); !errors.Is(err, ErrResourceOccupied) {
		t.Fatalf("err = %v, want ErrResourceOccupied", err)
	}
	if err := e2.RevokeCollateral("P1", "C1"); !errors.Is(err, ErrResourceOccupied) {
		t.Fatalf("err = %v, want ErrResourceOccupied", err)
	}
	if _, err := e2.PlanDisposal("D1", "POS2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// Idempotent re-plan with the same content returns the original plan.
	if _, err := e2.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatalf("idempotent re-plan failed: %v", err)
	}
}

func TestStalePlanConflictsOnResourceChange(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RegisterPosition("POS1", "P1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	// A top-up lands before execution: the frozen plan is stale.
	if err := e.TopUpCash("P1", 500); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExecuteDisposal("D1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// Balances must be untouched by the failed execution.
	r, _ := e.RemainingResources("P1")
	if r.Cash != 600 {
		t.Fatalf("cash = %d, want 600", r.Cash)
	}
}

func TestLateChangeCannotRewriteCompletedUsage(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RegisterPosition("POS1", "P1", 150); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	d1, err := e.ExecuteDisposal("D1")
	if err != nil {
		t.Fatal(err)
	}
	// Late top-up and revaluation must not rewrite the generated ledger.
	if err := e.TopUpCash("P1", 999); err != nil {
		t.Fatal(err)
	}
	if err := e.RevalueCollateral("P1", "C1", 5000); err != nil {
		t.Fatal(err)
	}
	d2, err := e.ExecuteDisposal("D1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Usage) != len(d1.Usage) {
		t.Fatalf("usage rewritten: %d vs %d records", len(d2.Usage), len(d1.Usage))
	}
	for i := range d1.Usage {
		if d1.Usage[i] != d2.Usage[i] {
			t.Fatalf("usage[%d] changed: %+v -> %+v", i, d1.Usage[i], d2.Usage[i])
		}
	}
	r, _ := e.RemainingResources("P1")
	if r.Cash != 999 {
		t.Fatalf("cash = %d, want 999 (top-up applies to remaining balance)", r.Cash)
	}
}

func TestRulesVersionChangeConflicts(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RegisterPosition("POS1", "P1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	e.SetRulesVersion("v2")
	if _, err := e.ExecuteDisposal("D1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestConcurrentTopUpAndExecution(t *testing.T) {
	for i := 0; i < 200; i++ {
		e := newEngineWithParticipant(t)
		if err := e.RegisterPosition("POS1", "P1", 150); err != nil {
			t.Fatal(err)
		}
		if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var execErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, execErr = e.ExecuteDisposal("D1")
		}()
		go func() {
			defer wg.Done()
			_ = e.TopUpCash("P1", 7)
		}()
		wg.Wait()
		r, _ := e.RemainingResources("P1")
		usage, _ := e.UsageRecords("D1")
		var used int64
		for _, u := range usage {
			used += u.Amount
		}
		if errors.Is(execErr, ErrConflict) {
			// Top-up won the race: nothing consumed, cash includes the top-up.
			if used != 0 || r.Cash != 107 {
				t.Fatalf("iter %d: conflict but state changed: used=%d cash=%d", i, used, r.Cash)
			}
		} else if execErr != nil {
			t.Fatalf("iter %d: unexpected err %v", i, execErr)
		} else {
			// Execution won: ledger and balance deductions must agree.
			// The late top-up may land afterwards, adding 7 to cash, but it
			// must not rewrite the generated usage ledger.
			if used != 150 || (r.Cash != 0 && r.Cash != 7) {
				t.Fatalf("iter %d: inconsistent state: used=%d cash=%d", i, used, r.Cash)
			}
			// The late top-up must not rewrite the ledger.
			d, _ := e.GetDisposal("D1")
			if d.RemainingGap != 0 {
				t.Fatalf("iter %d: remaining gap = %d", i, d.RemainingGap)
			}
		}
	}
}

func TestFailedExecutionConsumesNothing(t *testing.T) {
	e := newEngineWithParticipant(t)
	if err := e.RegisterPosition("POS1", "P1", 150); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanDisposal("D1", "POS1"); err != nil {
		t.Fatal(err)
	}
	if err := e.RevalueCollateral("P1", "C1", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExecuteDisposal("D1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	r, _ := e.RemainingResources("P1")
	if r.Cash != 100 || r.FundShare != 50 || r.Collaterals["C1"] != 60 || r.Collaterals["C2"] != 40 || r.MutualFund != 1000 {
		t.Fatalf("failed execution consumed resources: %+v", r)
	}
	usage, _ := e.UsageRecords("D1")
	if len(usage) != 0 {
		t.Fatalf("failed execution wrote usage records: %+v", usage)
	}
}
