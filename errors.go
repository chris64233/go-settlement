package settlement

import "errors"

var (
	ErrTransferNotFound     = errors.New("transfer not found")
	ErrTransferNotSucceeded = errors.New("transfer has not succeeded")
	ErrReturnConflict       = errors.New("bank return no conflicts with existing receipt")
	ErrCurrencyMismatch     = errors.New("currency does not match original transfer")
	ErrReturnAmountExceeded = errors.New("cumulative returned amount exceeds transfer amount")
	ErrAccountNotFound      = errors.New("account not found")
	ErrInsufficientBalance  = errors.New("insufficient available balance")
	ErrReturnNotFound       = errors.New("return receipt not found")
	ErrReversalAmountExceed = errors.New("cumulative reversal exceeds credited amount")
	ErrInvalidAmount        = errors.New("amount must be positive")
)

// ConflictError 表示幂等键相同但关键内容不一致。
type ConflictError struct {
	BankReturnNo string
	Field        string
}

func (e *ConflictError) Error() string {
	return "bank return no " + e.BankReturnNo + " conflicts on field " + e.Field
}

func (e *ConflictError) Is(target error) bool { return target == ErrReturnConflict }
