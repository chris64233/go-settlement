package settlement

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// CalendarVersion 是一个不可变的日历版本：记录工作日/非工作日、
// 每日截点以及适用范围（币种 + 渠道）。日历只能通过新版本调整，
// 已生效的版本不得删除。
type CalendarVersion struct {
	ID        string
	Version   int
	Currency  string
	Channel   string
	Cutoff    time.Duration // 每日截点，自当日零点起的偏移
	Holidays  map[string]bool
	Effective bool
	CreatedAt time.Time
}

// IsWorkday 判断某日是否为工作日（周一到周五且非节假日）。
func (c *CalendarVersion) IsWorkday(d time.Time) bool {
	key := dateKey(d)
	if c.Holidays[key] {
		return false
	}
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// ValueDate 根据受理时间计算价值日：
// 受理时间早于或等于当日截点，则候选日为受理当日；否则候选日顺延一天。
// 候选日若不是工作日则顺延到下一个工作日。
func (c *CalendarVersion) ValueDate(acceptedAt time.Time) time.Time {
	candidate := acceptedAt.Truncate(24 * time.Hour)
	if dayOffset(acceptedAt) > c.Cutoff {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for !c.IsWorkday(candidate) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// Explain 返回价值日的推导过程，供查询接口解释日期来源。
func (c *CalendarVersion) Explain(acceptedAt time.Time) Derivation {
	d := Derivation{
		CalendarID: c.ID,
		Version:    c.Version,
		Cutoff:     c.Cutoff.String(),
		AcceptedAt: acceptedAt,
	}
	candidate := acceptedAt.Truncate(24 * time.Hour)
	if dayOffset(acceptedAt) > c.Cutoff {
		d.Steps = append(d.Steps, fmt.Sprintf("受理时间 %s 晚于截点 %s，候选日顺延至 %s",
			dayOffset(acceptedAt), c.Cutoff, dateKey(candidate.AddDate(0, 0, 1))))
		candidate = candidate.AddDate(0, 0, 1)
	} else {
		d.Steps = append(d.Steps, fmt.Sprintf("受理时间 %s 未超过截点 %s，候选日为 %s",
			dayOffset(acceptedAt), c.Cutoff, dateKey(candidate)))
	}
	for !c.IsWorkday(candidate) {
		reason := "周末"
		if c.Holidays[dateKey(candidate)] {
			reason = "节假日"
		}
		d.Steps = append(d.Steps, fmt.Sprintf("%s 为%s，顺延", dateKey(candidate), reason))
		candidate = candidate.AddDate(0, 0, 1)
	}
	d.Steps = append(d.Steps, fmt.Sprintf("价值日确定为 %s", dateKey(candidate)))
	d.ValueDate = candidate
	return d
}

// Derivation 记录一次价值日计算的完整依据。
type Derivation struct {
	CalendarID string
	Version    int
	Cutoff     string
	AcceptedAt time.Time
	Steps      []string
	ValueDate  time.Time
}

func dateKey(t time.Time) string { return t.Format("2006-01-02") }

func dayOffset(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
}

// BatchStatus 批次状态。
type BatchStatus string

const (
	StatusPending BatchStatus = "pending" // 未提交银行
	StatusSent    BatchStatus = "sent"    // 已送银行，价值日冻结
)

// Batch 结算批次。
type Batch struct {
	ID           string
	Currency     string
	Channel      string
	CalendarID   string
	CalVersion   int
	AcceptedAt   time.Time
	ValueDate    time.Time
	Status       BatchStatus
	AdjustmentID string // 最近一次导致价值日迁移的调整号
}

var (
	ErrBatchNotFound     = errors.New("批次不存在")
	ErrCalendarNotFound  = errors.New("日历不存在")
	ErrBatchAlreadySent  = errors.New("批次已送银行")
	ErrAdjustmentExists  = errors.New("调整号已存在且内容不一致")
	ErrPreviewStale      = errors.New("影响清单已失效，请重新生成")
	ErrAdjustmentApplied = errors.New("调整已生效，不得删除")
)

// sortedHolidays 便于对比与展示。
func sortedHolidays(h map[string]bool) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
