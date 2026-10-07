package settlement

import (
	"fmt"
	"sync"
	"time"
)

// Service 结算转账与修复服务。所有状态变更在同一把锁内完成，
// 保证“生成新指令、迁移预留、记录修复关系”原子生效。
type Service struct {
	mu sync.Mutex

	instructions map[string]*Instruction
	byRequestNo  map[string]*Instruction
	receipts     map[string]*Receipt // by receipt no
	repairs      map[string]*Repair  // by repair no
	reservations map[string]*Reservation
	exceptions   []Exception

	seq int
	now func() time.Time
}

func NewService() *Service {
	return &Service{
		instructions: make(map[string]*Instruction),
		byRequestNo:  make(map[string]*Instruction),
		receipts:     make(map[string]*Receipt),
		repairs:      make(map[string]*Repair),
		reservations: make(map[string]*Reservation),
		now:          time.Now,
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%08d", prefix, s.seq)
}

type CreateTransferCommand struct {
	RequestNo   string
	Amount      int64
	Currency    string
	FundOwner   string
	Beneficiary Beneficiary
}

// CreateTransfer 创建结算转账指令并同时占用资金预留。
// 按 RequestNo 幂等：同号同内容返回原指令，同号不同内容返回冲突。
func (s *Service) CreateTransfer(cmd CreateTransferCommand) (*Instruction, error) {
	if cmd.RequestNo == "" || cmd.Currency == "" || cmd.FundOwner == "" ||
		cmd.Amount <= 0 || cmd.Beneficiary.AccountNo == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byRequestNo[cmd.RequestNo]; ok {
		if existing.Amount != cmd.Amount || existing.Currency != cmd.Currency ||
			existing.FundOwner != cmd.FundOwner || existing.Beneficiary != cmd.Beneficiary {
			return nil, ErrConflict
		}
		cp := *existing
		return &cp, nil
	}

	instr := &Instruction{
		ID:          s.nextID("INS"),
		RequestNo:   cmd.RequestNo,
		Version:     1,
		Amount:      cmd.Amount,
		Currency:    cmd.Currency,
		FundOwner:   cmd.FundOwner,
		Beneficiary: cmd.Beneficiary,
		Status:      StatusPending,
		CreatedAt:   s.now(),
	}
	res := &Reservation{
		ID:            s.nextID("RSV"),
		InstructionID: instr.ID,
		Amount:        cmd.Amount,
		Currency:      cmd.Currency,
		FundOwner:     cmd.FundOwner,
		Status:        ReservationHeld,
	}
	s.instructions[instr.ID] = instr
	s.byRequestNo[instr.RequestNo] = instr
	s.reservations[res.ID] = res
	cp := *instr
	return &cp, nil
}

type RegisterReceiptCommand struct {
	ReceiptNo     string
	InstructionID string
	Result        ReceiptResult
	RejectReason  string
}

// RegisterReceipt 登记银行回执。回执按 ReceiptNo 幂等且完整保留。
// 拒绝回执使指令进入 REJECTED；成功回执完成记账并释放预留。
// 若指令已被修复（新指令已生成），迟到的成功回执只标记人工处理，不重复记账。
func (s *Service) RegisterReceipt(cmd RegisterReceiptCommand) (*Receipt, error) {
	if cmd.ReceiptNo == "" || cmd.InstructionID == "" ||
		(cmd.Result != ReceiptSuccess && cmd.Result != ReceiptReject) {
		return nil, ErrInvalidArgument
	}
	if cmd.Result == ReceiptReject && cmd.RejectReason == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.receipts[cmd.ReceiptNo]; ok {
		if existing.InstructionID != cmd.InstructionID || existing.Result != cmd.Result ||
			existing.RejectReason != cmd.RejectReason {
			return nil, ErrConflict
		}
		cp := *existing
		return &cp, nil
	}

	instr, ok := s.instructions[cmd.InstructionID]
	if !ok {
		return nil, ErrNotFound
	}

	rcpt := &Receipt{
		ReceiptNo:     cmd.ReceiptNo,
		InstructionID: cmd.InstructionID,
		Result:        cmd.Result,
		RejectReason:  cmd.RejectReason,
		ReceivedAt:    s.now(),
	}

	switch cmd.Result {
	case ReceiptReject:
		// 已成功或已被修复的指令不再被回退；其余情况记录拒绝状态。
		if instr.Status == StatusPending || instr.Status == StatusRejected {
			instr.Status = StatusRejected
			instr.RejectReason = cmd.RejectReason
		}
	case ReceiptSuccess:
		switch {
		case instr.Status == StatusSucceeded:
			// 幂等：重复成功回执不重复记账。
		case instr.RepairedBy != "":
			// 修复新指令已生成，迟到的成功回执不能再次记账，转人工处理。
			rcpt.ManualReview = true
			instr.ManualReview = true
			s.exceptions = append(s.exceptions, Exception{
				InstructionID: instr.ID,
				ReceiptNo:     rcpt.ReceiptNo,
				Reason:        "late success receipt after repair confirmed",
				At:            s.now(),
			})
		default:
			instr.Status = StatusSucceeded
			s.releaseReservation(instr.ID)
		}
	}

	s.receipts[rcpt.ReceiptNo] = rcpt
	cp := *rcpt
	return &cp, nil
}

func (s *Service) releaseReservation(instructionID string) {
	for _, r := range s.reservations {
		if r.InstructionID == instructionID && r.Status == ReservationHeld {
			r.Status = ReservationReleased
		}
	}
}

func (s *Service) heldReservation(instructionID string) *Reservation {
	for _, r := range s.reservations {
		if r.InstructionID == instructionID && r.Status == ReservationHeld {
			return r
		}
	}
	return nil
}

type CreateRepairCommand struct {
	RepairNo        string
	InstructionID   string
	RejectReason    string
	OriginalVersion int
	NewBeneficiary  Beneficiary
}

// CreateRepair 发起修复申请。仅允许已收到明确拒绝回执且尚未成功的指令。
// 申请固定原指令、拒绝原因、原收款信息摘要、新收款信息与原指令版本。
// 按 RepairNo 幂等：同号同内容返回原申请，同号不同内容返回冲突。
func (s *Service) CreateRepair(cmd CreateRepairCommand) (*Repair, error) {
	if cmd.RepairNo == "" || cmd.InstructionID == "" || cmd.RejectReason == "" ||
		cmd.NewBeneficiary.AccountNo == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.repairs[cmd.RepairNo]; ok {
		if existing.InstructionID != cmd.InstructionID ||
			existing.RejectReason != cmd.RejectReason ||
			existing.OriginalVersion != cmd.OriginalVersion ||
			existing.NewBeneficiary != cmd.NewBeneficiary {
			return nil, ErrConflict
		}
		cp := *existing
		return &cp, nil
	}

	instr, ok := s.instructions[cmd.InstructionID]
	if !ok {
		return nil, ErrNotFound
	}
	if instr.Status != StatusRejected {
		return nil, ErrNotRejected
	}
	if instr.Version != cmd.OriginalVersion {
		return nil, ErrVersionMismatch
	}

	repair := &Repair{
		RepairNo:            cmd.RepairNo,
		InstructionID:       cmd.InstructionID,
		RejectReason:        cmd.RejectReason,
		OriginalBeneficiary: instr.Beneficiary.Summary(),
		OriginalVersion:     instr.Version,
		NewBeneficiary:      cmd.NewBeneficiary,
		Status:              RepairPending,
		CreatedAt:           s.now(),
	}
	s.repairs[repair.RepairNo] = repair
	cp := *repair
	return &cp, nil
}

type ConfirmRepairCommand struct {
	RepairNo     string
	NewRequestNo string
}

// ConfirmRepair 确认修复：生成关联的新银行指令（金额、币种、资金归属不变，
// 仅收款信息变更），并将原资金预留原子迁移到新指令。
// 若原指令已成功，修复作废。按 RepairNo+NewRequestNo 幂等。
func (s *Service) ConfirmRepair(cmd ConfirmRepairCommand) (*Instruction, error) {
	if cmd.RepairNo == "" || cmd.NewRequestNo == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	repair, ok := s.repairs[cmd.RepairNo]
	if !ok {
		return nil, ErrNotFound
	}

	// 幂等：同一修复已确认过。
	if repair.Status == RepairConfirmed {
		instr := s.instructions[repair.NewInstructionID]
		if instr.RequestNo != cmd.NewRequestNo {
			return nil, ErrConflict
		}
		cp := *instr
		return &cp, nil
	}
	if repair.Status != RepairPending {
		return nil, ErrRepairNotPending
	}

	// 新银行请求号冲突检查：同号不同内容返回冲突。
	if existing, ok := s.byRequestNo[cmd.NewRequestNo]; ok {
		return nil, fmt.Errorf("%w: request no %s already used by %s", ErrConflict, cmd.NewRequestNo, existing.ID)
	}

	orig := s.instructions[repair.InstructionID]
	if orig.Status == StatusSucceeded {
		// 原指令先成功，修复作废。
		repair.Status = RepairObsolete
		return nil, ErrRepairObsolete
	}

	res := s.heldReservation(orig.ID)
	if res == nil {
		return nil, fmt.Errorf("%w: held reservation of %s", ErrNotFound, orig.ID)
	}

	// 原子生效：新指令 + 预留迁移 + 修复关系。
	newInstr := &Instruction{
		ID:          s.nextID("INS"),
		RequestNo:   cmd.NewRequestNo,
		Version:     orig.Version + 1,
		Amount:      orig.Amount,
		Currency:    orig.Currency,
		FundOwner:   orig.FundOwner,
		Beneficiary: repair.NewBeneficiary,
		Status:      StatusPending,
		RepairOf:    orig.ID,
		CreatedAt:   s.now(),
	}
	s.instructions[newInstr.ID] = newInstr
	s.byRequestNo[newInstr.RequestNo] = newInstr

	res.InstructionID = newInstr.ID
	res.MigratedFrom = orig.ID

	orig.Status = StatusRepaired
	orig.RepairedBy = newInstr.ID

	repair.Status = RepairConfirmed
	repair.NewInstructionID = newInstr.ID

	cp := *newInstr
	return &cp, nil
}

// CancelRepair 取消待确认的修复申请，原拒绝状态仍可继续处理。
func (s *Service) CancelRepair(repairNo string) (*Repair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	repair, ok := s.repairs[repairNo]
	if !ok {
		return nil, ErrNotFound
	}
	if repair.Status != RepairPending {
		return nil, ErrRepairNotPending
	}
	repair.Status = RepairCancelled
	cp := *repair
	return &cp, nil
}

// GetInstruction 查询指令详情：原指令、全部修复版本、银行回执、
// 资金预留去向与需要人工处理的异常。
func (s *Service) GetInstruction(id string) (*InstructionDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	instr, ok := s.instructions[id]
	if !ok {
		return nil, ErrNotFound
	}
	detail := &InstructionDetail{Instruction: *instr}
	for _, r := range s.repairs {
		if r.InstructionID == id || r.NewInstructionID == id {
			detail.Repairs = append(detail.Repairs, *r)
		}
	}
	for _, rc := range s.receipts {
		if rc.InstructionID == id {
			detail.Receipts = append(detail.Receipts, *rc)
		}
	}
	for _, res := range s.reservations {
		if res.InstructionID == id || res.MigratedFrom == id {
			detail.Reservations = append(detail.Reservations, *res)
		}
	}
	for _, ex := range s.exceptions {
		if ex.InstructionID == id {
			detail.Exceptions = append(detail.Exceptions, ex)
		}
	}
	return detail, nil
}

// Exceptions 返回全部需要人工处理的异常。
func (s *Service) Exceptions() []Exception {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Exception, len(s.exceptions))
	copy(out, s.exceptions)
	return out
}
