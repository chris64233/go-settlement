package settlement

import (
	"errors"
	"fmt"
)

// SplitError 拆分失败原因，整次生成失败，不产生任何子指令。
type SplitError struct {
	Reason string
}

func (e *SplitError) Error() string { return "settlement: cannot split batch: " + e.Reason }

// SplitAmounts 按确定规则把总额拆分为若干子指令金额。
//
// 算法（确定性，相同输入必得相同输出）：
//  1. n = ceil(total / max)，即满足单笔上限所需的最少笔数；
//  2. base = total / n，rem = total % n；
//  3. 前 rem 笔为 base+1，其余为 base，金额按序号从大到小排列；
//  4. 所有金额之和恒等于 total，不丢分毫。
//
// 校验失败时返回 *SplitError：笔数超上限、无法满足单笔最小额、
// 或总额超过当日剩余额度。
func SplitAmounts(total Amount, rule BankRule) ([]Amount, string, error) {
	if err := rule.Validate(); err != nil {
		return nil, "", err
	}
	if total <= 0 {
		return nil, "", errors.New("settlement: total amount must be positive")
	}
	if total > rule.DailyRemaining {
		return nil, "", &SplitError{Reason: fmt.Sprintf(
			"total %d exceeds daily remaining quota %d", total, rule.DailyRemaining)}
	}
	if total < rule.MinPerTx {
		return nil, "", &SplitError{Reason: fmt.Sprintf(
			"total %d is below min per transaction %d", total, rule.MinPerTx)}
	}

	n := int((total + rule.MaxPerTx - 1) / rule.MaxPerTx)
	if n > rule.MaxInstructions {
		return nil, "", &SplitError{Reason: fmt.Sprintf(
			"need %d instructions but max per batch is %d", n, rule.MaxInstructions)}
	}

	base := total / int64(n)
	rem := total % int64(n)
	if base < rule.MinPerTx {
		return nil, "", &SplitError{Reason: fmt.Sprintf(
			"average amount %d over %d instructions is below min per transaction %d",
			base, n, rule.MinPerTx)}
	}

	amounts := make([]Amount, n)
	for i := 0; i < n; i++ {
		amounts[i] = base
		if int64(i) < rem {
			amounts[i] = base + 1
		}
	}

	rationale := fmt.Sprintf(
		"total=%d, rule=%s(min=%d,max=%d,dailyRemaining=%d,maxInstructions=%d): "+
			"n=ceil(%d/%d)=%d, base=%d, first %d instruction(s) carry one extra unit",
		total, rule.Version, rule.MinPerTx, rule.MaxPerTx, rule.DailyRemaining, rule.MaxInstructions,
		total, rule.MaxPerTx, n, base, rem)
	return amounts, rationale, nil
}
