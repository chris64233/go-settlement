package settlement

import "time"

// Money 以“分”为最小单位的金额，避免浮点误差。
type Money int64

// IdentityType 收款方税务身份类型，例如居民个人、非居民企业等。
type IdentityType string

// TaxIdentity 收款方税务身份，记录所属地区、身份类型与有效期。
// 同一收款方可存在多个版本，有效期不得由系统自行跨越套用。
type TaxIdentity struct {
	ID        string
	PayeeID   string
	Region    string
	Type      IdentityType
	ValidFrom time.Time // 含当日
	ValidTo   time.Time // 含当日
	Version   int
}

// Contains 判断身份在指定日期是否有效（含边界）。
func (id TaxIdentity) Contains(day time.Time) bool {
	d := day.UTC().Truncate(24 * time.Hour)
	from := id.ValidFrom.UTC().Truncate(24 * time.Hour)
	to := id.ValidTo.UTC().Truncate(24 * time.Hour)
	return !d.Before(from) && !d.After(to)
}

// TaxRateRule 税率规则，按地区、收入类型、金额区间与生效时间发布。
// 税率为万分比（bps），金额区间为 [MinAmount, MaxAmount)，MaxAmount 为 0 表示不设上限。
type TaxRateRule struct {
	ID            string
	Region        string
	IncomeType    string
	MinAmount     Money
	MaxAmount     Money
	RateBps       int64
	EffectiveFrom time.Time // 含当日
	EffectiveTo   time.Time // 含当日
	Version       int
	PublishedAt   time.Time
}

// Effective 判断规则在指定日期是否生效（含边界）。
func (r TaxRateRule) Effective(day time.Time) bool {
	d := day.UTC().Truncate(24 * time.Hour)
	from := r.EffectiveFrom.UTC().Truncate(24 * time.Hour)
	to := r.EffectiveTo.UTC().Truncate(24 * time.Hour)
	return !d.Before(from) && !d.After(to)
}

// Covers 判断毛额是否落入规则金额区间 [Min, Max)。
func (r TaxRateRule) Covers(gross Money) bool {
	if gross < r.MinAmount {
		return false
	}
	return r.MaxAmount == 0 || gross < r.MaxAmount
}

// SettlementLine 结算明细，受理时固定收款方身份版本、收入类型与交易发生日。
type SettlementLine struct {
	LineID          string
	PayeeID         string
	IncomeType      string
	Gross           Money
	TransactionDate time.Time
}

// Settlement 结算单，包含多条共享同一付款的明细。
type Settlement struct {
	ID    string
	Lines []SettlementLine
}

// LineResult 单条明细的扣税结果，Tax + Net 恒等于 Gross。
type LineResult struct {
	LineID          string
	PayeeID         string
	Gross           Money
	Tax             Money
	Net             Money
	IdentityID      string
	IdentityVersion int
	RuleID          string
	RuleVersion     int
	RateBps         int64
}

// WithholdingResult 一次付款确认产生的扣税结果，同一结算仅一份有效结果。
type WithholdingResult struct {
	SettlementID string
	PaymentID    string
	VoucherNo    string
	Lines        []LineResult
	TotalGross   Money
	TotalTax     Money
	TotalNet     Money
	ConfirmedAt  time.Time
}

// CorrectionType 更正方向：补扣或退回。
type CorrectionType string

const (
	CorrectionAdditional CorrectionType = "additional" // 补扣税款
	CorrectionRefund     CorrectionType = "refund"     // 退回税款
)

// Correction 独立更正记录，不覆盖历史扣税结果。
type Correction struct {
	ID           string
	SettlementID string
	VoucherNo    string // 关联原凭证
	Type         CorrectionType
	Amount       Money // 恒为正数
	Reason       string
	CreatedAt    time.Time
}
