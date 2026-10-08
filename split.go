package settlement

import "fmt"

// SplitError 拆分不可行错误，生成失败时不保存任何子指令。
type SplitError struct{ Reason string }

func (e *SplitError) Error() string { return "split infeasible: " + e.Reason }

// split 按确定规则把 total 拆成若干子指令金额。
// 规则：指令数取满足单笔上限的最小值 n = ceil(total/max)，
// 再尽量均分（前 total%n 条多一个最小单位），保证：
//   - 每条 ∈ [min, max]
//   - 条数 <= maxInstructions
//   - 总额 <= dailyRemaining
//   - 各条之和精确等于 total
// 同一 (total, rule) 输入永远得到同一输出。
func split(total int64, rule BankRule) ([]int64, error) {
	if total <= 0 {
		return nil, &SplitError{Reason: "total amount must be positive"}
	}
	if rule.MinPerInstruction <= 0 || rule.MaxPerInstruction < rule.MinPerInstruction {
		return nil, &SplitError{Reason: "invalid min/max per instruction"}
	}
	if rule.MaxInstructions <= 0 {
		return nil, &SplitError{Reason: "max instructions must be positive"}
	}
	if total > rule.DailyRemaining {
		return nil, &SplitError{Reason: fmt.Sprintf("total %d exceeds daily remaining quota %d", total, rule.DailyRemaining)}
	}
	// 最少指令数：每条最多 max。
	n := int((total + rule.MaxPerInstruction - 1) / rule.MaxPerInstruction)
	if n > rule.MaxInstructions {
		return nil, &SplitError{Reason: fmt.Sprintf("needs %d instructions, exceeds max %d per batch", n, rule.MaxInstructions)}
	}
	// 可行性：n 条每条至少 min，必须能装下 total。
	if int64(n)*rule.MinPerInstruction > total {
		return nil, &SplitError{Reason: fmt.Sprintf("min per instruction %d x %d instructions exceeds total %d", rule.MinPerInstruction, n, total)}
	}
	base := total / int64(n)
	rem := total % int64(n)
	amounts := make([]int64, n)
	for i := 0; i < n; i++ {
		amounts[i] = base
		if int64(i) < rem {
			amounts[i]++
		}
	}
	return amounts, nil
}
