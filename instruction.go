package settlement

import (
	"errors"
	"fmt"
	"time"
)

// State 是资金指令的生命周期状态。
type State string

const (
	StateHeld      State = "HELD"      // 已受理，占用额度
	StateUsed      State = "USED"      // 已结算完成，占用转为已用额度
	StateCancelled State = "CANCELLED" // 已取消，占用已释放
	StateFailed    State = "FAILED"    // 已失败，占用已释放
)

// Instruction 是一笔已受理的资金指令。
// 受理时固定付款方、收款方、金额、价值日与限额版本。
type Instruction struct {
	ID          string
	RequestID   string
	Payer       string
	Payee       string
	Currency    string
	Amount      int64
	ValueDate   time.Time
	RuleVersion int
	State       State
}

// AcceptRequest 是受理指令的请求。
type AcceptRequest struct {
	// RequestID 客户端请求标识，用于幂等：重复提交返回首次受理的指令。
	RequestID string
	Payer     string
	Payee     string
	Currency  string
	Amount    int64
	ValueDate time.Time
}

// Accept 受理一笔资金指令。
// 在同一持久化边界内完成限额校验与双方占用增加：
// 任一层限额不足时整笔拒绝，不会只增加其中一方的占用。
func (s *Service) Accept(req AcceptRequest) (*Instruction, error) {
	if req.Amount <= 0 {
		return nil, errors.New("settlement: amount must be positive")
	}
	if req.Payer == "" || req.Payee == "" || req.Payer == req.Payee {
		return nil, errors.New("settlement: invalid payer/payee")
	}
	if req.RequestID == "" {
		return nil, errors.New("settlement: request id required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 幂等：重复请求直接返回首次受理结果，不重复占用。
	if id, ok := s.byRequest[req.RequestID]; ok {
		cpy := *s.instructions[id]
		return &cpy, nil
	}

	rule := s.resolveRule(req.Payer, req.Payee, req.Currency, s.now())
	if rule == nil {
		return nil, ErrRuleNotFound
	}

	day := dayOf(req.ValueDate)
	payerKey := occKey{req.Payer, req.Currency, day}
	payeeKey := occKey{req.Payee, req.Currency, day}
	flow := flowKey{req.Payer, req.Payee, req.Currency, day}
	reverse := flowKey{req.Payee, req.Payer, req.Currency, day}

	// 第一层：单笔上限。
	if rule.MaxSingleAmount > 0 && req.Amount > rule.MaxSingleAmount {
		return nil, &LimitExceededError{Layer: "single", Limit: rule.MaxSingleAmount, Needed: req.Amount}
	}
	// 第二层：双边净敞口（绝对值）。
	if rule.MaxBilateralNet > 0 {
		newNet := s.net[flow] - s.net[reverse] + req.Amount
		if abs64(newNet) > rule.MaxBilateralNet {
			return nil, &LimitExceededError{Layer: "bilateral_net", Limit: rule.MaxBilateralNet, Needed: abs64(newNet)}
		}
	}
	// 第三层：单方总敞口，付款方与收款方分别校验。
	if rule.MaxUnilateralTotal > 0 {
		if s.unilateral[payerKey]+req.Amount > rule.MaxUnilateralTotal {
			return nil, &LimitExceededError{Layer: "unilateral_total", Limit: rule.MaxUnilateralTotal, Needed: s.unilateral[payerKey] + req.Amount}
		}
		if s.unilateral[payeeKey]+req.Amount > rule.MaxUnilateralTotal {
			return nil, &LimitExceededError{Layer: "unilateral_total", Limit: rule.MaxUnilateralTotal, Needed: s.unilateral[payeeKey] + req.Amount}
		}
	}

	// 校验全部通过，原子地增加双方占用。
	s.unilateral[payerKey] += req.Amount
	s.unilateral[payeeKey] += req.Amount
	s.net[flow] += req.Amount

	s.seq++
	ins := &Instruction{
		ID:          fmt.Sprintf("INS-%d", s.seq),
		RequestID:   req.RequestID,
		Payer:       req.Payer,
		Payee:       req.Payee,
		Currency:    req.Currency,
		Amount:      req.Amount,
		ValueDate:   req.ValueDate,
		RuleVersion: rule.Version,
		State:       StateHeld,
	}
	s.instructions[ins.ID] = ins
	s.byRequest[req.RequestID] = ins.ID
	cpy := *ins
	return &cpy, nil
}

// Complete 将指令标记为结算完成，占用转为已用额度（仍计入当日累计）。
// 重复完成是幂等的。
func (s *Service) Complete(id string) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.instructions[id]
	if !ok {
		return nil, ErrInstructionNotFound
	}
	if ins.State == StateUsed {
		cpy := *ins
		return &cpy, nil
	}
	if ins.State != StateHeld {
		return nil, ErrInvalidState
	}
	ins.State = StateUsed
	cpy := *ins
	return &cpy, nil
}

// Cancel 取消指令并按原占用记录释放额度。
// 对已完成的指令（迟到取消）返回 ErrInvalidState，不会释放
// 其他指令后来取得的额度；重复取消是幂等的。
func (s *Service) Cancel(id string) (*Instruction, error) {
	return s.release(id, StateCancelled)
}

// Fail 将指令标记为失败并释放额度，语义同 Cancel。
func (s *Service) Fail(id string) (*Instruction, error) {
	return s.release(id, StateFailed)
}

func (s *Service) release(id string, target State) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.instructions[id]
	if !ok {
		return nil, ErrInstructionNotFound
	}
	if ins.State == target {
		cpy := *ins
		return &cpy, nil
	}
	if ins.State != StateHeld {
		return nil, ErrInvalidState
	}

	day := dayOf(ins.ValueDate)
	s.unilateral[occKey{ins.Payer, ins.Currency, day}] -= ins.Amount
	s.unilateral[occKey{ins.Payee, ins.Currency, day}] -= ins.Amount
	s.net[flowKey{ins.Payer, ins.Payee, ins.Currency, day}] -= ins.Amount
	ins.State = target
	cpy := *ins
	return &cpy, nil
}

// GetInstruction 按 ID 查询指令。
func (s *Service) GetInstruction(id string) (*Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.instructions[id]
	if !ok {
		return nil, ErrInstructionNotFound
	}
	cpy := *ins
	return &cpy, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
