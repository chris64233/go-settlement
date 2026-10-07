package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrLimitExceeded 表示某一层限额不足，整笔指令被拒绝。
	ErrLimitExceeded = errors.New("settlement: limit exceeded")
	// ErrInstructionNotFound 表示指令不存在。
	ErrInstructionNotFound = errors.New("settlement: instruction not found")
	// ErrInvalidTransition 表示指令当前状态不允许该操作。
	ErrInvalidTransition = errors.New("settlement: invalid status transition")
)

// Service 提供限额配置、指令受理与生命周期管理。
//
// 并发安全：受理时的限额校验与占用登记在同一把互斥锁内完成，
// 构成单一持久化边界——任一层限额不足时整笔拒绝，
// 不会只增加其中一方的占用。
type Service struct {
	rules *RuleStore
	now   func() time.Time

	mu          sync.Mutex
	byID        map[string]*Instruction
	byRequestID map[string]string
	seq         int64
}

// NewService 创建结算限额服务。
func NewService(rules *RuleStore) *Service {
	return &Service{
		rules:       rules,
		now:         time.Now,
		byID:        make(map[string]*Instruction),
		byRequestID: make(map[string]string),
	}
}

// Rules 返回底层规则存储，用于限额配置。
func (s *Service) Rules() *RuleStore {
	return s.rules
}

// active 报告指令是否仍计入风险敞口（预留或已用）。
func active(st InstructionStatus) bool {
	return st == StatusReserved || st == StatusSettled
}

// sameDay 比较两个时间是否属于同一价值日（按 UTC 日期）。
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// netExposureLocked 计算 participant 相对 counterparty 的当日净敞口
// （流出减流入，仅统计预留与已用状态的指令）。调用方须持有 s.mu。
func (s *Service) netExposureLocked(participant, counterparty, currency string, valueDate time.Time) int64 {
	var net int64
	for _, ins := range s.byID {
		if ins.Currency != currency || !sameDay(ins.ValueDate, valueDate) || !active(ins.Status) {
			continue
		}
		if ins.Payer == participant && ins.Payee == counterparty {
			net += ins.Amount
		}
		if ins.Payer == counterparty && ins.Payee == participant {
			net -= ins.Amount
		}
	}
	return net
}

// grossExposureLocked 计算 participant 作为付款方的当日总敞口。调用方须持有 s.mu。
func (s *Service) grossExposureLocked(participant, currency string, valueDate time.Time) int64 {
	var total int64
	for _, ins := range s.byID {
		if ins.Currency != currency || !sameDay(ins.ValueDate, valueDate) || !active(ins.Status) {
			continue
		}
		if ins.Payer == participant {
			total += ins.Amount
		}
	}
	return total
}

// checkSide 校验一侧的规则（participant 相对 counterparty）。
// netDelta 为本笔指令对该侧净敞口的增量（流出为正、流入为负），
// grossDelta 为本笔指令对该侧总敞口的增量。
func (s *Service) checkSide(participant, counterparty, currency string, valueDate time.Time, rv RuleVersion, amount, netDelta, grossDelta int64) error {
	rule := rv.Rule
	if amount > 0 && rule.MaxSingleAmount > 0 && amount > rule.MaxSingleAmount {
		return fmt.Errorf("%w: single amount %d exceeds %d (version %d)",
			ErrLimitExceeded, amount, rule.MaxSingleAmount, rv.Version)
	}
	if rule.MaxNetExposure > 0 {
		net := s.netExposureLocked(participant, counterparty, currency, valueDate) + netDelta
		if net < 0 {
			net = -net
		}
		if net > rule.MaxNetExposure {
			return fmt.Errorf("%w: net exposure exceeds %d (version %d)",
				ErrLimitExceeded, rule.MaxNetExposure, rv.Version)
		}
	}
	if rule.MaxGrossExposure > 0 {
		gross := s.grossExposureLocked(participant, currency, valueDate) + grossDelta
		if gross > rule.MaxGrossExposure {
			return fmt.Errorf("%w: gross exposure exceeds %d (version %d)",
				ErrLimitExceeded, rule.MaxGrossExposure, rv.Version)
		}
	}
	return nil
}

// Accept 受理一笔资金指令。受理时固定付款方、收款方、金额、价值日
// 以及双方各自生效的限额版本，并在同一持久化边界内增加双方占用。
// 任一层限额不足时整笔拒绝，双方占用均不增加。
//
// 相同 RequestID 的重复请求返回首次受理的指令，不产生新的占用。
func (s *Service) Accept(requestID, payer, payee, currency string, amount int64, valueDate time.Time) (*Instruction, error) {
	if requestID == "" || payer == "" || payee == "" || currency == "" {
		return nil, errors.New("settlement: requestID, payer, payee and currency must not be empty")
	}
	if payer == payee {
		return nil, errors.New("settlement: payer and payee must differ")
	}
	if amount <= 0 {
		return nil, errors.New("settlement: amount must be positive")
	}

	now := s.now()
	payerKey := RuleKey{Participant: payer, Counterparty: payee, Currency: currency}
	payeeKey := RuleKey{Participant: payee, Counterparty: payer, Currency: currency}
	payerRV, err := s.rules.Resolve(payerKey, now)
	if err != nil {
		return nil, err
	}
	payeeRV, err := s.rules.Resolve(payeeKey, now)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.byRequestID[requestID]; ok {
		dup := *s.byID[id]
		return &dup, nil
	}

	if err := s.checkSide(payer, payee, currency, valueDate, payerRV, amount, amount, amount); err != nil {
		return nil, err
	}
	if err := s.checkSide(payee, payer, currency, valueDate, payeeRV, 0, -amount, 0); err != nil {
		return nil, err
	}

	s.seq++
	ins := &Instruction{
		ID:           fmt.Sprintf("INS-%d", s.seq),
		RequestID:    requestID,
		Payer:        payer,
		Payee:        payee,
		Currency:     currency,
		Amount:       amount,
		ValueDate:    valueDate,
		PayerVersion: payerRV.Version,
		PayeeVersion: payeeRV.Version,
		Status:       StatusReserved,
		AcceptedAt:   now,
	}
	s.byID[ins.ID] = ins
	s.byRequestID[requestID] = ins.ID
	dup := *ins
	return &dup, nil
}

// transition 将指令从预留状态迁移到目标状态。
// 释放或转换只依据该指令自身的占用记录，重复或迟到的操作
// 不会影响其他指令已取得的额度。
func (s *Service) transition(id string, target InstructionStatus) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInstructionNotFound, id)
	}
	if ins.Status == target {
		dup := *ins
		return &dup, nil
	}
	if ins.Status != StatusReserved {
		return nil, fmt.Errorf("%w: instruction %s is %s", ErrInvalidTransition, id, ins.Status)
	}
	ins.Status = target
	dup := *ins
	return &dup, nil
}

// Complete 将指令标记为结算完成，预留占用转为已用额度。
func (s *Service) Complete(id string) (*Instruction, error) {
	return s.transition(id, StatusSettled)
}

// Cancel 取消指令并释放其占用。对已取消的指令重复调用为幂等操作。
func (s *Service) Cancel(id string) (*Instruction, error) {
	return s.transition(id, StatusCancelled)
}

// Fail 将指令标记为失败并释放其占用。
func (s *Service) Fail(id string) (*Instruction, error) {
	return s.transition(id, StatusFailed)
}

// Get 返回指令快照。
func (s *Service) Get(id string) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInstructionNotFound, id)
	}
	dup := *ins
	return &dup, nil
}

// Usage 按参与方、对手方、币种和价值日聚合占用明细。
func (s *Service) Usage(participant, counterparty, currency string, valueDate time.Time) UsageSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := UsageSummary{
		Participant:  participant,
		Counterparty: counterparty,
		Currency:     currency,
		ValueDate:    valueDate,
	}
	for _, ins := range s.byID {
		if ins.Currency != currency || !sameDay(ins.ValueDate, valueDate) || !active(ins.Status) {
			continue
		}
		out := ins.Payer == participant && ins.Payee == counterparty
		in := ins.Payer == counterparty && ins.Payee == participant
		if !out && !in {
			continue
		}
		switch {
		case out && ins.Status == StatusReserved:
			sum.ReservedOutgoing += ins.Amount
		case out && ins.Status == StatusSettled:
			sum.UsedOutgoing += ins.Amount
		case in && ins.Status == StatusReserved:
			sum.ReservedIncoming += ins.Amount
		case in && ins.Status == StatusSettled:
			sum.UsedIncoming += ins.Amount
		}
	}
	return sum
}

// UsageDetails 返回参与方与对手方之间逐笔的占用明细。
func (s *Service) UsageDetails(participant, counterparty, currency string, valueDate time.Time) []UsageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []UsageEntry
	for _, ins := range s.byID {
		if ins.Currency != currency || !sameDay(ins.ValueDate, valueDate) || !active(ins.Status) {
			continue
		}
		out := ins.Payer == participant && ins.Payee == counterparty
		in := ins.Payer == counterparty && ins.Payee == participant
		if !out && !in {
			continue
		}
		e := UsageEntry{
			InstructionID: ins.ID,
			Participant:   participant,
			Counterparty:  counterparty,
			Currency:      currency,
			ValueDate:     ins.ValueDate,
		}
		if out {
			e.Outgoing = ins.Amount
		} else {
			e.Incoming = ins.Amount
		}
		if ins.Status == StatusReserved {
			e.Reserved = ins.Amount
		} else {
			e.Used = ins.Amount
		}
		entries = append(entries, e)
	}
	return entries
}
