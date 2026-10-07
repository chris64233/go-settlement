package settlement

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func newTestService() *Service {
	return NewService(func() time.Time { return testNow })
}

func valueDate() time.Time {
	return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
}

func mustAccept(t *testing.T, s *Service, req AcceptRequest) *Instruction {
	t.Helper()
	ins, err := s.Accept(req)
	if err != nil {
		t.Fatalf("Accept(%+v) failed: %v", req, err)
	}
	return ins
}

func TestRuleVersionSwitch(t *testing.T) {
	s := newTestService()
	v1 := s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt: testNow.Add(-time.Hour), MaxSingleAmount: 100,
	})
	ins1 := mustAccept(t, s, AcceptRequest{
		RequestID: "r1", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 90, ValueDate: valueDate(),
	})
	if ins1.RuleVersion != v1.Version {
		t.Fatalf("ins1 version = %d, want %d", ins1.RuleVersion, v1.Version)
	}

	// 发布新版本，放宽单笔上限。
	v2 := s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt: testNow.Add(-time.Minute), MaxSingleAmount: 1000,
	})
	if v2.Version != v1.Version+1 {
		t.Fatalf("v2.Version = %d, want %d", v2.Version, v1.Version+1)
	}
	ins2 := mustAccept(t, s, AcceptRequest{
		RequestID: "r2", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 500, ValueDate: valueDate(),
	})
	if ins2.RuleVersion != v2.Version {
		t.Fatalf("ins2 version = %d, want %d", ins2.RuleVersion, v2.Version)
	}

	// 已受理指令保留当时采用的版本。
	got, err := s.GetInstruction(ins1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RuleVersion != v1.Version {
		t.Fatalf("stored ins1 version = %d, want %d", got.RuleVersion, v1.Version)
	}

	// 未生效的规则不参与受理。
	s2 := newTestService()
	s2.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt: testNow.Add(time.Hour), MaxSingleAmount: 100,
	})
	if _, err := s2.Accept(AcceptRequest{
		RequestID: "r1", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 10, ValueDate: valueDate(),
	}); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("expected ErrRuleNotFound, got %v", err)
	}
}

func TestMultiLayerLimitConflict(t *testing.T) {
	s := newTestService()
	s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxSingleAmount:    100,
		MaxBilateralNet:    150,
		MaxUnilateralTotal: 200,
	})
	req := func(id string, amount int64) AcceptRequest {
		return AcceptRequest{RequestID: id, Payer: "A", Payee: "B", Currency: "CNY", Amount: amount, ValueDate: valueDate()}
	}

	// 单笔超限。
	if _, err := s.Accept(req("r1", 101)); err == nil {
		t.Fatal("expected single limit rejection")
	} else {
		var le *LimitExceededError
		if !errors.As(err, &le) || le.Layer != "single" {
			t.Fatalf("expected single layer error, got %v", err)
		}
	}

	mustAccept(t, s, req("r2", 100))

	// 双边净敞口超限（100+80>150）。
	if _, err := s.Accept(req("r3", 80)); err == nil {
		t.Fatal("expected bilateral net rejection")
	} else {
		var le *LimitExceededError
		if !errors.As(err, &le) || le.Layer != "bilateral_net" {
			t.Fatalf("expected bilateral_net layer error, got %v", err)
		}
	}

	// 反向指令冲销净敞口后可通过双边层，但受单方总敞口约束。
	s.PublishRule(LimitRule{
		Participant: "B", Counterparty: "A", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxBilateralNet:    150,
		MaxUnilateralTotal: 200,
	})
	mustAccept(t, s, AcceptRequest{
		RequestID: "r4", Payer: "B", Payee: "A", Currency: "CNY",
		Amount: 60, ValueDate: valueDate(),
	})
	// 此时 A 总敞口 160、B 总敞口 160；B 再付 50 给 A 将使双方达 210>200。
	if _, err := s.Accept(AcceptRequest{
		RequestID: "r5", Payer: "B", Payee: "A", Currency: "CNY",
		Amount: 50, ValueDate: valueDate(),
	}); err == nil {
		t.Fatal("expected unilateral total rejection")
	} else {
		var le *LimitExceededError
		if !errors.As(err, &le) || le.Layer != "unilateral_total" {
			t.Fatalf("expected unilateral_total layer error, got %v", err)
		}
	}

	// 被拒绝的指令不能留下任何一方的占用。
	detailA := s.QueryOccupancy("A", "B", "CNY", valueDate())
	if detailA.UnilateralTotal != 160 {
		t.Fatalf("A unilateral total = %d, want 160 (no partial occupancy)", detailA.UnilateralTotal)
	}
	detailB := s.QueryOccupancy("B", "A", "CNY", valueDate())
	if detailB.UnilateralTotal != 160 {
		t.Fatalf("B unilateral total = %d, want 160 (no partial occupancy)", detailB.UnilateralTotal)
	}
}

func TestConcurrentAcceptNeverExceedsLimit(t *testing.T) {
	s := newTestService()
	const limit = 1000
	s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxBilateralNet:    limit,
		MaxUnilateralTotal: limit,
	})

	var accepted int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Accept(AcceptRequest{
				RequestID: fmt.Sprintf("req-%d", i),
				Payer:     "A", Payee: "B", Currency: "CNY",
				Amount: 100, ValueDate: valueDate(),
			})
			if err == nil {
				atomic.AddInt64(&accepted, 100)
			}
		}(i)
	}
	wg.Wait()

	if accepted > limit {
		t.Fatalf("accepted total %d exceeds limit %d", accepted, limit)
	}
	if accepted != limit {
		t.Fatalf("accepted total %d, want exactly %d", accepted, limit)
	}
	detail := s.QueryOccupancy("A", "B", "CNY", valueDate())
	if detail.UnilateralTotal != limit {
		t.Fatalf("occupancy = %d, want %d", detail.UnilateralTotal, limit)
	}
}

func TestDuplicateRequestIdempotent(t *testing.T) {
	s := newTestService()
	s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxUnilateralTotal: 1000,
	})
	req := AcceptRequest{
		RequestID: "dup-1", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 300, ValueDate: valueDate(),
	}
	ins1 := mustAccept(t, s, req)
	ins2 := mustAccept(t, s, req)
	if ins1.ID != ins2.ID {
		t.Fatalf("duplicate request created new instruction %s, want %s", ins2.ID, ins1.ID)
	}
	if got := s.QueryOccupancy("A", "B", "CNY", valueDate()).UnilateralTotal; got != 300 {
		t.Fatalf("occupancy = %d, want 300 (no double booking)", got)
	}

	// 重复取消幂等。
	if _, err := s.Cancel(ins1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ins1.ID); err != nil {
		t.Fatalf("duplicate cancel should be idempotent: %v", err)
	}
	if got := s.QueryOccupancy("A", "B", "CNY", valueDate()).UnilateralTotal; got != 0 {
		t.Fatalf("occupancy after cancel = %d, want 0", got)
	}
	// 取消后失败属于冲突操作。
	if _, err := s.Fail(ins1.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
}

func TestLateCancelDoesNotReleaseOthersQuota(t *testing.T) {
	s := newTestService()
	s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxUnilateralTotal: 500,
	})
	req := func(id string, amount int64) AcceptRequest {
		return AcceptRequest{RequestID: id, Payer: "A", Payee: "B", Currency: "CNY", Amount: amount, ValueDate: valueDate()}
	}
	ins1 := mustAccept(t, s, req("r1", 300))
	if _, err := s.Complete(ins1.ID); err != nil {
		t.Fatal(err)
	}
	// ins2 取得剩余额度。
	mustAccept(t, s, req("r2", 200))

	// 迟到取消已完成的 ins1：必须拒绝且不影响 ins2 的额度。
	if _, err := s.Cancel(ins1.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState for late cancel, got %v", err)
	}
	detail := s.QueryOccupancy("A", "B", "CNY", valueDate())
	if detail.UnilateralTotal != 500 {
		t.Fatalf("occupancy = %d, want 500", detail.UnilateralTotal)
	}
	if detail.UsedAmount != 300 || detail.HeldAmount != 200 {
		t.Fatalf("used=%d held=%d, want 300/200", detail.UsedAmount, detail.HeldAmount)
	}
	// 额度仍被占满，新指令应被拒绝。
	if _, err := s.Accept(req("r3", 1)); err == nil {
		t.Fatal("expected rejection: quota still fully occupied")
	}
}

func TestFailReleasesOccupancy(t *testing.T) {
	s := newTestService()
	s.PublishRule(LimitRule{
		Participant: "A", Counterparty: "B", Currency: "CNY",
		EffectiveAt:        testNow.Add(-time.Hour),
		MaxUnilateralTotal: 100,
	})
	ins := mustAccept(t, s, AcceptRequest{
		RequestID: "r1", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 100, ValueDate: valueDate(),
	})
	if _, err := s.Fail(ins.ID); err != nil {
		t.Fatal(err)
	}
	// 额度释放后可再次受理。
	mustAccept(t, s, AcceptRequest{
		RequestID: "r2", Payer: "A", Payee: "B", Currency: "CNY",
		Amount: 100, ValueDate: valueDate(),
	})
}
