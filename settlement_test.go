package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mustCreate(t *testing.T, s *Service, id string, amount int64) {
	t.Helper()
	if _, err := s.CreateSettlement(id, "payer", "payee", amount); err != nil {
		t.Fatalf("CreateSettlement: %v", err)
	}
}

func mustDispute(t *testing.T, s *Service, number, settlementID string, amount int64) *Dispute {
	t.Helper()
	d, err := s.ApplyDispute(Dispute{
		Number:          number,
		SettlementID:    settlementID,
		Amount:          amount,
		Reason:          "货物短缺",
		EvidenceSummary: "签收单照片",
		EvidenceVersion: "v1",
		Initiator:       "payee",
	})
	if err != nil {
		t.Fatalf("ApplyDispute: %v", err)
	}
	return d
}

// 部分争议：冻结争议金额，未争议部分按原计划继续支付。
func TestPartialDisputeFreezesOnlyDisputedAmount(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)

	st, err := s.CompleteSettlement("S1")
	if err != nil {
		t.Fatalf("CompleteSettlement: %v", err)
	}
	if st.Status != StatusCompleted {
		t.Fatalf("status = %s, want COMPLETED", st.Status)
	}
	if st.PaidAmount != 700 {
		t.Fatalf("paid = %d, want 700（未争议部分）", st.PaidAmount)
	}
	if st.FrozenAmount != 300 {
		t.Fatalf("frozen = %d, want 300", st.FrozenAmount)
	}
	if st.Amount != 1000 {
		t.Fatalf("原结算金额被修改: %d", st.Amount)
	}
}

// 累计争议金额不得超过原结算金额。
func TestCumulativeDisputeLimit(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 600)
	mustDispute(t, s, "D2", "S1", 400)

	_, err := s.ApplyDispute(Dispute{Number: "D3", SettlementID: "S1", Amount: 1})
	if !errors.Is(err, ErrDisputeLimit) {
		t.Fatalf("err = %v, want ErrDisputeLimit", err)
	}
}

// 幂等：相同争议号内容一致返回原结果；金额或原结算变化返回冲突。
func TestDisputeIdempotencyAndConflict(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustCreate(t, s, "S2", 1000)
	first := mustDispute(t, s, "D1", "S1", 300)

	again, err := s.ApplyDispute(Dispute{Number: "D1", SettlementID: "S1", Amount: 300, Reason: "不同原因也视为同一申请"})
	if err != nil {
		t.Fatalf("重复申请应返回原结果: %v", err)
	}
	if again.CreatedAt != first.CreatedAt || again.Status != first.Status {
		t.Fatalf("重复申请未返回原结果: %+v", again)
	}

	if _, err = s.ApplyDispute(Dispute{Number: "D1", SettlementID: "S1", Amount: 400}); !errors.Is(err, ErrDisputeConflict) {
		t.Fatalf("金额变化应冲突, got %v", err)
	}
	if _, err = s.ApplyDispute(Dispute{Number: "D1", SettlementID: "S2", Amount: 300}); !errors.Is(err, ErrDisputeConflict) {
		t.Fatalf("原结算变化应冲突, got %v", err)
	}
}

// 已完成的结算：争议只记录待处理调整，成功状态不得回退。
func TestDisputeOnCompletedSettlementKeepsStatus(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	if _, err := s.CompleteSettlement("S1"); err != nil {
		t.Fatal(err)
	}
	mustDispute(t, s, "D1", "S1", 300)

	view, err := s.GetSettlementView("S1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Settlement.Status != StatusCompleted {
		t.Fatalf("已完成结算状态被回退: %s", view.Settlement.Status)
	}
	if view.Settlement.FrozenAmount != 0 {
		t.Fatalf("已完成结算不应冻结, frozen = %d", view.Settlement.FrozenAmount)
	}
	if len(view.Disputes) != 1 || view.Disputes[0].Status != DisputeAccepted {
		t.Fatalf("应记录待处理争议: %+v", view.Disputes)
	}

	// 决定产生独立调整记录，原结算金额与状态不变。
	_, adj, err := s.Decide("D1", DecisionRefundPayer, "ops", "证据充分", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if adj == nil || adj.Amount != 300 || adj.Type != DecisionRefundPayer {
		t.Fatalf("调整记录错误: %+v", adj)
	}
	view, _ = s.GetSettlementView("S1")
	if view.Settlement.Status != StatusCompleted || view.Settlement.Amount != 1000 {
		t.Fatalf("原结算被修改: %+v", view.Settlement)
	}
	if len(view.Adjustments) != 1 {
		t.Fatalf("应有一条调整记录: %+v", view.Adjustments)
	}
}

// 结算完成与争议受理并发：最终冻结+已付不得超过原结算金额。
func TestCompleteAndDisputeRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewService()
		mustCreate(t, s, "S1", 1000)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.CompleteSettlement("S1") }()
		go func() { defer wg.Done(); s.ApplyDispute(Dispute{Number: "D1", SettlementID: "S1", Amount: 400}) }()
		wg.Wait()

		view, err := s.GetSettlementView("S1")
		if err != nil {
			t.Fatal(err)
		}
		st := view.Settlement
		if st.Status != StatusCompleted {
			t.Fatalf("结算应完成: %s", st.Status)
		}
		if st.PaidAmount+st.FrozenAmount > st.Amount {
			t.Fatalf("已付 %d + 冻结 %d 超过原金额 %d", st.PaidAmount, st.FrozenAmount, st.Amount)
		}
		if len(view.Disputes) != 1 {
			t.Fatalf("争议应被受理: %+v", view.Disputes)
		}
		// 先完成后争议：只记录不冻结；先争议后完成：冻结 400、支付 600。
		if st.FrozenAmount == 400 && st.PaidAmount != 600 {
			t.Fatalf("冻结后支付金额错误: paid = %d", st.PaidAmount)
		}
		if st.FrozenAmount == 0 && st.PaidAmount != 1000 {
			t.Fatalf("先完成时应全额支付: paid = %d", st.PaidAmount)
		}
	}
}

// 并发决定同一争议：最多一个成功。
func TestConcurrentDecideOnlyOneSucceeds(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)

	const n = 20
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Decide("D1", DecisionPayPayee, fmt.Sprintf("ops-%d", i), "理由", "v1")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyResolved) {
			t.Fatalf("意外错误: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("成功决定数 = %d, want 1", successes)
	}
	view, _ := s.GetSettlementView("S1")
	if len(view.Decisions) != 1 || len(view.Adjustments) != 1 {
		t.Fatalf("决定与调整应各一条: %+v / %+v", view.Decisions, view.Adjustments)
	}
}

// 重复决定被拒绝，决定记录不可修改。
func TestDuplicateDecisionRejected(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)

	dec, _, err := s.Decide("D1", DecisionPayPayee, "ops-a", "首次决定", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Decide("D1", DecisionRefundPayer, "ops-b", "试图推翻", "v2"); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("重复决定应失败: %v", err)
	}
	view, _ := s.GetSettlementView("S1")
	if len(view.Decisions) != 1 || view.Decisions[0].DecidedBy != dec.DecidedBy {
		t.Fatalf("决定记录被修改: %+v", view.Decisions)
	}
}

// 决定执行失败时回滚：原结算与冻结金额保持可继续处理的状态。
func TestDecideFailureRollback(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)

	boom := errors.New("存储故障")
	s.beforeCommit = func() error { return boom }
	if _, _, err := s.Decide("D1", DecisionPayPayee, "ops", "理由", "v1"); !errors.Is(err, boom) {
		t.Fatalf("应返回注入错误: %v", err)
	}

	view, _ := s.GetSettlementView("S1")
	if view.Settlement.FrozenAmount != 300 {
		t.Fatalf("失败后冻结金额应保留: %d", view.Settlement.FrozenAmount)
	}
	if view.Disputes[0].Status != DisputeAccepted {
		t.Fatalf("失败后争议应仍为受理中: %s", view.Disputes[0].Status)
	}
	if len(view.Decisions) != 0 || len(view.Adjustments) != 0 {
		t.Fatalf("失败后不应写入决定或调整")
	}

	// 恢复后可继续处理并成功。
	s.beforeCommit = nil
	if _, adj, err := s.Decide("D1", DecisionPayPayee, "ops", "理由", "v1"); err != nil {
		t.Fatalf("重试应成功: %v", err)
	} else if adj == nil || adj.Amount != 300 {
		t.Fatalf("调整记录错误: %+v", adj)
	}
	view, _ = s.GetSettlementView("S1")
	if view.Settlement.FrozenAmount != 0 {
		t.Fatalf("冻结应解冻: %d", view.Settlement.FrozenAmount)
	}
}

// 驳回决定：不产生调整记录，冻结金额解冻回归原计划。
func TestRejectDecisionUnfreezes(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)

	_, adj, err := s.Decide("D1", DecisionReject, "ops", "证据不足", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if adj != nil {
		t.Fatalf("驳回不应产生调整: %+v", adj)
	}
	st, err := s.CompleteSettlement("S1")
	if err != nil {
		t.Fatal(err)
	}
	if st.PaidAmount != 1000 {
		t.Fatalf("驳回后应全额支付: %d", st.PaidAmount)
	}
}

// 查询视图：同时展示原结算、冻结金额、争议证据、决定与后续调整。
func TestSettlementView(t *testing.T) {
	s := NewService()
	mustCreate(t, s, "S1", 1000)
	mustDispute(t, s, "D1", "S1", 300)
	if _, _, err := s.Decide("D1", DecisionRefundPayer, "ops", "理由", "v1"); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetSettlementView("S1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Settlement.ID != "S1" || view.Settlement.Amount != 1000 {
		t.Fatalf("缺少原结算: %+v", view.Settlement)
	}
	if len(view.Disputes) != 1 || view.Disputes[0].EvidenceSummary == "" || view.Disputes[0].EvidenceVersion != "v1" {
		t.Fatalf("缺少争议证据: %+v", view.Disputes)
	}
	if len(view.Decisions) != 1 || view.Decisions[0].EvidenceVersion != "v1" || view.Decisions[0].DecidedBy != "ops" {
		t.Fatalf("缺少决定: %+v", view.Decisions)
	}
	if len(view.Adjustments) != 1 || view.Adjustments[0].Amount != 300 {
		t.Fatalf("缺少调整: %+v", view.Adjustments)
	}
}
