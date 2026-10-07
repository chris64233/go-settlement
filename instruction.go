package settlement

import (
	"errors"
	"fmt"
	"time"
)

// Status 表示指令的生命周期状态。
type Status string

const (
	StatusPending  Status = "PENDING"  // 等待释放
	StatusReleased Status = "RELEASED" // 已释放
	StatusExpired  Status = "EXPIRED"  // 超过最晚处理时间
)

// Instruction 是一条结算指令。
type Instruction struct {
	ExternalID    string    `json:"external_id"`    // 外部指令号（幂等键）
	Account       string    `json:"account"`        // 付款账户
	Currency      string    `json:"currency"`       // 币种
	Amount        Amount    `json:"amount"`         // 精确金额
	ValueDate     string    `json:"value_date"`     // 价值日
	Priority      int       `json:"priority"`       // 数值越大优先级越高
	Deadline      time.Time `json:"deadline"`       // 最晚处理时间
	AllowOvertake bool      `json:"allow_overtake"` // 明确允许越过前方资金缺口

	Seq        int64  `json:"seq"`                   // 受理序号，保证稳定排序
	Status     Status `json:"status"`                // 当前状态
	WaitReason string `json:"wait_reason,omitempty"` // 最近一次扫描给出的未释放原因

	ReleasedScanID string `json:"released_scan_id,omitempty"` // 释放它的扫描批次
}

// fingerprint 用于幂等校验的关键内容。
func (in *Instruction) fingerprint() string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%t",
		in.Account, in.Currency, in.Amount.String(), in.ValueDate,
		in.Priority, in.Deadline.UTC().Format(time.RFC3339Nano), in.AllowOvertake)
}

// ErrConflict 表示同一外部指令号提交了不同内容。
var ErrConflict = errors.New("settlement: instruction conflict for external id")

// ConflictError 携带已登记指令信息。
type ConflictError struct {
	ExternalID string
	Existing   Instruction
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: %s", ErrConflict, e.ExternalID)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// ErrVersionConflict 表示扫描期间队列版本发生变化，本次扫描未生效。
var ErrVersionConflict = errors.New("settlement: queue version changed during scan")

// ErrInsufficientBalance 表示入账扣减会导致余额为负。
var ErrInsufficientBalance = errors.New("settlement: balance would become negative")

// ErrNotFound 表示指令不存在。
var ErrNotFound = errors.New("settlement: instruction not found")
