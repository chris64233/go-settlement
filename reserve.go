package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Money 以最小货币单位（分）表示金额。
type Money = int64

var (
	ErrNotFound     = errors.New("settlement: not found")
	ErrConflict     = errors.New("settlement: idempotency conflict")
	ErrInsufficient = errors.New("settlement: insufficient reservable amount")
	ErrInvalid      = errors.New("settlement: invalid argument")
)

// Merchant 商户及其可用余额。保证金释放与风险扣款补回共享该余额。
type Merchant struct {
	ID               string
	AvailableBalance Money
}

// ReserveRule 保证金规则。每次调整生成新版本，已形成的批次不受后续调整影响。
type ReserveRule struct {
	MerchantID    string
	Version       int
	RateBP        int // 暂扣比例，万分之一（basis points）
	HoldDays      int // 持有天数
	EffectiveFrom time.Time
}

// ReserveBatch 保证金批次：登记结算款时按当时生效的规则生成，字段固定不变。
type ReserveBatch struct {
	ID           string
	MerchantID   string
	SettlementID string
	Withheld     Money // 原暂扣金额
	Occupied     Money // 累计占用（风险扣款，扣除补回）
	Released     Money // 累计释放
	Frozen       Money // 人工冻结中
	ReleaseDate  time.Time
	RuleVersion  int
	Version      int64 // 批次版本，所有变更基于同一版本递增
}

// Remaining 批次剩余可动用构成：未被占用、释放、冻结的部分。
func (b *ReserveBatch) Remaining() Money {
	return b.Withheld - b.Occupied - b.Released - b.Frozen
}

// BatchAllocation 风险扣款在某个批次上的占用明细。
type BatchAllocation struct {
	BatchID string
	Amount  Money
}

// Deduction 风险扣款记录。扣款号幂等，记录一旦写入不修改。
type Deduction struct {
	DeductionNo string
	MerchantID  string
	Amount      Money
	Reason      string
	Allocations []BatchAllocation
	Compensated Money
	CreatedAt   time.Time
}

// Compensation 补回记录：修正错误扣款的唯一方式，原扣款记录保持不变。
type Compensation struct {
	CompensationNo string
	DeductionNo    string
	MerchantID     string
	Amount         Money
	Reason         string
	CreatedAt      time.Time
}

// ReleaseEntry 单笔批次的释放明细。
type ReleaseEntry struct {
	BatchID string
	Amount  Money
}

// ReleaseResult 一次释放扫描的结果，按释放批次号幂等保存。
type ReleaseResult struct {
	ReleaseNo string
	Entries   []ReleaseEntry
	Total     Money
	ScannedAt time.Time
}

// BatchComposition 商户余额查询中每笔保证金的剩余构成。
type BatchComposition struct {
	BatchID      string
	SettlementID string
	Withheld     Money
	Occupied     Money
	Released     Money
	Frozen       Money
	Remaining    Money
	ReleaseDate  time.Time
	RuleVersion  int
}

// MerchantBalance 商户可用余额与全部保证金批次构成。
type MerchantBalance struct {
	MerchantID       string
	AvailableBalance Money
	Batches          []BatchComposition
}

// Service 滚动保证金管理服务。所有变更在内部串行化，
// 并以批次版本（ReserveBatch.Version）保证并发下的一致性。
type Service struct {
	mu            sync.Mutex
	merchants     map[string]*Merchant
	rules         map[string][]ReserveRule // merchantID -> 按版本升序
	settlements   map[string]string        // settlementID -> batchID（登记幂等）
	batches       map[string]*ReserveBatch
	deductions    map[string]*Deduction
	compensations map[string]*Compensation
	releases      map[string]*ReleaseResult
}

func NewService() *Service {
	return &Service{
		merchants:     make(map[string]*Merchant),
		rules:         make(map[string][]ReserveRule),
		settlements:   make(map[string]string),
		batches:       make(map[string]*ReserveBatch),
		deductions:    make(map[string]*Deduction),
		compensations: make(map[string]*Compensation),
		releases:      make(map[string]*ReleaseResult),
	}
}

// RegisterMerchant 注册商户。
func (s *Service) RegisterMerchant(id string) error {
	if id == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.merchants[id]; ok {
		return nil
	}
	s.merchants[id] = &Merchant{ID: id}
	return nil
}

// SetReserveRule 为商户发布新版本的保证金规则。
func (s *Service) SetReserveRule(merchantID string, rateBP, holdDays int, effectiveFrom time.Time) (ReserveRule, error) {
	if rateBP < 0 || rateBP > 10000 || holdDays < 0 {
		return ReserveRule{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.merchants[merchantID]; !ok {
		return ReserveRule{}, ErrNotFound
	}
	versions := s.rules[merchantID]
	for _, r := range versions {
		if r.EffectiveFrom.Equal(effectiveFrom) {
			return ReserveRule{}, fmt.Errorf("%w: duplicate effective time", ErrInvalid)
		}
	}
	rule := ReserveRule{
		MerchantID:    merchantID,
		Version:       len(versions) + 1,
		RateBP:        rateBP,
		HoldDays:      holdDays,
		EffectiveFrom: effectiveFrom,
	}
	s.rules[merchantID] = append(versions, rule)
	return rule, nil
}

// ruleAt 返回 at 时刻生效的规则版本。
func (s *Service) ruleAt(merchantID string, at time.Time) (ReserveRule, bool) {
	var best *ReserveRule
	for i := range s.rules[merchantID] {
		r := &s.rules[merchantID][i]
		if !r.EffectiveFrom.After(at) && (best == nil || r.EffectiveFrom.After(best.EffectiveFrom)) {
			best = r
		}
	}
	if best == nil {
		return ReserveRule{}, false
	}
	return *best, true
}

// RegisterSettlement 登记已确认结算款，按当时生效的规则生成保证金批次。
// 同一结算款重复登记返回原批次，不重复暂扣。
func (s *Service) RegisterSettlement(settlementID, merchantID string, amount Money, confirmedAt time.Time) (ReserveBatch, error) {
	if settlementID == "" || amount <= 0 {
		return ReserveBatch{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.merchants[merchantID]; !ok {
		return ReserveBatch{}, ErrNotFound
	}
	if batchID, ok := s.settlements[settlementID]; ok {
		return *s.batches[batchID], nil
	}
	rule, ok := s.ruleAt(merchantID, confirmedAt)
	if !ok {
		return ReserveBatch{}, fmt.Errorf("%w: no effective reserve rule", ErrInvalid)
	}
	batch := &ReserveBatch{
		ID:           fmt.Sprintf("RB-%s", settlementID),
		MerchantID:   merchantID,
		SettlementID: settlementID,
		Withheld:     amount * int64(rule.RateBP) / 10000,
		ReleaseDate:  confirmedAt.AddDate(0, 0, rule.HoldDays),
		RuleVersion:  rule.Version,
		Version:      1,
	}
	s.batches[batch.ID] = batch
	s.settlements[settlementID] = batch.ID
	return *batch, nil
}

// Deduct 风险扣款：按最早到期优先占用保证金。
// 扣款号幂等：同号同内容返回原结果，金额或原因变化返回 ErrConflict。
func (s *Service) Deduct(deductionNo, merchantID string, amount Money, reason string) (Deduction, error) {
	if deductionNo == "" || amount <= 0 {
		return Deduction{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.merchants[merchantID]; !ok {
		return Deduction{}, ErrNotFound
	}
	if existing, ok := s.deductions[deductionNo]; ok {
		if existing.MerchantID == merchantID && existing.Amount == amount && existing.Reason == reason {
			return *existing, nil
		}
		return Deduction{}, fmt.Errorf("%w: deduction %s", ErrConflict, deductionNo)
	}

	batches := s.occupiableBatches(merchantID)
	var total Money
	for _, b := range batches {
		total += b.Remaining()
	}
	if total < amount {
		return Deduction{}, ErrInsufficient
	}

	d := &Deduction{
		DeductionNo: deductionNo,
		MerchantID:  merchantID,
		Amount:      amount,
		Reason:      reason,
		CreatedAt:   time.Now(),
	}
	need := amount
	for _, b := range batches {
		if need == 0 {
			break
		}
		take := min(need, b.Remaining())
		b.Occupied += take
		b.Version++
		d.Allocations = append(d.Allocations, BatchAllocation{BatchID: b.ID, Amount: take})
		need -= take
	}
	s.deductions[deductionNo] = d
	return *d, nil
}

// occupiableBatches 按最早到期优先返回商户的保证金批次。
func (s *Service) occupiableBatches(merchantID string) []*ReserveBatch {
	var out []*ReserveBatch
	for _, b := range s.batches {
		if b.MerchantID == merchantID {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReleaseDate.Equal(out[j].ReleaseDate) {
			return out[i].ID < out[j].ID
		}
		return out[i].ReleaseDate.Before(out[j].ReleaseDate)
	})
	return out
}

// Compensate 补回错误扣款：生成补回记录并返还商户可用余额，
// 原扣款记录（金额与占用明细）保持不变。补回号幂等。
func (s *Service) Compensate(compensationNo, deductionNo string, amount Money, reason string) (Compensation, error) {
	if compensationNo == "" || amount <= 0 {
		return Compensation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.compensations[compensationNo]; ok {
		if existing.DeductionNo == deductionNo && existing.Amount == amount && existing.Reason == reason {
			return *existing, nil
		}
		return Compensation{}, fmt.Errorf("%w: compensation %s", ErrConflict, compensationNo)
	}
	d, ok := s.deductions[deductionNo]
	if !ok {
		return Compensation{}, ErrNotFound
	}
	if d.Amount-d.Compensated < amount {
		return Compensation{}, ErrInsufficient
	}

	// 按扣款占用明细的逆序回补批次占用。
	need := amount
	for i := len(d.Allocations) - 1; i >= 0 && need > 0; i-- {
		b := s.batches[d.Allocations[i].BatchID]
		back := min(need, b.Occupied)
		b.Occupied -= back
		b.Version++
		need -= back
	}
	d.Compensated += amount
	s.merchants[d.MerchantID].AvailableBalance += amount

	c := &Compensation{
		CompensationNo: compensationNo,
		DeductionNo:    deductionNo,
		MerchantID:     d.MerchantID,
		Amount:         amount,
		Reason:         reason,
		CreatedAt:      time.Now(),
	}
	s.compensations[compensationNo] = c
	return *c, nil
}

// ReleaseDue 释放扫描：对到期批次逐笔释放未被占用/冻结的金额，
// 进入商户可用余额。释放批次号幂等：同号返回原结果。
func (s *Service) ReleaseDue(releaseNo string, now time.Time) (ReleaseResult, error) {
	if releaseNo == "" {
		return ReleaseResult{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.releases[releaseNo]; ok {
		return *existing, nil
	}

	var batchList []*ReserveBatch
	for _, b := range s.batches {
		if !b.ReleaseDate.After(now) {
			batchList = append(batchList, b)
		}
	}
	sort.Slice(batchList, func(i, j int) bool { return batchList[i].ID < batchList[j].ID })

	result := &ReleaseResult{ReleaseNo: releaseNo, ScannedAt: now}
	for _, b := range batchList {
		releasable := b.Remaining()
		if releasable <= 0 {
			continue
		}
		b.Released += releasable
		b.Version++
		s.merchants[b.MerchantID].AvailableBalance += releasable
		result.Entries = append(result.Entries, ReleaseEntry{BatchID: b.ID, Amount: releasable})
		result.Total += releasable
	}
	s.releases[releaseNo] = result
	return *result, nil
}

// Freeze 人工冻结批次部分金额，冻结部分不参与释放与扣款占用。
func (s *Service) Freeze(batchID string, amount Money) error {
	return s.adjustFrozen(batchID, amount)
}

// Unfreeze 解除人工冻结。
func (s *Service) Unfreeze(batchID string, amount Money) error {
	return s.adjustFrozen(batchID, -amount)
}

func (s *Service) adjustFrozen(batchID string, delta Money) error {
	if delta == 0 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return ErrNotFound
	}
	if delta > 0 && b.Remaining() < delta {
		return ErrInsufficient
	}
	if delta < 0 && b.Frozen < -delta {
		return ErrInsufficient
	}
	b.Frozen += delta
	b.Version++
	return nil
}

// Balance 查询商户可用余额及每笔保证金的剩余构成。
func (s *Service) Balance(merchantID string) (MerchantBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.merchants[merchantID]
	if !ok {
		return MerchantBalance{}, ErrNotFound
	}
	mb := MerchantBalance{MerchantID: merchantID, AvailableBalance: m.AvailableBalance}
	for _, b := range s.occupiableBatches(merchantID) {
		mb.Batches = append(mb.Batches, BatchComposition{
			BatchID:      b.ID,
			SettlementID: b.SettlementID,
			Withheld:     b.Withheld,
			Occupied:     b.Occupied,
			Released:     b.Released,
			Frozen:       b.Frozen,
			Remaining:    b.Remaining(),
			ReleaseDate:  b.ReleaseDate,
			RuleVersion:  b.RuleVersion,
		})
	}
	return mb, nil
}

// DeductionOf 查询扣款记录（只读副本）。
func (s *Service) DeductionOf(deductionNo string) (Deduction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deductions[deductionNo]
	if !ok {
		return Deduction{}, ErrNotFound
	}
	return *d, nil
}
