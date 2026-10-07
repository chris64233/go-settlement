package settlement

import (
	"errors"
	"sync"
	"testing"
)

func newRejectedInstruction(t *testing.T, s *Service) *Instruction {
	t.Helper()
	instr, err := s.CreateTransfer(CreateTransferCommand{
		RequestNo:   "REQ-1",
		Amount:      10000,
		Currency:    "CNY",
		FundOwner:   "MERCHANT-A",
		Beneficiary: Beneficiary{AccountNo: "62220001", AccountName: "张三", BankCode: "ICBC"},
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if _, err := s.RegisterReceipt(RegisterReceiptCommand{
		ReceiptNo:     "RCPT-REJ-1",
		InstructionID: instr.ID,
		Result:        ReceiptReject,
		RejectReason:  "账号户名不符",
	}); err != nil {
		t.Fatalf("register reject receipt: %v", err)
	}
	return instr
}

func TestCreateTransferIdempotent(t *testing.T) {
	s := NewService()
	cmd := CreateTransferCommand{
		RequestNo:   "REQ-1",
		Amount:      100,
		Currency:    "CNY",
		FundOwner:   "M1",
		Beneficiary: Beneficiary{AccountNo: "A1", AccountName: "N1", BankCode: "B1"},
	}
	first, err := s.CreateTransfer(cmd)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := s.CreateTransfer(cmd)
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same instruction, got %s and %s", first.ID, second.ID)
	}

	cmd.Amount = 200
	if _, err := s.CreateTransfer(cmd); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestCreateRepairRequiresRejected(t *testing.T) {
	s := NewService()
	instr, err := s.CreateTransfer(CreateTransferCommand{
		RequestNo:   "REQ-1",
		Amount:      100,
		Currency:    "CNY",
		FundOwner:   "M1",
		Beneficiary: Beneficiary{AccountNo: "A1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRepair(CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "A2"},
	})
	if !errors.Is(err, ErrNotRejected) {
		t.Fatalf("expected ErrNotRejected, got %v", err)
	}
}

func TestRepairFlowSnapshotAndIdempotency(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)

	cmd := CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "62220002", AccountName: "张三", BankCode: "ICBC"},
	}
	repair, err := s.CreateRepair(cmd)
	if err != nil {
		t.Fatalf("create repair: %v", err)
	}
	if repair.OriginalBeneficiary != "62220001|张三|ICBC" {
		t.Fatalf("unexpected original summary: %s", repair.OriginalBeneficiary)
	}
	if repair.Status != RepairPending {
		t.Fatalf("unexpected status: %s", repair.Status)
	}

	// 幂等：同号同内容返回原申请。
	again, err := s.CreateRepair(cmd)
	if err != nil || again.RepairNo != repair.RepairNo {
		t.Fatalf("idempotent create repair failed: %v", err)
	}
	// 同号不同内容冲突。
	cmd.NewBeneficiary.AccountNo = "62229999"
	if _, err := s.CreateRepair(cmd); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// 版本不匹配。
	if _, err := s.CreateRepair(CreateRepairCommand{
		RepairNo:        "RPR-2",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 9,
		NewBeneficiary:  Beneficiary{AccountNo: "X"},
	}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected version mismatch, got %v", err)
	}
}

func TestConfirmRepairMigratesReservationAtomically(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)
	if _, err := s.CreateRepair(CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "62220002", AccountName: "张三", BankCode: "ICBC"},
	}); err != nil {
		t.Fatal(err)
	}

	newInstr, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"})
	if err != nil {
		t.Fatalf("confirm repair: %v", err)
	}
	if newInstr.Amount != instr.Amount || newInstr.Currency != instr.Currency || newInstr.FundOwner != instr.FundOwner {
		t.Fatal("amount/currency/fund owner must be preserved")
	}
	if newInstr.Beneficiary.AccountNo != "62220002" {
		t.Fatal("beneficiary not updated")
	}
	if newInstr.RepairOf != instr.ID || newInstr.Version != 2 {
		t.Fatal("repair relation or version wrong")
	}

	// 幂等确认。
	again, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"})
	if err != nil || again.ID != newInstr.ID {
		t.Fatalf("idempotent confirm failed: %v", err)
	}
	// 同修复号不同新请求号冲突。
	if _, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-OTHER"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}

	// 预留已迁移到新指令，且全程只有一条持有中的预留。
	detail, err := s.GetInstruction(newInstr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Reservations) != 1 {
		t.Fatalf("expected 1 reservation, got %d", len(detail.Reservations))
	}
	res := detail.Reservations[0]
	if res.InstructionID != newInstr.ID || res.MigratedFrom != instr.ID || res.Status != ReservationHeld {
		t.Fatalf("reservation not migrated: %+v", res)
	}

	orig, err := s.GetInstruction(instr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orig.Instruction.Status != StatusRepaired || orig.Instruction.RepairedBy != newInstr.ID {
		t.Fatalf("original instruction not marked repaired: %+v", orig.Instruction)
	}
	if len(orig.Repairs) != 1 || orig.Repairs[0].Status != RepairConfirmed {
		t.Fatalf("repair relation missing: %+v", orig.Repairs)
	}
}

func TestOriginalSucceedsFirstRepairObsolete(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)
	if _, err := s.CreateRepair(CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "62220002"},
	}); err != nil {
		t.Fatal(err)
	}

	// 原指令先成功。
	if _, err := s.RegisterReceipt(RegisterReceiptCommand{
		ReceiptNo:     "RCPT-OK-1",
		InstructionID: instr.ID,
		Result:        ReceiptSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"}); !errors.Is(err, ErrRepairObsolete) {
		t.Fatalf("expected obsolete, got %v", err)
	}
	detail, _ := s.GetInstruction(instr.ID)
	if detail.Instruction.Status != StatusSucceeded {
		t.Fatalf("expected succeeded, got %s", detail.Instruction.Status)
	}
	if detail.Repairs[0].Status != RepairObsolete {
		t.Fatalf("expected repair obsolete, got %s", detail.Repairs[0].Status)
	}
	// 预留已随成功释放。
	if detail.Reservations[0].Status != ReservationReleased {
		t.Fatal("reservation should be released on success")
	}
}

func TestLateSuccessAfterRepairGoesManual(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)
	if _, err := s.CreateRepair(CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "62220002"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"}); err != nil {
		t.Fatal(err)
	}

	// 新指令先生成，迟到的成功回执：标记人工处理，不能再次记账。
	rcpt, err := s.RegisterReceipt(RegisterReceiptCommand{
		ReceiptNo:     "RCPT-LATE-OK",
		InstructionID: instr.ID,
		Result:        ReceiptSuccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rcpt.ManualReview {
		t.Fatal("late success receipt must be flagged manual review")
	}
	detail, _ := s.GetInstruction(instr.ID)
	if detail.Instruction.Status != StatusRepaired {
		t.Fatalf("original must stay repaired, got %s", detail.Instruction.Status)
	}
	if len(detail.Exceptions) != 1 {
		t.Fatalf("expected 1 exception, got %d", len(detail.Exceptions))
	}
	if len(s.Exceptions()) != 1 {
		t.Fatal("exception list missing entry")
	}
}

func TestConcurrentSuccessAndConfirmSingleWinner(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewService()
		instr := newRejectedInstruction(t, s)
		if _, err := s.CreateRepair(CreateRepairCommand{
			RepairNo:        "RPR-1",
			InstructionID:   instr.ID,
			RejectReason:    "账号户名不符",
			OriginalVersion: 1,
			NewBeneficiary:  Beneficiary{AccountNo: "62220002"},
		}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = s.RegisterReceipt(RegisterReceiptCommand{
				ReceiptNo:     "RCPT-OK",
				InstructionID: instr.ID,
				Result:        ReceiptSuccess,
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"})
		}()
		go func() {
			defer wg.Done()
			_, _ = s.CancelRepair("RPR-1")
		}()
		wg.Wait()

		detail, _ := s.GetInstruction(instr.ID)
		succeeded := 0
		if detail.Instruction.Status == StatusSucceeded {
			succeeded++
		}
		repair := detail.Repairs[0]
		if repair.Status == RepairConfirmed {
			newDetail, _ := s.GetInstruction(repair.NewInstructionID)
			if newDetail.Instruction.Status == StatusSucceeded {
				succeeded++
			}
			// 新指令生成后，原指令不得再成功记账。
			if detail.Instruction.Status == StatusSucceeded {
				t.Fatal("original must not succeed after repair confirmed")
			}
		}
		if succeeded > 1 {
			t.Fatalf("more than one instruction succeeded")
		}
	}
}

func TestCancelRepairKeepsRejectedProcessable(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)
	cmd := CreateRepairCommand{
		RepairNo:        "RPR-1",
		InstructionID:   instr.ID,
		RejectReason:    "账号户名不符",
		OriginalVersion: 1,
		NewBeneficiary:  Beneficiary{AccountNo: "62220002"},
	}
	if _, err := s.CreateRepair(cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRepair("RPR-1"); err != nil {
		t.Fatal(err)
	}
	// 取消后不能确认。
	if _, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-1", NewRequestNo: "REQ-2"}); !errors.Is(err, ErrRepairNotPending) {
		t.Fatalf("expected not pending, got %v", err)
	}
	// 原拒绝状态仍可重新发起修复。
	cmd.RepairNo = "RPR-2"
	if _, err := s.CreateRepair(cmd); err != nil {
		t.Fatalf("re-create repair after cancel: %v", err)
	}
	if _, err := s.ConfirmRepair(ConfirmRepairCommand{RepairNo: "RPR-2", NewRequestNo: "REQ-2"}); err != nil {
		t.Fatalf("confirm after re-create: %v", err)
	}
}

func TestReceiptIdempotency(t *testing.T) {
	s := NewService()
	instr := newRejectedInstruction(t, s)
	cmd := RegisterReceiptCommand{
		ReceiptNo:     "RCPT-OK",
		InstructionID: instr.ID,
		Result:        ReceiptSuccess,
	}
	if _, err := s.RegisterReceipt(cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterReceipt(cmd); err != nil {
		t.Fatalf("idempotent receipt: %v", err)
	}
	cmd.Result = ReceiptReject
	cmd.RejectReason = "x"
	if _, err := s.RegisterReceipt(cmd); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	detail, _ := s.GetInstruction(instr.ID)
	if len(detail.Receipts) != 2 { // 拒绝回执 + 成功回执
		t.Fatalf("receipts must be fully preserved, got %d", len(detail.Receipts))
	}
}
