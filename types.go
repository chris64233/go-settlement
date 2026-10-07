package settlement

import "time"

// TransferStatus 银行转账指令状态。
type TransferStatus string

const (
	TransferStatusPending TransferStatus = "PENDING"
	TransferStatusSuccess TransferStatus = "SUCCESS"
)

// Transfer 银行转账指令。成功确认后状态与回执不可更改、不可删除。
type Transfer struct {
	ID             string
	MerchantID     string
	Amount         int64 // 最小货币单位
	Currency       string
	Status         TransferStatus
	SuccessReceipt string    // 银行成功回执号
	SucceededAt    time.Time // 成功时间
	CreatedAt      time.Time
}

// ReturnReceipt 退汇回执，关联原银行指令。
type ReturnReceipt struct {
	BankReturnNo string // 银行退汇号，全局唯一
	TransferID   string
	Amount       int64
	Currency     string
	Reason       string
	ReceiptAt    time.Time // 银行回执时间
	LedgerID     string    // 入账资金台账
	CreditedAmt  int64     // 已入账金额
	ReversedAmt  int64     // 累计已冲正金额
	CreatedAt    time.Time
}

// Reversal 冲正记录，用于扣回错误退汇。
type Reversal struct {
	ID           string
	BankReturnNo string // 被冲正的退汇号
	Amount       int64
	Reason       string
	LedgerID     string
	CreatedAt    time.Time
}

// LedgerEntry 资金台账，退汇入账与冲正扣回各生成独立台账。
type LedgerEntry struct {
	ID         string
	MerchantID string
	Type       LedgerType
	Amount     int64 // 正数入账，负数扣回
	Currency   string
	TransferID string
	RefNo      string // 退汇号或冲正号
	CreatedAt  time.Time
}

type LedgerType string

const (
	LedgerTypeReturnCredit  LedgerType = "RETURN_CREDIT"
	LedgerTypeReversalDebit LedgerType = "REVERSAL_DEBIT"
)

// Account 业务方账户。
type Account struct {
	MerchantID string
	Currency   string
	Available  int64
	Frozen     int64
}

// TransferView 查询视图：原转账 + 全部退汇 + 冲正关联 + 可退剩余金额。
type TransferView struct {
	Transfer         Transfer
	Returns          []ReturnView
	RemainingAmount  int64
	TotalReturned    int64
	AvailableBalance int64
}

// ReturnView 单笔退汇及其冲正。
type ReturnView struct {
	Receipt   ReturnReceipt
	Reversals []Reversal
}
