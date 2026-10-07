package settlement

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func submitDefault(t *testing.T, s *AccountService, accountID, accountNumber string) *AccountVersion {
	t.Helper()
	v, err := s.Submit(accountID, AccountContent{
		Holder:     "Alice",
		BankID:     "BANK001",
		Currencies: []string{"CNY", "USD"},
	}, accountNumber, t0)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return v
}

func approveTwice(t *testing.T, s *AccountService, accountID string, version int) {
	t.Helper()
	if err := s.Approve(accountID, version, "reviewer-1", "evt-1", t0); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if err := s.Approve(accountID, version, "reviewer-2", "evt-2", t0); err != nil {
		t.Fatalf("approve 2: %v", err)
	}
}

func TestApprovalQuorumRequiresTwoDistinctApprovers(t *testing.T) {
	s := NewAccountService()
	v := submitDefault(t, s, "acct-1", "6222021234567890")

	if err := s.Approve("acct-1", v.Version, "reviewer-1", "evt-1", t0); err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusPending {
		t.Fatalf("single approval must not pass quorum, got %s", v.Status)
	}
	// 同一审核人不能重复决定
	if err := s.Approve("acct-1", v.Version, "reviewer-1", "evt-2", t0); err != ErrDuplicateApprover {
		t.Fatalf("expected ErrDuplicateApprover, got %v", err)
	}
	if err := s.Approve("acct-1", v.Version, "reviewer-2", "evt-3", t0); err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusWaiting {
		t.Fatalf("expected waiting after quorum, got %s", v.Status)
	}
	if got := v.Approvals(); len(got) != 2 || got[0] != "reviewer-1" || got[1] != "reviewer-2" {
		t.Fatalf("unexpected approvals %v", got)
	}
}

func TestReviewEventIdempotent(t *testing.T) {
	s := NewAccountService()
	v := submitDefault(t, s, "acct-1", "6222021234567890")

	if err := s.Approve("acct-1", v.Version, "reviewer-1", "evt-1", t0); err != nil {
		t.Fatal(err)
	}
	// 相同事件号重复投递：幂等忽略，不触发重复决定错误
	if err := s.Approve("acct-1", v.Version, "reviewer-1", "evt-1", t0); err != nil {
		t.Fatalf("duplicate event must be idempotent, got %v", err)
	}
	if len(v.Approvals()) != 1 {
		t.Fatalf("duplicate event changed state: %v", v.Approvals())
	}
}

func TestRejectTerminatesVersion(t *testing.T) {
	s := NewAccountService()
	v := submitDefault(t, s, "acct-1", "6222021234567890")

	if err := s.Approve("acct-1", v.Version, "reviewer-1", "evt-1", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Reject("acct-1", v.Version, "reviewer-2", "evt-2", t0); err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusRejected || v.RejectedBy() != "reviewer-2" {
		t.Fatalf("expected rejected by reviewer-2, got %s/%s", v.Status, v.RejectedBy())
	}
	// 终止后不能再审核
	if err := s.Approve("acct-1", v.Version, "reviewer-3", "evt-3", t0); err != ErrVersionNotPending {
		t.Fatalf("expected ErrVersionNotPending, got %v", err)
	}
}

func TestWaitingPeriodBoundary(t *testing.T) {
	s := NewAccountService()
	v := submitDefault(t, s, "acct-1", "6222021234567890")
	approveTwice(t, s, "acct-1", v.Version)

	if !v.EffectiveAt.Equal(t0.Add(WaitingPeriod)) {
		t.Fatalf("effective time = %v, want %v", v.EffectiveAt, t0.Add(WaitingPeriod))
	}
	// 等待期结束前一秒：未生效
	if got := s.ActivateDue(t0.Add(WaitingPeriod - time.Second)); len(got) != 0 {
		t.Fatalf("activated too early")
	}
	if s.CurrentEffective("acct-1", t0.Add(WaitingPeriod-time.Second)) != nil {
		t.Fatalf("no version should be effective before waiting period ends")
	}
	// 恰好到达等待期边界：生效
	if got := s.ActivateDue(t0.Add(WaitingPeriod)); len(got) != 1 {
		t.Fatalf("expected activation at boundary")
	}
	if cur := s.CurrentEffective("acct-1", t0.Add(WaitingPeriod)); cur == nil || cur.Version != v.Version {
		t.Fatalf("expected version %d effective", v.Version)
	}
}

func TestRequestedEffectiveAtCanDelayActivation(t *testing.T) {
	s := NewAccountService()
	later := t0.Add(72 * time.Hour)
	v, err := s.Submit("acct-1", AccountContent{
		Holder: "Alice", BankID: "BANK001", Currencies: []string{"CNY"}, EffectiveAt: later,
	}, "6222021234567890", t0)
	if err != nil {
		t.Fatal(err)
	}
	approveTwice(t, s, "acct-1", v.Version)
	if !v.EffectiveAt.Equal(later) {
		t.Fatalf("effective time should honor requested time, got %v", v.EffectiveAt)
	}
	if got := s.ActivateDue(t0.Add(WaitingPeriod)); len(got) != 0 {
		t.Fatalf("should not activate before requested effective time")
	}
}

func TestContentChangeRequiresNewVersion(t *testing.T) {
	s := NewAccountService()
	v1 := submitDefault(t, s, "acct-1", "6222021234567890")
	v2 := submitDefault(t, s, "acct-1", "6222029999888877") // 修改账号 -> 新版本

	if v2.Version != v1.Version+1 {
		t.Fatalf("expected new version, got %d -> %d", v1.Version, v2.Version)
	}
	if v1.ContentDigest == v2.ContentDigest {
		t.Fatalf("content digest must change when fields change")
	}
	if v1.AccountDigest == v2.AccountDigest {
		t.Fatalf("account digest must change when account number changes")
	}
	// 旧审批不会套到新版本上
	approveTwice(t, s, "acct-1", v1.Version)
	if v2.Status != StatusPending || len(v2.Approvals()) != 0 {
		t.Fatalf("new version must not inherit approvals from old version")
	}
}

func TestRevoke(t *testing.T) {
	s := NewAccountService()
	v := submitDefault(t, s, "acct-1", "6222021234567890")
	if err := s.Revoke("acct-1", v.Version); err != ErrNotRevocable {
		t.Fatalf("pending version not revocable, got %v", err)
	}
	approveTwice(t, s, "acct-1", v.Version)
	if err := s.Revoke("acct-1", v.Version); err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusRevoked {
		t.Fatalf("expected revoked, got %s", v.Status)
	}
}

func TestHistoryAndMasking(t *testing.T) {
	s := NewAccountService()
	plain := "6222021234567890"
	submitDefault(t, s, "acct-1", plain)
	submitDefault(t, s, "acct-1", "6222029999888877")

	h := s.History("acct-1")
	if len(h) != 2 || h[0].Version != 1 || h[1].Version != 2 {
		t.Fatalf("unexpected history: %+v", h)
	}
	for _, v := range h {
		if v.AccountMasked != "****7890" && v.AccountMasked != "****8877" {
			t.Fatalf("unexpected mask %s", v.AccountMasked)
		}
		// 普通查询与日志中不得出现明文账号
		if strings.Contains(v.String(), plain) {
			t.Fatalf("plaintext account leaked in String(): %s", v.String())
		}
	}
	if got, err := s.GetVersion("acct-1", 1); err != nil || got == nil {
		t.Fatalf("GetVersion failed")
	}
	if _, err := s.GetVersion("acct-1", 99); err != ErrRequestNotFound {
		t.Fatalf("expected ErrRequestNotFound, got %v", err)
	}
}
