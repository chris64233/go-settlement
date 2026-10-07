package settlement

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// SkipPolicy 决定扫描遇到余额无法覆盖的指令时的行为。
type SkipPolicy int

const (
	// PolicyStopOnInsufficient 遇到无法覆盖的指令即停止本次扫描（默认，严格有序）。
	PolicyStopOnInsufficient SkipPolicy = iota
	// PolicyAllowMarkedOvertake 只放行明确标记 AllowOvertake 的后续指令，
	// 其余指令保持等待，避免小额指令任意插队。
	PolicyAllowMarkedOvertake
)

// RegisterCommand 是登记指令的请求。
type RegisterCommand struct {
	ExternalID    string
	Account       string
	Currency      string
	Amount        Amount
	ValueDate     string
	Priority      int
	Deadline      time.Time
	AllowOvertake bool
}

// BalanceKey 标识一个账户+币种的余额。
type BalanceKey struct {
	Account  string
	Currency string
}

// ReleaseRecord 记录一条指令的释放依据。
type ReleaseRecord struct {
	ExternalID    string `json:"external_id"`
	Amount        Amount `json:"amount"`
	BalanceBefore Amount `json:"balance_before"`
	BalanceAfter  Amount `json:"balance_after"`
	QueueVersion  int64  `json:"queue_version"`
}

// ScanResult 是一次释放扫描的结果。
type ScanResult struct {
	ScanID       string          `json:"scan_id"`
	QueueVersion int64           `json:"queue_version"`
	StartedAt    time.Time       `json:"started_at"`
	Released     []ReleaseRecord `json:"released"`
	Skipped      []SkipRecord    `json:"skipped"`
}

// SkipRecord 记录一条指令未释放的原因。
type SkipRecord struct {
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason"`
}

// QueueEntry 是队列查询返回的视图。
type QueueEntry struct {
	Instruction Instruction `json:"instruction"`
	Position    int         `json:"position"` // 在稳定顺序中的位置（从 1 开始）
}

type account struct {
	balance Amount
	frozen  bool // 人工冻结：冻结期间不允许释放扣减
}

// Service 是结算指令排队服务。所有方法可并发调用。
type Service struct {
	mu     sync.Mutex
	policy SkipPolicy

	seq     int64 // 受理序号
	version int64 // 队列版本：任何改变队列或余额的操作都会递增

	instructions map[string]*Instruction // external id -> instruction
	order        []*Instruction          // 稳定顺序
	accounts     map[BalanceKey]*account
	scans        map[string]*ScanResult
	scanSeq      int64
}

// NewService 创建排队服务。
func NewService(policy SkipPolicy) *Service {
	return &Service{
		policy:       policy,
		instructions: make(map[string]*Instruction),
		accounts:     make(map[BalanceKey]*account),
		scans:        make(map[string]*ScanResult),
	}
}

// Register 登记一条结算指令。相同外部指令号、相同内容返回原结果（幂等），
// 内容变化返回 *ConflictError。
func (s *Service) Register(cmd RegisterCommand) (*Instruction, error) {
	if cmd.ExternalID == "" {
		return nil, fmt.Errorf("settlement: external id is required")
	}
	if cmd.Account == "" || cmd.Currency == "" {
		return nil, fmt.Errorf("settlement: account and currency are required")
	}
	if cmd.Amount.Sign() <= 0 {
		return nil, fmt.Errorf("settlement: amount must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := &Instruction{
		ExternalID:    cmd.ExternalID,
		Account:       cmd.Account,
		Currency:      cmd.Currency,
		Amount:        cmd.Amount,
		ValueDate:     cmd.ValueDate,
		Priority:      cmd.Priority,
		Deadline:      cmd.Deadline,
		AllowOvertake: cmd.AllowOvertake,
	}
	if existing, ok := s.instructions[cmd.ExternalID]; ok {
		if existing.fingerprint() == candidate.fingerprint() {
			cp := *existing
			return &cp, nil // 幂等重放：返回原结果
		}
		return nil, &ConflictError{ExternalID: cmd.ExternalID, Existing: *existing}
	}

	s.seq++
	candidate.Seq = s.seq
	candidate.Status = StatusPending
	candidate.WaitReason = "awaiting first release scan"
	s.instructions[cmd.ExternalID] = candidate
	s.order = append(s.order, candidate)
	s.sortLocked()
	s.version++
	cp := *candidate
	return &cp, nil
}

// sortLocked 按优先级（降序）、受理序号（升序）、外部指令号（升序）稳定排序。
func (s *Service) sortLocked() {
	sort.SliceStable(s.order, func(i, j int) bool {
		a, b := s.order[i], s.order[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.ExternalID < b.ExternalID
	})
}

// ApplyBalanceChange 对账户余额做一次变动（delta 可正可负）。
// 余额不允许变为负数。
func (s *Service) ApplyBalanceChange(accountID, currency string, delta Amount, reason string) (Amount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := BalanceKey{Account: accountID, Currency: currency}
	acct := s.accounts[key]
	if acct == nil {
		acct = &account{balance: Zero()}
		s.accounts[key] = acct
	}
	next := acct.balance.Add(delta)
	if next.Sign() < 0 {
		return acct.balance, fmt.Errorf("%w: account=%s currency=%s balance=%s delta=%s",
			ErrInsufficientBalance, accountID, currency, acct.balance, delta)
	}
	acct.balance = next
	s.version++
	return next, nil
}

// SetFrozen 设置或解除账户+币种的人工冻结。冻结期间释放扫描不会动用该余额。
func (s *Service) SetFrozen(accountID, currency string, frozen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := BalanceKey{Account: accountID, Currency: currency}
	acct := s.accounts[key]
	if acct == nil {
		acct = &account{balance: Zero()}
		s.accounts[key] = acct
	}
	if acct.frozen != frozen {
		acct.frozen = frozen
		s.version++
	}
}

// Balance 查询当前可用余额。
func (s *Service) Balance(accountID, currency string) Amount {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acct := s.accounts[BalanceKey{accountID, currency}]; acct != nil {
		return acct.balance
	}
	return Zero()
}

type scanPlan struct {
	released []plannedRelease
	skipped  []SkipRecord
	reasons  map[string]string // external id -> wait reason
	expired  []string
}

type plannedRelease struct {
	instruction   *Instruction
	balanceBefore Amount
	balanceAfter  Amount
}

// Scan 执行一次释放扫描。扫描开始时固定队列版本与余额快照；
// 提交时若版本已变化则返回 ErrVersionConflict，队列与余额保持不变。
func (s *Service) Scan(now time.Time) (*ScanResult, error) {
	// 第一阶段：取快照。
	s.mu.Lock()
	version := s.version
	pending := make([]*Instruction, 0, len(s.order))
	for _, in := range s.order {
		if in.Status == StatusPending {
			cp := *in
			pending = append(pending, &cp)
		}
	}
	balances := make(map[BalanceKey]Amount, len(s.accounts))
	frozen := make(map[BalanceKey]bool, len(s.accounts))
	for k, acct := range s.accounts {
		balances[k] = acct.balance
		frozen[k] = acct.frozen
	}
	s.mu.Unlock()

	// 第二阶段：基于快照计算释放计划（不持锁）。
	plan := s.planLocked(pending, balances, frozen, now)

	// 第三阶段：校验版本并提交。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version {
		return nil, ErrVersionConflict
	}

	s.scanSeq++
	result := &ScanResult{
		ScanID:       fmt.Sprintf("scan-%d", s.scanSeq),
		QueueVersion: version,
		StartedAt:    now,
		Skipped:      plan.skipped,
	}
	for _, pr := range plan.released {
		key := BalanceKey{pr.instruction.Account, pr.instruction.Currency}
		acct := s.accounts[key]
		if acct == nil || acct.frozen || acct.balance.Cmp(pr.instruction.Amount) < 0 {
			// 防御性复核：版本未变时不应发生。
			return nil, ErrVersionConflict
		}
		acct.balance = acct.balance.Sub(pr.instruction.Amount)
		in := s.instructions[pr.instruction.ExternalID]
		in.Status = StatusReleased
		in.WaitReason = ""
		in.ReleasedScanID = result.ScanID
		result.Released = append(result.Released, ReleaseRecord{
			ExternalID:    in.ExternalID,
			Amount:        in.Amount,
			BalanceBefore: pr.balanceBefore,
			BalanceAfter:  pr.balanceAfter,
			QueueVersion:  version,
		})
	}
	for _, id := range plan.expired {
		in := s.instructions[id]
		in.Status = StatusExpired
		in.WaitReason = plan.reasons[id]
	}
	for id, reason := range plan.reasons {
		if in := s.instructions[id]; in.Status == StatusPending {
			in.WaitReason = reason
		}
	}
	if len(plan.released) > 0 || len(plan.expired) > 0 {
		s.version++
	}
	s.scans[result.ScanID] = result
	return result, nil
}

// planLocked 基于快照计算释放计划。
func (s *Service) planLocked(pending []*Instruction, balances map[BalanceKey]Amount, frozen map[BalanceKey]bool, now time.Time) *scanPlan {
	plan := &scanPlan{reasons: make(map[string]string)}
	blocked := false
	var blocker *Instruction
	gapSeen := false // 是否已遇到资金缺口/冻结（越过策略下生效）

	for _, in := range pending {
		key := BalanceKey{in.Account, in.Currency}
		if !in.Deadline.IsZero() && now.After(in.Deadline) {
			reason := fmt.Sprintf("deadline %s passed before scan", in.Deadline.UTC().Format(time.RFC3339))
			plan.expired = append(plan.expired, in.ExternalID)
			plan.reasons[in.ExternalID] = reason
			plan.skipped = append(plan.skipped, SkipRecord{in.ExternalID, reason})
			continue
		}
		if blocked {
			reason := fmt.Sprintf("blocked by earlier instruction %s (insufficient funds, policy=stop)", blocker.ExternalID)
			plan.reasons[in.ExternalID] = reason
			plan.skipped = append(plan.skipped, SkipRecord{in.ExternalID, reason})
			continue
		}
		if frozen[key] {
			reason := fmt.Sprintf("account %s/%s is frozen", in.Account, in.Currency)
			plan.reasons[in.ExternalID] = reason
			plan.skipped = append(plan.skipped, SkipRecord{in.ExternalID, reason})
			if s.policy == PolicyStopOnInsufficient {
				blocked = true
				blocker = in
			} else {
				gapSeen = true
			}
			continue
		}
		balance := balances[key]
		if balance.Cmp(in.Amount) >= 0 {
			if gapSeen && !in.AllowOvertake {
				reason := "earlier funding gap exists and overtake is not permitted for this instruction"
				plan.reasons[in.ExternalID] = reason
				plan.skipped = append(plan.skipped, SkipRecord{in.ExternalID, reason})
				continue
			}
			plan.released = append(plan.released, plannedRelease{
				instruction:   in,
				balanceBefore: balance,
				balanceAfter:  balance.Sub(in.Amount),
			})
			balances[key] = balance.Sub(in.Amount)
			continue
		}
		// 余额不足。
		reason := fmt.Sprintf("insufficient funds: need %s %s, available %s",
			in.Amount, in.Currency, balance)
		plan.reasons[in.ExternalID] = reason
		plan.skipped = append(plan.skipped, SkipRecord{in.ExternalID, reason})
		switch s.policy {
		case PolicyStopOnInsufficient:
			blocked = true
			blocker = in
		case PolicyAllowMarkedOvertake:
			// 继续扫描，但只放行明确允许越过的指令。
			gapSeen = true
		}
	}
	return plan
}

// Queue 返回当前等待队列的稳定顺序视图（含已处理指令的状态）。
func (s *Service) Queue() []QueueEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]QueueEntry, 0, len(s.order))
	for i, in := range s.order {
		entries = append(entries, QueueEntry{Instruction: *in, Position: i + 1})
	}
	return entries
}

// Explain 返回一条指令的当前状态与依据：等待原因或释放依据。
func (s *Service) Explain(externalID string) (*Instruction, *ReleaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instructions[externalID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	cp := *in
	if in.ReleasedScanID == "" {
		return &cp, nil, nil
	}
	scan := s.scans[in.ReleasedScanID]
	if scan == nil {
		return &cp, nil, nil
	}
	for _, rec := range scan.Released {
		if rec.ExternalID == externalID {
			return &cp, &rec, nil
		}
	}
	return &cp, nil, nil
}
