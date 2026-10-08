package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SettlementStatus 原结算记录的生命周期状态。
type SettlementStatus string

const (
	// StatusPending 结算已创建但尚未发送付款。
	StatusPending SettlementStatus = "PENDING"
	// StatusCompleted 结算付款已完成，状态不可回退。
	StatusCompleted SettlementStatus = "COMPLETED"
)

// DisputeStatus 争议的处理状态。
type DisputeStatus string

const (
	// DisputeAccepted 争议已受理，等待处理决定。
	DisputeAccepted DisputeStatus = "ACCEPTED"
	// DisputeResolved 争议已有最终决定，决定记录不可修改。
	DisputeResolved DisputeStatus = "RESOLVED"
)

// DecisionType 处理决定的类型。
type DecisionType string

const (
	// DecisionReject 驳回争议，冻结金额解冻并回归原结算流程。
	DecisionReject DecisionType = "REJECT"
	// DecisionRefundPayer 争议金额退回付款方。
	DecisionRefundPayer DecisionType = "REFUND_PAYER"
	// DecisionPayPayee 争议金额继续支付给收款方。
	DecisionPayPayee DecisionType = "PAY_PAYEE"
)

var (
	ErrSettlementNotFound = errors.New("settlement: 结算不存在")
	ErrDisputeNotFound    = errors.New("settlement: 争议不存在")
	ErrDisputeConflict    = errors.New("settlement: 相同争议号但内容不一致")
	ErrDisputeLimit       = errors.New("settlement: 累计争议金额超过原结算金额")
	ErrAlreadyResolved    = errors.New("settlement: 争议已有最终决定")
	ErrInvalidAmount      = errors.New("settlement: 金额必须为正数")
	ErrAlreadyCompleted   = errors.New("settlement: 结算已完成")
)

// Settlement 原结算记录。创建后金额、收付双方不可修改；
// 争议只影响 FrozenAmount 与后续调整记录，不回写原始金额。
type Settlement struct {
	ID           string
	Payer        string
	Payee        string
	Amount       int64
	Status       SettlementStatus
	FrozenAmount int64
	PaidAmount   int64
}

// Dispute 争议申请。Number 为幂等键。
type Dispute struct {
	Number          string
	SettlementID    string
	Amount          int64
	Reason          string
	EvidenceSummary string
	EvidenceVersion string
	Initiator       string
	Status          DisputeStatus
	CreatedAt       time.Time
}

// Decision 不可修改的处理决定记录。
type Decision struct {
	DisputeNumber   string
	Type            DecisionType
	DecidedBy       string
	Reason          string
	EvidenceVersion string
	DecidedAt       time.Time
}

// Adjustment 资金调整记录，与决定一次性写入，独立于原结算。
type Adjustment struct {
	ID            string
	DisputeNumber string
	SettlementID  string
	Type          DecisionType
	Amount        int64
	CreatedAt     time.Time
}

// SettlementView 查询视图：原结算 + 冻结金额 + 争议/证据 + 决定 + 调整。
type SettlementView struct {
	Settlement  Settlement
	Disputes    []Dispute
	Decisions   []Decision
	Adjustments []Adjustment
}

// Service 结算争议服务，内存实现，所有状态变更串行化。
type Service struct {
	mu          sync.Mutex
	settlements map[string]*Settlement
	disputes    map[string]*Dispute // key: 争议号
	decisions   map[string]*Decision
	adjustments []*Adjustment
	now         func() time.Time
	// beforeCommit 在决定落库前调用，用于注入失败以验证回滚。
	beforeCommit func() error
	seq          int
}

func NewService() *Service {
	return &Service{
		settlements: make(map[string]*Settlement),
		disputes:    make(map[string]*Dispute),
		decisions:   make(map[string]*Decision),
		now:         time.Now,
	}
}

// CreateSettlement 创建原结算记录。
func (s *Service) CreateSettlement(id, payer, payee string, amount int64) (*Settlement, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settlements[id]; ok {
		return nil, fmt.Errorf("settlement: 结算 %s 已存在", id)
	}
	st := &Settlement{ID: id, Payer: payer, Payee: payee, Amount: amount, Status: StatusPending}
	s.settlements[id] = st
	cp := *st
	return &cp, nil
}

// CompleteSettlement 按原计划支付未冻结部分并完成结算。
// 已完成结算的争议部分通过后续调整记录处理，不修改本记录金额。
func (s *Service) CompleteSettlement(id string) (*Settlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.settlements[id]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	if st.Status == StatusCompleted {
		return nil, ErrAlreadyCompleted
	}
	st.PaidAmount += st.Amount - st.FrozenAmount - st.PaidAmount
	st.Status = StatusCompleted
	cp := *st
	return &cp, nil
}

// ApplyDispute 受理争议。相同争议号内容一致时返回原结果；
// 金额或原结算不一致时返回冲突。未发送的结算冻结对应金额；
// 已完成的结算只记录待处理调整，不改变其成功状态。
func (s *Service) ApplyDispute(d Dispute) (*Dispute, error) {
	if d.Amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.disputes[d.Number]; ok {
		if existing.SettlementID != d.SettlementID || existing.Amount != d.Amount {
			return nil, ErrDisputeConflict
		}
		cp := *existing
		return &cp, nil
	}
	st, ok := s.settlements[d.SettlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	var frozenTotal int64
	for _, ex := range s.disputes {
		if ex.SettlementID == d.SettlementID {
			frozenTotal += ex.Amount
		}
	}
	if frozenTotal+d.Amount > st.Amount {
		return nil, ErrDisputeLimit
	}
	d.Status = DisputeAccepted
	d.CreatedAt = s.now()
	s.disputes[d.Number] = &d
	if st.Status == StatusPending {
		st.FrozenAmount += d.Amount
	}
	// 已完成的结算：仅记录争议，等待决定产生调整记录，状态保持 COMPLETED。
	cp := d
	return &cp, nil
}

// Decide 对争议作出处理决定并一次性写入决定记录与资金调整。
// 同一争议最多成功一次；失败时原结算与冻结金额保持不变，可继续处理。
func (s *Service) Decide(disputeNumber string, typ DecisionType, decidedBy, reason, evidenceVersion string) (*Decision, *Adjustment, error) {
	switch typ {
	case DecisionReject, DecisionRefundPayer, DecisionPayPayee:
	default:
		return nil, nil, fmt.Errorf("settlement: 未知决定类型 %q", typ)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.disputes[disputeNumber]
	if !ok {
		return nil, nil, ErrDisputeNotFound
	}
	if d.Status == DisputeResolved {
		return nil, nil, ErrAlreadyResolved
	}
	st, ok := s.settlements[d.SettlementID]
	if !ok {
		return nil, nil, ErrSettlementNotFound
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return nil, nil, err
		}
	}
	now := s.now()
	dec := &Decision{
		DisputeNumber:   disputeNumber,
		Type:            typ,
		DecidedBy:       decidedBy,
		Reason:          reason,
		EvidenceVersion: evidenceVersion,
		DecidedAt:       now,
	}
	var adj *Adjustment
	if typ != DecisionReject {
		s.seq++
		adj = &Adjustment{
			ID:            fmt.Sprintf("ADJ-%d", s.seq),
			DisputeNumber: disputeNumber,
			SettlementID:  d.SettlementID,
			Type:          typ,
			Amount:        d.Amount,
			CreatedAt:     now,
		}
		s.adjustments = append(s.adjustments, adj)
	}
	// 未发送结算上的冻结金额随决定解冻：驳回回归原计划，
	// 退回付款方/支付收款方通过调整记录独立核算。
	if st.Status == StatusPending {
		st.FrozenAmount -= d.Amount
		if st.FrozenAmount < 0 {
			st.FrozenAmount = 0
		}
	}
	d.Status = DisputeResolved
	s.decisions[disputeNumber] = dec
	return dec, adj, nil
}

// GetSettlementView 查询原结算及其冻结金额、争议证据、决定与后续调整。
func (s *Service) GetSettlementView(id string) (*SettlementView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.settlements[id]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	view := &SettlementView{Settlement: *st}
	for _, d := range s.disputes {
		if d.SettlementID == id {
			view.Disputes = append(view.Disputes, *d)
		}
	}
	sort.Slice(view.Disputes, func(i, j int) bool { return view.Disputes[i].Number < view.Disputes[j].Number })
	for _, d := range view.Disputes {
		if dec, ok := s.decisions[d.Number]; ok {
			view.Decisions = append(view.Decisions, *dec)
		}
	}
	for _, a := range s.adjustments {
		if a.SettlementID == id {
			view.Adjustments = append(view.Adjustments, *a)
		}
	}
	return view, nil
}
