package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service 大额结算批次拆分服务（内存实现，可替换为持久化存储）。
type Service struct {
	mu      sync.Mutex
	batches map[string]Batch
	plans   map[string]*Plan // key: planNo
	byBatch map[string]string
	now     func() time.Time
}

func NewService() *Service {
	return &Service{
		batches: make(map[string]Batch),
		plans:   make(map[string]*Plan),
		byBatch: make(map[string]string),
		now:     time.Now,
	}
}

// GeneratePlan 为批次生成拆分方案并冻结批次与规则快照。
//
// 幂等：相同 planNo 重复调用时，若批次号、总金额、规则版本一致，
// 返回已存在的方案；否则返回 ErrConflict。批次号同理（一个批次只允许一个方案）。
// 拆分失败时不保存任何子指令。
func (s *Service) GeneratePlan(planNo string, b Batch, rule BankRule) (*Plan, error) {
	if planNo == "" || b.BatchNo == "" {
		return nil, errors.New("settlement: planNo and batchNo are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if p, ok := s.plans[planNo]; ok {
		if p.BatchNo != b.BatchNo || p.Batch.TotalAmount != b.TotalAmount || p.Rule.Version != rule.Version {
			return nil, fmt.Errorf("%w: plan %s already exists with different batch/total/rule",
				ErrConflict, planNo)
		}
		return p, nil
	}
	if existingPlanNo, ok := s.byBatch[b.BatchNo]; ok {
		p := s.plans[existingPlanNo]
		if p.Batch.TotalAmount != b.TotalAmount || p.Rule.Version != rule.Version {
			return nil, fmt.Errorf("%w: batch %s already has plan with different total/rule",
				ErrConflict, b.BatchNo)
		}
		return nil, fmt.Errorf("%w: batch %s already has plan %s", ErrConflict, b.BatchNo, existingPlanNo)
	}

	amounts, rationale, err := SplitAmounts(b.TotalAmount, rule)
	if err != nil {
		return nil, err // 整次失败，不落任何子指令
	}

	p := &Plan{
		PlanNo:    planNo,
		BatchNo:   b.BatchNo,
		Batch:     b,
		Rule:      rule,
		CreatedAt: s.now(),
		Rationale: rationale,
	}
	for i, amt := range amounts {
		p.Instructions = append(p.Instructions, &Instruction{
			InstructionID: fmt.Sprintf("%s-%s-%03d", b.BatchNo, planNo, i+1),
			PlanNo:        planNo,
			BatchNo:       b.BatchNo,
			Seq:           i + 1,
			Amount:        amt,
			Status:        StatusPending,
		})
	}
	if p.TotalAmount() != b.TotalAmount {
		return nil, errors.New("settlement: internal error, split sum mismatch")
	}

	s.batches[b.BatchNo] = b
	s.plans[planNo] = p
	s.byBatch[b.BatchNo] = planNo
	return p, nil
}

// ConfirmPlan 确认方案，子指令进入待发送（PENDING）状态并允许上报银行结果。
// 幂等：重复确认返回相同结果，不产生副作用。
func (s *Service) ConfirmPlan(planNo string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planNo]
	if !ok {
		return nil, fmt.Errorf("settlement: plan %s not found", planNo)
	}
	p.Confirmed = true
	return p, nil
}

// MarkSent 将未发送的子指令标记为处理中（已发送银行，等待回执）。
func (s *Service) MarkSent(instructionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, err := s.findInstruction(instructionID)
	if err != nil {
		return err
	}
	if ins.Status == StatusPending {
		ins.Status = StatusProcessing
		ins.Attempts++
	}
	return nil
}

// RecordReceipt 接收银行回执，更新子指令状态。仅处理中的指令可接收回执。
func (s *Service) RecordReceipt(instructionID, result, bankRef string) error {
	if result != "SUCCESS" && result != "FAILED" {
		return errors.New("settlement: receipt result must be SUCCESS or FAILED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, err := s.findInstruction(instructionID)
	if err != nil {
		return err
	}
	if ins.Status != StatusProcessing {
		return fmt.Errorf("settlement: instruction %s is %s, cannot accept receipt", instructionID, ins.Status)
	}
	ins.Receipts = append(ins.Receipts, Receipt{
		InstructionID: instructionID,
		Attempt:       ins.Attempts,
		Result:        result,
		BankRef:       bankRef,
		ReceivedAt:    s.now(),
	})
	if result == "SUCCESS" {
		ins.Status = StatusSuccess
	} else {
		ins.Status = StatusFailed
	}
	return nil
}

// RetryFailed 重试方案下所有失败的子指令：沿用原 InstructionID（业务身份），
// 尝试次数递增，状态回到处理中。已成功或处理中的指令不受影响，不重新拆分。
func (s *Service) RetryFailed(planNo string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planNo]
	if !ok {
		return nil, fmt.Errorf("settlement: plan %s not found", planNo)
	}
	if !p.Confirmed {
		return nil, errors.New("settlement: plan not confirmed")
	}
	var retried []string
	for _, ins := range p.Instructions {
		if ins.Status == StatusFailed {
			ins.Status = StatusProcessing
			ins.Attempts++
			retried = append(retried, ins.InstructionID)
		}
	}
	return retried, nil
}

// Summary 批次金额核对：按状态汇总未发送/处理中/成功/失败金额。
func (s *Service) Summary(batchNo string) (BatchSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	planNo, ok := s.byBatch[batchNo]
	if !ok {
		return BatchSummary{}, fmt.Errorf("settlement: batch %s not found", batchNo)
	}
	p := s.plans[planNo]
	sum := BatchSummary{BatchNo: batchNo, PlanNo: planNo, TotalAmount: p.Batch.TotalAmount}
	for _, ins := range p.Instructions {
		switch ins.Status {
		case StatusPending:
			sum.UnsentAmount += ins.Amount
			sum.UnsentCount++
		case StatusProcessing:
			sum.ProcessingAmount += ins.Amount
			sum.ProcessingCount++
		case StatusSuccess:
			sum.SuccessAmount += ins.Amount
			sum.SuccessCount++
		case StatusFailed:
			sum.FailedAmount += ins.Amount
			sum.FailedCount++
		}
	}
	return sum, nil
}

// PlanOf 查询方案（含拆分依据与子指令状态）。
func (s *Service) PlanOf(planNo string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planNo]
	if !ok {
		return nil, fmt.Errorf("settlement: plan %s not found", planNo)
	}
	return p, nil
}

// Trace 从批次追到每条银行指令及其回执。
func (s *Service) Trace(batchNo string) ([]Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	planNo, ok := s.byBatch[batchNo]
	if !ok {
		return nil, fmt.Errorf("settlement: batch %s not found", batchNo)
	}
	p := s.plans[planNo]
	out := make([]Instruction, 0, len(p.Instructions))
	for _, ins := range p.Instructions {
		cp := *ins
		cp.Receipts = append([]Receipt(nil), ins.Receipts...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (s *Service) findInstruction(id string) (*Instruction, error) {
	for _, p := range s.plans {
		for _, ins := range p.Instructions {
			if ins.InstructionID == id {
				return ins, nil
			}
		}
	}
	return nil, fmt.Errorf("settlement: instruction %s not found", id)
}
