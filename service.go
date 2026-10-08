package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrConflict = errors.New("conflict: idempotency key reused with different payload")
	ErrNotFound = errors.New("not found")
	ErrBadState = errors.New("invalid state for operation")
)

// Service 结算拆分服务，内存存储，所有写操作原子完成。
type Service struct {
	mu           sync.Mutex
	seq          int64
	batches      map[string]*Batch
	batchByNo    map[string]*Batch
	plans        map[string]*Plan
	planByNo     map[string]*Plan
	planByBatch  map[string]*Plan
	instructions map[string]*Instruction
	instrsByPlan map[string][]*Instruction
	receipts     map[string][]*Receipt // instructionID -> receipts
}

func NewService() *Service {
	return &Service{
		batches:      map[string]*Batch{},
		batchByNo:    map[string]*Batch{},
		plans:        map[string]*Plan{},
		planByNo:     map[string]*Plan{},
		planByBatch:  map[string]*Plan{},
		instructions: map[string]*Instruction{},
		instrsByPlan: map[string][]*Instruction{},
		receipts:     map[string][]*Receipt{},
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// CreateBatch 创建批次，BatchNo 幂等：
// 相同 BatchNo 且关键字段一致返回已有批次；总金额等关键字段不同返回冲突。
func (s *Service) CreateBatch(b Batch) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.BatchNo == "" {
		return nil, errors.New("batch no required")
	}
	if b.TotalAmount <= 0 {
		return nil, errors.New("total amount must be positive")
	}
	if existing, ok := s.batchByNo[b.BatchNo]; ok {
		if existing.TotalAmount != b.TotalAmount || existing.Currency != b.Currency ||
			existing.PayerAccount != b.PayerAccount || existing.Payee != b.Payee ||
			existing.ValueDate != b.ValueDate {
			return nil, ErrConflict
		}
		return existing, nil
	}
	b.ID = s.nextID("BAT")
	b.CreatedAt = time.Now()
	s.batches[b.ID] = &b
	s.batchByNo[b.BatchNo] = &b
	return &b, nil
}

// GeneratePlan 为批次生成拆分方案，PlanNo 幂等。
// 冻结批次总额与规则快照；若批次已有关联方案，
// 规则或总金额变化返回冲突，否则返回已有方案。
// 拆分不可行时整次失败，不保存任何子指令。
func (s *Service) GeneratePlan(batchID, planNo string, rule BankRule) (*Plan, []*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if planNo == "" {
		return nil, nil, errors.New("plan no required")
	}
	batch, ok := s.batches[batchID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	if existing, ok := s.planByBatch[batchID]; ok {
		if existing.TotalAmount != batch.TotalAmount || existing.Rule != rule {
			return nil, nil, ErrConflict
		}
		return existing, s.instrsByPlan[existing.ID], nil
	}
	if existing, ok := s.planByNo[planNo]; ok {
		return nil, nil, fmt.Errorf("%w: plan no %s already used by batch %s", ErrConflict, planNo, existing.BatchID)
	}
	amounts, err := split(batch.TotalAmount, rule)
	if err != nil {
		return nil, nil, err // 不保存半套子指令
	}
	plan := &Plan{
		ID:          s.nextID("PLN"),
		PlanNo:      planNo,
		BatchID:     batchID,
		TotalAmount: batch.TotalAmount,
		Rule:        rule,
		Rationale: fmt.Sprintf("total=%d split into %d instructions (min=%d max=%d dailyRemaining=%d maxInstructions=%d, rule version %s)",
			batch.TotalAmount, len(amounts), rule.MinPerInstruction, rule.MaxPerInstruction,
			rule.DailyRemaining, rule.MaxInstructions, rule.Version),
		CreatedAt: time.Now(),
	}
	instrs := make([]*Instruction, len(amounts))
	for i, amt := range amounts {
		instrs[i] = &Instruction{
			ID:      s.nextID("INS"),
			PlanID:  plan.ID,
			BatchID: batchID,
			Seq:     i + 1,
			Amount:  amt,
			Status:  StatusPending,
		}
	}
	// 原子提交：方案与全部子指令一次性落库。
	s.plans[plan.ID] = plan
	s.planByNo[planNo] = plan
	s.planByBatch[batchID] = plan
	s.instrsByPlan[plan.ID] = instrs
	for _, in := range instrs {
		s.instructions[in.ID] = in
	}
	return plan, instrs, nil
}

// ConfirmPlan 确认方案，子指令进入待发送。重复确认幂等，不产生重复效果。
func (s *Service) ConfirmPlan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[planID]
	if !ok {
		return nil, ErrNotFound
	}
	if plan.Confirmed {
		return plan, nil // 幂等
	}
	plan.Confirmed = true
	return plan, nil
}

// SendInstruction 将子指令标记为已发送（处理中）。
func (s *Service) SendInstruction(instructionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instructions[instructionID]
	if !ok {
		return ErrNotFound
	}
	plan := s.plans[in.PlanID]
	if plan == nil || !plan.Confirmed {
		return ErrBadState
	}
	if in.Status != StatusPending {
		return ErrBadState
	}
	in.Status = StatusProcessing
	in.Attempts++
	return nil
}

// RecordReceipt 接收银行回执，更新子指令为成功或失败。
func (s *Service) RecordReceipt(instructionID string, success bool, code, message string) (*Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instructions[instructionID]
	if !ok {
		return nil, ErrNotFound
	}
	if in.Status != StatusProcessing {
		return nil, ErrBadState
	}
	rc := &Receipt{
		ID:            s.nextID("RCP"),
		InstructionID: instructionID,
		Success:       success,
		Code:          code,
		Message:       message,
		ReceivedAt:    time.Now(),
	}
	s.receipts[instructionID] = append(s.receipts[instructionID], rc)
	in.LastReceiptID = rc.ID
	if success {
		in.Status = StatusSuccess
	} else {
		in.Status = StatusFailed
	}
	return rc, nil
}

// RetryInstruction 重试失败子指令：沿用原业务身份（同一指令 ID），
// 仅重置状态为处理中，不触碰其他子指令，不重新拆分。
func (s *Service) RetryInstruction(instructionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instructions[instructionID]
	if !ok {
		return ErrNotFound
	}
	if in.Status != StatusFailed {
		return ErrBadState
	}
	in.Status = StatusProcessing
	in.Attempts++
	return nil
}

// Summary 批次金额核对：按状态汇总，校验总额精确守恒。
func (s *Service) Summary(batchID string) (*BatchSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := s.batches[batchID]
	if !ok {
		return nil, ErrNotFound
	}
	sum := &BatchSummary{BatchID: batchID, TotalAmount: batch.TotalAmount}
	plan := s.planByBatch[batchID]
	if plan == nil {
		sum.Balanced = batch.TotalAmount == 0
		return sum, nil
	}
	for _, in := range s.instrsByPlan[plan.ID] {
		switch in.Status {
		case StatusPending:
			sum.PendingAmount += in.Amount
			sum.PendingCount++
		case StatusProcessing:
			sum.ProcessingAmt += in.Amount
			sum.ProcessingCnt++
		case StatusSuccess:
			sum.SuccessAmount += in.Amount
			sum.SuccessCount++
		case StatusFailed:
			sum.FailedAmount += in.Amount
			sum.FailedCount++
		}
	}
	sum.Balanced = sum.PendingAmount+sum.ProcessingAmt+sum.SuccessAmount+sum.FailedAmount == batch.TotalAmount
	return sum, nil
}

// GetPlan 查询方案及拆分依据。
func (s *Service) GetPlan(planID string) (*Plan, []*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[planID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	return plan, s.instrsByPlan[planID], nil
}

// Trace 从批次追到每条银行指令及其回执。
func (s *Service) Trace(batchID string) (*Batch, *Plan, []*Instruction, map[string][]*Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := s.batches[batchID]
	if !ok {
		return nil, nil, nil, nil, ErrNotFound
	}
	plan := s.planByBatch[batchID]
	if plan == nil {
		return batch, nil, nil, nil, nil
	}
	instrs := s.instrsByPlan[plan.ID]
	rcpts := map[string][]*Receipt{}
	for _, in := range instrs {
		rcpts[in.ID] = s.receipts[in.ID]
	}
	return batch, plan, instrs, rcpts, nil
}
