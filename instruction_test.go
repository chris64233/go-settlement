package settlement

import (
	"errors"
	"strings"
	"testing"
)

// setupActiveAccount returns a store pair with one active account version.
func setupActiveAccount(t *testing.T) (*MandateStore, *InstructionStore, string) {
	t.Helper()
	m := newStore()
	v := submit(t, m, sampleApp())
	approveBoth(t, m, v.AccountID, v.VersionNo)
	m.ActivateDue(t0.Add(waiting))
	return m, NewInstructionStore(m), v.AccountID
}

func TestInstructionBindsActiveVersionAndSends(t *testing.T) {
	_, ins, accountID := setupActiveAccount(t)

	created, err := ins.CreateInstruction(accountID, 10000, "CNY", t0.Add(waiting))
	if err != nil {
		t.Fatal(err)
	}
	if created.Evidence.VersionNo != 1 || created.Evidence.ContentDigest == "" {
		t.Fatalf("instruction must record the authorized version evidence, got %+v", created.Evidence)
	}
	if err := ins.Send(created.ID, t0.Add(waiting)); err != nil {
		t.Fatal(err)
	}
	got, _ := ins.GetInstruction(created.ID)
	if got.Status != InstrSent {
		t.Fatalf("want SENT, got %s", got.Status)
	}
}

func TestInstructionRequiresActiveVersion(t *testing.T) {
	m := newStore()
	ins := NewInstructionStore(m)
	v := submit(t, m, sampleApp()) // pending, never approved
	if _, err := ins.CreateInstruction(v.AccountID, 100, "CNY", t0); !errors.Is(err, ErrNoActiveVersion) {
		t.Fatalf("want ErrNoActiveVersion, got %v", err)
	}
}

func TestInstructionCurrencyMustBeCovered(t *testing.T) {
	_, ins, accountID := setupActiveAccount(t)
	if _, err := ins.CreateInstruction(accountID, 100, "EUR", t0.Add(waiting)); !errors.Is(err, ErrCurrencyNotCovered) {
		t.Fatalf("want ErrCurrencyNotCovered, got %v", err)
	}
}

func TestRevokedVersionStopsPendingInstruction(t *testing.T) {
	m, ins, accountID := setupActiveAccount(t)
	created, err := ins.CreateInstruction(accountID, 100, "CNY", t0.Add(waiting))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(accountID, 1, "fraud alert"); err != nil {
		t.Fatal(err)
	}
	err = ins.Send(created.ID, t0.Add(waiting))
	if err == nil {
		t.Fatal("send must fail after the bound version is revoked")
	}
	got, _ := ins.GetInstruction(created.ID)
	if got.Status != InstrStopped || !strings.Contains(got.StopReason, "revoked") {
		t.Fatalf("want STOPPED with revoked reason, got %s %q", got.Status, got.StopReason)
	}
}

func TestSupersededVersionStopsPendingInstruction(t *testing.T) {
	m, ins, accountID := setupActiveAccount(t)
	created, err := ins.CreateInstruction(accountID, 100, "CNY", t0.Add(waiting))
	if err != nil {
		t.Fatal(err)
	}

	// A new version is approved and takes effect, replacing version 1.
	app2 := sampleApp()
	app2.AccountID = accountID
	app2.BankID = "ICBKCNBJ"
	app2.EffectiveAt = t0.Add(2 * waiting)
	v2 := submit(t, m, app2)
	approveBoth(t, m, accountID, v2.VersionNo)
	m.ActivateDue(t0.Add(2 * waiting))

	err = ins.Send(created.ID, t0.Add(2*waiting))
	if err == nil {
		t.Fatal("send must fail after the bound version is superseded")
	}
	got, _ := ins.GetInstruction(created.ID)
	if got.Status != InstrStopped || !strings.Contains(got.StopReason, "superseded") {
		t.Fatalf("want STOPPED with superseded reason, got %s %q", got.Status, got.StopReason)
	}
}

func TestSentInstructionKeepsOriginalVersionEvidence(t *testing.T) {
	m, ins, accountID := setupActiveAccount(t)
	created, err := ins.CreateInstruction(accountID, 100, "CNY", t0.Add(waiting))
	if err != nil {
		t.Fatal(err)
	}
	if err := ins.Send(created.ID, t0.Add(waiting)); err != nil {
		t.Fatal(err)
	}

	// Account moves to a new version after the instruction was sent.
	app2 := sampleApp()
	app2.AccountID = accountID
	app2.BankID = "ICBKCNBJ"
	app2.EffectiveAt = t0.Add(2 * waiting)
	v2 := submit(t, m, app2)
	approveBoth(t, m, accountID, v2.VersionNo)
	m.ActivateDue(t0.Add(2 * waiting))

	got, _ := ins.GetInstruction(created.ID)
	if got.Status != InstrSent {
		t.Fatalf("sent instruction must stay SENT, got %s", got.Status)
	}
	if got.Evidence.VersionNo != 1 || got.Evidence.BankID != "BKCHCNBJ" {
		t.Fatalf("sent instruction must keep original version evidence, got %+v", got.Evidence)
	}
	if strings.Contains(got.Evidence.MaskedAccount, "6222020200112233445") {
		t.Fatal("evidence must not contain the plain account number")
	}
}
