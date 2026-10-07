// Package reserve implements merchant rolling-reserve management:
// withholding part of confirmed settlements, releasing batches after
// their holding period, and sharing one available balance between
// releases and risk deductions.
package reserve

import (
	"errors"
	"fmt"
	"time"
)

// Money is an amount in the smallest currency unit (e.g. cents).
type Money int64

var (
	ErrNotFound         = errors.New("reserve: not found")
	ErrConflict         = errors.New("reserve: idempotency conflict")
	ErrVersion          = errors.New("reserve: batch version conflict")
	ErrInvalidAmount    = errors.New("reserve: invalid amount")
	ErrRuleNotEffective = errors.New("reserve: no effective rule for merchant")
)

// ReserveRule is one immutable version of a merchant's withholding rule.
// Later adjustments create a new version and never rewrite existing batches.
type ReserveRule struct {
	ID            string
	MerchantID    string
	Version       int
	RateBP        int // withheld rate in basis points (1/10000)
	HoldDays      int
	EffectiveFrom time.Time
}

// Settlement is a confirmed settlement registered for a merchant.
type Settlement struct {
	ID          string
	MerchantID  string
	Amount      Money
	ConfirmedAt time.Time
}

// ReserveBatch is the withheld portion of one settlement, frozen with the
// rule version that produced it. Version is an optimistic-lock counter
// incremented on every mutation.
type ReserveBatch struct {
	ID           string
	MerchantID   string
	SettlementID string
	Amount       Money // original withheld amount, immutable
	ReleaseDate  time.Time
	RuleID       string
	RuleVersion  int
	Deducted     Money // cumulative risk deductions
	Released     Money // cumulative releases to available balance
	Version      int64
	CreatedAt    time.Time
}

// Remaining is the part of the batch that can still be released or deducted.
func (b *ReserveBatch) Remaining() Money {
	return b.Amount - b.Deducted - b.Released
}

// DeductionAlloc records how much one risk deduction took from one batch.
type DeductionAlloc struct {
	BatchID string
	Amount  Money
}

// RiskDeduction occupies reserve batches, earliest release date first.
type RiskDeduction struct {
	ID         string
	MerchantID string
	Amount     Money
	Reason     string
	Allocs     []DeductionAlloc
	CreatedAt  time.Time
}

// ReleaseRun is one idempotent release scan over due batches.
type ReleaseRun struct {
	ID         string
	MerchantID string
	AsOf       time.Time
	Released   Money
	BatchIDs   []string
	CreatedAt  time.Time
}

// Compensation credits the merchant balance back for a wrong deduction.
// The original deduction record is never modified.
type Compensation struct {
	ID          string
	MerchantID  string
	DeductionID string
	Amount      Money
	Reason      string
	CreatedAt   time.Time
}

// BatchView shows the remaining composition of one reserve batch.
type BatchView struct {
	BatchID      string
	SettlementID string
	Amount       Money
	Deducted     Money
	Released     Money
	Remaining    Money
	ReleaseDate  time.Time
	RuleVersion  int
}

// BalanceView is the merchant's available balance plus per-batch breakdown.
type BalanceView struct {
	MerchantID string
	Available  Money
	Batches    []BatchView
}

func (m Money) validate() error {
	if m <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidAmount, m)
	}
	return nil
}
