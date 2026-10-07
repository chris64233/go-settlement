package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 金额一律使用最小货币单位（整数）表示，避免浮点误差。

var (
	ErrReceiptNotFound   = errors.New("receipt not found")
	ErrClaimNotFound     = errors.New("claim not found")
	ErrConflict          = errors.New("receipt conflicts with existing bank serial")
	ErrVersionConflict   = errors.New("stale receipt version")
	ErrOverAllocation    = errors.New("cumulative allocated amount exceeds receipt amount")
	ErrInvalidAmount     = errors.New("amount must be positive")
	ErrEmptyAllocation   = errors.New("claim must contain at least one allocation")
	ErrClaimNotPending   = errors.New("claim is not pending")
	ErrClaimNotConfirmed = errors.New("claim is not confirmed")
	ErrOverReversal      = errors.New("reversal amount exceeds remaining confirmed amount")
)

// Receipt 是一笔进入暂收账户的银行来账。
type Receipt struct {
	ID           string
	BankSerialNo string // 银行流水号，全局唯一
	Payer        string
	Currency     string
	Amount       int64
	ReceivedAt   time.Time
	Narrative    string // 原始摘要
	Version      int64  // 每次资金状态变化递增
}

// ClaimStatus 认领申请状态。
type ClaimStatus string

const (
	ClaimPending   ClaimStatus = "pending"
	ClaimConfirmed ClaimStatus = "confirmed"
	ClaimWithdrawn ClaimStatus = "withdrawn"
	ClaimFailed    ClaimStatus = "failed"
)

// Allocation 是认领申请中针对一个收款对象的分配。
type Allocation struct {
	Payee  string
	Amount int64
}

// Claim 是一份认领申请，固定创建时的来账版本。
type Claim struct {
	ID             string
	ReceiptID      string
	ReceiptVersion int64
	Allocations    []Allocation
	Status         ClaimStatus
	Reason         string // 失败原因
	ReversedAmount int64  // 已冲正金额
	CreatedAt      time.Time
}

// Reversal 是一条独立的冲正记录，用于恢复暂收余额。
type Reversal struct {
	ID             string
	IdempotencyKey string
	ClaimID        string
	ReceiptID      string
	Amount         int64
	CreatedAt      time.Time
}

// LedgerEntryType 资金台账类型。
type LedgerEntryType string

const (
	LedgerClaimOut   LedgerEntryType = "claim_out"   // 暂收 -> 收款对象
	LedgerReversalIn LedgerEntryType = "reversal_in" // 收款对象 -> 暂收
)

// LedgerEntry 资金台账，可通过 BankSerialNo 追溯原银行流水。
type LedgerEntry struct {
	ID           string
	Type         LedgerEntryType
	BankSerialNo string
	ReceiptID    string
	ClaimID      string
	ReversalID   string
	Payee        string
	Currency     string
	Amount       int64
	CreatedAt    time.Time
}

// Store 是暂收款认领的并发安全存储。
type Store struct {
	mu            sync.Mutex
	receipts      map[string]*Receipt
	bySerial      map[string]string
	claims        map[string]*Claim
	reversals     map[string]*Reversal
	reversalByKey map[string]string
	ledger        []*LedgerEntry
	seq           int64
	now           func() time.Time
}

func NewStore() *Store {
	return &Store{
		receipts:      make(map[string]*Receipt),
		bySerial:      make(map[string]string),
		claims:        make(map[string]*Claim),
		reversals:     make(map[string]*Reversal),
		reversalByKey: make(map[string]string),
		now:           time.Now,
	}
}

func (s *Store) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// ImportReceipt 导入来账。银行流水号全局唯一：
// 完全相同的来账重复导入返回原结果；金额、币种或付款方不同返回冲突。
func (s *Store) ImportReceipt(serialNo, payer, currency string, amount int64, receivedAt time.Time, narrative string) (*Receipt, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.bySerial[serialNo]; ok {
		existing := s.receipts[id]
		if existing.Payer != payer || existing.Currency != currency || existing.Amount != amount {
			return nil, fmt.Errorf("%w: %s", ErrConflict, serialNo)
		}
		cp := *existing
		return &cp, nil
	}
	r := &Receipt{
		ID:           s.nextID("rcpt"),
		BankSerialNo: serialNo,
		Payer:        payer,
		Currency:     currency,
		Amount:       amount,
		ReceivedAt:   receivedAt,
		Narrative:    narrative,
		Version:      1,
	}
	s.receipts[r.ID] = r
	s.bySerial[serialNo] = r.ID
	cp := *r
	return &cp, nil
}

// SubmitClaim 提交认领申请。申请固定来账版本，多项分配全部成功或全部不生效。
// 校验失败时保留一条 failed 申请及原因，但不生成资金台账。
func (s *Store) SubmitClaim(receiptID string, expectedVersion int64, allocations []Allocation) (*Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[receiptID]
	if !ok {
		return nil, ErrReceiptNotFound
	}
	claim := &Claim{
		ID:             s.nextID("claim"),
		ReceiptID:      receiptID,
		ReceiptVersion: expectedVersion,
		Allocations:    append([]Allocation(nil), allocations...),
		CreatedAt:      s.now(),
	}
	fail := func(reason error) (*Claim, error) {
		claim.Status = ClaimFailed
		claim.Reason = reason.Error()
		s.claims[claim.ID] = claim
		return claim, reason
	}
	if r.Version != expectedVersion {
		return fail(ErrVersionConflict)
	}
	if len(allocations) == 0 {
		return fail(ErrEmptyAllocation)
	}
	var total int64
	for _, a := range allocations {
		if a.Amount <= 0 {
			return fail(ErrInvalidAmount)
		}
		total += a.Amount
	}
	if s.reservedLocked(receiptID)+total > r.Amount {
		return fail(ErrOverAllocation)
	}
	claim.Status = ClaimPending
	s.claims[claim.ID] = claim
	r.Version++
	return claim, nil
}

// reservedLocked 返回已入账 + 处理中的金额（冲正会释放已入账部分）。
func (s *Store) reservedLocked(receiptID string) int64 {
	var sum int64
	for _, c := range s.claims {
		if c.ReceiptID != receiptID {
			continue
		}
		switch c.Status {
		case ClaimPending:
			for _, a := range c.Allocations {
				sum += a.Amount
			}
		case ClaimConfirmed:
			for _, a := range c.Allocations {
				sum += a.Amount
			}
			sum -= c.ReversedAmount
		}
	}
	return sum
}

// ConfirmClaim 确认认领，生成资金台账。携带的版本必须是最新版本，
// 否则视为旧版本申请，不得覆盖较新的剩余金额。
func (s *Store) ConfirmClaim(claimID string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[claimID]
	if !ok {
		return ErrClaimNotFound
	}
	r := s.receipts[c.ReceiptID]
	if r.Version != expectedVersion {
		return ErrVersionConflict
	}
	if c.Status != ClaimPending {
		return ErrClaimNotPending
	}
	c.Status = ClaimConfirmed
	for _, a := range c.Allocations {
		s.ledger = append(s.ledger, &LedgerEntry{
			ID:           s.nextID("ledger"),
			Type:         LedgerClaimOut,
			BankSerialNo: r.BankSerialNo,
			ReceiptID:    r.ID,
			ClaimID:      c.ID,
			Payee:        a.Payee,
			Currency:     r.Currency,
			Amount:       a.Amount,
			CreatedAt:    s.now(),
		})
	}
	r.Version++
	return nil
}

// WithdrawClaim 撤回处理中的认领，释放占用的暂收余额。
func (s *Store) WithdrawClaim(claimID string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[claimID]
	if !ok {
		return ErrClaimNotFound
	}
	r := s.receipts[c.ReceiptID]
	if r.Version != expectedVersion {
		return ErrVersionConflict
	}
	if c.Status != ClaimPending {
		return ErrClaimNotPending
	}
	c.Status = ClaimWithdrawn
	r.Version++
	return nil
}

// ReverseClaim 对已确认的认领做冲正，恢复暂收余额。
// 冲正金额不能超过该认领剩余可冲正金额；idempotencyKey 保证重复冲正不重复加回余额。
func (s *Store) ReverseClaim(claimID string, amount int64, idempotencyKey string) (*Reversal, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.reversalByKey[idempotencyKey]; ok {
		return s.reversals[id], nil
	}
	c, ok := s.claims[claimID]
	if !ok {
		return nil, ErrClaimNotFound
	}
	if c.Status != ClaimConfirmed {
		return nil, ErrClaimNotConfirmed
	}
	var confirmedTotal int64
	for _, a := range c.Allocations {
		confirmedTotal += a.Amount
	}
	if c.ReversedAmount+amount > confirmedTotal {
		return nil, ErrOverReversal
	}
	r := s.receipts[c.ReceiptID]
	rev := &Reversal{
		ID:             s.nextID("rev"),
		IdempotencyKey: idempotencyKey,
		ClaimID:        claimID,
		ReceiptID:      c.ReceiptID,
		Amount:         amount,
		CreatedAt:      s.now(),
	}
	s.reversals[rev.ID] = rev
	s.reversalByKey[idempotencyKey] = rev.ID
	c.ReversedAmount += amount
	s.ledger = append(s.ledger, &LedgerEntry{
		ID:           s.nextID("ledger"),
		Type:         LedgerReversalIn,
		BankSerialNo: r.BankSerialNo,
		ReceiptID:    r.ID,
		ClaimID:      c.ID,
		ReversalID:   rev.ID,
		Currency:     r.Currency,
		Amount:       amount,
		CreatedAt:    s.now(),
	})
	r.Version++
	return rev, nil
}

// Balance 返回来账的剩余暂收余额。
func (s *Store) Balance(receiptID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[receiptID]
	if !ok {
		return 0, ErrReceiptNotFound
	}
	return r.Amount - s.reservedLocked(receiptID), nil
}

// ClaimProgress 返回来账的认领进度（含失败申请及原因）。
func (s *Store) ClaimProgress(receiptID string) []*Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Claim
	for _, c := range s.claims {
		if c.ReceiptID == receiptID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Allocations 返回来账有效（处理中或已确认）的分配明细。
func (s *Store) Allocations(receiptID string) []Allocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Allocation
	for _, c := range s.claims {
		if c.ReceiptID != receiptID || (c.Status != ClaimPending && c.Status != ClaimConfirmed) {
			continue
		}
		out = append(out, c.Allocations...)
	}
	return out
}

// Reversals 返回来账的冲正记录。
func (s *Store) Reversals(receiptID string) []*Reversal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Reversal
	for _, rev := range s.reversals {
		if rev.ReceiptID == receiptID {
			out = append(out, rev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Ledger 按银行流水号查询资金去向，可追溯原银行流水。
func (s *Store) Ledger(bankSerialNo string) []*LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*LedgerEntry
	for _, e := range s.ledger {
		if e.BankSerialNo == bankSerialNo {
			out = append(out, e)
		}
	}
	return out
}

// GetReceipt 按 ID 查询来账。
func (s *Store) GetReceipt(receiptID string) (*Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[receiptID]
	if !ok {
		return nil, ErrReceiptNotFound
	}
	cp := *r
	return &cp, nil
}
