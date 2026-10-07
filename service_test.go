package settlement

import (
	"errors"
	"testing"
)

func createCmd(reqNo string) CreateInstructionCmd {
	return CreateInstructionCmd{
		RequestNo: reqNo,
		Amount:    10000,
		Currency:  "CNY",
		FundOwner: "MERCHANT-1",
		Beneficiary: Beneficiary{
			AccountNo:   "6222020200112233",
			AccountName: "张三",
			BankCode:    "ICBC",
		},
	}
}

func newRejected(t *testing.T, s *Service, reqNo string) *Instruction {
	t.Helper()
	ins, err := s.CreateInstruction(createCmd(reqNo))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RegisterReceipt(Receipt{ReceiptID: "RC-" + reqNo, RequestNo: reqNo, Type: ReceiptReject, Reason: "账号不存在"}); err != nil {
		t.Fatalf("reject receipt: %v", err)
	}
	return ins
}

func TestCreateInstructionIdempotent(t *testing.T) {
	s := NewService()
	a, err := s.CreateInstruction(createCmd("REQ-1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateInstruction(createCmd("REQ-1"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same request no should return same instruction")
	}
	c := createCmd("REQ-1")
	c.Amount = 999
	if _, err := s.CreateInstruction(c); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestRepairRequiresRejected(t *testing.T) {
	s := NewService()
	ins, _ := s.CreateInstruction(createCmd("REQ-1"))
	if _, err := s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: Beneficiary{AccountNo: "1", AccountName: "李四", BankCode: "ICBC"}}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pending instruction must not be repairable: %v", err)
	}

	ins2, _ := s.CreateInstruction(createCmd("REQ-2"))
	s.RegisterReceipt(Receipt{ReceiptID: "RC-2", RequestNo: "REQ-2", Type: ReceiptSuccess})
	if _, err := s.RequestRepair(RequestRepairCmd{RepairNo: "R-2", OriginalID: ins2.ID, NewBeneficiary: Beneficiary{AccountNo: "1", AccountName: "李四", BankCode: "ICBC"}}); !errors.Is(err, ErrOriginalSucceeded) {
		t.Fatalf("succeeded instruction must not be repairable: %v", err)
	}
}

func TestRepairRequestSnapshotAndIdempotency(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	rp, err := s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})
	if err != nil {
		t.Fatal(err)
	}
	if rp.RejectReason != "账号不存在" || rp.OriginalVersion != 1 {
		t.Fatalf("repair must pin reject reason and version: %+v", rp)
	}
	if rp.OldBeneficiaryMD != DigestBeneficiary(ins.Beneficiary) {
		t.Fatalf("repair must pin old beneficiary digest")
	}
	// 幂等：同号同内容
	if _, err := s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb}); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	// 冲突：同号不同内容
	nb2 := nb
	nb2.AccountNo = "000"
	if _, err := s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb2}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// 原指令未被修改
	got, _ := s.GetInstructionView(ins.ID)
	if got.Instruction.Beneficiary != ins.Beneficiary || got.Instruction.Status != StatusRejected {
		t.Fatalf("original instruction must stay untouched")
	}
}

func TestConfirmRepairCreatesLinkedInstructionAndMovesReservation(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})

	ni, err := s.ConfirmRepair("R-1", "REQ-2")
	if err != nil {
		t.Fatal(err)
	}
	if ni.Amount != ins.Amount || ni.Currency != ins.Currency || ni.FundOwner != ins.FundOwner {
		t.Fatalf("amount/currency/fund owner must be inherited")
	}
	if ni.Beneficiary != nb || ni.RepairOf != ins.ID || ni.Version != 2 {
		t.Fatalf("new instruction must link to original with new beneficiary: %+v", ni)
	}
	if ni.ReservationID != ins.ReservationID {
		t.Fatalf("reservation must be transferred, not re-created")
	}
	view, _ := s.GetInstructionView(ins.ID)
	if view.Instruction.Status != StatusSuperseded {
		t.Fatalf("original should be superseded, got %s", view.Instruction.Status)
	}
	if len(view.Reservations) != 1 || view.Reservations[0].InstructionID != ni.ID || view.Reservations[0].Status != ReservationActive {
		t.Fatalf("reservation must stay active and point to new instruction: %+v", view.Reservations)
	}
	if len(view.Reservations[0].TransferTrail) != 2 {
		t.Fatalf("transfer trail should record both instructions")
	}

	// 确认幂等：同修复号同请求号
	again, err := s.ConfirmRepair("R-1", "REQ-2")
	if err != nil || again.ID != ni.ID {
		t.Fatalf("idempotent confirm failed: %v", err)
	}
	// 同修复号不同请求号冲突
	if _, err := s.ConfirmRepair("R-1", "REQ-3"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestConfirmRepairFailureKeepsRejectedState(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})

	// 新请求号已被占用 -> 冲突失败
	s.CreateInstruction(createCmd("REQ-X"))
	if _, err := s.ConfirmRepair("R-1", "REQ-X"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	view, _ := s.GetInstructionView(ins.ID)
	if view.Instruction.Status != StatusRejected {
		t.Fatalf("original must remain rejected after failed confirm")
	}
	if view.Reservations[0].InstructionID != ins.ID || view.Reservations[0].Status != ReservationActive {
		t.Fatalf("reservation must remain with original instruction")
	}
	// 修复仍可继续处理
	if _, err := s.ConfirmRepair("R-1", "REQ-2"); err != nil {
		t.Fatalf("repair should still be confirmable: %v", err)
	}
}

func TestOriginalSuccessVoidsRepair(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})

	// 原指令先成功
	s.RegisterReceipt(Receipt{ReceiptID: "RC-S", RequestNo: "REQ-1", Type: ReceiptSuccess})
	if _, err := s.ConfirmRepair("R-1", "REQ-2"); !errors.Is(err, ErrOriginalSucceeded) {
		t.Fatalf("repair must be voided, got %v", err)
	}
	view, _ := s.GetInstructionView(ins.ID)
	if view.Instruction.Status != StatusSucceeded {
		t.Fatalf("original should be succeeded")
	}
	if view.Repairs[0].Status != RepairVoided {
		t.Fatalf("repair should be voided")
	}
	if len(s.Ledger()) != 1 {
		t.Fatalf("exactly one booking expected")
	}
}

func TestLateSuccessAfterRepairMarkedManualReview(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})
	ni, _ := s.ConfirmRepair("R-1", "REQ-2")

	// 新指令成功记账
	s.RegisterReceipt(Receipt{ReceiptID: "RC-S2", RequestNo: "REQ-2", Type: ReceiptSuccess})
	// 原指令迟到成功回执
	s.RegisterReceipt(Receipt{ReceiptID: "RC-S1", RequestNo: "REQ-1", Type: ReceiptSuccess})

	view, _ := s.GetInstructionView(ins.ID)
	if view.Instruction.Status != StatusManualReview {
		t.Fatalf("original should be manual review, got %s", view.Instruction.Status)
	}
	if len(view.Exceptions) != 1 {
		t.Fatalf("exception should be recorded")
	}
	if len(s.Ledger()) != 1 || s.Ledger()[0].InstructionID != ni.ID {
		t.Fatalf("late success must not double-book")
	}
}

func TestReceiptIdempotency(t *testing.T) {
	s := NewService()
	s.CreateInstruction(createCmd("REQ-1"))
	rc := Receipt{ReceiptID: "RC-1", RequestNo: "REQ-1", Type: ReceiptSuccess}
	if err := s.RegisterReceipt(rc); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterReceipt(rc); err != nil {
		t.Fatalf("duplicate receipt should be ignored: %v", err)
	}
	if len(s.Ledger()) != 1 {
		t.Fatalf("duplicate receipt must not double-book")
	}
	bad := rc
	bad.Type = ReceiptReject
	if err := s.RegisterReceipt(bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestCancelRepair(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})
	if err := s.CancelRepair("R-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmRepair("R-1", "REQ-2"); !errors.Is(err, ErrRepairNotPending) {
		t.Fatalf("cancelled repair must not confirm: %v", err)
	}
	view, _ := s.GetInstructionView(ins.ID)
	if view.Instruction.Status != StatusRejected {
		t.Fatalf("original stays rejected and repairable")
	}
}

func TestQueryView(t *testing.T) {
	s := NewService()
	ins := newRejected(t, s, "REQ-1")
	nb := Beneficiary{AccountNo: "6222020200998877", AccountName: "张三", BankCode: "ICBC"}
	s.RequestRepair(RequestRepairCmd{RepairNo: "R-1", OriginalID: ins.ID, NewBeneficiary: nb})
	ni, _ := s.ConfirmRepair("R-1", "REQ-2")
	s.RegisterReceipt(Receipt{ReceiptID: "RC-S2", RequestNo: "REQ-2", Type: ReceiptSuccess})

	view, err := s.GetInstructionView(ni.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Versions) != 2 || view.Versions[0].ID != ins.ID || view.Versions[1].ID != ni.ID {
		t.Fatalf("view should list all versions from root")
	}
	if len(view.Repairs) != 1 || view.Repairs[0].Status != RepairConfirmed {
		t.Fatalf("view should list repairs")
	}
	if len(view.Receipts) != 2 {
		t.Fatalf("view should list all receipts of the chain, got %d", len(view.Receipts))
	}
	if len(view.Reservations) != 1 || view.Reservations[0].Status != ReservationConsumed {
		t.Fatalf("reservation should be consumed after success")
	}
}
