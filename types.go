package settlement

import "time"

// InstructionStatus 子指令生命周期状态。
type InstructionStatus string

const (
	StatusPending    InstructionStatus = "PENDING"    // 未发送
	StatusProcessing InstructionStatus = "PROCESSING" // 已发送，等待银行回执
	StatusSuccess    InstructionStatus = "SUCCESS"    // 银行确认成功
	StatusFailed     InstructionStatus = "FAILED"     // 银行确认失败，可重试
)

// BankRule 银行通道规则。生成方案时整体快照冻结，
// 之后规则版本变化不会回溯影响已生成方案。
type BankRule struct {
	Version           string // 规则版本号
	MinPerInstruction int64  // 单笔最小额（最小货币单位）
	MaxPerInstruction int64  // 单笔最大额（最小货币单位）
	DailyRemaining    int64  // 当日剩余可发送额度
	MaxInstructions   int    // 每批最多指令数
}

// Batch 大额结算批次。
type Batch struct {
	ID           string
	BatchNo      string // 业务批次号，幂等键
	PayerAccount string
	Payee        string
	Currency     string
	TotalAmount  int64 // 总金额（最小货币单位）
	ValueDate    string
	CreatedAt    time.Time
}

// Plan 拆分方案。冻结生成时刻的批次总额与银行规则快照。
type Plan struct {
	ID          string
	PlanNo      string // 方案号，幂等键
	BatchID     string
	TotalAmount int64
	Rule        BankRule // 规则快照
	Rationale   string   // 拆分依据说明
	Confirmed   bool
	CreatedAt   time.Time
}

// Instruction 子指令（一条银行指令）。
type Instruction struct {
	ID            string
	PlanID        string
	BatchID       string
	Seq           int // 方案内序号，从 1 开始
	Amount        int64
	Status        InstructionStatus
	Attempts      int // 发送次数（含重试）
	LastReceiptID string
}

// Receipt 银行回执。
type Receipt struct {
	ID            string
	InstructionID string
	Success       bool
	Code          string
	Message       string
	ReceivedAt    time.Time
}

// BatchSummary 批次金额核对汇总。
type BatchSummary struct {
	BatchID       string
	TotalAmount   int64
	PendingAmount int64
	PendingCount  int
	ProcessingAmt int64
	ProcessingCnt int
	SuccessAmount int64
	SuccessCount  int
	FailedAmount  int64
	FailedCount   int
	// Balanced 为 true 表示各状态金额之和精确等于批次总额。
	Balanced bool
}
