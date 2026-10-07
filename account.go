package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ApprovalQuorum 是账户变更生效所需的审核通过人数。
const ApprovalQuorum = 2

// WaitingPeriod 是审核通过后到账户版本生效之间的安全等待期。
const WaitingPeriod = 24 * time.Hour

// VersionStatus 表示账户版本的生命周期状态。
type VersionStatus string

const (
	StatusPending    VersionStatus = "pending"    // 待审核
	StatusWaiting    VersionStatus = "waiting"    // 审核通过，等待安全期结束
	StatusEffective  VersionStatus = "effective"  // 已生效
	StatusRejected   VersionStatus = "rejected"   // 审核拒绝，终止
	StatusRevoked    VersionStatus = "revoked"    // 已撤销
	StatusSuperseded VersionStatus = "superseded" // 被新版本替代
)

var (
	ErrRequestNotFound   = errors.New("account change request not found")
	ErrVersionNotPending = errors.New("version is not pending review")
	ErrDuplicateApprover = errors.New("approver has already decided on this version")
	ErrNotRevocable      = errors.New("only waiting or effective versions can be revoked")
)

// AccountContent 是收款账户申请的业务内容。创建申请时内容摘要被冻结，
// 任何字段变化都必须通过新的申请版本提交。
type AccountContent struct {
	Holder      string    // 账户持有人
	BankID      string    // 银行标识
	Currencies  []string  // 适用币种
	EffectiveAt time.Time // 期望生效时间（不早于等待期结束）
}

// contentDigest 计算冻结的内容摘要。
func contentDigest(c AccountContent, accountDigest string) string {
	cur := append([]string(nil), c.Currencies...)
	sort.Strings(cur)
	h := sha256.New()
	fmt.Fprintf(h, "holder=%s\nbank=%s\naccount=%s\ncurrencies=%v\neffective=%s\n",
		c.Holder, c.BankID, accountDigest, cur, c.EffectiveAt.UTC().Format(time.RFC3339Nano))
	return hex.EncodeToString(h.Sum(nil))
}

// DigestAccountNumber 计算账号的安全摘要，明文账号不会被保存。
func DigestAccountNumber(accountNumber string) string {
	sum := sha256.Sum256([]byte(accountNumber))
	return hex.EncodeToString(sum[:])
}

// MaskAccountNumber 生成账号的掩码展示形式（仅保留后四位）。
func MaskAccountNumber(accountNumber string) string {
	if len(accountNumber) <= 4 {
		return "****"
	}
	return "****" + accountNumber[len(accountNumber)-4:]
}

// AccountVersion 是一次账户申请产生的不可变版本。
type AccountVersion struct {
	AccountID      string
	Version        int
	Content        AccountContent
	ContentDigest  string // 冻结的内容摘要
	AccountDigest  string // 账号安全摘要
	AccountMasked  string // 掩码展示，如 ****1234
	Status         VersionStatus
	SubmittedAt    time.Time
	ApprovedAt     time.Time // 达到审核门槛的时间
	EffectiveAt    time.Time // 实际生效时间（审核通过时间 + 等待期）
	approvals      map[string]bool
	rejectedBy     string
	decisionEvents map[string]struct{} // 已处理的审核事件号，保证幂等
}

// Approvals 返回已通过的审核人列表（有序）。
func (v *AccountVersion) Approvals() []string {
	out := make([]string, 0, len(v.approvals))
	for a := range v.approvals {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// RejectedBy 返回拒绝该版本的审核人（若有）。
func (v *AccountVersion) RejectedBy() string { return v.rejectedBy }

// AccountService 管理收款账户申请、审核、撤销与版本历史。
type AccountService struct {
	accounts map[string][]*AccountVersion // accountID -> 按版本号升序
	seq      map[string]int
}

func NewAccountService() *AccountService {
	return &AccountService{accounts: map[string][]*AccountVersion{}, seq: map[string]int{}}
}

// Submit 提交账户新增或变更申请，生成新的版本。
// accountNumber 仅用于计算安全摘要与掩码，不会被保存。
func (s *AccountService) Submit(accountID string, c AccountContent, accountNumber string, now time.Time) (*AccountVersion, error) {
	if accountID == "" || c.Holder == "" || c.BankID == "" || accountNumber == "" || len(c.Currencies) == 0 {
		return nil, errors.New("holder, bank, account number and currencies are required")
	}
	accDigest := DigestAccountNumber(accountNumber)
	s.seq[accountID]++
	v := &AccountVersion{
		AccountID:      accountID,
		Version:        s.seq[accountID],
		Content:        c,
		ContentDigest:  contentDigest(c, accDigest),
		AccountDigest:  accDigest,
		AccountMasked:  MaskAccountNumber(accountNumber),
		Status:         StatusPending,
		SubmittedAt:    now,
		approvals:      map[string]bool{},
		decisionEvents: map[string]struct{}{},
	}
	s.accounts[accountID] = append(s.accounts[accountID], v)
	return v, nil
}

// Approve 审核通过。eventID 保证幂等；同一审核人不能对同一版本重复决定。
func (s *AccountService) Approve(accountID string, version int, approver, eventID string, now time.Time) error {
	return s.decide(accountID, version, approver, eventID, true, now)
}

// Reject 审核拒绝，任一拒绝即终止该版本。
func (s *AccountService) Reject(accountID string, version int, approver, eventID string, now time.Time) error {
	return s.decide(accountID, version, approver, eventID, false, now)
}

func (s *AccountService) decide(accountID string, version int, approver, eventID string, approve bool, now time.Time) error {
	if approver == "" || eventID == "" {
		return errors.New("approver and eventID are required")
	}
	v, err := s.getVersion(accountID, version)
	if err != nil {
		return err
	}
	if _, seen := v.decisionEvents[eventID]; seen {
		return nil // 事件号幂等：重复事件直接忽略
	}
	if v.Status != StatusPending {
		return ErrVersionNotPending
	}
	if _, decided := v.approvals[approver]; decided {
		return ErrDuplicateApprover
	}
	v.decisionEvents[eventID] = struct{}{}
	if !approve {
		v.rejectedBy = approver
		v.Status = StatusRejected
		return nil
	}
	v.approvals[approver] = true
	if len(v.approvals) >= ApprovalQuorum {
		v.Status = StatusWaiting
		v.ApprovedAt = now
		eff := now.Add(WaitingPeriod)
		if v.Content.EffectiveAt.After(eff) {
			eff = v.Content.EffectiveAt
		}
		v.EffectiveAt = eff
	}
	return nil
}

// ActivateDue 将所有等待期已到的版本置为生效，并替代旧的生效版本。
// 返回本次新生效的版本。
func (s *AccountService) ActivateDue(now time.Time) []*AccountVersion {
	var activated []*AccountVersion
	for _, versions := range s.accounts {
		for _, v := range versions {
			if v.Status != StatusWaiting || v.EffectiveAt.After(now) {
				continue
			}
			for _, other := range versions {
				if other.Status == StatusEffective {
					other.Status = StatusSuperseded
				}
			}
			v.Status = StatusEffective
			activated = append(activated, v)
		}
	}
	return activated
}

// Revoke 撤销等待中或已生效的版本。
func (s *AccountService) Revoke(accountID string, version int) error {
	v, err := s.getVersion(accountID, version)
	if err != nil {
		return err
	}
	if v.Status != StatusWaiting && v.Status != StatusEffective {
		return ErrNotRevocable
	}
	v.Status = StatusRevoked
	return nil
}

// CurrentEffective 返回账户当前生效的版本，没有则返回 nil。
func (s *AccountService) CurrentEffective(accountID string, now time.Time) *AccountVersion {
	s.ActivateDue(now)
	for _, v := range s.accounts[accountID] {
		if v.Status == StatusEffective {
			return v
		}
	}
	return nil
}

// GetVersion 查询指定版本（只读视图，不含明文账号）。
func (s *AccountService) GetVersion(accountID string, version int) (*AccountVersion, error) {
	return s.getVersion(accountID, version)
}

// History 返回账户全部版本历史，按版本号升序。
func (s *AccountService) History(accountID string) []*AccountVersion {
	src := s.accounts[accountID]
	out := make([]*AccountVersion, len(src))
	copy(out, src)
	return out
}

func (s *AccountService) getVersion(accountID string, version int) (*AccountVersion, error) {
	for _, v := range s.accounts[accountID] {
		if v.Version == version {
			return v, nil
		}
	}
	return nil, ErrRequestNotFound
}

// String 保证日志输出不包含敏感账号明文。
func (v *AccountVersion) String() string {
	return fmt.Sprintf("AccountVersion{account=%s v%d status=%s holder=%s bank=%s account=%s digest=%s}",
		v.AccountID, v.Version, v.Status, v.Content.Holder, v.Content.BankID, v.AccountMasked, v.ContentDigest[:12])
}
