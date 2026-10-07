package settlement

import (
	"errors"
	"time"
)

var (
	ErrTransferNotFound    = errors.New("settlement: transfer not found")
	ErrTransferExists      = errors.New("settlement: transfer already exists")
	ErrTransferNotSuccess  = errors.New("settlement: transfer is not in success state")
	ErrAccountNotFound     = errors.New("settlement: account not found")
	ErrInsufficientBalance = errors.New("settlement: insufficient available balance")
	ErrInvalidAmount       = errors.New("settlement: amount must be positive")
	ErrCurrencyMismatch    = errors.New("settlement: currency does not match original transfer")
	ErrReturnExceeded      = errors.New("settlement: cumulative returned amount exceeds transfer amount")
	ErrReturnConflict      = errors.New("settlement: bank return number reused with different content")
	ErrReturnNotFound      = errors.New("settlement: return receipt not found")
	ErrReversalExceeded    = errors.New("settlement: cumulative reversal exceeds credited amount")
	ErrReversalConflict    = errors.New("settlement: reversal number reused with different content")
	ErrFrozenExceeded      = errors.New("settlement: freeze amount exceeds available balance")
)

// TransferStatus 银行转账指令状态。
type TransferStatus string

const (
	TransferStatusPending TransferStatus = "PENDING"
	TransferStatusSuccess TransferStatus = "SUCCESS"
)

// Transfer 银行转账指令。结算成功后状态与成功回执不可变。
type Transfer struct {
	ID             string
	AccountID      string
	Amount         int64
	Currency       string
	Status         TransferStatus
	BankReceiptNo  string
	SuccessAt      time.Time
	ReturnedAmount int64
}

// RemainingReturnable 当前可退剩余金额。
func (t *Transfer) RemainingReturnable() int64 {
	return t.Amount - t.ReturnedAmount
}

// ReturnReceipt 银行退汇回执，关联原银行指令。
type ReturnReceipt struct {
	BankReturnNo string
	TransferID   string
	Amount       int64
	Currency     string
	Reason       string
	ReceiptAt    time.Time
	ReversedAmt  int64
}

// CreditedAmount 该退汇当前仍有效的入账金额（扣除冲正后）。
func (r *ReturnReceipt) CreditedAmount() int64 {
	return r.Amount - r.ReversedAmt
}

// Reversal 退汇冲正记录，通过独立冲正扣回错误入账。
type Reversal struct {
	ReversalNo   string
	BankReturnNo string
	Amount       int64
	Reason       string
	CreatedAt    time.Time
}

// LedgerType 资金台账类型。
type LedgerType string

const (
	LedgerTransferOut LedgerType = "TRANSFER_OUT"
	LedgerReturnIn    LedgerType = "RETURN_IN"
	LedgerReversalOut LedgerType = "REVERSAL_OUT"
	LedgerFreeze      LedgerType = "FREEZE"
	LedgerUnfreeze    LedgerType = "UNFREEZE"
	LedgerWithdraw    LedgerType = "WITHDRAW"
	LedgerDeposit     LedgerType = "DEPOSIT"
)

// LedgerEntry 资金台账，每笔余额变化生成独立条目。
type LedgerEntry struct {
	Seq          int64
	Type         LedgerType
	AccountID    string
	TransferID   string
	BankReturnNo string
	ReversalNo   string
	Amount       int64
	Currency     string
	BalanceAfter int64
	CreatedAt    time.Time
}

// Account 业务方账户。
type Account struct {
	ID        string
	Available int64
	Frozen    int64
	Currency  string
}

// TransferDetail 查询视图：原转账、全部退汇、冲正与台账。
type TransferDetail struct {
	Transfer  Transfer
	Returns   []ReturnReceipt
	Reversals []Reversal
	Ledger    []LedgerEntry
	Remaining int64
}
