package settlement

import (
	"errors"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func newServiceWithBasics(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	err := s.RegisterIdentity(TaxIdentity{
		ID: "id-1", PayeeID: "payee-1", Region: "CN-SH", Type: "resident",
		ValidFrom: day(2026, 1, 1), ValidTo: day(2026, 12, 31), Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.PublishRateRule(TaxRateRule{
		ID: "rule-1", Region: "CN-SH", IncomeType: "service",
		MinAmount: 0, MaxAmount: 1000000, RateBps: 1000, // 10%
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2026, 12, 31), Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.PublishRateRule(TaxRateRule{
		ID: "rule-2", Region: "CN-SH", IncomeType: "service",
		MinAmount: 1000000, MaxAmount: 0, RateBps: 2000, // 20%
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2026, 12, 31), Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAcceptSettlementFixesIdentityAtTransactionDate(t *testing.T) {
	s := newServiceWithBasics(t)
	st := Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 6, 1)},
	}}
	if err := s.AcceptSettlement(st); err != nil {
		t.Fatal(err)
	}
	res, err := s.TrialCalculate("st-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Lines[0].IdentityID != "id-1" || res.Lines[0].IdentityVersion != 1 {
		t.Fatalf("unexpected identity snapshot: %+v", res.Lines[0])
	}
}

func TestIdentityValidityBoundary(t *testing.T) {
	s := newServiceWithBasics(t)
	// 有效期最后一天：有效
	if err := s.AcceptSettlement(Settlement{ID: "st-in", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 12, 31)},
	}}); err != nil {
		t.Fatalf("boundary day should be valid: %v", err)
	}
	// 有效期次日：不得套用其他身份
	err := s.AcceptSettlement(Settlement{ID: "st-out", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2027, 1, 1)},
	}})
	if !errors.Is(err, ErrNoValidIdentity) {
		t.Fatalf("want ErrNoValidIdentity, got %v", err)
	}
}

func TestNoIdentityFallbackForbidden(t *testing.T) {
	s := newServiceWithBasics(t)
	err := s.AcceptSettlement(Settlement{ID: "st-x", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "unknown-payee", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 6, 1)},
	}})
	if !errors.Is(err, ErrNoValidIdentity) {
		t.Fatalf("want ErrNoValidIdentity, got %v", err)
	}
}

func TestRateRuleBrackets(t *testing.T) {
	s := newServiceWithBasics(t)
	// 区间下边界：999999 分适用 10%
	if err := s.AcceptSettlement(Settlement{ID: "st-low", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 999999, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	// 区间上边界：1000000 分适用 20%
	if err := s.AcceptSettlement(Settlement{ID: "st-high", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 1000000, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	low, _ := s.TrialCalculate("st-low")
	high, _ := s.TrialCalculate("st-high")
	if low.Lines[0].RuleID != "rule-1" || low.Lines[0].Tax != 100000 {
		t.Fatalf("low bracket wrong: %+v", low.Lines[0])
	}
	if high.Lines[0].RuleID != "rule-2" || high.Lines[0].Tax != 200000 {
		t.Fatalf("high bracket wrong: %+v", high.Lines[0])
	}
}

func TestRuleEffectiveBoundary(t *testing.T) {
	s := newServiceWithBasics(t)
	// 补充覆盖 2025 年的身份版本，隔离出“规则未生效”这一变量
	if err := s.RegisterIdentity(TaxIdentity{
		ID: "id-0", PayeeID: "payee-1", Region: "CN-SH", Type: "resident",
		ValidFrom: day(2025, 1, 1), ValidTo: day(2025, 12, 31), Version: 0,
	}); err != nil {
		t.Fatal(err)
	}
	// 规则生效前一日：无可用规则
	if err := s.AcceptSettlement(Settlement{ID: "st-pre", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2025, 12, 31)},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrialCalculate("st-pre"); !errors.Is(err, ErrNoRateRule) {
		t.Fatalf("want ErrNoRateRule, got %v", err)
	}
}

func TestRoundingAllocationStable(t *testing.T) {
	s := NewService()
	if err := s.RegisterIdentity(TaxIdentity{
		ID: "id-1", PayeeID: "p1", Region: "R", Type: "resident",
		ValidFrom: day(2026, 1, 1), ValidTo: day(2026, 12, 31), Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 33.33% 税率，三条 1 分明细共享一个付款：分组税额 round(3*3333/10000)=1 分
	if err := s.PublishRateRule(TaxRateRule{
		ID: "r1", Region: "R", IncomeType: "svc", MinAmount: 0, MaxAmount: 0, RateBps: 3333,
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2026, 12, 31), Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	st := Settlement{ID: "st-r", Lines: []SettlementLine{
		{LineID: "A", PayeeID: "p1", IncomeType: "svc", Gross: 1, TransactionDate: day(2026, 6, 1)},
		{LineID: "B", PayeeID: "p1", IncomeType: "svc", Gross: 1, TransactionDate: day(2026, 6, 1)},
		{LineID: "C", PayeeID: "p1", IncomeType: "svc", Gross: 1, TransactionDate: day(2026, 6, 1)},
	}}
	if err := s.AcceptSettlement(st); err != nil {
		t.Fatal(err)
	}
	res, err := s.ConfirmPayment("st-r", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalTax != 1 {
		t.Fatalf("total tax = %d, want 1", res.TotalTax)
	}
	if res.TotalTax+res.TotalNet != res.TotalGross {
		t.Fatalf("tax+net != gross: %+v", res)
	}
	// 余数相同，按 LineID 字典序稳定分配：A 得 1 分
	for _, lr := range res.Lines {
		if lr.Tax+lr.Net != lr.Gross {
			t.Fatalf("line imbalance: %+v", lr)
		}
		if lr.LineID == "A" && lr.Tax != 1 {
			t.Fatalf("stable allocation expected A to get the cent: %+v", res.Lines)
		}
		if lr.LineID != "A" && lr.Tax != 0 {
			t.Fatalf("unexpected tax on %s: %+v", lr.LineID, res.Lines)
		}
	}
}

func TestConfirmPaymentIdempotent(t *testing.T) {
	s := newServiceWithBasics(t)
	if err := s.AcceptSettlement(Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 12345, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	r1, err := s.ConfirmPayment("st-1", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.ConfirmPayment("st-1", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.VoucherNo != r2.VoucherNo || r1.TotalTax != r2.TotalTax {
		t.Fatalf("duplicate confirm produced different result: %v vs %v", r1.VoucherNo, r2.VoucherNo)
	}
	if r1.TotalTax != 1235 || r1.TotalNet != 11110 { // round(12345*0.1)=1235（四舍五入）
		t.Fatalf("unexpected amounts: tax=%d net=%d", r1.TotalTax, r1.TotalNet)
	}
}

func TestIdentityAndRuleUpdatesDoNotOverwriteHistory(t *testing.T) {
	s := newServiceWithBasics(t)
	if err := s.AcceptSettlement(Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.ConfirmPayment("st-1", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	// 发布更高版本身份与税率（不影响历史结果）
	if err := s.RegisterIdentity(TaxIdentity{
		ID: "id-2", PayeeID: "payee-1", Region: "CN-BJ", Type: "non_resident",
		ValidFrom: day(2026, 1, 1), ValidTo: day(2026, 12, 31), Version: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRateRule(TaxRateRule{
		ID: "rule-3", Region: "CN-SH", IncomeType: "service",
		MinAmount: 0, MaxAmount: 1000000, RateBps: 500,
		EffectiveFrom: day(2026, 1, 1), EffectiveTo: day(2026, 12, 31), Version: 2,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.ConfirmPayment("st-1", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalTax != before.TotalTax || after.VoucherNo != before.VoucherNo {
		t.Fatalf("history overwritten: before=%+v after=%+v", before, after)
	}
	if after.Lines[0].IdentityVersion != 1 || after.Lines[0].RuleVersion != 1 {
		t.Fatalf("voucher lost original versions: %+v", after.Lines[0])
	}
}

func TestCorrections(t *testing.T) {
	s := newServiceWithBasics(t)
	if err := s.AcceptSettlement(Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	// 未确认不得更正
	if _, err := s.AddCorrection("st-1", CorrectionAdditional, 100, "rate fix"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	v, err := s.ConfirmPayment("st-1", "pay-1")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := s.AddCorrection("st-1", CorrectionAdditional, 50, "补扣：税率更正")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.AddCorrection("st-1", CorrectionRefund, 20, "退回：身份更正")
	if err != nil {
		t.Fatal(err)
	}
	if c1.VoucherNo != v.VoucherNo || c2.VoucherNo != v.VoucherNo {
		t.Fatalf("corrections not linked to voucher: %v %v", c1.VoucherNo, c2.VoucherNo)
	}
	list := s.ListCorrections("st-1")
	if len(list) != 2 || list[0].ID != c1.ID || list[1].ID != c2.ID {
		t.Fatalf("unexpected corrections: %+v", list)
	}
	// 历史结果保持不变
	again, _ := s.GetVoucher("st-1")
	if again.TotalTax != v.TotalTax {
		t.Fatalf("correction modified history: %d -> %d", v.TotalTax, again.TotalTax)
	}
	if _, err := s.AddCorrection("st-1", CorrectionRefund, -1, "bad"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("want ErrInvalidAmount, got %v", err)
	}
}

func TestVoucherShowsIdentityAndRuleVersions(t *testing.T) {
	s := newServiceWithBasics(t)
	if err := s.AcceptSettlement(Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 50000, TransactionDate: day(2026, 3, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.ConfirmPayment("st-1", "pay-9")
	if err != nil {
		t.Fatal(err)
	}
	if res.VoucherNo == "" {
		t.Fatal("missing voucher number")
	}
	lr := res.Lines[0]
	if lr.IdentityID != "id-1" || lr.IdentityVersion != 1 || lr.RuleID != "rule-1" || lr.RuleVersion != 1 {
		t.Fatalf("voucher missing version trace: %+v", lr)
	}
}

func TestTrialCalculateDoesNotConfirm(t *testing.T) {
	s := newServiceWithBasics(t)
	if err := s.AcceptSettlement(Settlement{ID: "st-1", Lines: []SettlementLine{
		{LineID: "L1", PayeeID: "payee-1", IncomeType: "service", Gross: 10000, TransactionDate: day(2026, 6, 1)},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrialCalculate("st-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVoucher("st-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trial must not create voucher, got %v", err)
	}
}
