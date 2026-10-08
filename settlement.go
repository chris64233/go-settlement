package settlement

import (
	"errors"
	"fmt"
	"sync"
)

// SettlementStatus 表示原结算的生命周期状态。
type SettlementStatus string

const (
	// SettlementPending 结算已创建但尚未发送，此时争议会冻结对应金额。
	SettlementPending SettlementStatus = "PENDING"
	// SettlementSent 结算已发送但尚未完成。
	SettlementSent SettlementStatus = "SENT"
	// SettlementCompleted 结算已完成，争议只能形成独立调整。
	SettlementCompleted SettlementStatus = "COMPLETED"
)

// Settlement 是不可被争议修改的原结算记录。
// 争议只会改变 FrozenAmount（冻结额）或生成独立的 Adjustment，
// 原始金额、状态流转不会被回退。
type Settlement struct {
	ID           string
	Amount       int64
	Status       SettlementStatus
	FrozenAmount int64
}

// DisputeStatus 表示争议的处理状态。
type DisputeStatus string

const (
	DisputeOpen    DisputeStatus = "OPEN"
	DisputeDecided DisputeStatus = "DECIDED"
)

// Dispute 是关联原结算的争议申请。
type Dispute struct {
	ID            string
	SettlementID  string
	Amount        int64
	Reason        string
	Evidence      string
	EvidenceVer   int
	Initiator     string
	Status        DisputeStatus
	FrozenApplied bool // 受理时是否冻结了原结算金额
}

// DecisionType 是争议处理决定类型。
type DecisionType string

const (
	// DecisionReject 驳回争议，冻结金额解冻。
	DecisionReject DecisionType = "REJECT"
	// DecisionReturnToPayer 争议金额退回付款方。
	DecisionReturnToPayer DecisionType = "RETURN_TO_PAYER"
	// DecisionPayPayee 争议金额继续支付给收款方。
	DecisionPayPayee DecisionType = "PAY_PAYEE"
)

// Decision 是一次性写入且不可修改的处理决定记录。
type Decision struct {
	DisputeID   string
	Type        DecisionType
	Decider     string
	Reason      string
	EvidenceVer int
}

// Adjustment 是争议决定产生的独立资金调整记录，不修改原结算。
type Adjustment struct {
	ID           string
	SettlementID string
	DisputeID    string
	Amount       int64 // 正数表示支付给收款方，负数表示退回付款方
	Type         DecisionType
}

// SettlementView 是查询视图：原结算 + 冻结金额 + 争议/证据 + 决定 + 调整。
type SettlementView struct {
	Settlement  Settlement
	Disputes    []Dispute
	Decisions   []Decision
	Adjustments []Adjustment
}

var (
	ErrSettlementNotFound = errors.New("settlement not found")
	ErrDisputeNotFound    = errors.New("dispute not found")
	ErrDisputeConflict    = errors.New("dispute id exists with different content")
	ErrDisputeLimit       = errors.New("cumulative disputed amount exceeds settlement amount")
	ErrAlreadyDecided     = errors.New("dispute already decided")
	ErrInvalidAmount      = errors.New("dispute amount must be positive")
)

// Store 是并发安全的内存存储。
type Store struct {
	mu          sync.Mutex
	settlements map[string]*Settlement
	disputes    map[string]*Dispute
	decisions   map[string]*Decision // key: disputeID，决定不可修改
	adjustments []*Adjustment
	adjSeq      int

	// applyAdjustment 用于测试注入失败，验证失败时状态可回滚。
	applyAdjustment func(adj *Adjustment) error
}

func NewStore() *Store {
	return &Store{
		settlements: make(map[string]*Settlement),
		disputes:    make(map[string]*Dispute),
		decisions:   make(map[string]*Decision),
	}
}

// CreateSettlement 创建一笔原结算记录。
func (s *Store) CreateSettlement(id string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settlements[id]; ok {
		return fmt.Errorf("settlement %s already exists", id)
	}
	s.settlements[id] = &Settlement{ID: id, Amount: amount, Status: SettlementPending}
	return nil
}

// MarkSent / MarkCompleted 推进原结算状态（只能前进，不能回退）。
func (s *Store) MarkSent(id string) error {
	return s.setStatus(id, SettlementSent)
}

func (s *Store) MarkCompleted(id string) error {
	return s.setStatus(id, SettlementCompleted)
}

func (s *Store) setStatus(id string, st SettlementStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.settlements[id]
	if !ok {
		return ErrSettlementNotFound
	}
	set.Status = st
	return nil
}

// FileDispute 受理争议申请。
// 相同争议号且内容一致时返回原有争议（幂等）；金额或原结算不同时返回冲突。
// 累计争议金额不得超过原结算金额。
// 原结算尚未完成时冻结对应金额；已完成时仅记录待处理调整（FrozenApplied=false）。
func (s *Store) FileDispute(disputeID, settlementID string, amount int64, reason, evidence, initiator string) (*Dispute, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.disputes[disputeID]; ok {
		if existing.SettlementID == settlementID && existing.Amount == amount &&
			existing.Reason == reason && existing.Evidence == evidence && existing.Initiator == initiator {
			return cloneDispute(existing), nil
		}
		return nil, ErrDisputeConflict
	}

	set, ok := s.settlements[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}

	var total int64
	for _, d := range s.disputes {
		if d.SettlementID == settlementID {
			total += d.Amount
		}
	}
	if total+amount > set.Amount {
		return nil, ErrDisputeLimit
	}

	d := &Dispute{
		ID:           disputeID,
		SettlementID: settlementID,
		Amount:       amount,
		Reason:       reason,
		Evidence:     evidence,
		EvidenceVer:  1,
		Initiator:    initiator,
		Status:       DisputeOpen,
	}
	if set.Status != SettlementCompleted {
		// 未完成的结算：冻结争议金额，未争议部分可按原计划继续。
		set.FrozenAmount += amount
		d.FrozenApplied = true
	}
	// 已完成的结算：只记录待处理调整，不把成功状态改回处理中。
	s.disputes[disputeID] = d
	return cloneDispute(d), nil
}

// Decide 对争议做出处理决定。
// 决定与资金调整一次性写入；同一争议最多一个成功决定；
// 写入失败时原结算与冻结金额保持不变，可继续处理。
func (s *Store) Decide(disputeID string, dtype DecisionType, decider, reason string) (*Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.disputes[disputeID]
	if !ok {
		return nil, ErrDisputeNotFound
	}
	if d.Status == DisputeDecided {
		return nil, ErrAlreadyDecided
	}
	set := s.settlements[d.SettlementID]

	adj := &Adjustment{
		SettlementID: d.SettlementID,
		DisputeID:    d.ID,
		Type:         dtype,
	}
	switch dtype {
	case DecisionReject:
		adj.Amount = 0
	case DecisionReturnToPayer:
		adj.Amount = -d.Amount
	case DecisionPayPayee:
		adj.Amount = d.Amount
	default:
		return nil, fmt.Errorf("unknown decision type %q", dtype)
	}

	// 先执行调整写入，失败时不修改任何状态，争议仍可继续处理。
	if s.applyAdjustment != nil {
		if err := s.applyAdjustment(adj); err != nil {
			return nil, fmt.Errorf("apply adjustment: %w", err)
		}
	}

	s.adjSeq++
	adj.ID = fmt.Sprintf("ADJ-%d", s.adjSeq)
	s.adjustments = append(s.adjustments, adj)

	dec := &Decision{
		DisputeID:   disputeID,
		Type:        dtype,
		Decider:     decider,
		Reason:      reason,
		EvidenceVer: d.EvidenceVer,
	}
	s.decisions[disputeID] = dec
	d.Status = DisputeDecided

	// 解冻：驳回时全额解冻；退回/支付时冻结额转化为调整，同样释放冻结。
	if d.FrozenApplied {
		set.FrozenAmount -= d.Amount
		d.FrozenApplied = false
	}
	return dec, nil
}

// GetSettlementView 返回原结算、冻结金额、争议证据、决定与后续调整。
func (s *Store) GetSettlementView(settlementID string) (*SettlementView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.settlements[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	view := &SettlementView{Settlement: *set}
	for _, d := range s.disputes {
		if d.SettlementID == settlementID {
			view.Disputes = append(view.Disputes, *cloneDispute(d))
			if dec, ok := s.decisions[d.ID]; ok {
				view.Decisions = append(view.Decisions, *dec)
			}
		}
	}
	for _, adj := range s.adjustments {
		if adj.SettlementID == settlementID {
			view.Adjustments = append(view.Adjustments, *adj)
		}
	}
	return view, nil
}

func cloneDispute(d *Dispute) *Dispute {
	cp := *d
	return &cp
}
