package settlement

import (
	"strings"
	"testing"
)

// 建立已生效账户并创建一条指令，返回服务与指令。
func setupEffectiveInstruction(t *testing.T) (*AccountService, *InstructionService, *Instruction) {
	t.Helper()
	accts := NewAccountService()
	v, err := accts.Submit("acct-1", AccountContent{
		Holder: "Alice", BankID: "BANK001", Currencies: []string{"CNY"},
	}, "6222021234567890", t0)
	if err != nil {
		t.Fatal(err)
	}
	approveTwice(t, accts, "acct-1", v.Version)
	accts.ActivateDue(t0.Add(WaitingPeriod))

	instrs := NewInstructionService(accts)
	instr, err := instrs.CreateInstruction("acct-1", 10000, "CNY", t0.Add(WaitingPeriod))
	if err != nil {
		t.Fatal(err)
	}
	return accts, instrs, instr
}

func TestInstructionRecordsVersionEvidence(t *testing.T) {
	accts, _, instr := setupEffectiveInstruction(t)
	v, _ := accts.GetVersion("acct-1", 1)
	if instr.AccountVersion != 1 || instr.VersionDigest != v.ContentDigest || instr.AccountDigest != v.AccountDigest {
		t.Fatalf("instruction must freeze version evidence: %+v", instr)
	}
}

func TestInstructionRequiresEffectiveVersion(t *testing.T) {
	accts := NewAccountService()
	instrs := NewInstructionService(accts)
	if _, err := instrs.CreateInstruction("acct-x", 100, "CNY", t0); err != ErrNoEffectiveVersion {
		t.Fatalf("expected ErrNoEffectiveVersion, got %v", err)
	}
}

func TestInstructionCurrencyMustBeCovered(t *testing.T) {
	_, instrs, _ := setupEffectiveInstruction(t)
	if _, err := instrs.CreateInstruction("acct-1", 100, "EUR", t0.Add(WaitingPeriod)); err != ErrCurrencyNotCovered {
		t.Fatalf("expected ErrCurrencyNotCovered, got %v", err)
	}
}

func TestSendSucceedsWhenVersionStillEffective(t *testing.T) {
	_, instrs, instr := setupEffectiveInstruction(t)
	if err := instrs.SendInstruction(instr.ID, t0.Add(WaitingPeriod)); err != nil {
		t.Fatal(err)
	}
	if instr.Status != InstrSent {
		t.Fatalf("expected sent, got %s", instr.Status)
	}
	// 重复发送被拒绝
	if err := instrs.SendInstruction(instr.ID, t0.Add(WaitingPeriod)); err != ErrInstructionNotNew {
		t.Fatalf("expected ErrInstructionNotNew, got %v", err)
	}
}

func TestSendHaltedWhenVersionRevoked(t *testing.T) {
	accts, instrs, instr := setupEffectiveInstruction(t)
	if err := accts.Revoke("acct-1", instr.AccountVersion); err != nil {
		t.Fatal(err)
	}
	err := instrs.SendInstruction(instr.ID, t0.Add(WaitingPeriod))
	if err == nil || instr.Status != InstrHalted {
		t.Fatalf("expected halt, got err=%v status=%s", err, instr.Status)
	}
	if !strings.Contains(instr.HaltReason, "revoked") {
		t.Fatalf("halt reason must mention revocation: %s", instr.HaltReason)
	}
}

// 账户切换竞态：指令创建后、发送前新版本生效，旧指令必须停止。
func TestSendHaltedWhenVersionSuperseded(t *testing.T) {
	accts, instrs, instr := setupEffectiveInstruction(t)

	// 受益人提交变更并通过审核，等待期结束后新版本生效
	v2, err := accts.Submit("acct-1", AccountContent{
		Holder: "Alice", BankID: "BANK001", Currencies: []string{"CNY"},
	}, "6222029999888877", t0.Add(WaitingPeriod))
	if err != nil {
		t.Fatal(err)
	}
	approveTwice(t, accts, "acct-1", v2.Version)
	accts.ActivateDue(t0.Add(2 * WaitingPeriod))

	err = instrs.SendInstruction(instr.ID, t0.Add(2*WaitingPeriod))
	if err == nil || instr.Status != InstrHalted {
		t.Fatalf("expected halt, got err=%v status=%s", err, instr.Status)
	}
	if !strings.Contains(instr.HaltReason, "superseded") {
		t.Fatalf("halt reason must mention supersession: %s", instr.HaltReason)
	}
	// 新指令应采用新版本
	instr2, err := instrs.CreateInstruction("acct-1", 100, "CNY", t0.Add(2*WaitingPeriod))
	if err != nil {
		t.Fatal(err)
	}
	if instr2.AccountVersion != v2.Version {
		t.Fatalf("new instruction should use version %d, got %d", v2.Version, instr2.AccountVersion)
	}
}

// 已发送的指令保留原版本证据，不能静默换成新账户。
func TestSentInstructionKeepsOriginalVersionEvidence(t *testing.T) {
	accts, instrs, instr := setupEffectiveInstruction(t)
	if err := instrs.SendInstruction(instr.ID, t0.Add(WaitingPeriod)); err != nil {
		t.Fatal(err)
	}
	origDigest := instr.VersionDigest
	origAcctDigest := instr.AccountDigest

	// 新版本生效，旧版本被替代
	v2, err := accts.Submit("acct-1", AccountContent{
		Holder: "Alice", BankID: "BANK002", Currencies: []string{"CNY"},
	}, "6222029999888877", t0.Add(WaitingPeriod))
	if err != nil {
		t.Fatal(err)
	}
	approveTwice(t, accts, "acct-1", v2.Version)
	accts.ActivateDue(t0.Add(2 * WaitingPeriod))

	got, err := instrs.GetInstruction(instr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != InstrSent {
		t.Fatalf("sent instruction must stay sent, got %s", got.Status)
	}
	if got.AccountVersion != 1 || got.VersionDigest != origDigest || got.AccountDigest != origAcctDigest {
		t.Fatalf("sent instruction evidence must not change: %+v", got)
	}
}

func TestInstructionLogMasking(t *testing.T) {
	_, _, instr := setupEffectiveInstruction(t)
	if strings.Contains(instr.String(), "6222021234567890") {
		t.Fatalf("plaintext account leaked in instruction log: %s", instr.String())
	}
	if !strings.Contains(instr.String(), "****7890") {
		t.Fatalf("expected masked account in log: %s", instr.String())
	}
}
