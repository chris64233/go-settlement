package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrConflict          = errors.New("settlement: idempotency conflict")
	ErrNotFound          = errors.New("settlement: not found")
	ErrInvalidState      = errors.New("settlement: invalid state")
	ErrOriginalSucceeded = errors.New("settlement: original instruction already succeeded")
	ErrRepairNotPending  = errors.New("settlement: repair is not pending")
)

// CreateInstructionCmd 创建结算转账指令。
type CreateInstructionCmd struct {
	RequestNo   string
	Amount      int64
	Currency    string
	FundOwner   string
	Beneficiary Beneficiary
}

// RequestRepairCmd 发起修复申请。
type RequestRepairCmd struct {
	RepairNo       string
	OriginalID     string
	NewBeneficiary Beneficiary
}

// Service 结算转账服务，内存实现，全部状态迁移在单锁内原子完成。
type Service struct {
	mu           sync.Mutex
	seq          int64
	instructions map[string]*Instruction // by ID
	byRequestNo  map[string]*Instruction
	repairs      map[string]*Repair // by RepairNo
	receipts     map[string]*Receipt
	reservations map[string]*Reservation
	ledger       []LedgerEntry
	exceptions   []Exception
}

func NewService() *Service {
	return &Service{
		instructions: make(map[string]*Instruction),
		byRequestNo:  make(map[string]*Instruction),
		repairs:      make(map[string]*Repair),
		receipts:     make(map[string]*Receipt),
		reservations: make(map[string]*Reservation),
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// DigestBeneficiary 计算收款信息摘要。
func DigestBeneficiary(b Beneficiary) string {
	sum := sha256.Sum256([]byte(b.AccountNo + "|" + b.AccountName + "|" + b.BankCode))
	return hex.EncodeToString(sum[:8])
}

// CreateInstruction 创建转账指令并占用资金预留。
// 银行请求号幂等：同号同内容返回已有指令，同号不同内容返回冲突。
func (s *Service) CreateInstruction(cmd CreateInstructionCmd) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cmd.RequestNo == "" || cmd.Amount <= 0 || cmd.Currency == "" || cmd.FundOwner == "" {
		return nil, fmt.Errorf("%w: invalid create command", ErrInvalidState)
	}
	if existing, ok := s.byRequestNo[cmd.RequestNo]; ok {
		if existing.Amount != cmd.Amount || existing.Currency != cmd.Currency ||
			existing.FundOwner != cmd.FundOwner || existing.Beneficiary != cmd.Beneficiary {
			return nil, fmt.Errorf("%w: request no %s", ErrConflict, cmd.RequestNo)
		}
		cp := *existing
		return &cp, nil
	}

	now := time.Now()
	ins := &Instruction{
		ID:          s.nextID("INS"),
		RequestNo:   cmd.RequestNo,
		Version:     1,
		Amount:      cmd.Amount,
		Currency:    cmd.Currency,
		FundOwner:   cmd.FundOwner,
		Beneficiary: cmd.Beneficiary,
		Status:      StatusPending,
		CreatedAt:   now,
	}
	res := &Reservation{
		ID:            s.nextID("RSV"),
		InstructionID: ins.ID,
		Amount:        cmd.Amount,
		Currency:      cmd.Currency,
		FundOwner:     cmd.FundOwner,
		Status:        ReservationActive,
		TransferTrail: []string{ins.ID},
	}
	ins.ReservationID = res.ID
	s.instructions[ins.ID] = ins
	s.byRequestNo[ins.RequestNo] = ins
	s.reservations[res.ID] = res
	cp := *ins
	return &cp, nil
}

// RegisterReceipt 登记银行回执。回执号幂等：同号同内容忽略，
// 同号不同内容返回冲突。回执永久保留。
func (s *Service) RegisterReceipt(rc Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rc.ReceiptID == "" || rc.RequestNo == "" {
		return fmt.Errorf("%w: invalid receipt", ErrInvalidState)
	}
	if existing, ok := s.receipts[rc.ReceiptID]; ok {
		if existing.RequestNo != rc.RequestNo || existing.Type != rc.Type || existing.Reason != rc.Reason {
			return fmt.Errorf("%w: receipt %s", ErrConflict, rc.ReceiptID)
		}
		return nil
	}
	ins, ok := s.byRequestNo[rc.RequestNo]
	if !ok {
		return fmt.Errorf("%w: instruction for request no %s", ErrNotFound, rc.RequestNo)
	}

	rc.ReceivedAt = time.Now()
	s.receipts[rc.ReceiptID] = &rc

	switch rc.Type {
	case ReceiptSuccess:
		s.applySuccess(ins)
	case ReceiptReject:
		if ins.Status == StatusPending {
			ins.Status = StatusRejected
			ins.RejectReason = rc.Reason
		}
	default:
		return fmt.Errorf("%w: unknown receipt type", ErrInvalidState)
	}
	return nil
}

func (s *Service) applySuccess(ins *Instruction) {
	switch ins.Status {
	case StatusPending, StatusRejected:
		// 原指令先成功：终态成功，作废未确认的修复。
		ins.Status = StatusSucceeded
		s.consumeReservation(ins)
		s.book(ins)
		for _, rp := range s.repairs {
			if rp.OriginalID == ins.ID && rp.Status == RepairPending {
				rp.Status = RepairVoided
				rp.VoidReason = "original instruction succeeded"
			}
		}
	case StatusSuperseded:
		// 修复已生成新指令后收到迟到成功回执：标记人工处理，不重复记账。
		ins.Status = StatusManualReview
		s.exceptions = append(s.exceptions, Exception{
			InstructionID: ins.ID,
			RequestNo:     ins.RequestNo,
			Reason:        "late success receipt after repair confirmed; no double booking",
			CreatedAt:     time.Now(),
		})
	case StatusSucceeded, StatusManualReview:
		// 重复成功回执：不重复记账。
	}
}

func (s *Service) consumeReservation(ins *Instruction) {
	if res, ok := s.reservations[ins.ReservationID]; ok && res.Status == ReservationActive {
		res.Status = ReservationConsumed
	}
}

func (s *Service) book(ins *Instruction) {
	s.ledger = append(s.ledger, LedgerEntry{
		InstructionID: ins.ID,
		RequestNo:     ins.RequestNo,
		Amount:        ins.Amount,
		Currency:      ins.Currency,
		FundOwner:     ins.FundOwner,
		CreatedAt:     time.Now(),
	})
}

// RequestRepair 发起修复申请。只有收到明确拒绝回执且尚未成功的
// 转账可以修复。修复申请固定原指令、拒绝原因、原收款信息摘要、
// 新收款信息和原指令版本，不修改旧指令。
func (s *Service) RequestRepair(cmd RequestRepairCmd) (*Repair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cmd.RepairNo == "" || cmd.OriginalID == "" {
		return nil, fmt.Errorf("%w: invalid repair command", ErrInvalidState)
	}
	if existing, ok := s.repairs[cmd.RepairNo]; ok {
		if existing.OriginalID != cmd.OriginalID || existing.NewBeneficiary != cmd.NewBeneficiary {
			return nil, fmt.Errorf("%w: repair no %s", ErrConflict, cmd.RepairNo)
		}
		cp := *existing
		return &cp, nil
	}
	orig, ok := s.instructions[cmd.OriginalID]
	if !ok {
		return nil, fmt.Errorf("%w: instruction %s", ErrNotFound, cmd.OriginalID)
	}
	if orig.Status == StatusSucceeded || orig.Status == StatusManualReview {
		return nil, ErrOriginalSucceeded
	}
	if orig.Status != StatusRejected {
		return nil, fmt.Errorf("%w: instruction %s is not rejected", ErrInvalidState, cmd.OriginalID)
	}
	if cmd.NewBeneficiary == orig.Beneficiary {
		return nil, fmt.Errorf("%w: new beneficiary identical to old", ErrInvalidState)
	}

	rp := &Repair{
		RepairNo:         cmd.RepairNo,
		OriginalID:       orig.ID,
		OriginalVersion:  orig.Version,
		RejectReason:     orig.RejectReason,
		OldBeneficiaryMD: DigestBeneficiary(orig.Beneficiary),
		NewBeneficiary:   cmd.NewBeneficiary,
		Status:           RepairPending,
		CreatedAt:        time.Now(),
	}
	s.repairs[rp.RepairNo] = rp
	cp := *rp
	return &cp, nil
}

// ConfirmRepair 确认修复：原子地生成关联新指令、迁移原资金预留、
// 记录修复关系。任何一步校验失败都不产生副作用，原拒绝状态仍可继续处理。
// 新银行请求号幂等：同修复号同请求号返回已有结果，不同请求号返回冲突。
func (s *Service) ConfirmRepair(repairNo, newRequestNo string) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rp, ok := s.repairs[repairNo]
	if !ok {
		return nil, fmt.Errorf("%w: repair %s", ErrNotFound, repairNo)
	}
	if newRequestNo == "" {
		return nil, fmt.Errorf("%w: empty new request no", ErrInvalidState)
	}

	switch rp.Status {
	case RepairConfirmed:
		if rp.NewRequestNo != newRequestNo {
			return nil, fmt.Errorf("%w: repair %s confirmed with %s", ErrConflict, repairNo, rp.NewRequestNo)
		}
		cp := *s.instructions[rp.NewInstructionID]
		return &cp, nil
	case RepairVoided:
		if rp.VoidReason == "original instruction succeeded" {
			return nil, ErrOriginalSucceeded
		}
		return nil, fmt.Errorf("%w: repair %s voided: %s", ErrRepairNotPending, repairNo, rp.VoidReason)
	}

	orig := s.instructions[rp.OriginalID]
	if orig.Status == StatusSucceeded || orig.Status == StatusManualReview {
		rp.Status = RepairVoided
		rp.VoidReason = "original instruction succeeded"
		return nil, ErrOriginalSucceeded
	}
	if orig.Status != StatusRejected {
		return nil, fmt.Errorf("%w: original instruction status %s", ErrInvalidState, orig.Status)
	}
	if existing, ok := s.byRequestNo[newRequestNo]; ok {
		return nil, fmt.Errorf("%w: request no %s already used by %s", ErrConflict, newRequestNo, existing.ID)
	}
	res, ok := s.reservations[orig.ReservationID]
	if !ok || res.Status != ReservationActive {
		return nil, fmt.Errorf("%w: reservation not active", ErrInvalidState)
	}

	// 全部校验通过，原子提交：新指令 + 预留迁移 + 修复关系。
	now := time.Now()
	ins := &Instruction{
		ID:            s.nextID("INS"),
		RequestNo:     newRequestNo,
		Version:       orig.Version + 1,
		Amount:        orig.Amount,
		Currency:      orig.Currency,
		FundOwner:     orig.FundOwner,
		Beneficiary:   rp.NewBeneficiary,
		Status:        StatusPending,
		RepairOf:      orig.ID,
		RepairNo:      rp.RepairNo,
		ReservationID: res.ID,
		CreatedAt:     now,
	}
	res.InstructionID = ins.ID
	res.TransferTrail = append(res.TransferTrail, ins.ID)
	orig.Status = StatusSuperseded
	rp.Status = RepairConfirmed
	rp.NewInstructionID = ins.ID
	rp.NewRequestNo = newRequestNo
	s.instructions[ins.ID] = ins
	s.byRequestNo[ins.RequestNo] = ins
	cp := *ins
	return &cp, nil
}

// CancelRepair 取消未确认的修复申请。
func (s *Service) CancelRepair(repairNo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rp, ok := s.repairs[repairNo]
	if !ok {
		return fmt.Errorf("%w: repair %s", ErrNotFound, repairNo)
	}
	switch rp.Status {
	case RepairVoided:
		return nil
	case RepairConfirmed:
		return fmt.Errorf("%w: repair %s already confirmed", ErrRepairNotPending, repairNo)
	}
	rp.Status = RepairVoided
	rp.VoidReason = "cancelled by operator"
	return nil
}

// GetInstructionView 查询指令全貌：原指令、全部修复版本、银行回执、
// 资金预留去向和需要人工处理的异常。
func (s *Service) GetInstructionView(id string) (*InstructionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ins, ok := s.instructions[id]
	if !ok {
		return nil, fmt.Errorf("%w: instruction %s", ErrNotFound, id)
	}
	// 找到链根。
	root := ins
	for root.RepairOf != "" {
		root = s.instructions[root.RepairOf]
	}
	// 收集链上全部版本。
	byRepairOf := map[string][]*Instruction{}
	for _, it := range s.instructions {
		if it.RepairOf != "" {
			byRepairOf[it.RepairOf] = append(byRepairOf[it.RepairOf], it)
		}
	}
	var versions []Instruction
	var walk func(*Instruction)
	walk = func(cur *Instruction) {
		versions = append(versions, *cur)
		for _, nxt := range byRepairOf[cur.ID] {
			walk(nxt)
		}
	}
	walk(root)
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Version != versions[j].Version {
			return versions[i].Version < versions[j].Version
		}
		return versions[i].CreatedAt.Before(versions[j].CreatedAt)
	})

	view := &InstructionView{Instruction: *ins, Versions: versions}
	inChain := map[string]bool{}
	for _, v := range versions {
		inChain[v.ID] = true
	}
	for _, rp := range s.repairs {
		if inChain[rp.OriginalID] {
			view.Repairs = append(view.Repairs, *rp)
		}
	}
	sort.Slice(view.Repairs, func(i, j int) bool { return view.Repairs[i].CreatedAt.Before(view.Repairs[j].CreatedAt) })
	reqNos := map[string]bool{}
	for _, v := range versions {
		reqNos[v.RequestNo] = true
	}
	for _, rc := range s.receipts {
		if reqNos[rc.RequestNo] {
			view.Receipts = append(view.Receipts, *rc)
		}
	}
	sort.Slice(view.Receipts, func(i, j int) bool { return view.Receipts[i].ReceivedAt.Before(view.Receipts[j].ReceivedAt) })
	seenRes := map[string]bool{}
	for _, v := range versions {
		if res, ok := s.reservations[v.ReservationID]; ok && !seenRes[res.ID] {
			seenRes[res.ID] = true
			cp := *res
			cp.TransferTrail = append([]string(nil), res.TransferTrail...)
			view.Reservations = append(view.Reservations, cp)
		}
	}
	for _, ex := range s.exceptions {
		if inChain[ex.InstructionID] {
			view.Exceptions = append(view.Exceptions, ex)
		}
	}
	return view, nil
}

// Ledger 返回全部成功记账记录。
func (s *Service) Ledger() []LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LedgerEntry(nil), s.ledger...)
}

// Exceptions 返回全部需要人工处理的异常。
func (s *Service) Exceptions() []Exception {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exception(nil), s.exceptions...)
}
