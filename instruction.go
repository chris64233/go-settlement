package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// InstructionStatus is the lifecycle state of a settlement instruction.
type InstructionStatus string

const (
	InstrPending InstructionStatus = "PENDING"
	InstrSent    InstructionStatus = "SENT"
	InstrStopped InstructionStatus = "STOPPED"
)

var (
	ErrInstructionNotFound    = errors.New("instruction not found")
	ErrInstructionNotSendable = errors.New("instruction is not pending")
	ErrCurrencyNotCovered     = errors.New("currency is not covered by the authorized account version")
)

// AccountEvidence is the frozen proof of which authorized account version a
// settlement instruction was created against. It is captured at creation and
// never silently swapped for a newer account version.
type AccountEvidence struct {
	AccountID     string
	VersionNo     int
	ContentDigest string
	MaskedAccount string
	HolderName    string
	BankID        string
}

// Instruction is a settlement payment instruction bound to an authorized
// account version.
type Instruction struct {
	ID        string
	AccountID string
	Amount    int64
	Currency  string
	Evidence  AccountEvidence
	Status    InstructionStatus
	// StopReason explains why a pending instruction was stopped, e.g. the
	// bound account version was revoked or superseded before sending.
	StopReason string
	CreatedAt  time.Time
	SentAt     time.Time
}

// InstructionStore creates and sends settlement instructions, enforcing that
// only the currently authorized account version may be used.
type InstructionStore struct {
	mu       sync.Mutex
	mandates *MandateStore
	items    map[string]*Instruction
	seq      int
}

func NewInstructionStore(mandates *MandateStore) *InstructionStore {
	return &InstructionStore{mandates: mandates, items: make(map[string]*Instruction)}
}

// CreateInstruction binds a new instruction to the account's currently
// active authorized version and freezes the version evidence.
func (s *InstructionStore) CreateInstruction(accountID string, amount int64, currency string, now time.Time) (Instruction, error) {
	if amount <= 0 {
		return Instruction{}, errors.New("amount must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active, err := s.mandates.ActiveVersion(accountID)
	if err != nil {
		return Instruction{}, err
	}
	covered := false
	for _, c := range active.Currencies {
		if c == currency {
			covered = true
			break
		}
	}
	if !covered {
		return Instruction{}, fmt.Errorf("%w: %s", ErrCurrencyNotCovered, currency)
	}

	s.seq++
	ins := &Instruction{
		ID:        fmt.Sprintf("ins-%d", s.seq),
		AccountID: accountID,
		Amount:    amount,
		Currency:  currency,
		Evidence: AccountEvidence{
			AccountID:     accountID,
			VersionNo:     active.VersionNo,
			ContentDigest: s.contentDigestOf(accountID, active.VersionNo),
			MaskedAccount: active.MaskedAccount,
			HolderName:    active.HolderName,
			BankID:        active.BankID,
		},
		Status:    InstrPending,
		CreatedAt: now,
	}
	s.items[ins.ID] = ins
	return *ins, nil
}

func (s *InstructionStore) contentDigestOf(accountID string, versionNo int) string {
	v, err := s.mandates.snapshot(accountID, versionNo)
	if err != nil {
		return ""
	}
	return v.ContentDigest
}

// Send validates that the bound account version is still the active one and
// then marks the instruction as sent. If the bound version was revoked or
// superseded before sending, the instruction is stopped with an explicit
// reason instead of silently switching to a newer account.
func (s *InstructionStore) Send(id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ins, ok := s.items[id]
	if !ok {
		return ErrInstructionNotFound
	}
	if ins.Status != InstrPending {
		return ErrInstructionNotSendable
	}

	v, err := s.mandates.snapshot(ins.Evidence.AccountID, ins.Evidence.VersionNo)
	if err != nil {
		ins.Status = InstrStopped
		ins.StopReason = "bound account version no longer exists"
		return fmt.Errorf("instruction %s stopped: %s", ins.ID, ins.StopReason)
	}
	switch v.Status {
	case StatusActive:
		// still authorized
	case StatusRevoked:
		ins.Status = InstrStopped
		ins.StopReason = fmt.Sprintf("account version %d was revoked before sending", v.VersionNo)
		return fmt.Errorf("instruction %s stopped: %s", ins.ID, ins.StopReason)
	case StatusSuperseded:
		ins.Status = InstrStopped
		ins.StopReason = fmt.Sprintf("account version %d was superseded by a newer version before sending", v.VersionNo)
		return fmt.Errorf("instruction %s stopped: %s", ins.ID, ins.StopReason)
	default:
		ins.Status = InstrStopped
		ins.StopReason = fmt.Sprintf("account version %d is no longer active (status %s)", v.VersionNo, v.Status)
		return fmt.Errorf("instruction %s stopped: %s", ins.ID, ins.StopReason)
	}

	ins.Status = InstrSent
	ins.SentAt = now
	return nil
}

// GetInstruction returns a copy of the instruction including its frozen
// account-version evidence. Sent instructions keep their original evidence
// even if the account later moves to a new version.
func (s *InstructionStore) GetInstruction(id string) (Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.items[id]
	if !ok {
		return Instruction{}, ErrInstructionNotFound
	}
	return *ins, nil
}
