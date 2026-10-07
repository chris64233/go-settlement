package settlement

import "time"

// InstructionStatus 表示资金指令的生命周期状态。
type InstructionStatus string

const (
	// StatusReserved 指令已受理，额度处于占用（预留）状态。
	StatusReserved InstructionStatus = "RESERVED"
	// StatusSettled 指令已结算完成，占用转为已用额度。
	StatusSettled InstructionStatus = "SETTLED"
	// StatusCancelled 指令已取消，占用已释放。
	StatusCancelled InstructionStatus = "CANCELLED"
	// StatusFailed 指令失败，占用已释放。
	StatusFailed InstructionStatus = "FAILED"
)

// Instruction 是已受理的资金指令，受理时固定付款方、收款方、
// 金额、价值日和所采用的限额版本。
type Instruction struct {
	ID           string
	RequestID    string
	Payer        string
	Payee        string
	Currency     string
	Amount       int64
	ValueDate    time.Time
	PayerVersion int
	PayeeVersion int
	Status       InstructionStatus
	AcceptedAt   time.Time
}

// UsageEntry 是参与方与对手方之间一条逐笔占用明细。
type UsageEntry struct {
	InstructionID string
	Participant   string
	Counterparty  string
	Currency      string
	ValueDate     time.Time
	// Outgoing / Incoming 为该参与方视角下的流出、流入金额。
	Outgoing int64
	Incoming int64
	// Reserved 为预留状态金额，Used 为已结算转为已用额度的金额。
	Reserved int64
	Used     int64
}

// UsageSummary 是按参与方、对手方、币种、价值日聚合的占用视图。
type UsageSummary struct {
	Participant  string
	Counterparty string
	Currency     string
	ValueDate    time.Time
	// ReservedOutgoing / ReservedIncoming 为预留状态的双向金额。
	ReservedOutgoing int64
	ReservedIncoming int64
	// UsedOutgoing / UsedIncoming 为已用额度的双向金额。
	UsedOutgoing int64
	UsedIncoming int64
}

// NetExposure 返回该参与方视角下仍计入风险敞口的净额
// （预留 + 已用，流出减流入）。
func (s UsageSummary) NetExposure() int64 {
	return s.ReservedOutgoing + s.UsedOutgoing - s.ReservedIncoming - s.UsedIncoming
}
