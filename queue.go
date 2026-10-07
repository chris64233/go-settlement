package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 指令状态。
const (
	StatusWaiting  = "WAITING"
	StatusReleased = "RELEASED"
)

// 等待原因。
const (
	ReasonValueDateNotReached = "VALUE_DATE_NOT_REACHED" // 价值日未到
	ReasonInsufficientFunds   = "INSUFFICIENT_FUNDS"     // 可用余额不足
	ReasonBlockedByAhead      = "BLOCKED_BY_AHEAD"       // 被前方资金缺口阻塞
)

var (
	// ErrInstructionConflict 同一外部指令号提交了不同关键内容。
	ErrInstructionConflict = errors.New("settlement: instruction conflict")
	// ErrInsufficientBalance 余额变动会使可用余额为负。
	ErrInsufficientBalance = errors.New("settlement: insufficient balance")
	// ErrInstructionNotFound 指令不存在。
	ErrInstructionNotFound = errors.New("settlement: instruction not found")
	// ErrInvalidInstruction 指令字段非法。
	ErrInvalidInstruction = errors.New("settlement: invalid instruction")
)

// Instruction 是一条结算指令。Priority 数值越大越优先。
type Instruction struct {
	ExternalID string    // 外部指令号，幂等键
	Account    string    // 付款账户
	Amount     Money     // 金额与币种
	ValueDate  time.Time // 价值日，到达后才允许释放
	Priority   int       // 优先级，越大越优先
	Deadline   time.Time // 最晚处理时间

	Seq        int64     // 受理顺序号，服务分配
	Status     string    // 当前状态
	AcceptedAt time.Time // 受理时间
}

// contentEqual 比较参与幂等判定的关键内容。
func (i Instruction) contentEqual(o Instruction) bool {
	return i.Account == o.Account &&
		i.Amount.Currency == o.Amount.Currency &&
		i.Amount.Amount.Equal(o.Amount.Amount) &&
		i.ValueDate.Equal(o.ValueDate) &&
		i.Priority == o.Priority &&
		i.Deadline.Equal(o.Deadline)
}

// ScanPolicy 控制释放扫描遇到资金不足指令时的行为。
type ScanPolicy struct {
	// StopOnInsufficient 为 true 时，遇到第一条无法覆盖的指令即停止扫描。
	StopOnInsufficient bool
	// AllowPrioritySkip 为 true 时，仅允许优先级严格高于阻塞指令的候选越过缺口。
	// 绝不允许任意挑选小额指令凑数。
	AllowPrioritySkip bool
}

// Balance 是某账户某币种的余额视图。
type Balance struct {
	Account   string
	Currency  string
	Available Decimal // 可用余额
	Frozen    Decimal // 人工冻结部分
}

// QueueEntry 是队列查询返回的等待指令及其未处理原因。
type QueueEntry struct {
	Instruction Instruction
	Reason      string
}

// ReleaseRecord 记录一条指令的释放依据。
type ReleaseRecord struct {
	ScanID        int64
	InstructionID string
	Seq           int64
	Account       string
	Amount        Money
	BalanceBefore Decimal // 扫描固定的可用余额快照（释放本条前）
	BalanceAfter  Decimal // 释放本条后的可用余额
	QueueVersion  int64   // 扫描固定的队列版本
	ReleasedAt    time.Time
	Policy        ScanPolicy
}

// ScanResult 是一次释放扫描的结果。
type ScanResult struct {
	ScanID       int64
	Account      string
	QueueVersion int64
	Released     []Instruction
	Skipped      []QueueEntry // 扫描时仍处于等待的指令及原因
}

type accountKey struct {
	account  string
	currency string
}

type ledger struct {
	available Decimal
	frozen    Decimal
}

// Queue 是受可用资金约束的结算指令排队服务，所有方法并发安全。
type Queue struct {
	mu       sync.Mutex
	now      func() time.Time
	policy   ScanPolicy
	seq      int64 // 指令受理顺序号
	scanSeq  int64 // 扫描序号
	version  int64 // 队列版本，任何影响顺序或余额的变更都会递增
	byExtID  map[string]*Instruction
	waiting  map[string]*Instruction // ExternalID -> 等待中的指令
	ledgers  map[accountKey]*ledger
	releases map[string]*ReleaseRecord // ExternalID -> 释放依据
}

// NewQueue 创建排队服务。now 用于测试注入时钟，可为 nil（使用真实时间）。
func NewQueue(policy ScanPolicy, now func() time.Time) *Queue {
	if now == nil {
		now = time.Now
	}
	return &Queue{
		now:      now,
		policy:   policy,
		byExtID:  make(map[string]*Instruction),
		waiting:  make(map[string]*Instruction),
		ledgers:  make(map[accountKey]*ledger),
		releases: make(map[string]*ReleaseRecord),
	}
}

func (q *Queue) ledgerFor(account, currency string) *ledger {
	k := accountKey{account: account, currency: currency}
	l, ok := q.ledgers[k]
	if !ok {
		l = &ledger{available: ZeroDecimal(), frozen: ZeroDecimal()}
		q.ledgers[k] = l
	}
	return l
}

// Register 登记结算指令。同一外部指令号提交相同内容时返回原指令（幂等重放），
// 关键内容变化时返回 ErrInstructionConflict。
func (q *Queue) Register(ins Instruction) (Instruction, bool, error) {
	if ins.ExternalID == "" || ins.Account == "" || ins.Amount.Currency == "" {
		return Instruction{}, false, fmt.Errorf("%w: external id, account and currency are required", ErrInvalidInstruction)
	}
	if ins.Amount.Amount.Sign() <= 0 {
		return Instruction{}, false, fmt.Errorf("%w: amount must be positive", ErrInvalidInstruction)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if existing, ok := q.byExtID[ins.ExternalID]; ok {
		if !existing.contentEqual(ins) {
			return Instruction{}, false, ErrInstructionConflict
		}
		return *existing, true, nil
	}

	q.seq++
	ins.Seq = q.seq
	ins.Status = StatusWaiting
	ins.AcceptedAt = q.now()
	stored := ins
	q.byExtID[ins.ExternalID] = &stored
	q.waiting[ins.ExternalID] = &stored
	q.version++
	return ins, false, nil
}

// Credit 入账，增加可用余额。
func (q *Queue) Credit(account string, amount Money) error {
	if amount.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: credit amount must be positive", ErrInvalidInstruction)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.ledgerFor(account, amount.Currency)
	l.available = l.available.Add(amount.Amount)
	q.version++
	return nil
}

// Debit 扣减可用余额，余额不足时返回 ErrInsufficientBalance。
func (q *Queue) Debit(account string, amount Money) error {
	if amount.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: debit amount must be positive", ErrInvalidInstruction)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.ledgerFor(account, amount.Currency)
	if l.available.Cmp(amount.Amount) < 0 {
		return ErrInsufficientBalance
	}
	l.available = l.available.Sub(amount.Amount)
	q.version++
	return nil
}

// Freeze 人工冻结，将部分可用余额转为冻结，冻结部分不参与释放。
func (q *Queue) Freeze(account string, amount Money) error {
	if amount.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: freeze amount must be positive", ErrInvalidInstruction)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.ledgerFor(account, amount.Currency)
	if l.available.Cmp(amount.Amount) < 0 {
		return ErrInsufficientBalance
	}
	l.available = l.available.Sub(amount.Amount)
	l.frozen = l.frozen.Add(amount.Amount)
	q.version++
	return nil
}

// Unfreeze 解冻，将冻结金额返还可用余额。
func (q *Queue) Unfreeze(account string, amount Money) error {
	if amount.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: unfreeze amount must be positive", ErrInvalidInstruction)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.ledgerFor(account, amount.Currency)
	if l.frozen.Cmp(amount.Amount) < 0 {
		return ErrInsufficientBalance
	}
	l.frozen = l.frozen.Sub(amount.Amount)
	l.available = l.available.Add(amount.Amount)
	q.version++
	return nil
}

// BalanceOf 查询账户余额。
func (q *Queue) BalanceOf(account, currency string) Balance {
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.ledgerFor(account, currency)
	return Balance{Account: account, Currency: currency, Available: l.available, Frozen: l.frozen}
}

// sortedWaiting 返回账户下按（优先级降序、受理时间升序、指令号升序）排序的等待指令。
// 调用方必须持有锁。
func (q *Queue) sortedWaiting(account string) []*Instruction {
	var list []*Instruction
	for _, ins := range q.waiting {
		if ins.Account == account {
			list = append(list, ins)
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		x, y := list[a], list[b]
		if x.Priority != y.Priority {
			return x.Priority > y.Priority
		}
		if !x.AcceptedAt.Equal(y.AcceptedAt) {
			return x.AcceptedAt.Before(y.AcceptedAt)
		}
		return x.Seq < y.Seq
	})
	return list
}

// classify 计算一条等待指令在指定扫描时刻的原因（空串表示可释放）。
// 调用方必须持有锁。
func (q *Queue) classify(ins *Instruction, available Decimal, asOf time.Time, blockedBy *Instruction) string {
	if ins.ValueDate.After(asOf) {
		return ReasonValueDateNotReached
	}
	if blockedBy != nil {
		if !(q.policy.AllowPrioritySkip && ins.Priority > blockedBy.Priority) {
			return ReasonBlockedByAhead
		}
	}
	if available.Cmp(ins.Amount.Amount) < 0 {
		return ReasonInsufficientFunds
	}
	return ""
}

// Scan 对指定账户执行一次释放扫描。扫描固定当前可用余额与队列版本，
// 余额能够覆盖指令时才释放；遇到无法覆盖的指令按策略停止或仅放行
// 明确允许越过的更高优先级指令。整个扫描在单次临界区内完成，
// 因此并发扫描、入账与冻结不会重复使用同一笔余额，
// 每条指令最多从等待进入释放一次。
func (q *Queue) Scan(account string, asOf time.Time) (ScanResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.scanSeq++
	result := ScanResult{
		ScanID:       q.scanSeq,
		Account:      account,
		QueueVersion: q.version,
	}

	ordered := q.sortedWaiting(account)
	type plan struct {
		ins    *Instruction
		before Decimal
		after  Decimal
	}
	var plans []plan

	// 按币种分别固定可用余额快照。
	availableByCcy := make(map[string]Decimal)
	snapshot := func(currency string) Decimal {
		if v, ok := availableByCcy[currency]; ok {
			return v
		}
		v := q.ledgerFor(account, currency).available
		availableByCcy[currency] = v
		return v
	}

	var blockedBy *Instruction // 第一条资金不足且未被越过的指令
	for _, ins := range ordered {
		available := snapshot(ins.Amount.Currency)
		reason := q.classify(ins, available, asOf, blockedBy)
		if reason != "" {
			result.Skipped = append(result.Skipped, QueueEntry{Instruction: *ins, Reason: reason})
			if reason == ReasonInsufficientFunds {
				if q.policy.StopOnInsufficient {
					// 停止扫描：其后指令一律记为被前方阻塞。
					blockedBy = ins
					continue
				}
				if blockedBy == nil {
					blockedBy = ins
				}
			}
			continue
		}
		after := available.Sub(ins.Amount.Amount)
		plans = append(plans, plan{ins: ins, before: available, after: after})
		availableByCcy[ins.Amount.Currency] = after
	}

	// 提交：状态迁移与余额扣减在同一临界区内原子完成。
	releasedAt := q.now()
	for _, p := range plans {
		if p.ins.Status != StatusWaiting {
			continue // 防御：每条指令最多释放一次
		}
		p.ins.Status = StatusReleased
		delete(q.waiting, p.ins.ExternalID)
		l := q.ledgerFor(account, p.ins.Amount.Currency)
		l.available = l.available.Sub(p.ins.Amount.Amount)
		q.releases[p.ins.ExternalID] = &ReleaseRecord{
			ScanID:        result.ScanID,
			InstructionID: p.ins.ExternalID,
			Seq:           p.ins.Seq,
			Account:       account,
			Amount:        p.ins.Amount,
			BalanceBefore: p.before,
			BalanceAfter:  p.after,
			QueueVersion:  result.QueueVersion,
			ReleasedAt:    releasedAt,
			Policy:        q.policy,
		}
		result.Released = append(result.Released, *p.ins)
	}
	if len(plans) > 0 {
		q.version++
	}
	return result, nil
}

// Pending 返回账户当前的等待队列及每条指令未处理的原因。
func (q *Queue) Pending(account string, asOf time.Time) []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	ordered := q.sortedWaiting(account)
	entries := make([]QueueEntry, 0, len(ordered))
	availableByCcy := make(map[string]Decimal)
	var blockedBy *Instruction
	for _, ins := range ordered {
		available, ok := availableByCcy[ins.Amount.Currency]
		if !ok {
			available = q.ledgerFor(ins.Account, ins.Amount.Currency).available
			availableByCcy[ins.Amount.Currency] = available
		}
		reason := q.classify(ins, available, asOf, blockedBy)
		if reason == "" {
			availableByCcy[ins.Amount.Currency] = available.Sub(ins.Amount.Amount)
			reason = "RELEASABLE" // 当前余额可覆盖，等待下一次扫描
		} else if reason == ReasonInsufficientFunds && blockedBy == nil {
			blockedBy = ins
		}
		entries = append(entries, QueueEntry{Instruction: *ins, Reason: reason})
	}
	return entries
}

// ReleaseBasis 返回一条已释放指令的释放依据。
func (q *Queue) ReleaseBasis(externalID string) (ReleaseRecord, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	rec, ok := q.releases[externalID]
	if !ok {
		return ReleaseRecord{}, ErrInstructionNotFound
	}
	return *rec, nil
}

// Version 返回当前队列版本。
func (q *Queue) Version() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.version
}
