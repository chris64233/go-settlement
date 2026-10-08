package settlement

import (
	"errors"
	"testing"
)

var testRule = BankRule{
	Version:         "R1",
	MinPerTx:        100,
	MaxPerTx:        1000,
	DailyRemaining:  100000,
	MaxInstructions: 10,
}

func testBatch(no string, total Amount) Batch {
	return Batch{
		BatchNo:      no,
		PayerAccount: "ACC-001",
		Payee:        "SUPPLIER-X",
		Currency:     "CNY",
		TotalAmount:  total,
		ValueDate:    "2026-10-09",
	}
}

func sum(amounts []Amount) Amount {
	var s Amount
	for _, a := range amounts {
		s += a
	}
	return s
}

// 舍入边界：整除、余数、余数小于最小额、总额恰等于上限等。
func TestSplitRoundingEdges(t *testing.T) {
	cases := []struct {
		name  string
		total Amount
		rule  BankRule
		want  []Amount
	}{
		{"整除", 3000, testRule, []Amount{1000, 1000, 1000}},
		{"带余数", 3001, testRule, []Amount{751, 750, 750, 750}},
		{"余数逐笔分配", 7003, testRule, []Amount{876, 876, 876, 875, 875, 875, 875, 875}},
		{"单笔即可", 500, testRule, []Amount{500}},
		{"总额等于上限", 1000, testRule, []Amount{1000}},
		{"总额等于最小额", 100, testRule, []Amount{100}},
		{"尾笔不小于最小额", 1050, BankRule{Version: "R1", MinPerTx: 500, MaxPerTx: 1000, DailyRemaining: 100000, MaxInstructions: 10}, []Amount{525, 525}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rationale, err := SplitAmounts(c.total, c.rule)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rationale == "" {
				t.Fatal("rationale must not be empty")
			}
			if sum(got) != c.total {
				t.Fatalf("sum %d != total %d", sum(got), c.total)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
				if got[i] < c.rule.MinPerTx || got[i] > c.rule.MaxPerTx {
					t.Fatalf("amount %d out of [%d,%d]", got[i], c.rule.MinPerTx, c.rule.MaxPerTx)
				}
			}
		})
	}
}

// 确定性：相同输入多次拆分结果一致。
func TestSplitDeterministic(t *testing.T) {
	a, _, err := SplitAmounts(9999, testRule)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		b, _, err := SplitAmounts(9999, testRule)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != len(b) {
			t.Fatal("non-deterministic split")
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatal("non-deterministic split")
			}
		}
	}
}

// 规则限制导致无法拆分：整次失败，不保存任何子指令。
func TestSplitFailuresAreAtomic(t *testing.T) {
	svc := NewService()
	cases := []struct {
		name  string
		total Amount
		rule  BankRule
	}{
		{"超过每日额度", 200000, testRule},
		{"笔数超上限", 10001, BankRule{Version: "R1", MinPerTx: 1, MaxPerTx: 1000, DailyRemaining: 100000, MaxInstructions: 10}},
		{"无法满足最小额", 1500, BankRule{Version: "R1", MinPerTx: 800, MaxPerTx: 1000, DailyRemaining: 100000, MaxInstructions: 10}},
		{"总额低于最小额", 50, testRule},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			planNo := "P-FAIL-" + string(rune('A'+i))
			_, err := svc.GeneratePlan(planNo, testBatch("B-FAIL-"+string(rune('A'+i)), c.total), c.rule)
			if err == nil {
				t.Fatal("expected split failure")
			}
			if _, perr := svc.PlanOf(planNo); perr == nil {
				t.Fatal("partial plan must not be persisted")
			}
			if _, serr := svc.Summary("B-FAIL-" + string(rune('A'+i))); serr == nil {
				t.Fatal("failed batch must not have summary")
			}
		})
	}
}

// 幂等与冲突：批次号、方案号重复时参数一致返回原方案，规则或总金额变化返回冲突。
func TestIdempotencyAndConflict(t *testing.T) {
	svc := NewService()
	b := testBatch("B-1", 5000)
	p1, err := svc.GeneratePlan("P-1", b, testRule)
	if err != nil {
		t.Fatal(err)
	}

	p2, err := svc.GeneratePlan("P-1", b, testRule)
	if err != nil || p2 != p1 {
		t.Fatal("same planNo with same params must return existing plan")
	}

	changedTotal := b
	changedTotal.TotalAmount = 6000
	if _, err := svc.GeneratePlan("P-1", changedTotal, testRule); !errors.Is(err, ErrConflict) {
		t.Fatal("total change on same planNo must conflict")
	}

	changedRule := testRule
	changedRule.Version = "R2"
	if _, err := svc.GeneratePlan("P-1", b, changedRule); !errors.Is(err, ErrConflict) {
		t.Fatal("rule version change on same planNo must conflict")
	}
	if _, err := svc.GeneratePlan("P-2", changedTotal, testRule); !errors.Is(err, ErrConflict) {
		t.Fatal("total change on same batchNo must conflict")
	}
	if _, err := svc.GeneratePlan("P-2", b, changedRule); !errors.Is(err, ErrConflict) {
		t.Fatal("rule change on same batchNo must conflict")
	}
}

// 重复确认幂等。
func TestDuplicateConfirm(t *testing.T) {
	svc := NewService()
	if _, err := svc.GeneratePlan("P-1", testBatch("B-1", 2500), testRule); err != nil {
		t.Fatal(err)
	}
	p1, err := svc.ConfirmPlan("P-1")
	if err != nil || !p1.Confirmed {
		t.Fatal("first confirm failed")
	}
	p2, err := svc.ConfirmPlan("P-1")
	if err != nil || p2 != p1 || !p2.Confirmed {
		t.Fatal("duplicate confirm must be idempotent")
	}
	if len(p2.Instructions) != len(p1.Instructions) {
		t.Fatal("duplicate confirm must not change instructions")
	}
}

// 部分回执：批次汇总准确区分各状态金额，且总额核对一致。
func TestPartialReceiptsAndSummary(t *testing.T) {
	svc := NewService()
	p, err := svc.GeneratePlan("P-1", testBatch("B-1", 3001), testRule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmPlan("P-1"); err != nil {
		t.Fatal(err)
	}
	ids := []string{p.Instructions[0].InstructionID, p.Instructions[1].InstructionID}
	for _, id := range ids {
		if err := svc.MarkSent(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RecordReceipt(ids[0], "SUCCESS", "BR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordReceipt(ids[1], "FAILED", "BR-2"); err != nil {
		t.Fatal(err)
	}

	sum, err := svc.Summary("B-1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.SuccessAmount != 751 || sum.FailedAmount != 750 || sum.UnsentAmount != 1500 || sum.ProcessingAmount != 0 {
		t.Fatalf("bad summary: %s", sum)
	}
	if !sum.Reconciled() {
		t.Fatalf("summary not reconciled: %s", sum)
	}

	// 从批次追到每条指令及回执。
	trace, err := svc.Trace("B-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace) != 4 {
		t.Fatalf("expected 4 instructions, got %d", len(trace))
	}
	if len(trace[0].Receipts) != 1 || trace[0].Receipts[0].BankRef != "BR-1" {
		t.Fatal("receipt trace broken")
	}
	if trace[3].Status != StatusPending {
		t.Fatal("unsent instruction must stay PENDING")
	}
}

// 重试失败子指令：沿用业务身份，不重新拆分已成功部分。
func TestRetryFailedKeepsIdentity(t *testing.T) {
	svc := NewService()
	p, err := svc.GeneratePlan("P-1", testBatch("B-1", 2000), testRule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmPlan("P-1"); err != nil {
		t.Fatal(err)
	}
	okID := p.Instructions[0].InstructionID
	failID := p.Instructions[1].InstructionID
	for _, id := range []string{okID, failID} {
		if err := svc.MarkSent(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RecordReceipt(okID, "SUCCESS", "BR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordReceipt(failID, "FAILED", "BR-2"); err != nil {
		t.Fatal(err)
	}

	retried, err := svc.RetryFailed("P-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(retried) != 1 || retried[0] != failID {
		t.Fatalf("retry must reuse failed instruction identity, got %v", retried)
	}

	p2, _ := svc.PlanOf("P-1")
	if len(p2.Instructions) != 2 {
		t.Fatal("retry must not re-split")
	}
	if p2.Instructions[0].Status != StatusSuccess || p2.Instructions[0].Attempts != 1 {
		t.Fatal("succeeded instruction must be untouched")
	}
	if p2.Instructions[1].Status != StatusProcessing || p2.Instructions[1].Attempts != 2 {
		t.Fatal("retried instruction must be PROCESSING with attempt 2")
	}

	// 重试后成功，批次全部成功。
	if err := svc.RecordReceipt(failID, "SUCCESS", "BR-3"); err != nil {
		t.Fatal(err)
	}
	sum, _ := svc.Summary("B-1")
	if sum.SuccessAmount != 2000 || !sum.Reconciled() {
		t.Fatalf("bad summary after retry: %s", sum)
	}
	trace, _ := svc.Trace("B-1")
	if len(trace[1].Receipts) != 2 {
		t.Fatal("retried instruction must keep both receipts")
	}
}

// 未发送的指令不能接收回执。
func TestReceiptRequiresProcessing(t *testing.T) {
	svc := NewService()
	p, err := svc.GeneratePlan("P-1", testBatch("B-1", 500), testRule)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordReceipt(p.Instructions[0].InstructionID, "SUCCESS", "BR-1"); err == nil {
		t.Fatal("receipt on PENDING instruction must fail")
	}
}
