package settlement

import (
	"fmt"
	"sort"
	"time"
)

// Scope 日历适用范围：币种 + 渠道。
type Scope struct {
	Currency string
	Channel  string
}

// Calendar 一个不可变的日历版本。日历的任何调整都会生成新版本，
// 已生效的版本不会被修改或删除。
type Calendar struct {
	Version      int
	Scope        Scope
	Cutoff       string // 每日截点，格式 "15:04"
	Holidays     map[string]bool
	ExtraWorkday map[string]bool
	AdjustmentNo string
	Effective    bool
	CreatedAt    time.Time
}

func dateKey(t time.Time) string {
	return t.Format("2006-01-02")
}

// IsWorkday 判断某天是否为工作日：默认周一到周五为工作日，
// Holidays 中的日期为非工作日，ExtraWorkday 中的周末日期视为工作日。
func (c *Calendar) IsWorkday(t time.Time) bool {
	key := dateKey(t)
	if c.ExtraWorkday[key] {
		return true
	}
	if c.Holidays[key] {
		return false
	}
	wd := t.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

func (c *Calendar) cutoffTime(day time.Time) (time.Time, error) {
	parsed, err := time.ParseInLocation("15:04", c.Cutoff, day.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cutoff %q: %w", c.Cutoff, err)
	}
	return time.Date(day.Year(), day.Month(), day.Day(),
		parsed.Hour(), parsed.Minute(), 0, 0, day.Location()), nil
}

// DateExplanation 解释价值日是如何计算出来的。
type DateExplanation struct {
	CalendarVersion int
	Scope           Scope
	ReceivedAt      time.Time
	Cutoff          string
	ValueDate       string
	Steps           []string
}

// ComputeValueDate 根据受理时间和日历计算价值日，并返回完整的计算依据。
// 规则：受理日为工作日且受理时间不晚于当日截点，则价值日为受理日当天；
// 否则顺延到下一个工作日。
func (c *Calendar) ComputeValueDate(receivedAt time.Time) (string, DateExplanation, error) {
	exp := DateExplanation{
		CalendarVersion: c.Version,
		Scope:           c.Scope,
		ReceivedAt:      receivedAt,
		Cutoff:          c.Cutoff,
	}
	day := time.Date(receivedAt.Year(), receivedAt.Month(), receivedAt.Day(), 0, 0, 0, 0, receivedAt.Location())
	cutoff, err := c.cutoffTime(day)
	if err != nil {
		return "", exp, err
	}
	if c.IsWorkday(day) && !receivedAt.After(cutoff) {
		exp.Steps = append(exp.Steps,
			fmt.Sprintf("%s 是工作日", dateKey(day)),
			fmt.Sprintf("受理时间 %s 不晚于当日截点 %s", receivedAt.Format("15:04:05"), c.Cutoff),
			fmt.Sprintf("价值日取受理日当天 %s", dateKey(day)),
		)
		exp.ValueDate = dateKey(day)
		return exp.ValueDate, exp, nil
	}
	if !c.IsWorkday(day) {
		exp.Steps = append(exp.Steps, fmt.Sprintf("%s 是非工作日，顺延", dateKey(day)))
	} else {
		exp.Steps = append(exp.Steps,
			fmt.Sprintf("受理时间 %s 晚于当日截点 %s，顺延", receivedAt.Format("15:04:05"), c.Cutoff))
	}
	next := day
	for {
		next = next.AddDate(0, 0, 1)
		if c.IsWorkday(next) {
			break
		}
		exp.Steps = append(exp.Steps, fmt.Sprintf("%s 是非工作日，继续顺延", dateKey(next)))
	}
	exp.Steps = append(exp.Steps, fmt.Sprintf("价值日取下一个工作日 %s", dateKey(next)))
	exp.ValueDate = dateKey(next)
	return exp.ValueDate, exp, nil
}

// DiffCalendars 返回两个日历版本之间的差异说明。
func DiffCalendars(oldCal, newCal *Calendar) []string {
	var diffs []string
	if oldCal.Cutoff != newCal.Cutoff {
		diffs = append(diffs, fmt.Sprintf("截点: %s -> %s", oldCal.Cutoff, newCal.Cutoff))
	}
	keys := map[string]bool{}
	for k := range oldCal.Holidays {
		keys[k] = true
	}
	for k := range newCal.Holidays {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		oldH, newH := oldCal.Holidays[k], newCal.Holidays[k]
		oldW, newW := oldCal.ExtraWorkday[k], newCal.ExtraWorkday[k]
		if oldH != newH || oldW != newW {
			diffs = append(diffs, fmt.Sprintf("%s: %s -> %s", k, dayKind(oldH, oldW), dayKind(newH, newW)))
		}
	}
	if len(diffs) == 0 {
		diffs = append(diffs, "无变化")
	}
	return diffs
}

func dayKind(holiday, extraWorkday bool) string {
	switch {
	case extraWorkday:
		return "工作日(调休)"
	case holiday:
		return "非工作日(节假日)"
	default:
		return "默认(按星期)"
	}
}
