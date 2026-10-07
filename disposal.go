package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Layer 标识担保资源的层级，处置时严格按 Cash -> Collateral -> FundShare -> MutualFund 顺序动用。
type Layer int

const (
	LayerCash Layer = iota
	LayerCollateral
	LayerFundShare
	LayerMutualFund
)

func (l Layer) String() string {
	switch l {
	case LayerCash:
		return "cash"
	case LayerCollateral:
		return "collateral"
	case LayerFundShare:
		return "fund_share"
	case LayerMutualFund:
		return "mutual_fund"
	}
	return "unknown"
}

var (
	ErrParticipantNotFound = errors.New("participant not found")
	ErrParticipantExists   = errors.New("participant already exists")
	ErrPositionNotFound    = errors.New("position not found")
	ErrPositionNotOpen     = errors.New("position is not open")
	ErrDisposalNotFound    = errors.New("disposal not found")
	ErrConflict            = errors.New("version conflict")
	ErrResourceFrozen      = errors.New("resource frozen by another disposal")
	ErrInvalidAmount       = errors.New("amount must be positive")
)

// Participant 清算参与方及其担保资源。
type Participant struct {
	ID         string
	Cash       int64
	Collateral int64 // 专属担保品（已按估值版本折算）
	FundShare  int64 // 共同违约基金中归属该参与方的份额

	CashVersion       int64
	CollateralVersion int64
	FundShareVersion  int64

	FrozenBy string // 占用该资源的处置号，空表示未冻结
}

// Position 未结清的净应付头寸。
type Position struct {
	ID            string
	ParticipantID string
	Amount        int64
	Version       int64
	Open          bool
	FrozenBy      string
}

// Usage 单层资源的使用台账记录。
type Usage struct {
	Layer     Layer
	Amount    int64
	Position  PositionSnapshot
	Resources ResourceSnapshot
}

// PositionSnapshot 处置发起时冻结的头寸快照。
type PositionSnapshot struct {
	PositionID    string
	ParticipantID string
	Amount        int64
	Version       int64
}

// ResourceSnapshot 处置发起时冻结的资源余额与版本。
type ResourceSnapshot struct {
	Cash              int64
	Collateral        int64
	FundShare         int64
	CashVersion       int64
	CollateralVersion int64
	FundShareVersion  int64
	ValuationVersion  int64
}

// Disposal 一次违约处置。
type Disposal struct {
	ID               string
	Position         PositionSnapshot
	Resources        ResourceSnapshot
	RuleVersion      int64
	Executed         bool
	Usages           []Usage
	RemainingGap     int64 // 处置执行后仍未弥补的缺口
	MutualFundBefore int64
	MutualFundAfter  int64
}

// Completed 表示缺口已被全部弥补。
func (d *Disposal) Completed() bool { return d.Executed && d.RemainingGap == 0 }

// Store 违约处置内存存储，所有写操作在同一互斥锁下完成，保证余额扣减与台账一致。
type Store struct {
	mu           sync.Mutex
	participants map[string]*Participant
	positions    map[string]*Position
	disposals    map[string]*Disposal
	mutualFund   int64 // 共同违约基金池（扣除各参与方份额后的公共部分）
	ruleVersion  int64
	valuation    int64
}

func NewStore() *Store {
	return &Store{
		participants: make(map[string]*Participant),
		positions:    make(map[string]*Position),
		disposals:    make(map[string]*Disposal),
		ruleVersion:  1,
		valuation:    1,
	}
}

// RegisterParticipant 登记参与方及其现金、专属担保品和违约基金份额。
func (s *Store) RegisterParticipant(id string, cash, collateral, fundShare int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.participants[id]; ok {
		return ErrParticipantExists
	}
	s.participants[id] = &Participant{
		ID: id, Cash: cash, Collateral: collateral, FundShare: fundShare,
		CashVersion: 1, CollateralVersion: 1, FundShareVersion: 1,
	}
	return nil
}

// RegisterPosition 登记未结清的净应付头寸。
func (s *Store) RegisterPosition(id, participantID string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.participants[participantID]; !ok {
		return ErrParticipantNotFound
	}
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.positions[id] = &Position{ID: id, ParticipantID: participantID, Amount: amount, Version: 1, Open: true}
	return nil
}

// FundMutualFund 向共同违约基金公共池注资。
func (s *Store) FundMutualFund(amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutualFund += amount
}

// SetRuleVersion 设置当前处置规则版本。
func (s *Store) SetRuleVersion(v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleVersion = v
}

// resourceLayer 选择要变更的资源层。
type resourceLayer int

const (
	resCash resourceLayer = iota
	resCollateral
	resFundShare
)

// changeResource 补缴现金、担保品估值变化或基金份额调整。
// 资源被未执行的处置冻结时返回冲突；处置已执行完成的，变更只作用于剩余余额，
// 不会改写已生成的使用明细。
func (s *Store) changeResource(participantID string, layer resourceLayer, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.participants[participantID]
	if !ok {
		return ErrParticipantNotFound
	}
	if p.FrozenBy != "" {
		if d := s.disposals[p.FrozenBy]; d != nil && !d.Executed {
			return fmt.Errorf("%w: disposal %s", ErrResourceFrozen, p.FrozenBy)
		}
		p.FrozenBy = "" // 处置已执行，解除冻结
	}
	switch layer {
	case resCash:
		if p.Cash+delta < 0 {
			return ErrInvalidAmount
		}
		p.Cash += delta
		p.CashVersion++
	case resCollateral:
		if p.Collateral+delta < 0 {
			return ErrInvalidAmount
		}
		p.Collateral += delta
		p.CollateralVersion++
		s.valuation++ // 担保品估值变化推进估值版本
	case resFundShare:
		if p.FundShare+delta < 0 {
			return ErrInvalidAmount
		}
		p.FundShare += delta
		p.FundShareVersion++
	}
	return nil
}

// TopUpCash 参与方补缴现金。
func (s *Store) TopUpCash(participantID string, amount int64) error {
	return s.changeResource(participantID, resCash, amount)
}

// RevalueCollateral 担保品估值变化（delta 可为负）。
func (s *Store) RevalueCollateral(participantID string, delta int64) error {
	return s.changeResource(participantID, resCollateral, delta)
}

// AdjustFundShare 调整参与方违约基金份额。
func (s *Store) AdjustFundShare(participantID string, delta int64) error {
	return s.changeResource(participantID, resFundShare, delta)
}

// InitiateDisposal 发起处置：冻结头寸、各层资源余额、估值版本和规则版本。
// 相同处置号相同内容重复提交返回原结果；头寸、规则或资源版本不同返回冲突。
func (s *Store) InitiateDisposal(disposalID, positionID string) (*Disposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pos, ok := s.positions[positionID]
	if !ok {
		return nil, ErrPositionNotFound
	}
	snap := PositionSnapshot{
		PositionID:    pos.ID,
		ParticipantID: pos.ParticipantID,
		Amount:        pos.Amount,
		Version:       pos.Version,
	}
	p := s.participants[pos.ParticipantID]
	res := ResourceSnapshot{
		Cash: p.Cash, Collateral: p.Collateral, FundShare: p.FundShare,
		CashVersion: p.CashVersion, CollateralVersion: p.CollateralVersion,
		FundShareVersion: p.FundShareVersion, ValuationVersion: s.valuation,
	}

	if existing, ok := s.disposals[disposalID]; ok {
		if existing.Position == snap && existing.Resources == res && existing.RuleVersion == s.ruleVersion {
			return existing, nil // 幂等：相同内容返回原结果
		}
		return nil, fmt.Errorf("%w: disposal %s already initiated with different content", ErrConflict, disposalID)
	}

	if !pos.Open || pos.FrozenBy != "" {
		return nil, ErrPositionNotOpen
	}
	if p.FrozenBy != "" {
		return nil, fmt.Errorf("%w: participant %s", ErrResourceFrozen, pos.ParticipantID)
	}

	pos.FrozenBy = disposalID
	p.FrozenBy = disposalID
	d := &Disposal{
		ID: disposalID, Position: snap, Resources: res,
		RuleVersion: s.ruleVersion, MutualFundBefore: s.mutualFund,
	}
	s.disposals[disposalID] = d
	return d, nil
}

// ExecuteDisposal 按冻结版本执行瀑布式处置。
// 资源在冻结后发生变化时返回冲突；余额扣减与使用台账在同一临界区内完成。
func (s *Store) ExecuteDisposal(disposalID string) (*Disposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.disposals[disposalID]
	if !ok {
		return nil, ErrDisposalNotFound
	}
	if d.Executed {
		return d, nil // 幂等：重复执行返回原结果
	}
	p := s.participants[d.Position.ParticipantID]
	pos := s.positions[d.Position.PositionID]
	if p.CashVersion != d.Resources.CashVersion ||
		p.CollateralVersion != d.Resources.CollateralVersion ||
		p.FundShareVersion != d.Resources.FundShareVersion ||
		pos.Version != d.Position.Version ||
		s.ruleVersion != d.RuleVersion ||
		s.valuation != d.Resources.ValuationVersion {
		return nil, fmt.Errorf("%w: resources changed after freeze", ErrConflict)
	}

	gap := d.Position.Amount
	use := func(layer Layer, available int64) int64 {
		if gap <= 0 || available <= 0 {
			return 0
		}
		amount := available
		if amount > gap {
			amount = gap
		}
		gap -= amount
		d.Usages = append(d.Usages, Usage{Layer: layer, Amount: amount, Position: d.Position, Resources: d.Resources})
		return amount
	}

	p.Cash -= use(LayerCash, p.Cash)
	p.Collateral -= use(LayerCollateral, p.Collateral)
	p.FundShare -= use(LayerFundShare, p.FundShare)
	s.mutualFund -= use(LayerMutualFund, s.mutualFund)

	d.RemainingGap = gap
	d.MutualFundAfter = s.mutualFund
	d.Executed = true
	p.FrozenBy = ""
	pos.FrozenBy = ""
	if gap == 0 {
		pos.Open = false
	}
	return d, nil
}

// GetDisposal 查询处置结果：缺口、各层使用明细和冻结快照。
func (s *Store) GetDisposal(disposalID string) (*Disposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.disposals[disposalID]
	if !ok {
		return nil, ErrDisposalNotFound
	}
	return d, nil
}

// RemainingResources 查询参与方剩余资源。
func (s *Store) RemainingResources(participantID string) (cash, collateral, fundShare int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.participants[participantID]
	if !ok {
		return 0, 0, 0, ErrParticipantNotFound
	}
	return p.Cash, p.Collateral, p.FundShare, nil
}

// MutualFundAllocation 查询共同基金分摊：返回各处置使用共同基金的明细及池余额。
func (s *Store) MutualFundAllocation() (allocations map[string]int64, poolBalance int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allocations = make(map[string]int64)
	ids := make([]string, 0, len(s.disposals))
	for id := range s.disposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, u := range s.disposals[id].Usages {
			if u.Layer == LayerMutualFund {
				allocations[id] += u.Amount
			}
		}
	}
	return allocations, s.mutualFund
}
