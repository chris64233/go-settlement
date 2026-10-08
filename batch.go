package settlement

import "time"

// BatchStatus 批次状态。
type BatchStatus string

const (
	// StatusPending 批次已创建但尚未送往银行，可被日历调整重新排期。
	StatusPending BatchStatus = "PENDING"
	// StatusSent 批次已送往银行，价值日永久固定，不再参与重新排期。
	StatusSent BatchStatus = "SENT"
)

// Batch 结算批次。创建时记录命中的日历版本、受理时间和计算出的价值日。
type Batch struct {
	ID              string
	Scope           Scope
	ReceivedAt      time.Time
	CalendarVersion int
	ValueDate       string
	Status          BatchStatus
	SentAt          time.Time
}
