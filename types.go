package settlement

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound         = errors.New("settlement: not found")
	ErrConflict         = errors.New("settlement: idempotency conflict")
	ErrInvalidArgument  = errors.New("settlement: invalid argument")
	ErrNotRejected      = errors.New("settlement: instruction is not in rejected state")
	ErrVersionMismatch  = errors.New("settlement: original instruction version mismatch")
	ErrRepairNotPending = errors.New("settlement: repair is not pending")
	ErrRepairObsolete   = errors.New("settlement: original instruction already succeeded, repair obsolete")
)

type InstructionStatus string

const (
	StatusPending   InstructionStatus = "PENDING"
	StatusRejected  InstructionStatus = "REJECTED"
	StatusSucceeded InstructionStatus = "SUCCEEDED"
	StatusRepaired  InstructionStatus = "REPAIRED"
)

type RepairStatus string

const (
	RepairPending   RepairStatus = "PENDING"
	RepairConfirmed RepairStatus = "CONFIRMED"
	RepairCancelled RepairStatus = "CANCELLED"
	RepairObsolete  RepairStatus = "OBSOLETE"
)

type ReceiptResult string

const (
	ReceiptSuccess ReceiptResult = "SUCCESS"
	ReceiptReject  ReceiptResult = "REJECT"
)

type ReservationStatus string

const (
	ReservationHeld     ReservationStatus = "HELD"
	ReservationReleased ReservationStatus = "RELEASED"
)

// Beneficiary 收款信息。修复时仅允许变更这些字段。
type Beneficiary struct {
	AccountNo   string
	AccountName string
	BankCode    string
}

func (b Beneficiary) Summary() string {
	return fmt.Sprintf("%s|%s|%s", b.AccountNo, b.AccountName, b.BankCode)
}

// Instruction 结算转账银行指令。
type Instruction struct {
	ID           string
	RequestNo    string
	Version      int
	Amount       int64
	Currency     string
	FundOwner    string
	Beneficiary  Beneficiary
	Status       InstructionStatus
	RepairOf     string // 若是修复生成的新指令，指向原指令 ID
	RepairedBy   string // 若已被修复，指向新指令 ID
	RejectReason string
	ManualReview bool
	CreatedAt    time.Time
}

// Receipt 银行回执，完整保留不修改。
type Receipt struct {
	ReceiptNo     string
	InstructionID string
	Result        ReceiptResult
	RejectReason  string
	ManualReview  bool
	ReceivedAt    time.Time
}

// Repair 修复申请，固定原指令快照与新收款信息。
type Repair struct {
	RepairNo            string
	InstructionID       string
	RejectReason        string
	OriginalBeneficiary string
	OriginalVersion     int
	NewBeneficiary      Beneficiary
	Status              RepairStatus
	NewInstructionID    string
	CreatedAt           time.Time
}

// Reservation 资金预留。
type Reservation struct {
	ID            string
	InstructionID string
	Amount        int64
	Currency      string
	FundOwner     string
	Status        ReservationStatus
	MigratedFrom  string // 来源指令 ID（修复迁移时记录）
}

// Exception 需要人工处理的异常。
type Exception struct {
	InstructionID string
	ReceiptNo     string
	Reason        string
	At            time.Time
}

// InstructionDetail 查询视图。
type InstructionDetail struct {
	Instruction  Instruction
	Repairs      []Repair
	Receipts     []Receipt
	Reservations []Reservation
	Exceptions   []Exception
}
