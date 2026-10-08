package settlement

import (
	"errors"
	"sync"
	"testing"
)

func mustCreate(t *testing.T, s *Store, id string, amount int64) {
	t.Helper()
	if err := s.CreateSettlement(id, amount); err != nil {
		t.Fatalf("create settlement: %v", err)
	}
}

func mustFile(t *testing.T, s *Store, did, sid string, amount int64) *Dispute {
	t.Helper()
	d, err := s.FileDispute(did, sid, amount, "reason", "evidence", "payer")
	if err != nil {
		t.Fatalf("file dispute: %v", err)
	}
	return d
}

func TestPartialDisputeFreezesOnlyDisputedAmount(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)

	d := mustFile(t, s, "D1", "S1", 300)
	if !d.FrozenApplied {
		t.Fatal("expected freeze applied on pending settlement")
	}
	view, err := s.GetSettlementView("S1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Settlement.FrozenAmount != 300 {
		t.Fatalf("frozen = %d, want 300", view.Settlement.FrozenAmount)
	}
	// 未争议部分可按原计划继续：结算仍可推进状态。
	if err := s.MarkSent("S1"); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if got := view.Settlement.Amount - view.Settlement.FrozenAmount; got != 700 {
		t.Fatalf("uncontested amount = %d, want 700", got)
	}
}

func TestCumulativeDisputeLimit(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustFile(t, s, "D1", "S1", 400)
	mustFile(t, s, "D2", "S1", 600)

	if _, err := s.FileDispute("D3", "S1", 1, "r", "e", "payer"); !errors.Is(err, ErrDisputeLimit) {
		t.Fatalf("err = %v, want ErrDisputeLimit", err)
	}
	if _, err := s.FileDispute("D4", "S1", 0, "r", "e", "payer"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("err = %v, want ErrInvalidAmount", err)
	}
}

func TestDisputeIdempotencyAndConflict(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustCreate(t, s, "S2", 1000)
	first := mustFile(t, s, "D1", "S1", 300)

	// 相同争议号、内容一致：返回原结果，不重复冻结。
	again, err := s.FileDispute("D1", "S1", 300, "reason", "evidence", "payer")
	if err != nil {
		t.Fatalf("idempotent refile: %v", err)
	}
	if again.ID != first.ID || again.Amount != first.Amount {
		t.Fatalf("idempotent refile returned different dispute")
	}
	view, _ := s.GetSettlementView("S1")
	if view.Settlement.FrozenAmount != 300 {
		t.Fatalf("frozen = %d, want 300 (no double freeze)", view.Settlement.FrozenAmount)
	}

	// 金额变化：冲突。
	if _, err := s.FileDispute("D1", "S1", 400, "reason", "evidence", "payer"); !errors.Is(err, ErrDisputeConflict) {
		t.Fatalf("err = %v, want ErrDisputeConflict", err)
	}
	// 原结算变化：冲突。
	if _, err := s.FileDispute("D1", "S2", 300, "reason", "evidence", "payer"); !errors.Is(err, ErrDisputeConflict) {
		t.Fatalf("err = %v, want ErrDisputeConflict", err)
	}
}

func TestCompletedSettlementDisputeRecordsAdjustmentOnly(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	if err := s.MarkCompleted("S1"); err != nil {
		t.Fatal(err)
	}

	d := mustFile(t, s, "D1", "S1", 500)
	if d.FrozenApplied {
		t.Fatal("completed settlement must not freeze")
	}
	view, _ := s.GetSettlementView("S1")
	if view.Settlement.Status != SettlementCompleted {
		t.Fatalf("status = %s, want COMPLETED (must not revert)", view.Settlement.Status)
	}
	if view.Settlement.FrozenAmount != 0 {
		t.Fatalf("frozen = %d, want 0", view.Settlement.FrozenAmount)
	}

	if _, err := s.Decide("D1", DecisionReturnToPayer, "ops", "verified"); err != nil {
		t.Fatal(err)
	}
	view, _ = s.GetSettlementView("S1")
	if view.Settlement.Status != SettlementCompleted {
		t.Fatalf("status changed after decision: %s", view.Settlement.Status)
	}
	if len(view.Adjustments) != 1 || view.Adjustments[0].Amount != -500 {
		t.Fatalf("adjustments = %+v, want one -500 adjustment", view.Adjustments)
	}
}

func TestDecideReleasesFreezeAndWritesDecision(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustFile(t, s, "D1", "S1", 300)

	dec, err := s.Decide("D1", DecisionPayPayee, "ops-1", "evidence accepted")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Decider != "ops-1" || dec.EvidenceVer != 1 {
		t.Fatalf("decision = %+v", dec)
	}
	view, _ := s.GetSettlementView("S1")
	if view.Settlement.FrozenAmount != 0 {
		t.Fatalf("frozen = %d, want 0 after decision", view.Settlement.FrozenAmount)
	}
	if len(view.Decisions) != 1 || len(view.Adjustments) != 1 || view.Adjustments[0].Amount != 300 {
		t.Fatalf("view = %+v", view)
	}

	// 重复决定被拒绝。
	if _, err := s.Decide("D1", DecisionReject, "ops-2", "retry"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("err = %v, want ErrAlreadyDecided", err)
	}
	view, _ = s.GetSettlementView("S1")
	if len(view.Decisions) != 1 || view.Decisions[0].Decider != "ops-1" {
		t.Fatalf("decision record was modified: %+v", view.Decisions)
	}
}

func TestConcurrentFileAndCompleteRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewStore()
		mustCreate(t, s, "S1", 1000)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.FileDispute("D1", "S1", 400, "r", "e", "payer") }()
		go func() { defer wg.Done(); s.MarkCompleted("S1") }()
		wg.Wait()

		view, _ := s.GetSettlementView("S1")
		if len(view.Disputes) != 1 {
			t.Fatalf("disputes = %d, want 1", len(view.Disputes))
		}
		// 竞态下要么冻结（受理先于完成），要么不冻结（完成先于受理），
		// 但冻结状态必须与受理时的快照一致。
		d := view.Disputes[0]
		if d.FrozenApplied && view.Settlement.FrozenAmount != 400 {
			t.Fatalf("frozen inconsistent: %+v", view.Settlement)
		}
		if !d.FrozenApplied && view.Settlement.FrozenAmount != 0 {
			t.Fatalf("unexpected freeze after completion: %+v", view.Settlement)
		}
	}
}

func TestConcurrentDecideOnlyOneSucceeds(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustFile(t, s, "D1", "S1", 300)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Decide("D1", DecisionPayPayee, "ops", "race")
		}(i)
	}
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1", succeeded)
	}
	view, _ := s.GetSettlementView("S1")
	if len(view.Decisions) != 1 || len(view.Adjustments) != 1 {
		t.Fatalf("decisions=%d adjustments=%d, want 1/1", len(view.Decisions), len(view.Adjustments))
	}
	if view.Settlement.FrozenAmount != 0 {
		t.Fatalf("frozen = %d, want 0", view.Settlement.FrozenAmount)
	}
}

func TestDecideFailureRollsBack(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustFile(t, s, "D1", "S1", 300)

	failErr := errors.New("adjustment store unavailable")
	s.applyAdjustment = func(adj *Adjustment) error { return failErr }

	if _, err := s.Decide("D1", DecisionReturnToPayer, "ops", "try"); !errors.Is(err, failErr) {
		t.Fatalf("err = %v, want injected failure", err)
	}
	// 失败后：冻结保持、争议仍可处理、无决定无调整。
	view, _ := s.GetSettlementView("S1")
	if view.Settlement.FrozenAmount != 300 {
		t.Fatalf("frozen = %d, want 300 after failed decide", view.Settlement.FrozenAmount)
	}
	if len(view.Decisions) != 0 || len(view.Adjustments) != 0 {
		t.Fatalf("state mutated on failure: %+v", view)
	}

	// 恢复后可继续处理成功。
	s.applyAdjustment = nil
	if _, err := s.Decide("D1", DecisionReturnToPayer, "ops", "retry"); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	view, _ = s.GetSettlementView("S1")
	if len(view.Adjustments) != 1 || view.Adjustments[0].Amount != -300 {
		t.Fatalf("adjustments = %+v", view.Adjustments)
	}
}

func TestViewAggregatesEverything(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, "S1", 1000)
	mustFile(t, s, "D1", "S1", 200)
	mustFile(t, s, "D2", "S1", 300)
	if _, err := s.Decide("D1", DecisionReject, "ops", "insufficient evidence"); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetSettlementView("S1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Settlement.FrozenAmount != 300 {
		t.Fatalf("frozen = %d, want 300 (D2 still open)", view.Settlement.FrozenAmount)
	}
	if len(view.Disputes) != 2 {
		t.Fatalf("disputes = %d, want 2", len(view.Disputes))
	}
	if view.Disputes[0].Evidence == "" || view.Disputes[1].Evidence == "" {
		t.Fatal("evidence missing from view")
	}
	if len(view.Decisions) != 1 || view.Decisions[0].Type != DecisionReject {
		t.Fatalf("decisions = %+v", view.Decisions)
	}
	if len(view.Adjustments) != 1 || view.Adjustments[0].Type != DecisionReject {
		t.Fatalf("adjustments = %+v", view.Adjustments)
	}
}
