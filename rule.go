package settlement

import (
	"errors"
	"fmt"
	"time"
)

// Bracket 表示一个金额区间 [Lower, Upper) 及其税率（万分比）。
// Upper 为 0 表示上不封顶。金额单位为分。
type Bracket struct {
	Lower   int64
	Upper   int64
	RateBps int64
}

// TaxRule 是一次税率发布的内容，按地区、收入类型、金额区间与生效时间定义。
// 生效时间为左闭右开区间 [EffectiveFrom, EffectiveTo)。
type TaxRule struct {
	Region        string
	IncomeType    string
	Brackets      []Bracket
	EffectiveFrom time.Time
	EffectiveTo   time.Time
}

// RuleVersion 是税率规则的一个不可变版本，每次发布生成一个新版本号。
type RuleVersion struct {
	Version int
	TaxRule
}

// Contains 判断交易发生日是否落在规则生效期内（左闭右开）。
func (v RuleVersion) Contains(day time.Time) bool {
	day = dateOnly(day)
	from := dateOnly(v.EffectiveFrom)
	to := dateOnly(v.EffectiveTo)
	return !day.Before(from) && day.Before(to)
}

func validateRule(rule TaxRule) error {
	if rule.Region == "" || rule.IncomeType == "" {
		return errors.New("税率规则缺少地区或收入类型")
	}
	if !dateOnly(rule.EffectiveFrom).Before(dateOnly(rule.EffectiveTo)) {
		return errors.New("税率规则生效区间无效")
	}
	if len(rule.Brackets) == 0 {
		return errors.New("税率规则至少需要一个金额区间")
	}
	prevUpper := int64(0)
	for i, b := range rule.Brackets {
		if b.RateBps < 0 || b.RateBps > 10000 {
			return fmt.Errorf("第 %d 个区间税率超出 [0, 10000] 万分比范围", i)
		}
		if b.Lower != prevUpper {
			return fmt.Errorf("第 %d 个区间起点 %d 与上一区间终点 %d 不连续", i, b.Lower, prevUpper)
		}
		if b.Upper != 0 && b.Upper <= b.Lower {
			return fmt.Errorf("第 %d 个区间 [%d, %d) 无效", i, b.Lower, b.Upper)
		}
		if b.Upper == 0 && i != len(rule.Brackets)-1 {
			return fmt.Errorf("第 %d 个区间不封顶但必须为最后一个区间", i)
		}
		prevUpper = b.Upper
	}
	if rule.Brackets[0].Lower != 0 {
		return errors.New("第一个区间必须从 0 开始")
	}
	if rule.Brackets[len(rule.Brackets)-1].Upper != 0 {
		return errors.New("最后一个区间必须不封顶")
	}
	return nil
}

// exactTaxNumerator 按超额累进方式计算税额，返回以 1/10000 分为单位的精确分子，
// 即精确税额 = numerator / 10000 分。
func (v RuleVersion) exactTaxNumerator(gross int64) int64 {
	if gross <= 0 {
		return 0
	}
	var total int64
	for _, b := range v.Brackets {
		if gross <= b.Lower {
			break
		}
		upper := b.Upper
		if upper == 0 || gross < upper {
			upper = gross
		}
		total += (upper - b.Lower) * b.RateBps
	}
	return total
}
