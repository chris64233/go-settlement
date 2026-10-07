package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrReceiptNotFound   = errors.New("settlement: receipt not found")
	ErrReceiptConflict   = errors.New("settlement: receipt conflicts with existing bank serial")
	ErrClaimNotFound     = errors.New("settlement: claim not found")
	ErrStaleVersion      = errors.New("settlement: stale receipt version")
	ErrOverAllocation    = errors.New("settlement: allocation exceeds receipt amount")
	ErrInvalidAmount     = errors.New("settlement: invalid amount")
	ErrClaimNotPending   = errors.New("settlement: claim is not pending")
	ErrClaimNotConfirmed = errors.New("settlement: claim is not confirmed")
	ErrReversalExceeded  = errors.New("settlement: reversal exceeds confirmed claim amount")
)

// Allocation 指定一笔资金的去向：收款对象与金额（最小货币单位）。
type Allocation struct {
	Payee  string
	Amount int64
}

// Receipt 是进入暂收账户的一笔来账。
type Receipt struct {
	SerialNo   string    // 银行流水号，全局唯一
	Payer      string    // 付款方
	Currency   string    // 币种
	Amount     int64     // 金额（最小货币单位）
	ReceivedAt time.Time // 到账时间
	Memo       string    // 原始摘要

	Version   int64 // 每次资金变动递增
	Confirmed int64 // 已确认入账金额
	Pending   int64 // 处理中（待确认）金额
}

// Available 返回仍可认领的金额。
func (r *Receipt) Available() int64 { return r.Amount - r.Confirmed - r.Pending }

// ClaimStatus 认领申请状态。
type ClaimStatus string

const (
	ClaimPending   ClaimStatus = "PENDING"   // 处理中
	ClaimConfirmed ClaimStatus = "CONFIRMED" // 已确认入账
	ClaimWithdrawn ClaimStatus = "WITHDRAWN" // 已撤回
	ClaimFailed    ClaimStatus = "FAILED"    // 确认失败，保留原因
)

// Claim 是一份认领申请，固定提交时的来账版本。
type Claim struct {
	ID        string
	SerialNo  string
	Version   int64 // 提交时锁定的来账版本
	Items     []Allocation
	Status    ClaimStatus
	Reason    string // 失败原因
	Reversed  int64  // 已被冲正的金额
	CreatedAt time.Time
}

// Total 返回申请分配总额。
func (c *Claim) Total() int64 {
	var sum int64
	for _, it := range c.Items {
		sum += it.Amount
	}
	return sum
}

// Reversible 返回该认领仍可冲正的金额。
func (c *Claim) Reversible() int64 { return c.Total() - c.Reversed }

// Reversal 是独立的冲正记录，用于恢复暂收余额。
type Reversal struct {
	ID        string
	ClaimID   string
	SerialNo  string
	Amount    int64
	Reason    string
	CreatedAt time.Time
}

// LedgerType 资金台账类型。
type LedgerType string

const (
	LedgerClaim    LedgerType = "CLAIM"    // 认领入账
	LedgerReversal LedgerType = "REVERSAL" // 冲正回退
)

// LedgerEntry 资金台账，可追溯到银行流水号。
type LedgerEntry struct {
	SerialNo   string
	Type       LedgerType
	ClaimID    string
	ReversalID string
	Payee      string
	Amount     int64 // 认领为正，冲正为负
	CreatedAt  time.Time
}

// Store 是暂收款认领的并发安全存储。
type Store struct {
	mu        sync.Mutex
	receipts  map[string]*Receipt
	claims    map[string]*Claim
	reversals map[string]*Reversal
	ledger    []LedgerEntry
	now       func() time.Time
}

func NewStore() *Store {
	return &Store{
		receipts:  make(map[string]*Receipt),
		claims:    make(map[string]*Claim),
		reversals: make(map[string]*Reversal),
		now:       time.Now,
	}
}

// ImportReceipt 导入来账。流水号重复且关键字段一致时返回原记录；
// 金额、币种或付款方不一致时返回冲突错误。
func (s *Store) ImportReceipt(r Receipt) (*Receipt, error) {
	if r.SerialNo == "" || r.Amount <= 0 || r.Currency == "" || r.Payer == "" {
		return nil, fmt.Errorf("%w: serial/payer/currency required and amount must be positive", ErrInvalidAmount)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.receipts[r.SerialNo]; ok {
		if existing.Amount != r.Amount || existing.Currency != r.Currency || existing.Payer != r.Payer {
			return nil, fmt.Errorf("%w: serial %s", ErrReceiptConflict, r.SerialNo)
		}
		return existing, nil
	}
	cp := r
	s.receipts[r.SerialNo] = &cp
	return &cp, nil
}

// SubmitClaim 提交认领申请。version 必须是操作员观察到的来账当前版本，
// 否则视为旧版本申请并拒绝。累计已入账与处理中金额不得超过来账金额。
func (s *Store) SubmitClaim(id, serialNo string, version int64, items []Allocation) (*Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[serialNo]
	if !ok {
		return nil, ErrReceiptNotFound
	}
	if _, dup := s.claims[id]; dup {
		return nil, fmt.Errorf("settlement: claim %s already exists", id)
	}
	if r.Version != version {
		return nil, fmt.Errorf("%w: want %d got %d", ErrStaleVersion, r.Version, version)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: claim must contain at least one allocation", ErrInvalidAmount)
	}
	var sum int64
	for _, it := range items {
		if it.Payee == "" || it.Amount <= 0 {
			return nil, fmt.Errorf("%w: payee required and amount must be positive", ErrInvalidAmount)
		}
		sum += it.Amount
	}
	if sum > r.Available() {
		return nil, fmt.Errorf("%w: available %d, requested %d", ErrOverAllocation, r.Available(), sum)
	}
	c := &Claim{
		ID:        id,
		SerialNo:  serialNo,
		Version:   version,
		Items:     append([]Allocation(nil), items...),
		Status:    ClaimPending,
		CreatedAt: s.now(),
	}
	s.claims[id] = c
	r.Pending += sum
	r.Version++
	return c, nil
}

// ConfirmClaim 确认认领：一份申请中的全部分配要么全部入账，要么全部不生效。
// 申请锁定的版本与来账当前版本不一致时确认失败，保留原因且不生成台账。
func (s *Store) ConfirmClaim(id string) (*Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[id]
	if !ok {
		return nil, ErrClaimNotFound
	}
	if c.Status != ClaimPending {
		return nil, ErrClaimNotPending
	}
	r := s.receipts[c.SerialNo]
	fail := func(reason string) (*Claim, error) {
		r.Pending -= c.Total()
		r.Version++
		c.Status = ClaimFailed
		c.Reason = reason
		return c, errors.New(reason)
	}
	if r.Version != c.Version+1 {
		return fail(fmt.Sprintf("stale version: claim pinned %d, receipt now %d", c.Version, r.Version))
	}
	if c.Total() > r.Amount-r.Confirmed {
		return fail(fmt.Sprintf("insufficient remaining amount: %d < %d", r.Amount-r.Confirmed, c.Total()))
	}
	r.Pending -= c.Total()
	r.Confirmed += c.Total()
	r.Version++
	c.Status = ClaimConfirmed
	now := s.now()
	for _, it := range c.Items {
		s.ledger = append(s.ledger, LedgerEntry{
			SerialNo:  c.SerialNo,
			Type:      LedgerClaim,
			ClaimID:   c.ID,
			Payee:     it.Payee,
			Amount:    it.Amount,
			CreatedAt: now,
		})
	}
	return c, nil
}

// WithdrawClaim 撤回处理中的认领，释放占用金额。
func (s *Store) WithdrawClaim(id string) (*Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[id]
	if !ok {
		return nil, ErrClaimNotFound
	}
	if c.Status != ClaimPending {
		return nil, ErrClaimNotPending
	}
	r := s.receipts[c.SerialNo]
	r.Pending -= c.Total()
	r.Version++
	c.Status = ClaimWithdrawn
	return c, nil
}

// Reverse 对已确认的认领做冲正，恢复暂收余额。冲正金额不得超过
// 该认领剩余可冲正金额；相同冲正号重复提交返回原记录，不重复加回余额。
func (s *Store) Reverse(id, claimID string, amount int64, reason string) (*Reversal, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: reversal amount must be positive", ErrInvalidAmount)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.reversals[id]; ok {
		return existing, nil
	}
	c, ok := s.claims[claimID]
	if !ok {
		return nil, ErrClaimNotFound
	}
	if c.Status != ClaimConfirmed {
		return nil, ErrClaimNotConfirmed
	}
	if amount > c.Reversible() {
		return nil, fmt.Errorf("%w: reversible %d, requested %d", ErrReversalExceeded, c.Reversible(), amount)
	}
	r := s.receipts[c.SerialNo]
	c.Reversed += amount
	r.Confirmed -= amount
	r.Version++
	rev := &Reversal{
		ID:        id,
		ClaimID:   claimID,
		SerialNo:  c.SerialNo,
		Amount:    amount,
		Reason:    reason,
		CreatedAt: s.now(),
	}
	s.reversals[id] = rev
	s.ledger = append(s.ledger, LedgerEntry{
		SerialNo:   c.SerialNo,
		Type:       LedgerReversal,
		ClaimID:    claimID,
		ReversalID: id,
		Amount:     -amount,
		CreatedAt:  rev.CreatedAt,
	})
	return rev, nil
}

// Balance 来账余额与认领进度。
type Balance struct {
	SerialNo  string
	Amount    int64
	Confirmed int64
	Pending   int64
	Available int64
	Version   int64
}

func (s *Store) GetBalance(serialNo string) (Balance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[serialNo]
	if !ok {
		return Balance{}, ErrReceiptNotFound
	}
	return Balance{
		SerialNo:  r.SerialNo,
		Amount:    r.Amount,
		Confirmed: r.Confirmed,
		Pending:   r.Pending,
		Available: r.Available(),
		Version:   r.Version,
	}, nil
}

// GetClaim 查询认领申请及其进度。
func (s *Store) GetClaim(id string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[id]
	if !ok {
		return Claim{}, ErrClaimNotFound
	}
	cp := *c
	cp.Items = append([]Allocation(nil), c.Items...)
	return cp, nil
}

// ListClaims 按来账查询全部认领申请（含分配明细），按创建时间排序。
func (s *Store) ListClaims(serialNo string) []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Claim
	for _, c := range s.claims {
		if c.SerialNo == serialNo {
			cp := *c
			cp.Items = append([]Allocation(nil), c.Items...)
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ListReversals 按来账查询冲正记录。
func (s *Store) ListReversals(serialNo string) []Reversal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Reversal
	for _, rev := range s.reversals {
		if rev.SerialNo == serialNo {
			out = append(out, *rev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ListLedger 查询资金去向台账，每条记录均可追溯到银行流水号。
func (s *Store) ListLedger(serialNo string) []LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LedgerEntry
	for _, e := range s.ledger {
		if e.SerialNo == serialNo {
			out = append(out, e)
		}
	}
	return out
}
