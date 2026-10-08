package settlement

import (
	"errors"
	"testing"
)

func testRule() BankRule {
	return BankRule{Version: "v1", MinPerInstruction: 100, MaxPerInstruction: 1000, DailyRemaining: 100000, MaxInstructions: 10}
}

func newBatch(t *testing.T, s *Service, total int64) *Batch {
	t.Helper()
	b, err := s.CreateBatch(Batch{BatchNo: "B1", PayerAccount: "A", Payee: "P", Currency: "CNY", TotalAmount: total, ValueDate: "2026-10-09"})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	return b
}

func TestSplitSumExactAndDeterministic(t *testing.T) {
	rule := testRule()
	for _, total := range []int64{100, 999, 1000, 1001, 9999, 10000} {
		a1, err := split(total, rule)
		if err != nil {
			t.Fatalf("total %d: %v", total, err)
		}
		a2, _ := split(total, rule)
		var sum int64
		for i, amt := range a1 {
			sum += amt
			if amt < rule.MinPerInstruction || amt > rule.MaxPerInstruction {
				t.Fatalf("total %d: amount %d out of bounds", total, amt)
			}
			if amt != a2[i] {
				t.Fatalf("total %d: split not deterministic", total)
			}
		}
		if sum != total {
			t.Fatalf("total %d: sum %d", total, sum)
		}
	}
}

func TestSplitRoundingBoundary(t *testing.T) {
	// 10001 / 1000 = 11 条 > 上限 10，必须失败
	if _, err := split(10001, testRule()); err == nil {
		t.Fatal("expected max instructions failure")
	}
	// 恰好 10 条上限：10000 可拆
	amounts, err := split(10000, testRule())
	if err != nil || len(amounts) != 10 {
		t.Fatalf("expected 10 instructions, got %v %v", amounts, err)
	}
	// 最小额约束：total=150, max=100 -> n=2, 2*min=200 > 150 不可行
	r := BankRule{Version: "v1", MinPerInstruction: 100, MaxPerInstruction: 100, DailyRemaining: 1000, MaxInstructions: 5}
	if _, err := split(150, r); err == nil {
		t.Fatal("expected min amount infeasibility")
	}
	// 每日额度边界：恰好等于额度可行，超出 1 即失败
	r2 := testRule()
	r2.DailyRemaining = 5000
	if _, err := split(5000, r2); err != nil {
		t.Fatalf("exact daily quota should pass: %v", err)
	}
	if _, err := split(5001, r2); err == nil {
		t.Fatal("expected daily quota failure")
	}
}

func TestGeneratePlanFailureSavesNothing(t *testing.T) {
	s := NewService()
	b := newBatch(t, s, 10001) // 超过 10 条上限
	_, _, err := s.GeneratePlan(b.ID, "P1", testRule())
	if err == nil {
		t.Fatal("expected split failure")
	}
	sum, _ := s.Summary(b.ID)
	if sum.PendingCount != 0 || sum.SuccessCount != 0 || sum.FailedCount != 0 {
		t.Fatal("no instructions should be saved on failure")
	}
	_, plan, instrs, _, _ := s.Trace(b.ID)
	if plan != nil || len(instrs) != 0 {
		t.Fatal("partial plan must not be persisted")
	}
}

func TestIdempotencyAndConflict(t *testing.T) {
	s := NewService()
	b := newBatch(t, s, 5000)
	// 批次号幂等：相同负载返回同一批次
	b2, err := s.CreateBatch(Batch{BatchNo: "B1", PayerAccount: "A", Payee: "P", Currency: "CNY", TotalAmount: 5000, ValueDate: "2026-10-09"})
	if err != nil || b2.ID != b.ID {
		t.Fatal("same batch no should be idempotent")
	}
	// 总金额变化 -> 冲突
	if _, err := s.CreateBatch(Batch{BatchNo: "B1", PayerAccount: "A", Payee: "P", Currency: "CNY", TotalAmount: 6000, ValueDate: "2026-10-09"}); !errors.Is(err, ErrConflict) {
		t.Fatal("expected conflict on total change")
	}
	// 方案号幂等：相同规则重复生成返回同一方案
	p1, i1, err := s.GeneratePlan(b.ID, "P1", testRule())
	if err != nil {
		t.Fatal(err)
	}
	p2, i2, err := s.GeneratePlan(b.ID, "P1", testRule())
	if err != nil || p2.ID != p1.ID || len(i2) != len(i1) {
		t.Fatal("same plan no should be idempotent")
	}
	// 规则变化 -> 冲突
	r2 := testRule()
	r2.Version = "v2"
	if _, _, err := s.GeneratePlan(b.ID, "P2", r2); !errors.Is(err, ErrConflict) {
		t.Fatal("expected conflict on rule change")
	}
	// 方案号被其他批次占用 -> 冲突
	b3, _ := s.CreateBatch(Batch{BatchNo: "B2", PayerAccount: "A", Payee: "P", Currency: "CNY", TotalAmount: 1000, ValueDate: "2026-10-09"})
	if _, _, err := s.GeneratePlan(b3.ID, "P1", testRule()); !errors.Is(err, ErrConflict) {
		t.Fatal("expected conflict on reused plan no")
	}
}

func TestDuplicateConfirmIdempotent(t *testing.T) {
	s := NewService()
	b := newBatch(t, s, 3000)
	p, instrs, _ := s.GeneratePlan(b.ID, "P1", testRule())
	if _, err := s.ConfirmPlan(p.ID); err != nil {
		t.Fatal(err)
	}
	p2, err := s.ConfirmPlan(p.ID)
	if err != nil || !p2.Confirmed {
		t.Fatal("duplicate confirm should be idempotent")
	}
	// 确认后可发送
	if err := s.SendInstruction(instrs[0].ID); err != nil {
		t.Fatal(err)
	}
	// 重复发送同一指令非法
	if err := s.SendInstruction(instrs[0].ID); !errors.Is(err, ErrBadState) {
		t.Fatal("expected bad state on double send")
	}
}

func TestPartialReceiptsAndSummary(t *testing.T) {
	s := NewService()
	b := newBatch(t, s, 2500) // 3 条: 834/833/833
	p, instrs, _ := s.GeneratePlan(b.ID, "P1", testRule())
	s.ConfirmPlan(p.ID)
	// 未确认方案不能发送
	s.SendInstruction(instrs[0].ID)
	s.SendInstruction(instrs[1].ID)
	if _, err := s.RecordReceipt(instrs[0].ID, true, "OK", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReceipt(instrs[1].ID, false, "E01", "insufficient"); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.SuccessAmount != 834 || sum.FailedAmount != 833 || sum.PendingAmount != 833 {
		t.Fatalf("bad summary: %+v", sum)
	}
	if !sum.Balanced {
		t.Fatal("summary must balance to total")
	}
	// 追踪：批次 -> 指令 -> 回执
	_, _, traced, receipts, _ := s.Trace(b.ID)
	if len(traced) != 3 || len(receipts[instrs[0].ID]) != 1 || !receipts[instrs[0].ID][0].Success {
		t.Fatal("trace should link instructions to receipts")
	}
}

func TestRetryKeepsIdentity(t *testing.T) {
	s := NewService()
	b := newBatch(t, s, 2000)
	p, instrs, _ := s.GeneratePlan(b.ID, "P1", testRule())
	s.ConfirmPlan(p.ID)
	s.SendInstruction(instrs[0].ID)
	s.SendInstruction(instrs[1].ID)
	s.RecordReceipt(instrs[0].ID, true, "OK", "")
	s.RecordReceipt(instrs[1].ID, false, "E01", "fail")
	// 成功指令不可重试
	if err := s.RetryInstruction(instrs[0].ID); !errors.Is(err, ErrBadState) {
		t.Fatal("success instruction must not be retried")
	}
	// 失败指令沿用原 ID 重试
	if err := s.RetryInstruction(instrs[1].ID); err != nil {
		t.Fatal(err)
	}
	_, after, _ := s.GetPlan(p.ID)
	if after[1].ID != instrs[1].ID || after[1].Attempts != 2 || after[1].Status != StatusProcessing {
		t.Fatal("retry must reuse identity and increment attempts")
	}
	// 重试后成功，总额仍守恒
	s.RecordReceipt(instrs[1].ID, true, "OK", "")
	sum, _ := s.Summary(b.ID)
	if sum.SuccessAmount != 2000 || !sum.Balanced {
		t.Fatalf("bad summary after retry: %+v", sum)
	}
}
