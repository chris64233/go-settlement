package settlement

import "time"

// InstructionStatus 银行指令状态。
type InstructionStatus string

const (
	StatusPending      InstructionStatus = "PENDING"       // 已创建，等待银行回执
	StatusRejected     InstructionStatus = "REJECTED"      // 银行明确拒绝，可发起修复
	StatusSucceeded    InstructionStatus = "SUCCEEDED"     // 银行成功，终态
	StatusSuperseded   InstructionStatus = "SUPERSEDED"    // 已被修复生成的新指令取代
	StatusManualReview InstructionStatus = "MANUAL_REVIEW" // 迟到成功回执等异常，需人工处理
)

// RepairStatus 修复申请状态。
type RepairStatus string

const (
	RepairPending   RepairStatus = "PENDING"   // 已申请，待确认
	RepairConfirmed RepairStatus = "CONFIRMED" // 已确认并生成新指令
	RepairVoided    RepairStatus = "VOIDED"    // 作废（原指令已成功或被取消）
)

// ReservationStatus 资金预留状态。
type ReservationStatus string

const (
	ReservationActive   ReservationStatus = "ACTIVE"   // 占用中
	ReservationConsumed ReservationStatus = "CONSUMED" // 指令成功，预留已记账核销
	ReservationReleased ReservationStatus = "RELEASED" // 已释放
)

// ReceiptType 银行回执类型。
type ReceiptType string

const (
	ReceiptSuccess ReceiptType = "SUCCESS"
	ReceiptReject  ReceiptType = "REJECT"
)

// Beneficiary 收款信息。
type Beneficiary struct {
	AccountNo   string
	AccountName string
	BankCode    string
}

// Instruction 结算转账银行指令。指令一旦创建不可修改，
// 修复只会生成关联的新指令。
type Instruction struct {
	ID            string
	RequestNo     string // 银行请求号，幂等键
	Version       int    // 同一原指令链上的版本号，初始为 1
	Amount        int64
	Currency      string
	FundOwner     string // 资金归属
	Beneficiary   Beneficiary
	Status        InstructionStatus
	RejectReason  string
	RepairOf      string // 若非空，表示本指令由该原指令修复生成
	RepairNo      string // 生成本指令的修复号
	ReservationID string
	CreatedAt     time.Time
}

// Repair 修复申请。固定原指令、拒绝原因、原收款信息摘要、
// 新收款信息和原指令版本。
type Repair struct {
	RepairNo         string // 修复号，幂等键
	OriginalID       string // 原指令 ID
	OriginalVersion  int    // 原指令版本
	RejectReason     string // 原指令的拒绝原因
	OldBeneficiaryMD string // 原收款信息摘要
	NewBeneficiary   Beneficiary
	Status           RepairStatus
	NewInstructionID string // 确认后生成的新指令
	NewRequestNo     string // 确认时使用的新银行请求号
	VoidReason       string
	CreatedAt        time.Time
}

// Receipt 银行回执，完整保留，不随修复删除。
type Receipt struct {
	ReceiptID  string // 回执号，幂等键
	RequestNo  string // 对应的银行请求号
	Type       ReceiptType
	Reason     string // 拒绝原因或附言
	ReceivedAt time.Time
}

// Reservation 资金预留。修复确认时原预留直接转移到新指令，
// 不存在先释放再重复占用的中间态。
type Reservation struct {
	ID            string
	InstructionID string // 当前归属的指令
	Amount        int64
	Currency      string
	FundOwner     string
	Status        ReservationStatus
	// TransferTrail 记录预留依次服务过的指令，便于查询去向。
	TransferTrail []string
}

// LedgerEntry 成功记账记录。迟到成功回执不会重复记账。
type LedgerEntry struct {
	InstructionID string
	RequestNo     string
	Amount        int64
	Currency      string
	FundOwner     string
	CreatedAt     time.Time
}

// Exception 需要人工处理的异常。
type Exception struct {
	InstructionID string
	RequestNo     string
	Reason        string
	CreatedAt     time.Time
}

// InstructionView 查询视图：原指令、全部修复版本、银行回执、
// 资金预留去向和人工处理异常。
type InstructionView struct {
	Instruction  Instruction
	Versions     []Instruction // 同一链上的全部版本（含原指令）
	Repairs      []Repair
	Receipts     []Receipt
	Reservations []Reservation
	Exceptions   []Exception
}
