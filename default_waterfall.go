package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Layer identifies one tier of the default waterfall.
type Layer string

const (
	LayerCash       Layer = "CASH"
	LayerCollateral Layer = "COLLATERAL"
	LayerFundShare  Layer = "FUND_SHARE"
	LayerMutualFund Layer = "MUTUAL_FUND"
)

// DisposalStatus is the lifecycle state of a default disposal.
type DisposalStatus string

const (
	DisposalPlanned   DisposalStatus = "PLANNED"
	DisposalCompleted DisposalStatus = "COMPLETED"
)

var (
	ErrParticipantExists   = errors.New("settlement: participant already registered")
	ErrParticipantNotFound = errors.New("settlement: participant not found")
	ErrPositionExists      = errors.New("settlement: position already registered")
	ErrPositionNotFound    = errors.New("settlement: position not found")
	ErrPositionNotOpen     = errors.New("settlement: position is not open")
	ErrCollateralExists    = errors.New("settlement: collateral already registered")
	ErrCollateralNotFound  = errors.New("settlement: collateral not found")
	ErrCollateralRevoked   = errors.New("settlement: collateral revoked")
	ErrResourceOccupied    = errors.New("settlement: resource occupied by another disposal")
	ErrDisposalNotFound    = errors.New("settlement: disposal not found")
	ErrConflict            = errors.New("settlement: version conflict")
	ErrInvalidAmount       = errors.New("settlement: amount must be positive")
)

// Collateral is a dedicated collateral asset pledged by a participant.
type Collateral struct {
	ID               string
	ParticipantID    string
	Value            int64
	Revoked          bool
	OccupiedBy       string // disposal ID that froze this collateral, "" if free
	ValuationVersion int64
}

// Position is an unsettled net payable position.
type Position struct {
	ID            string
	ParticipantID string
	Amount        int64
	Open          bool
	Version       int64
}

// Participant holds the resources a participant contributes to the waterfall.
type Participant struct {
	ID          string
	Cash        int64
	FundShare   int64
	Collaterals map[string]*Collateral
	Version     int64
}

// UsageRecord is one ledger entry describing resources consumed by a layer.
// It always references the frozen position and the frozen resource versions.
type UsageRecord struct {
	DisposalID       string
	Layer            Layer
	ResourceID       string
	Amount           int64
	PositionID       string
	PositionVersion  int64
	ValuationVersion int64
	RulesVersion     string
}

// Disposal is a default handling plan and its execution result.
type Disposal struct {
	ID               string
	PositionID       string
	ParticipantID    string
	RulesVersion     string
	PositionVersion  int64
	ValuationVersion int64
	ResourceVersion  int64
	Status           DisposalStatus
	Deficit          int64 // total gap at freeze time
	RemainingGap     int64 // gap left uncovered after execution
	Usage            []UsageRecord
}

// Engine registers participants, positions and resources, and runs the
// default waterfall. All methods are safe for concurrent use.
type Engine struct {
	mu                sync.Mutex
	participants      map[string]*Participant
	positions         map[string]*Position
	disposals         map[string]*Disposal
	mutualFund        int64
	mutualFundVersion int64
	rulesVersion      string
}

func NewEngine() *Engine {
	return &Engine{
		participants: make(map[string]*Participant),
		positions:    make(map[string]*Position),
		disposals:    make(map[string]*Disposal),
		rulesVersion: "v1",
	}
}

// SetRulesVersion switches the active disposal rules version.
func (e *Engine) SetRulesVersion(v string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rulesVersion = v
}

// RegisterParticipant registers a participant with its available cash and
// its dedicated default fund share.
func (e *Engine) RegisterParticipant(id string, cash, fundShare int64) error {
	if cash < 0 || fundShare < 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.participants[id]; ok {
		return ErrParticipantExists
	}
	e.participants[id] = &Participant{
		ID:          id,
		Cash:        cash,
		FundShare:   fundShare,
		Collaterals: make(map[string]*Collateral),
		Version:     1,
	}
	return nil
}

// AddCollateral pledges a dedicated collateral asset for a participant.
func (e *Engine) AddCollateral(participantID, collateralID string, value int64) error {
	if value < 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.participants[participantID]
	if !ok {
		return ErrParticipantNotFound
	}
	if _, ok := p.Collaterals[collateralID]; ok {
		return ErrCollateralExists
	}
	p.Collaterals[collateralID] = &Collateral{
		ID:               collateralID,
		ParticipantID:    participantID,
		Value:            value,
		ValuationVersion: 1,
	}
	p.Version++
	return nil
}

// RevokeCollateral withdraws a collateral asset. Assets frozen by a planned
// disposal cannot be revoked.
func (e *Engine) RevokeCollateral(participantID, collateralID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.participants[participantID]
	if !ok {
		return ErrParticipantNotFound
	}
	c, ok := p.Collaterals[collateralID]
	if !ok {
		return ErrCollateralNotFound
	}
	if c.OccupiedBy != "" {
		return ErrResourceOccupied
	}
	c.Revoked = true
	p.Version++
	return nil
}

// RegisterPosition registers an unsettled net payable position.
func (e *Engine) RegisterPosition(id, participantID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.participants[participantID]; !ok {
		return ErrParticipantNotFound
	}
	if _, ok := e.positions[id]; ok {
		return ErrPositionExists
	}
	e.positions[id] = &Position{ID: id, ParticipantID: participantID, Amount: amount, Open: true, Version: 1}
	return nil
}

// TopUpCash records a margin top-up for a participant.
func (e *Engine) TopUpCash(participantID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.participants[participantID]
	if !ok {
		return ErrParticipantNotFound
	}
	p.Cash += amount
	p.Version++
	return nil
}

// RevalueCollateral applies a valuation change to a collateral asset.
func (e *Engine) RevalueCollateral(participantID, collateralID string, newValue int64) error {
	if newValue < 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.participants[participantID]
	if !ok {
		return ErrParticipantNotFound
	}
	c, ok := p.Collaterals[collateralID]
	if !ok {
		return ErrCollateralNotFound
	}
	if c.Revoked {
		return ErrCollateralRevoked
	}
	c.Value = newValue
	c.ValuationVersion++
	p.Version++
	return nil
}

// ContributeMutualFund adds funds to the mutual default fund.
func (e *Engine) ContributeMutualFund(amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mutualFund += amount
	e.mutualFundVersion++
	return nil
}

// resourceVersion combines every versioned resource of a participant so a
// plan can detect any later change.
func resourceVersion(p *Participant) int64 {
	v := p.Version
	for _, c := range p.Collaterals {
		v += c.ValuationVersion
	}
	return v
}

// PlanDisposal initiates a default disposal: it freezes the position, the
// balances of every waterfall layer, the valuation versions and the rules
// version. Revoked collateral and resources occupied by another planned
// disposal are rejected. Planning is idempotent per disposal ID.
func (e *Engine) PlanDisposal(disposalID, positionID string) (*Disposal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.disposals[disposalID]; ok {
		if existing.PositionID != positionID {
			return nil, fmt.Errorf("%w: disposal %q already targets position %q", ErrConflict, disposalID, existing.PositionID)
		}
		return existing, nil
	}
	pos, ok := e.positions[positionID]
	if !ok {
		return nil, ErrPositionNotFound
	}
	if !pos.Open {
		return nil, ErrPositionNotOpen
	}
	p := e.participants[pos.ParticipantID]
	for _, c := range p.Collaterals {
		if c.OccupiedBy != "" {
			return nil, fmt.Errorf("%w: collateral %q held by disposal %q", ErrResourceOccupied, c.ID, c.OccupiedBy)
		}
	}
	var valuationVersion int64
	for _, c := range p.Collaterals {
		if c.Revoked {
			return nil, fmt.Errorf("%w: collateral %q", ErrCollateralRevoked, c.ID)
		}
		valuationVersion += c.ValuationVersion
	}
	d := &Disposal{
		ID:               disposalID,
		PositionID:       positionID,
		ParticipantID:    pos.ParticipantID,
		RulesVersion:     e.rulesVersion,
		PositionVersion:  pos.Version,
		ValuationVersion: valuationVersion,
		ResourceVersion:  resourceVersion(p),
		Status:           DisposalPlanned,
		Deficit:          pos.Amount,
		RemainingGap:     pos.Amount,
	}
	pos.Open = false
	for _, c := range p.Collaterals {
		c.OccupiedBy = disposalID
	}
	e.disposals[disposalID] = d
	return d, nil
}

// ExecuteDisposal runs the frozen plan through the waterfall in strict
// order: participant cash, dedicated collateral, the participant's default
// fund share, then the mutual fund. A layer is only touched after every
// previous layer is exhausted. Execution is idempotent: repeating it with
// the same disposal ID returns the original result. If any frozen version
// changed since planning, the stale plan is rejected with ErrConflict.
func (e *Engine) ExecuteDisposal(disposalID string) (*Disposal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.disposals[disposalID]
	if !ok {
		return nil, ErrDisposalNotFound
	}
	if d.Status == DisposalCompleted {
		return d, nil
	}
	pos := e.positions[d.PositionID]
	p := e.participants[d.ParticipantID]
	if pos.Version != d.PositionVersion ||
		resourceVersion(p) != d.ResourceVersion ||
		e.rulesVersion != d.RulesVersion {
		return nil, fmt.Errorf("%w: disposal %q planned against stale versions", ErrConflict, disposalID)
	}

	gap := d.Deficit
	record := func(layer Layer, resourceID string, amount int64) {
		d.Usage = append(d.Usage, UsageRecord{
			DisposalID:       d.ID,
			Layer:            layer,
			ResourceID:       resourceID,
			Amount:           amount,
			PositionID:       d.PositionID,
			PositionVersion:  d.PositionVersion,
			ValuationVersion: d.ValuationVersion,
			RulesVersion:     d.RulesVersion,
		})
		gap -= amount
	}

	// Layer 1: participant cash.
	if gap > 0 && p.Cash > 0 {
		use := min(gap, p.Cash)
		p.Cash -= use
		record(LayerCash, p.ID, use)
	}
	// Layer 2: dedicated collateral, deterministic order by collateral ID.
	if gap > 0 {
		ids := make([]string, 0, len(p.Collaterals))
		for id := range p.Collaterals {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if gap == 0 {
				break
			}
			c := p.Collaterals[id]
			if c.Value == 0 {
				continue
			}
			use := min(gap, c.Value)
			c.Value -= use
			record(LayerCollateral, c.ID, use)
		}
	}
	// Layer 3: participant's own default fund share.
	if gap > 0 && p.FundShare > 0 {
		use := min(gap, p.FundShare)
		p.FundShare -= use
		record(LayerFundShare, p.ID, use)
	}
	// Layer 4: mutual default fund.
	if gap > 0 && e.mutualFund > 0 {
		use := min(gap, e.mutualFund)
		e.mutualFund -= use
		e.mutualFundVersion++
		record(LayerMutualFund, "MUTUAL_FUND", use)
	}

	d.RemainingGap = gap
	d.Status = DisposalCompleted
	for _, c := range p.Collaterals {
		c.OccupiedBy = ""
	}
	return d, nil
}

// GetDisposal returns a copy of a disposal, including its gap and usage.
func (e *Engine) GetDisposal(disposalID string) (*Disposal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.disposals[disposalID]
	if !ok {
		return nil, ErrDisposalNotFound
	}
	cp := *d
	cp.Usage = append([]UsageRecord(nil), d.Usage...)
	return &cp, nil
}

// UsageRecords returns the usage ledger of a disposal. Every record traces
// back to the frozen position and resource versions.
func (e *Engine) UsageRecords(disposalID string) ([]UsageRecord, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.disposals[disposalID]
	if !ok {
		return nil, ErrDisposalNotFound
	}
	return append([]UsageRecord(nil), d.Usage...), nil
}

// Resources is a snapshot of the remaining resources of a participant and
// the mutual fund.
type Resources struct {
	Cash        int64
	Collaterals map[string]int64
	FundShare   int64
	MutualFund  int64
}

// RemainingResources reports the resources left after all executions so far.
func (e *Engine) RemainingResources(participantID string) (*Resources, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.participants[participantID]
	if !ok {
		return nil, ErrParticipantNotFound
	}
	r := &Resources{
		Cash:        p.Cash,
		Collaterals: make(map[string]int64, len(p.Collaterals)),
		FundShare:   p.FundShare,
		MutualFund:  e.mutualFund,
	}
	for id, c := range p.Collaterals {
		r.Collaterals[id] = c.Value
	}
	return r, nil
}

// MutualFundAllocations reports, per disposal, how much of the mutual fund
// was allocated to cover defaults.
func (e *Engine) MutualFundAllocations() map[string]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int64)
	for id, d := range e.disposals {
		for _, u := range d.Usage {
			if u.Layer == LayerMutualFund {
				out[id] += u.Amount
			}
		}
	}
	return out
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
