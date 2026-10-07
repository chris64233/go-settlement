package settlement

import (
	"errors"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// 标准规则：0~10000 分 10%，10000 分以上 20%（超额累进）。
func stdRule(region, incomeType string, from, to time.Time) TaxRule {
	return TaxRule{
		Region:     region,
		IncomeType: incomeType,
		Brackets: []Bracket{
			{Lower: 0, Upper: 10000, RateBps: 1000},
			{Lower: 10000, Upper: 0, RateBps: 2000},
		},
		EffectiveFrom: from,
		EffectiveTo:   to,
	}
}

func newEngineWithData(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine()
	if _, err := e.RegisterIdentity(TaxIdentity{
		PartyID: "P1", Region: "CN", Type: IdentityResidentIndividual,
		ValidFrom: day(2026, 1, 1), ValidTo: day(2026, 7, 1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PublishRule(stdRule("CN", "ROYALTY", day(2026, 1, 1), day(2027, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestIdentityValidityBoundary(t *testing.T) {
	e := newEngineWithData(t)
	lines := []Line{{ID: "L1", Gross: 5000}}

	// 生效起点当日可用（左闭）。
	if _, err := e.AcceptSettlement("S-from", "P1", "ROYALTY", day(2026, 1, 1), lines); err != nil {
		t.Fatalf("有效期起点当日应受理成功: %v", err)
	}
	// 截止当日已失效（右开）。
	if _, err := e.AcceptSettlement("S-to", "P1", "ROYALTY", day(2026, 7, 1), lines); !errors.Is(err, ErrNoValidIdentity) {
		t.Fatalf("有效期截止当日应拒绝, got %v", err)
	}
	// 有效期之前不可用，且不得套用其他身份。
	if _, err := e.AcceptSettlement("S-before", "P1", "ROYALTY", day(2025, 12, 31), lines); !errors.Is(err, ErrNoValidIdentity) {
		t.Fatalf("有效期之前应拒绝, got %v", err)
	}
}

func TestNoFallbackToOtherIdentity(t *testing.T) {
	e := NewEngine()
	// 仅登记了 2 月起有效的身份，1 月交易不得套用它。
	if _, err := e.RegisterIdentity(TaxIdentity{
		PartyID: "P1", Region: "CN", Type: IdentityResidentIndividual,
		ValidFrom: day(2026, 2, 1), ValidTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PublishRule(stdRule("CN", "ROYALTY", day(2026, 1, 1), day(2027, 1, 1))); err != nil {
		t.Fatal(err)
	}
	_, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 1, 15), []Line{{ID: "L1", Gross: 1000}})
	if !errors.Is(err, ErrNoValidIdentity) {
		t.Fatalf("缺少有效身份时不得自行套用, got %v", err)
	}
}

func TestRuleEffectiveBoundaryAndSegmentation(t *testing.T) {
	e := newEngineWithData(t)
	// 旧规则 1 月有效（单一 5%），新规则 2 月起生效（标准累进）。
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "SERVICE",
		Brackets:      []Bracket{{Lower: 0, Upper: 0, RateBps: 500}},
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2026, 2, 1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "SERVICE",
		Brackets: []Bracket{
			{Lower: 0, Upper: 10000, RateBps: 1000},
			{Lower: 10000, Upper: 0, RateBps: 2000},
		},
		EffectiveFrom: day(2026, 2, 1), EffectiveTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}

	// 1 月 31 日适用旧规则 5%。
	if _, err := e.AcceptSettlement("S-jan", "P1", "SERVICE", day(2026, 1, 31), []Line{{ID: "L1", Gross: 20000}}); err != nil {
		t.Fatal(err)
	}
	v, err := e.ConfirmPayment("S-jan")
	if err != nil {
		t.Fatal(err)
	}
	if v.RuleVersion != 1 || v.TotalTax != 1000 {
		t.Fatalf("1 月交易应适用旧规则版本 1、税额 1000, got version=%d tax=%d", v.RuleVersion, v.TotalTax)
	}

	// 2 月 1 日（生效边界）适用新规则，分段累进：10000*10% + 10000*20% = 3000。
	if _, err := e.AcceptSettlement("S-feb", "P1", "SERVICE", day(2026, 2, 1), []Line{{ID: "L1", Gross: 20000}}); err != nil {
		t.Fatal(err)
	}
	v, err = e.ConfirmPayment("S-feb")
	if err != nil {
		t.Fatal(err)
	}
	if v.RuleVersion != 2 || v.TotalTax != 3000 {
		t.Fatalf("2 月交易应适用新规则版本 2、税额 3000, got version=%d tax=%d", v.RuleVersion, v.TotalTax)
	}
	if v.TotalTax+v.TotalNet != v.TotalGross {
		t.Fatalf("税额与实付之和必须等于毛额: %d+%d != %d", v.TotalTax, v.TotalNet, v.TotalGross)
	}
}

func TestProgressiveBrackets(t *testing.T) {
	e := newEngineWithData(t)
	// 毛额 25000：10000*10% + 15000*20% = 1000 + 3000 = 4000。
	if _, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 3, 1), []Line{{ID: "L1", Gross: 25000}}); err != nil {
		t.Fatal(err)
	}
	v, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	if v.TotalTax != 4000 || v.TotalNet != 21000 {
		t.Fatalf("累进税额应为 4000、实付 21000, got tax=%d net=%d", v.TotalTax, v.TotalNet)
	}
}

func TestRoundingAllocationAcrossLines(t *testing.T) {
	e := NewEngine()
	if _, err := e.RegisterIdentity(TaxIdentity{
		PartyID: "P1", Region: "CN", Type: IdentityResidentCompany,
		ValidFrom: day(2026, 1, 1), ValidTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	// 单一 33%（3300 万分比），每行 1 分 -> 每行精确税额 0.33 分。
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "DIVIDEND",
		Brackets:      []Bracket{{Lower: 0, Upper: 0, RateBps: 3300}},
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	lines := []Line{{ID: "A", Gross: 1}, {ID: "B", Gross: 1}, {ID: "C", Gross: 1}}
	if _, err := e.AcceptSettlement("S1", "P1", "DIVIDEND", day(2026, 3, 1), lines); err != nil {
		t.Fatal(err)
	}
	v, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	// 合计精确税额 0.99 分 -> 四舍五入为 1 分，按编号升序的稳定规则分给 A。
	if v.TotalTax != 1 {
		t.Fatalf("合计税额应为 1, got %d", v.TotalTax)
	}
	want := map[string]int64{"A": 1, "B": 0, "C": 0}
	for _, r := range v.Lines {
		if r.Tax != want[r.LineID] {
			t.Fatalf("明细 %s 税额应为 %d, got %d", r.LineID, want[r.LineID], r.Tax)
		}
		if r.Tax+r.Net != r.Gross {
			t.Fatalf("明细 %s 税额与实付之和不等于毛额", r.LineID)
		}
	}
	if v.TotalTax+v.TotalNet != v.TotalGross {
		t.Fatal("合计税额与实付之和必须等于毛额")
	}
}

func TestDuplicateConfirmIsIdempotent(t *testing.T) {
	e := newEngineWithData(t)
	if _, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 3, 1), []Line{{ID: "L1", Gross: 5000}}); err != nil {
		t.Fatal(err)
	}
	v1, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	// 确认后发布新税率，再次确认不得改变历史结果。
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "ROYALTY",
		Brackets:      []Bracket{{Lower: 0, Upper: 0, RateBps: 5000}},
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	v2, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatal("重复确认必须返回同一份扣税结果")
	}
	if v2.Number != "WHT-S1" || v2.TotalTax != 500 || v2.RuleVersion != 1 {
		t.Fatalf("历史结果不得被税率更新覆盖, got number=%s tax=%d ruleVersion=%d", v2.Number, v2.TotalTax, v2.RuleVersion)
	}
}

func TestIdentityCorrectionKeepsHistory(t *testing.T) {
	e := newEngineWithData(t)
	if _, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 3, 1), []Line{{ID: "L1", Gross: 5000}}); err != nil {
		t.Fatal(err)
	}
	v1, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	// 身份更正（登记新版本）不得覆盖已生成的凭证。
	if _, err := e.RegisterIdentity(TaxIdentity{
		PartyID: "P1", Region: "CN", Type: IdentityResidentCompany,
		ValidFrom: day(2026, 1, 1), ValidTo: day(2027, 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	v2, err := e.GetVoucher("S1")
	if err != nil {
		t.Fatal(err)
	}
	if v2.IdentityVersion != v1.IdentityVersion || v2.IdentityType != IdentityResidentIndividual {
		t.Fatal("身份更正不得覆盖历史凭证中的身份版本")
	}
	// 通过独立更正记录退回差额。
	c, err := e.AddCorrection("S1", CorrectionRefund, 200, "身份更正退回差额")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CorrectionRefund || c.Amount != 200 {
		t.Fatalf("更正记录内容错误: %+v", c)
	}
	// 退回不得超过累计已扣税额。
	if _, err := e.AddCorrection("S1", CorrectionRefund, 10000, "超额退回"); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("超额退回应被拒绝, got %v", err)
	}
	// 补扣后余额增加，可继续退回。
	if _, err := e.AddCorrection("S1", CorrectionAdditional, 300, "补扣"); err != nil {
		t.Fatal(err)
	}
	cs := e.ListCorrections("S1")
	if len(cs) != 2 {
		t.Fatalf("被拒绝的更正不得落库，应有 2 条更正记录, got %d", len(cs))
	}
	// 历史凭证税额保持 500 不变。
	if v2.TotalTax != 500 {
		t.Fatalf("历史凭证税额不得被更正修改, got %d", v2.TotalTax)
	}
}

func TestSimulateDoesNotPersist(t *testing.T) {
	e := newEngineWithData(t)
	if _, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 3, 1), []Line{{ID: "L1", Gross: 5000}}); err != nil {
		t.Fatal(err)
	}
	v, err := e.SimulateTax("S1")
	if err != nil {
		t.Fatal(err)
	}
	if v.TotalTax != 500 {
		t.Fatalf("试算税额应为 500, got %d", v.TotalTax)
	}
	if _, err := e.GetVoucher("S1"); !errors.Is(err, ErrSettlementNotFound) {
		t.Fatal("试算不得生成凭证")
	}
}

func TestVoucherExposesIdentityAndRuleVersion(t *testing.T) {
	e := newEngineWithData(t)
	if _, err := e.AcceptSettlement("S1", "P1", "ROYALTY", day(2026, 3, 1), []Line{{ID: "L1", Gross: 5000}}); err != nil {
		t.Fatal(err)
	}
	v, err := e.ConfirmPayment("S1")
	if err != nil {
		t.Fatal(err)
	}
	if v.IdentityVersion != 1 || v.IdentityRegion != "CN" || v.IdentityType != IdentityResidentIndividual || v.RuleVersion != 1 {
		t.Fatalf("凭证必须包含身份与税率版本: %+v", v)
	}
}

func TestRuleValidation(t *testing.T) {
	e := NewEngine()
	// 区间不连续。
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "X",
		Brackets:      []Bracket{{Lower: 0, Upper: 100, RateBps: 100}, {Lower: 200, Upper: 0, RateBps: 200}},
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2027, 1, 1),
	}); err == nil {
		t.Fatal("区间不连续应被拒绝")
	}
	// 最后区间必须不封顶。
	if _, err := e.PublishRule(TaxRule{
		Region: "CN", IncomeType: "X",
		Brackets:      []Bracket{{Lower: 0, Upper: 100, RateBps: 100}},
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2027, 1, 1),
	}); err == nil {
		t.Fatal("最后区间封顶应被拒绝")
	}
}
