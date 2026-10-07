package settlement

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrNoValidIdentity    = errors.New("交易发生日不存在有效的收款方税务身份")
	ErrNoApplicableRule   = errors.New("交易发生日不存在适用的税率规则")
	ErrAlreadyConfirmed   = errors.New("该结算已确认付款，存在有效扣税结果")
	ErrSettlementNotFound = errors.New("结算不存在")
	ErrInvalidCorrection  = errors.New("更正金额不合法")
)

// Line 是结算的一条付款明细，金额为毛额（分）。
type Line struct {
	ID    string
	Gross int64
}

// LineResult 是单条明细的扣税结果。
type LineResult struct {
	LineID string
	Gross  int64
	Tax    int64
	Net    int64
}

// Settlement 是已受理的结算，受理时固定身份版本、收入类型与交易发生日。
type Settlement struct {
	ID              string
	PartyID         string
	IncomeType      string
	TransactionDate time.Time
	IdentityVersion int
	Lines           []Line
}

// Voucher 是完税凭证，记录所采用的身份与税率版本，生成后不可变。
type Voucher struct {
	Number          string
	SettlementID    string
	PartyID         string
	IdentityVersion int
	IdentityRegion  string
	IdentityType    IdentityType
	RuleVersion     int
	IncomeType      string
	TransactionDate time.Time
	Lines           []LineResult
	TotalGross      int64
	TotalTax        int64
	TotalNet        int64
	ConfirmedAt     time.Time
}

// CorrectionKind 表示更正方向：补扣或退回。
type CorrectionKind string

const (
	CorrectionAdditional CorrectionKind = "ADDITIONAL"
	CorrectionRefund     CorrectionKind = "REFUND"
)

// Correction 是一条独立的更正记录，不修改历史扣税结果。
type Correction struct {
	ID           string
	SettlementID string
	Kind         CorrectionKind
	Amount       int64
	Reason       string
	CreatedAt    time.Time
}

// Engine 提供身份登记、税率发布、税额试算、付款确认与查询能力。
type Engine struct {
	identities  map[string][]IdentityVersion
	rules       map[string][]RuleVersion
	settlements map[string]*Settlement
	vouchers    map[string]*Voucher
	corrections map[string][]Correction
	corrSeq     int
	now         func() time.Time
}

func NewEngine() *Engine {
	return &Engine{
		identities:  make(map[string][]IdentityVersion),
		rules:       make(map[string][]RuleVersion),
		settlements: make(map[string]*Settlement),
		vouchers:    make(map[string]*Voucher),
		corrections: make(map[string][]Correction),
		now:         time.Now,
	}
}

func ruleKey(region, incomeType string) string { return region + "|" + incomeType }

// RegisterIdentity 登记收款方税务身份，返回新版本号（同一收款方自 1 起递增）。
func (e *Engine) RegisterIdentity(id TaxIdentity) (int, error) {
	if id.PartyID == "" || id.Region == "" || id.Type == "" {
		return 0, errors.New("身份缺少收款方、地区或类型")
	}
	if !dateOnly(id.ValidFrom).Before(dateOnly(id.ValidTo)) {
		return 0, errors.New("身份有效期区间无效")
	}
	versions := e.identities[id.PartyID]
	v := IdentityVersion{Version: len(versions) + 1, TaxIdentity: id}
	e.identities[id.PartyID] = append(versions, v)
	return v.Version, nil
}

// PublishRule 发布税率规则，返回新版本号（同一地区+收入类型自 1 起递增）。
func (e *Engine) PublishRule(rule TaxRule) (int, error) {
	if err := validateRule(rule); err != nil {
		return 0, err
	}
	key := ruleKey(rule.Region, rule.IncomeType)
	versions := e.rules[key]
	v := RuleVersion{Version: len(versions) + 1, TaxRule: rule}
	e.rules[key] = append(versions, v)
	return v.Version, nil
}

// identityOn 返回交易发生日当天有效的身份版本；不存在时返回错误，不套用其他身份。
func (e *Engine) identityOn(partyID string, day time.Time) (IdentityVersion, error) {
	var found *IdentityVersion
	for i := range e.identities[partyID] {
		v := &e.identities[partyID][i]
		if v.Contains(day) {
			if found == nil || v.Version > found.Version {
				found = v
			}
		}
	}
	if found == nil {
		return IdentityVersion{}, ErrNoValidIdentity
	}
	return *found, nil
}

// ruleOn 返回交易发生日当天生效的税率版本；多个版本重叠时取版本号最高者。
func (e *Engine) ruleOn(region, incomeType string, day time.Time) (RuleVersion, error) {
	var found *RuleVersion
	for i := range e.rules[ruleKey(region, incomeType)] {
		v := &e.rules[ruleKey(region, incomeType)][i]
		if v.Contains(day) {
			if found == nil || v.Version > found.Version {
				found = v
			}
		}
	}
	if found == nil {
		return RuleVersion{}, ErrNoApplicableRule
	}
	return *found, nil
}

// AcceptSettlement 受理结算，固定收款方身份版本、收入类型与交易发生日。
func (e *Engine) AcceptSettlement(id, partyID, incomeType string, txnDate time.Time, lines []Line) (*Settlement, error) {
	if id == "" || partyID == "" || incomeType == "" {
		return nil, errors.New("结算缺少编号、收款方或收入类型")
	}
	if _, exists := e.settlements[id]; exists {
		return nil, fmt.Errorf("结算 %s 已存在", id)
	}
	if len(lines) == 0 {
		return nil, errors.New("结算至少需要一条明细")
	}
	seen := make(map[string]bool)
	for _, l := range lines {
		if l.ID == "" || seen[l.ID] {
			return nil, fmt.Errorf("明细编号 %q 为空或重复", l.ID)
		}
		if l.Gross <= 0 {
			return nil, fmt.Errorf("明细 %s 毛额必须为正", l.ID)
		}
		seen[l.ID] = true
	}
	identity, err := e.identityOn(partyID, txnDate)
	if err != nil {
		return nil, err
	}
	if _, err := e.ruleOn(identity.Region, incomeType, txnDate); err != nil {
		return nil, err
	}
	s := &Settlement{
		ID:              id,
		PartyID:         partyID,
		IncomeType:      incomeType,
		TransactionDate: dateOnly(txnDate),
		IdentityVersion: identity.Version,
		Lines:           append([]Line(nil), lines...),
	}
	e.settlements[id] = s
	cp := *s
	cp.Lines = append([]Line(nil), lines...)
	return &cp, nil
}

// allocateTax 按最大余数法把税额总额分配到各明细：
// 先取每行精确税额的整数部分，剩余差额按小数余数降序、明细编号升序（稳定规则）逐分补齐。
func allocateTax(lines []Line, rule RuleVersion) []LineResult {
	type rem struct {
		idx  int
		frac int64
	}
	results := make([]LineResult, len(lines))
	rems := make([]rem, len(lines))
	var totalNum, baseSum int64
	for i, l := range lines {
		num := rule.exactTaxNumerator(l.Gross)
		floor := num / 10000
		results[i] = LineResult{LineID: l.ID, Gross: l.Gross, Tax: floor}
		rems[i] = rem{idx: i, frac: num % 10000}
		totalNum += num
		baseSum += floor
	}
	target := (totalNum + 5000) / 10000
	sort.SliceStable(rems, func(a, b int) bool {
		if rems[a].frac != rems[b].frac {
			return rems[a].frac > rems[b].frac
		}
		return lines[rems[a].idx].ID < lines[rems[b].idx].ID
	})
	for i := int64(0); i < target-baseSum; i++ {
		results[rems[i%int64(len(rems))].idx].Tax++
	}
	for i := range results {
		results[i].Net = results[i].Gross - results[i].Tax
	}
	return results
}

func (e *Engine) compute(s *Settlement) (*Voucher, error) {
	var identity *IdentityVersion
	for i := range e.identities[s.PartyID] {
		if e.identities[s.PartyID][i].Version == s.IdentityVersion {
			identity = &e.identities[s.PartyID][i]
		}
	}
	if identity == nil {
		return nil, ErrNoValidIdentity
	}
	rule, err := e.ruleOn(identity.Region, s.IncomeType, s.TransactionDate)
	if err != nil {
		return nil, err
	}
	results := allocateTax(s.Lines, rule)
	v := &Voucher{
		Number:          "WHT-" + s.ID,
		SettlementID:    s.ID,
		PartyID:         s.PartyID,
		IdentityVersion: identity.Version,
		IdentityRegion:  identity.Region,
		IdentityType:    identity.Type,
		RuleVersion:     rule.Version,
		IncomeType:      s.IncomeType,
		TransactionDate: s.TransactionDate,
		Lines:           results,
	}
	for _, r := range results {
		v.TotalGross += r.Gross
		v.TotalTax += r.Tax
		v.TotalNet += r.Net
	}
	return v, nil
}

// SimulateTax 税额试算：按受理时固定的身份与交易发生日计算，不产生凭证、不落库。
func (e *Engine) SimulateTax(settlementID string) (*Voucher, error) {
	s, ok := e.settlements[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	return e.compute(s)
}

// ConfirmPayment 确认付款：生成唯一扣税结果与凭证编号。
// 重复确认返回既有结果，保证同一结算只有一份有效扣税结果。
func (e *Engine) ConfirmPayment(settlementID string) (*Voucher, error) {
	s, ok := e.settlements[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	if v, done := e.vouchers[settlementID]; done {
		return v, nil
	}
	v, err := e.compute(s)
	if err != nil {
		return nil, err
	}
	v.ConfirmedAt = e.now()
	e.vouchers[settlementID] = v
	return v, nil
}

// GetVoucher 查询结算的完税凭证。
func (e *Engine) GetVoucher(settlementID string) (*Voucher, error) {
	if v, ok := e.vouchers[settlementID]; ok {
		return v, nil
	}
	return nil, ErrSettlementNotFound
}

// AddCorrection 为已确认的结算追加独立更正记录（补扣或退回），历史结果不被覆盖。
func (e *Engine) AddCorrection(settlementID string, kind CorrectionKind, amount int64, reason string) (*Correction, error) {
	v, ok := e.vouchers[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	if amount <= 0 {
		return nil, ErrInvalidCorrection
	}
	if kind != CorrectionAdditional && kind != CorrectionRefund {
		return nil, ErrInvalidCorrection
	}
	if kind == CorrectionRefund {
		var balance int64 = v.TotalTax
		for _, c := range e.corrections[settlementID] {
			if c.Kind == CorrectionAdditional {
				balance += c.Amount
			} else {
				balance -= c.Amount
			}
		}
		if amount > balance {
			return nil, fmt.Errorf("%w: 退回金额 %d 超过累计已扣税额 %d", ErrInvalidCorrection, amount, balance)
		}
	}
	e.corrSeq++
	c := Correction{
		ID:           fmt.Sprintf("CORR-%s-%d", settlementID, e.corrSeq),
		SettlementID: settlementID,
		Kind:         kind,
		Amount:       amount,
		Reason:       reason,
		CreatedAt:    e.now(),
	}
	e.corrections[settlementID] = append(e.corrections[settlementID], c)
	return &c, nil
}

// ListCorrections 查询结算的全部更正记录（按追加顺序）。
func (e *Engine) ListCorrections(settlementID string) []Correction {
	return append([]Correction(nil), e.corrections[settlementID]...)
}
