package settlement

import (
	"errors"
	"fmt"
	"time"
)

// ErrConflict 表示幂等键已存在但业务参数（总金额或规则版本）不一致。
var ErrConflict = errors.New("settlement: conflict with existing record")

// Amount 金额一律使用最小货币单位（分）的整数表示，避免浮点舍入误差。
type Amount = int64

// InstructionStatus 子指令状态。
type InstructionStatus string

const (
	StatusPending    InstructionStatus = "PENDING"    // 未发送
	StatusProcessing InstructionStatus = "PROCESSING" // 已发送，等待银行结果
	StatusSuccess    InstructionStatus = "SUCCESS"    // 银行确认成功
	StatusFailed     InstructionStatus = "FAILED"     // 银行确认失败，可重试
)

// Batch 大额结算批次。
type Batch struct {
	BatchNo      string // 批次号（幂等键）
	PayerAccount string // 付款账户
	Payee        string // 收款方
	Currency     string // 币种
	TotalAmount  Amount // 总金额（最小货币单位）
	ValueDate    string // 期望价值日 YYYY-MM-DD
}

// BankRule 银行规则版本。
type BankRule struct {
	Version         string // 规则版本号
	MinPerTx        Amount // 单笔最小额
	MaxPerTx        Amount // 单笔最大额
	DailyRemaining  Amount // 当日剩余可发送额度
	MaxInstructions int    // 每批最多指令数
}

// Validate 校验规则自身合法性。
func (r BankRule) Validate() error {
	if r.Version == "" {
		return errors.New("settlement: rule version is required")
	}
	if r.MinPerTx <= 0 {
		return errors.New("settlement: min per transaction must be positive")
	}
	if r.MaxPerTx < r.MinPerTx {
		return errors.New("settlement: max per transaction must be >= min")
	}
	if r.MaxInstructions <= 0 {
		return errors.New("settlement: max instructions must be positive")
	}
	if r.DailyRemaining < 0 {
		return errors.New("settlement: daily remaining quota must not be negative")
	}
	return nil
}

// Receipt 银行回执。
type Receipt struct {
	InstructionID string
	Attempt       int    // 第几次尝试（重试沿用业务身份，Attempt 递增）
	Result        string // "SUCCESS" 或 "FAILED"
	BankRef       string // 银行流水号
	ReceivedAt    time.Time
}

// Instruction 子指令。InstructionID 是业务身份，重试时保持不变。
type Instruction struct {
	InstructionID string
	PlanNo        string
	BatchNo       string
	Seq           int
	Amount        Amount
	Status        InstructionStatus
	Attempts      int
	Receipts      []Receipt
}

// Plan 拆分方案，冻结批次与规则快照。
type Plan struct {
	PlanNo       string // 方案号（幂等键）
	BatchNo      string
	Batch        Batch    // 冻结的批次快照
	Rule         BankRule // 冻结的规则快照
	Instructions []*Instruction
	Confirmed    bool
	CreatedAt    time.Time
	Rationale    string // 拆分依据说明
}

// TotalAmount 方案内子指令金额合计。
func (p *Plan) TotalAmount() Amount {
	var sum Amount
	for _, ins := range p.Instructions {
		sum += ins.Amount
	}
	return sum
}

// BatchSummary 批次金额核对汇总。
type BatchSummary struct {
	BatchNo          string
	PlanNo           string
	TotalAmount      Amount
	UnsentAmount     Amount // 未发送
	ProcessingAmount Amount // 处理中
	SuccessAmount    Amount // 成功
	FailedAmount     Amount // 失败
	UnsentCount      int
	ProcessingCount  int
	SuccessCount     int
	FailedCount      int
}

// Reconciled 校验各状态金额之和等于批次总额。
func (s BatchSummary) Reconciled() bool {
	return s.UnsentAmount+s.ProcessingAmount+s.SuccessAmount+s.FailedAmount == s.TotalAmount
}

func (s BatchSummary) String() string {
	return fmt.Sprintf("batch=%s total=%d unsent=%d processing=%d success=%d failed=%d",
		s.BatchNo, s.TotalAmount, s.UnsentAmount, s.ProcessingAmount, s.SuccessAmount, s.FailedAmount)
}
