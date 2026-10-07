package settlement

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const waiting = 24 * time.Hour

func newStore() *MandateStore {
	return NewMandateStore(MandateConfig{RequiredApprovals: 2, WaitingPeriod: waiting})
}

func sampleApp() Application {
	return Application{
		HolderName:    "Alice Trading Ltd",
		BankID:        "BKCHCNBJ",
		AccountDigest: DigestAccount("6222020200112233445"),
		Currencies:    []string{"CNY", "USD"},
		EffectiveAt:   t0.Add(waiting),
	}
}

func submit(t *testing.T, s *MandateStore, app Application) VersionView {
	t.Helper()
	v, err := s.Submit(app, t0)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return v
}

func approveBoth(t *testing.T, s *MandateStore, accountID string, versionNo int) {
	t.Helper()
	suffix := fmt.Sprintf("%s-%d", accountID, versionNo)
	t.Helper()
	if err := s.Approve(accountID, versionNo, "reviewer-1", "evt-"+suffix+"-1", t0); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if err := s.Approve(accountID, versionNo, "reviewer-2", "evt-"+suffix+"-2", t0); err != nil {
		t.Fatalf("approve 2: %v", err)
	}
}

func TestApprovalThresholdRequiresTwoDistinctReviewers(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())

	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-th-1", t0); err != nil {
		t.Fatal(err)
	}
	// Same reviewer cannot decide twice on the same version.
	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-1b", t0); !errors.Is(err, ErrDuplicateReviewer) {
		t.Fatalf("want ErrDuplicateReviewer, got %v", err)
	}
	got, _ := s.GetVersion(v.AccountID, v.VersionNo)
	if got.Status != StatusPending {
		t.Fatalf("single approval must not pass the threshold, got %s", got.Status)
	}

	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-2", "evt-th-2", t0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetVersion(v.AccountID, v.VersionNo)
	if got.Status != StatusAwaitingEffect {
		t.Fatalf("want AWAITING_EFFECT after two approvals, got %s", got.Status)
	}
}

func TestReviewEventIdempotency(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())

	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-dup", t0); err != nil {
		t.Fatal(err)
	}
	// Replaying the same event ID is a no-op, not an error.
	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-dup", t0); err != nil {
		t.Fatalf("idempotent replay should succeed, got %v", err)
	}
	got, _ := s.GetVersion(v.AccountID, v.VersionNo)
	if len(got.Reviewers) != 1 {
		t.Fatalf("duplicate event must not add a second review, got %v", got.Reviewers)
	}
}

func TestRejectionTerminatesVersion(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())

	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-rj-1", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Reject(v.AccountID, v.VersionNo, "reviewer-2", "evt-2", "bank info mismatch", t0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetVersion(v.AccountID, v.VersionNo)
	if got.Status != StatusRejected {
		t.Fatalf("want REJECTED, got %s", got.Status)
	}
	// No further decisions on a terminated version.
	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-3", "evt-3", t0); !errors.Is(err, ErrVersionNotPending) {
		t.Fatalf("want ErrVersionNotPending, got %v", err)
	}
}

func TestWaitingPeriodBoundary(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())
	approveBoth(t, s, v.AccountID, v.VersionNo)

	// One second before the waiting period ends: not yet effective.
	if got := s.ActivateDue(t0.Add(waiting - time.Second)); len(got) != 0 {
		t.Fatalf("must not activate before waiting period ends")
	}
	if _, err := s.ActiveVersion(v.AccountID); !errors.Is(err, ErrNoActiveVersion) {
		t.Fatalf("want ErrNoActiveVersion, got %v", err)
	}

	// Exactly at the boundary: effective.
	if got := s.ActivateDue(t0.Add(waiting)); len(got) != 1 {
		t.Fatalf("must activate at waiting period boundary")
	}
	active, err := s.ActiveVersion(v.AccountID)
	if err != nil || active.VersionNo != v.VersionNo {
		t.Fatalf("want active version %d, got %+v err=%v", v.VersionNo, active, err)
	}
}

func TestEffectiveAtBeforeWaitingPeriodRejected(t *testing.T) {
	s := newStore()
	app := sampleApp()
	app.EffectiveAt = t0.Add(time.Hour) // earlier than final approval + waiting
	v := submit(t, s, app)
	if err := s.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-et-1", t0); err != nil {
		t.Fatal(err)
	}
	err := s.Approve(v.AccountID, v.VersionNo, "reviewer-2", "evt-2", t0)
	if !errors.Is(err, ErrEffectiveTooEarly) {
		t.Fatalf("want ErrEffectiveTooEarly, got %v", err)
	}
}

func TestFieldChangeRequiresNewVersion(t *testing.T) {
	s := newStore()
	v1 := submit(t, s, sampleApp())
	approveBoth(t, s, v1.AccountID, v1.VersionNo)
	s.ActivateDue(t0.Add(waiting))

	// Changing bank info creates a new version; the old approval must not
	// carry over.
	app2 := sampleApp()
	app2.AccountID = v1.AccountID
	app2.BankID = "ICBKCNBJ"
	app2.AccountDigest = DigestAccount("6222020200998877665")
	app2.EffectiveAt = t0.Add(2 * waiting)
	v2 := submit(t, s, app2)
	if v2.VersionNo != 2 {
		t.Fatalf("want version 2, got %d", v2.VersionNo)
	}
	got, _ := s.GetVersion(v1.AccountID, 2)
	if got.Status != StatusPending || len(got.Reviewers) != 0 {
		t.Fatalf("new version must restart review, got %s reviewers=%v", got.Status, got.Reviewers)
	}
	// Old version stays active until the new one takes effect.
	active, _ := s.ActiveVersion(v1.AccountID)
	if active.VersionNo != 1 {
		t.Fatalf("old version must remain active, got %d", active.VersionNo)
	}

	approveBoth(t, s, v1.AccountID, 2)
	s.ActivateDue(t0.Add(2 * waiting))
	active, _ = s.ActiveVersion(v1.AccountID)
	if active.VersionNo != 2 {
		t.Fatalf("want version 2 active, got %d", active.VersionNo)
	}
	old, _ := s.GetVersion(v1.AccountID, 1)
	if old.Status != StatusSuperseded {
		t.Fatalf("want old version SUPERSEDED, got %s", old.Status)
	}
}

func TestRevoke(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())
	approveBoth(t, s, v.AccountID, v.VersionNo)
	s.ActivateDue(t0.Add(waiting))

	if err := s.Revoke(v.AccountID, v.VersionNo, "holder request"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveVersion(v.AccountID); !errors.Is(err, ErrNoActiveVersion) {
		t.Fatalf("want no active version after revoke, got %v", err)
	}
	// Terminal versions cannot be revoked again.
	if err := s.Revoke(v.AccountID, v.VersionNo, "again"); err == nil {
		t.Fatal("revoking a revoked version must fail")
	}
}

func TestConcurrentAccountSwitchRace(t *testing.T) {
	s := newStore()
	v := submit(t, s, sampleApp())
	approveBoth(t, s, v.AccountID, v.VersionNo)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.ActivateDue(t0.Add(waiting))
			_, _ = s.ActiveVersion(v.AccountID)
			_ = s.History(v.AccountID)
		}(i)
	}
	wg.Wait()

	active, err := s.ActiveVersion(v.AccountID)
	if err != nil || active.VersionNo != 1 {
		t.Fatalf("concurrent activation must yield exactly one active version, got %+v err=%v", active, err)
	}
}

func TestQueriesNeverExposePlainAccountNumber(t *testing.T) {
	s := newStore()
	plain := "6222020200112233445"
	app := sampleApp()
	app.AccountDigest = DigestAccount(plain)
	v := submit(t, s, app)
	approveBoth(t, s, v.AccountID, v.VersionNo)
	s.ActivateDue(t0.Add(waiting))

	var views []VersionView
	active, _ := s.ActiveVersion(v.AccountID)
	views = append(views, active)
	views = append(views, s.History(v.AccountID)...)
	one, _ := s.GetVersion(v.AccountID, 1)
	views = append(views, one)

	for _, view := range views {
		if strings.Contains(view.MaskedAccount, plain) {
			t.Fatalf("plain account number leaked in view: %+v", view)
		}
		if !strings.Contains(view.MaskedAccount, "****") {
			t.Fatalf("account digest must be masked, got %q", view.MaskedAccount)
		}
	}
}
