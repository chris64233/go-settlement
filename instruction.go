package settlement

import (
	"errors"
	"fmt"
	"time"
)

// InstructionStatus 表示结算指令的生命周期状态。
type InstructionStatus string

const (
	InstrCreated InstructionStatus = "created"
	InstrSent    InstructionStatus = "sent"
	InstrHalted  InstructionStatus = "halted" // 因账户版本失效而停止
)

var (
	ErrInstructionNotFound = errors.New("instruction not found")
	ErrNoEffectiveVersion  = errors.New("no effective account version for instruction")
	ErrCurrencyNotCovered  = errors.New("currency not covered by account version")
	ErrInstructionNotNew   = errors.New("instruction already sent or halted")
)

// Instruction 是一笔结算付款指令，创建时冻结所采用的授权账户版本证据。
type Instruction struct {
	ID             string
	AccountID      string
	Amount         int64
	Currency       string
	Status         InstructionStatus
	AccountVersion int    // 创建时采用的账户版本号
	VersionDigest  string // 该版本冻结的内容摘要（证据）
	AccountDigest  string // 该版本的账号安全摘要（证据）
	AccountMasked  string
	CreatedAt      time.Time
	SentAt         time.Time
	HaltReason     string
}

// InstructionService 管理结算指令，并依赖 AccountService 校验账户版本。
type InstructionService struct {
	accounts *AccountService
	instrs   map[string]*Instruction
	seq      int
}

func NewInstructionService(accounts *AccountService) *InstructionService {
	return &InstructionService{accounts: accounts, instrs: map[string]*Instruction{}}
}

// CreateInstruction 创建结算指令，记录当前生效账户版本的证据。
func (s *InstructionService) CreateInstruction(accountID string, amount int64, currency string, now time.Time) (*Instruction, error) {
	v := s.accounts.CurrentEffective(accountID, now)
	if v == nil {
		return nil, ErrNoEffectiveVersion
	}
	ok := false
	for _, c := range v.Content.Currencies {
		if c == currency {
			ok = true
			break
		}
	}
	if !ok {
		return nil, ErrCurrencyNotCovered
	}
	s.seq++
	instr := &Instruction{
		ID:             fmt.Sprintf("INS-%06d", s.seq),
		AccountID:      accountID,
		Amount:         amount,
		Currency:       currency,
		Status:         InstrCreated,
		AccountVersion: v.Version,
		VersionDigest:  v.ContentDigest,
		AccountDigest:  v.AccountDigest,
		AccountMasked:  v.AccountMasked,
		CreatedAt:      now,
	}
	s.instrs[instr.ID] = instr
	return instr, nil
}

// SendInstruction 发送指令。发送前重新校验所记录的账户版本仍然有效：
// 若该版本已被撤销或被新版本替代，则停止指令并记录明确原因。
// 已发送的指令保留原版本证据，不会静默切换到新账户。
func (s *InstructionService) SendInstruction(id string, now time.Time) error {
	instr, ok := s.instrs[id]
	if !ok {
		return ErrInstructionNotFound
	}
	if instr.Status != InstrCreated {
		return ErrInstructionNotNew
	}
	s.accounts.ActivateDue(now) // 先应用到期的版本切换，再校验
	v, err := s.accounts.GetVersion(instr.AccountID, instr.AccountVersion)
	if err != nil {
		return err
	}
	switch v.Status {
	case StatusEffective:
		instr.Status = InstrSent
		instr.SentAt = now
		return nil
	case StatusRevoked:
		instr.Status = InstrHalted
		instr.HaltReason = fmt.Sprintf("account version %d was revoked before send", v.Version)
		return errors.New(instr.HaltReason)
	case StatusSuperseded:
		instr.Status = InstrHalted
		instr.HaltReason = fmt.Sprintf("account version %d was superseded by a newer version before send", v.Version)
		return errors.New(instr.HaltReason)
	default:
		instr.Status = InstrHalted
		instr.HaltReason = fmt.Sprintf("account version %d is not effective (status=%s)", v.Version, v.Status)
		return errors.New(instr.HaltReason)
	}
}

// GetInstruction 查询指令（含冻结的版本证据，不含明文账号）。
func (s *InstructionService) GetInstruction(id string) (*Instruction, error) {
	instr, ok := s.instrs[id]
	if !ok {
		return nil, ErrInstructionNotFound
	}
	return instr, nil
}

// String 保证日志输出不包含敏感账号明文。
func (i *Instruction) String() string {
	return fmt.Sprintf("Instruction{id=%s account=%s v%d status=%s %d %s account=%s}",
		i.ID, i.AccountID, i.AccountVersion, i.Status, i.Amount, i.Currency, i.AccountMasked)
}
