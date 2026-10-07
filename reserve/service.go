package reserve

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Merchant is a registered reserve account owner.
type Merchant struct {
	ID   string
	Name string
}

type merchantState struct {
	merchant  Merchant
	available Money
	rules     []*ReserveRule // ascending by Version
	batches   map[string]*ReserveBatch
}

// Service manages merchants, rules, withholding, deductions and releases.
// All mutations run under one mutex and bump each batch's optimistic
// version, so concurrent release scans, risk deductions and manual freezes
// always update the same batch version and cannot double-spend a batch.
type Service struct {
	mu            sync.Mutex
	now           func() time.Time
	merchants     map[string]*merchantState
	settlements   map[string]*Settlement
	deductions    map[string]*RiskDeduction
	releaseRuns   map[string]*ReleaseRun
	compensations map[string]*Compensation
	nextBatchSeq  int64
}

// NewService creates an empty Service using the wall clock.
func NewService() *Service { return NewServiceWithClock(time.Now) }

// NewServiceWithClock creates a Service with an injectable clock (for tests).
func NewServiceWithClock(now func() time.Time) *Service {
	return &Service{
		now:           now,
		merchants:     map[string]*merchantState{},
		settlements:   map[string]*Settlement{},
		deductions:    map[string]*RiskDeduction{},
		releaseRuns:   map[string]*ReleaseRun{},
		compensations: map[string]*Compensation{},
	}
}

// RegisterMerchant registers a merchant. Re-registering the same ID with the
// same name returns the existing merchant; a different name conflicts.
func (s *Service) RegisterMerchant(id, name string) (*Merchant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.merchants[id]; ok {
		if st.merchant.Name != name {
			return nil, fmt.Errorf("%w: merchant %s", ErrConflict, id)
		}
		m := st.merchant
		return &m, nil
	}
	st := &merchantState{merchant: Merchant{ID: id, Name: name}, batches: map[string]*ReserveBatch{}}
	s.merchants[id] = st
	m := st.merchant
	return &m, nil
}

// CreateRule adds a new immutable rule version for a merchant.
func (s *Service) CreateRule(merchantID, ruleID string, rateBP, holdDays int, effectiveFrom time.Time) (*ReserveRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, err
	}
	if rateBP < 0 || rateBP > 10000 {
		return nil, fmt.Errorf("%w: rateBP %d", ErrInvalidAmount, rateBP)
	}
	if holdDays < 0 {
		return nil, fmt.Errorf("%w: holdDays %d", ErrInvalidAmount, holdDays)
	}
	version := 1
	if n := len(st.rules); n > 0 {
		version = st.rules[n-1].Version + 1
	}
	rule := &ReserveRule{
		ID:            ruleID,
		MerchantID:    merchantID,
		Version:       version,
		RateBP:        rateBP,
		HoldDays:      holdDays,
		EffectiveFrom: effectiveFrom,
	}
	st.rules = append(st.rules, rule)
	cp := *rule
	return &cp, nil
}

// effectiveRule returns the highest rule version effective at t.
func (st *merchantState) effectiveRule(t time.Time) *ReserveRule {
	var best *ReserveRule
	for _, r := range st.rules {
		if !r.EffectiveFrom.After(t) && (best == nil || r.Version > best.Version) {
			best = r
		}
	}
	return best
}

// RegisterSettlement confirms a settlement and withholds a reserve batch
// using the rule effective at ConfirmedAt. Idempotent by settlement ID:
// same ID and amount returns the original batch; a different amount
// conflicts. Later rule changes never recompute this batch.
func (s *Service) RegisterSettlement(merchantID, settlementID string, amount Money, confirmedAt time.Time) (*Settlement, *ReserveBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, nil, err
	}
	if err := amount.validate(); err != nil {
		return nil, nil, err
	}
	if existing, ok := s.settlements[settlementID]; ok {
		if existing.MerchantID != merchantID || existing.Amount != amount {
			return nil, nil, fmt.Errorf("%w: settlement %s", ErrConflict, settlementID)
		}
		batch := findBatchBySettlement(st, settlementID)
		return existing, batch, nil
	}
	rule := st.effectiveRule(confirmedAt)
	if rule == nil {
		return nil, nil, fmt.Errorf("%w: merchant %s", ErrRuleNotEffective, merchantID)
	}
	settlement := &Settlement{ID: settlementID, MerchantID: merchantID, Amount: amount, ConfirmedAt: confirmedAt}
	s.settlements[settlementID] = settlement

	withheld := amount * Money(rule.RateBP) / 10000
	s.nextBatchSeq++
	batch := &ReserveBatch{
		ID:           fmt.Sprintf("RB-%d", s.nextBatchSeq),
		MerchantID:   merchantID,
		SettlementID: settlementID,
		Amount:       withheld,
		ReleaseDate:  confirmedAt.AddDate(0, 0, rule.HoldDays),
		RuleID:       rule.ID,
		RuleVersion:  rule.Version,
		CreatedAt:    s.now(),
	}
	st.batches[batch.ID] = batch
	cp := *batch
	return settlement, &cp, nil
}

func findBatchBySettlement(st *merchantState, settlementID string) *ReserveBatch {
	for _, b := range st.batches {
		if b.SettlementID == settlementID {
			cp := *b
			return &cp
		}
	}
	return nil
}

// ScanReleases releases every due batch (ReleaseDate <= asOf) into the
// merchant's available balance, one batch at a time. Only the unoccupied
// remainder (Amount - Deducted - Released) is released. Idempotent by run
// ID: same ID and asOf returns the original result; a different asOf
// conflicts.
func (s *Service) ScanReleases(merchantID, runID string, asOf time.Time) (*ReleaseRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, err
	}
	if existing, ok := s.releaseRuns[runID]; ok {
		if existing.MerchantID != merchantID || !existing.AsOf.Equal(asOf) {
			return nil, fmt.Errorf("%w: release run %s", ErrConflict, runID)
		}
		cp := *existing
		return &cp, nil
	}
	due := dueBatches(st, asOf)
	run := &ReleaseRun{ID: runID, MerchantID: merchantID, AsOf: asOf, CreatedAt: s.now()}
	for _, b := range due {
		releasable := b.Remaining()
		if releasable <= 0 {
			continue
		}
		b.Released += releasable
		b.Version++
		st.available += releasable
		run.Released += releasable
		run.BatchIDs = append(run.BatchIDs, b.ID)
	}
	s.releaseRuns[runID] = run
	cp := *run
	return &cp, nil
}

func dueBatches(st *merchantState, asOf time.Time) []*ReserveBatch {
	var out []*ReserveBatch
	for _, b := range st.batches {
		if !b.ReleaseDate.After(asOf) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReleaseDate.Equal(out[j].ReleaseDate) {
			return out[i].ReleaseDate.Before(out[j].ReleaseDate)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Deduct occupies reserve for a risk deduction, taking from the batches with
// the earliest release date first. A batch's cumulative Deducted + Released
// never exceeds its original Amount. Idempotent by deduction ID: same ID,
// amount and reason returns the original record; any change conflicts.
func (s *Service) Deduct(merchantID, deductionID string, amount Money, reason string) (*RiskDeduction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, err
	}
	if err := amount.validate(); err != nil {
		return nil, err
	}
	if existing, ok := s.deductions[deductionID]; ok {
		if existing.MerchantID != merchantID || existing.Amount != amount || existing.Reason != reason {
			return nil, fmt.Errorf("%w: deduction %s", ErrConflict, deductionID)
		}
		cp := *existing
		return &cp, nil
	}
	batches := sortedBatches(st)
	var totalFree Money
	for _, b := range batches {
		totalFree += b.Remaining()
	}
	if totalFree < amount {
		return nil, fmt.Errorf("%w: insufficient reserve, short %d", ErrInvalidAmount, amount-totalFree)
	}
	remaining := amount
	d := &RiskDeduction{ID: deductionID, MerchantID: merchantID, Amount: amount, Reason: reason, CreatedAt: s.now()}
	for _, b := range batches {
		if remaining == 0 {
			break
		}
		free := b.Remaining()
		if free <= 0 {
			continue
		}
		take := free
		if take > remaining {
			take = remaining
		}
		b.Deducted += take
		b.Version++
		d.Allocs = append(d.Allocs, DeductionAlloc{BatchID: b.ID, Amount: take})
		remaining -= take
	}
	s.deductions[deductionID] = d
	cp := *d
	return &cp, nil
}

func sortedBatches(st *merchantState) []*ReserveBatch {
	out := make([]*ReserveBatch, 0, len(st.batches))
	for _, b := range st.batches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReleaseDate.Equal(out[j].ReleaseDate) {
			return out[i].ReleaseDate.Before(out[j].ReleaseDate)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Compensate credits the merchant's available balance back for a wrong
// deduction. The original deduction record is never modified; the
// compensation is a separate correcting record. Idempotent by compensation
// ID: same ID, deduction, amount and reason returns the original record;
// any change conflicts.
func (s *Service) Compensate(merchantID, compensationID, deductionID string, amount Money, reason string) (*Compensation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, err
	}
	if err := amount.validate(); err != nil {
		return nil, err
	}
	if existing, ok := s.compensations[compensationID]; ok {
		if existing.MerchantID != merchantID || existing.DeductionID != deductionID ||
			existing.Amount != amount || existing.Reason != reason {
			return nil, fmt.Errorf("%w: compensation %s", ErrConflict, compensationID)
		}
		cp := *existing
		return &cp, nil
	}
	d, ok := s.deductions[deductionID]
	if !ok || d.MerchantID != merchantID {
		return nil, fmt.Errorf("%w: deduction %s", ErrNotFound, deductionID)
	}
	if amount > d.Amount {
		return nil, fmt.Errorf("%w: compensation exceeds deduction", ErrInvalidAmount)
	}
	c := &Compensation{
		ID:          compensationID,
		MerchantID:  merchantID,
		DeductionID: deductionID,
		Amount:      amount,
		Reason:      reason,
		CreatedAt:   s.now(),
	}
	s.compensations[compensationID] = c
	st.available += amount
	cp := *c
	return &cp, nil
}

// Balance returns the merchant's available balance and the remaining
// composition of every reserve batch.
func (s *Service) Balance(merchantID string) (*BalanceView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.merchant(merchantID)
	if err != nil {
		return nil, err
	}
	view := &BalanceView{MerchantID: merchantID, Available: st.available}
	for _, b := range sortedBatches(st) {
		view.Batches = append(view.Batches, BatchView{
			BatchID:      b.ID,
			SettlementID: b.SettlementID,
			Amount:       b.Amount,
			Deducted:     b.Deducted,
			Released:     b.Released,
			Remaining:    b.Remaining(),
			ReleaseDate:  b.ReleaseDate,
			RuleVersion:  b.RuleVersion,
		})
	}
	return view, nil
}

func (s *Service) merchant(id string) (*merchantState, error) {
	st, ok := s.merchants[id]
	if !ok {
		return nil, fmt.Errorf("%w: merchant %s", ErrNotFound, id)
	}
	return st, nil
}
