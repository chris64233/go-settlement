package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var testDay = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

// testNow 固定业务时钟，避免依赖真实时间。
var testNow = testDay.Add(2 * time.Hour)

func newServiceWithRules(t *testing.T, payerRule, payeeRule LimitRule) *Service {
	t.Helper()
	rules := NewRuleStore()
	rules.now = func() time.Time { return testNow }
	eff := testDay.Add(-24 * time.Hour)
	if _, err := rules.Publish(RuleKey{Participant: "A", Counterparty: "B", Currency: "CNY"}, payerRule, eff); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Publish(RuleKey{Participant: "B", Counterparty: "A", Currency: "CNY"}, payeeRule, eff); err != nil {
		t.Fatal(err)
	}
	svc := NewService(rules)
	svc.now = func() time.Time { return testNow }
	return svc
}

func TestAcceptFixesVersionAndUsage(t *testing.T) {
	svc := newServiceWithRules(t,
		LimitRule{MaxSingleAmount: 1000, MaxNetExposure: 5000, MaxGrossExposure: 8000},
		LimitRule{MaxNetExposure: 5000},
	)
	ins, err := svc.Accept("req-1", "A", "B", "CNY", 900, testDay)
	if err != nil {
		t.Fatal(err)
	}
	if ins.PayerVersion != 1 || ins.PayeeVersion != 1 {
		t.Fatalf("expected version 1/1, got %d/%d", ins.PayerVersion, ins.PayeeVersion)
	}
	if ins.Status != StatusReserved {
		t.Fatalf("expected RESERVED, got %s", ins.Status)
	}
	sum := svc.Usage("A", "B", "CNY", testDay)
	if sum.ReservedOutgoing != 900 {
		t.Fatalf("expected reserved outgoing 900, got %d", sum.ReservedOutgoing)
	}
	back := svc.Usage("B", "A", "CNY", testDay)
	if back.ReservedIncoming != 900 {
		t.Fatalf("expected reserved incoming 900, got %d", back.ReservedIncoming)
	}
}

func TestVersionSwitchKeepsAcceptedInstructions(t *testing.T) {
	rules := NewRuleStore()
	rules.now = func() time.Time { return testNow }
	eff := testDay.Add(-24 * time.Hour)
	if _, err := rules.Publish(RuleKey{Participant: "A", Counterparty: "B", Currency: "CNY"},
		LimitRule{MaxSingleAmount: 1000}, eff); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Publish(RuleKey{Participant: "B", Counterparty: "A", Currency: "CNY"},
		LimitRule{}, eff); err != nil {
		t.Fatal(err)
	}
	svc := NewService(rules)
	svc.now = func() time.Time { return testNow }

	first, err := svc.Accept("req-1", "A", "B", "CNY", 900, testDay)
	if err != nil {
		t.Fatal(err)
	}

	// 发布收紧的新版本后，新指令按 v2 校验，旧指令仍固定 v1。
	if _, err := rules.Publish(RuleKey{Participant: "A", Counterparty: "B", Currency: "CNY"},
		LimitRule{MaxSingleAmount: 500}, testDay.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept("req-2", "A", "B", "CNY", 900, testDay); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("expected limit exceeded under v2, got %v", err)
	}
	got, err := svc.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayerVersion != 1 {
		t.Fatalf("accepted instruction must keep version 1, got %d", got.PayerVersion)
	}
	// 已受理指令的生命周期不受版本切换影响。
	if _, err := svc.Complete(first.ID); err != nil {
		t.Fatal(err)
	}
	sum := svc.Usage("A", "B", "CNY", testDay)
	if sum.UsedOutgoing != 900 || sum.ReservedOutgoing != 0 {
		t.Fatalf("expected used 900, got %+v", sum)
	}
}

func TestLayeredLimitConflicts(t *testing.T) {
	// 单笔上限。
	svc := newServiceWithRules(t, LimitRule{MaxSingleAmount: 100}, LimitRule{})
	if _, err := svc.Accept("r1", "A", "B", "CNY", 101, testDay); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("single amount limit: got %v", err)
	}

	// 双边净敞口：A->B 600 后 B->A 500 可净额，再 A->B 突破上限。
	svc = newServiceWithRules(t, LimitRule{MaxNetExposure: 1000}, LimitRule{MaxNetExposure: 1000})
	if _, err := svc.Accept("r1", "A", "B", "CNY", 600, testDay); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept("r2", "B", "A", "CNY", 500, testDay); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept("r3", "A", "B", "CNY", 950, testDay); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("net exposure limit: got %v", err)
	}

	// 单方总敞口：多笔累计超限。
	svc = newServiceWithRules(t, LimitRule{MaxGrossExposure: 1000}, LimitRule{})
	if _, err := svc.Accept("r1", "A", "B", "CNY", 700, testDay); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept("r2", "A", "B", "CNY", 400, testDay); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("gross exposure limit: got %v", err)
	}

	// 收款方一侧限额不足同样整笔拒绝，且双方都不产生占用。
	svc = newServiceWithRules(t, LimitRule{}, LimitRule{MaxNetExposure: 100})
	if _, err := svc.Accept("r1", "A", "B", "CNY", 200, testDay); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("payee side limit: got %v", err)
	}
	if sum := svc.Usage("A", "B", "CNY", testDay); sum.ReservedOutgoing != 0 || sum.ReservedIncoming != 0 {
		t.Fatalf("rejected instruction must not reserve usage, got %+v", sum)
	}
}

func TestCompleteCancelFailLifecycle(t *testing.T) {
	svc := newServiceWithRules(t,
		LimitRule{MaxGrossExposure: 1000},
		LimitRule{},
	)
	done, _ := svc.Accept("r1", "A", "B", "CNY", 400, testDay)
	cancelled, _ := svc.Accept("r2", "A", "B", "CNY", 300, testDay)
	failed, _ := svc.Accept("r3", "A", "B", "CNY", 200, testDay)

	if _, err := svc.Complete(done.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Fail(failed.ID); err != nil {
		t.Fatal(err)
	}

	sum := svc.Usage("A", "B", "CNY", testDay)
	if sum.UsedOutgoing != 400 || sum.ReservedOutgoing != 0 {
		t.Fatalf("expected used 400 reserved 0, got %+v", sum)
	}

	// 释放后额度可被新指令取得。
	if _, err := svc.Accept("r4", "A", "B", "CNY", 500, testDay); err != nil {
		t.Fatalf("released quota should be reusable: %v", err)
	}

	// 迟到取消：已结算指令不可取消，重复取消幂等，均不影响他人额度。
	if _, err := svc.Cancel(done.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("late cancel of settled instruction: got %v", err)
	}
	if _, err := svc.Cancel(cancelled.ID); err != nil {
		t.Fatalf("repeated cancel should be idempotent: %v", err)
	}
	sum = svc.Usage("A", "B", "CNY", testDay)
	if sum.UsedOutgoing != 400 || sum.ReservedOutgoing != 500 {
		t.Fatalf("late cancel must not release others' quota, got %+v", sum)
	}
}

func TestConcurrentAcceptNeverExceedsLimit(t *testing.T) {
	svc := newServiceWithRules(t,
		LimitRule{MaxGrossExposure: 1000},
		LimitRule{},
	)
	const workers = 32
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Accept(fmt.Sprintf("req-%d", i), "A", "B", "CNY", 100, testDay)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if accepted != 10 {
		t.Fatalf("expected exactly 10 accepted, got %d", accepted)
	}
	if sum := svc.Usage("A", "B", "CNY", testDay); sum.ReservedOutgoing != 1000 {
		t.Fatalf("expected reserved 1000, got %+v", sum)
	}
}

func TestDuplicateRequestIsIdempotent(t *testing.T) {
	svc := newServiceWithRules(t, LimitRule{MaxGrossExposure: 1000}, LimitRule{})
	first, err := svc.Accept("req-dup", "A", "B", "CNY", 600, testDay)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Accept("req-dup", "A", "B", "CNY", 600, testDay)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate request must return same instruction, got %s vs %s", first.ID, second.ID)
	}
	if sum := svc.Usage("A", "B", "CNY", testDay); sum.ReservedOutgoing != 600 {
		t.Fatalf("duplicate request must not reserve twice, got %+v", sum)
	}

	// 并发重复请求同样只占用一次。
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Accept("req-dup", "A", "B", "CNY", 600, testDay); err != nil {
				t.Errorf("duplicate accept: %v", err)
			}
		}()
	}
	wg.Wait()
	if sum := svc.Usage("A", "B", "CNY", testDay); sum.ReservedOutgoing != 600 {
		t.Fatalf("concurrent duplicates must reserve once, got %+v", sum)
	}
}

func TestUsageDetails(t *testing.T) {
	svc := newServiceWithRules(t, LimitRule{}, LimitRule{})
	a1, _ := svc.Accept("r1", "A", "B", "CNY", 100, testDay)
	if _, err := svc.Accept("r2", "B", "A", "CNY", 40, testDay); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(a1.ID); err != nil {
		t.Fatal(err)
	}
	entries := svc.UsageDetails("A", "B", "CNY", testDay)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	var outUsed, inReserved int64
	for _, e := range entries {
		outUsed += e.Used
		if e.Incoming > 0 {
			inReserved += e.Reserved
		}
	}
	if outUsed != 100 || inReserved != 40 {
		t.Fatalf("unexpected details: %+v", entries)
	}
}
