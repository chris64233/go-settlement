package settlement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrNoValidIdentity  = errors.New("settlement: no valid tax identity for payee at transaction date")
	ErrNoRateRule       = errors.New("settlement: no effective tax rate rule")
	ErrSettlementExists = errors.New("settlement: settlement already exists")
	ErrNotFound         = errors.New("settlement: settlement not found")
	ErrInvalidAmount    = errors.New("settlement: invalid amount")
)

// Service 提供身份登记、税率发布、税额试算、付款确认、凭证与更正查询。
// 所有状态保存在内存中，互斥锁保证并发安全。
type Service struct {
	mu          sync.Mutex
	identities  []*TaxIdentity
	rules       []*TaxRateRule
	settlements map[string]*Settlement
	results     map[string]*WithholdingResult // settlementID -> 唯一有效结果
	corrections []*Correction
	voucherSeq  int
	corrSeq     int
}

func NewService() *Service {
	return &Service{
		settlements: make(map[string]*Settlement),
		results:     make(map[string]*WithholdingResult),
	}
}

// RegisterIdentity 登记收款方税务身份，版本号由调用方显式指定。
func (s *Service) RegisterIdentity(id TaxIdentity) error {
	if id.PayeeID == "" || id.Region == "" || id.ID == "" {
		return errors.New("settlement: identity requires id, payee and region")
	}
	if id.ValidTo.Before(id.ValidFrom) {
		return errors.New("settlement: identity valid_to before valid_from")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := id
	s.identities = append(s.identities, &cp)
	return nil
}

// PublishRateRule 发布税率规则。
func (s *Service) PublishRateRule(r TaxRateRule) error {
	if r.ID == "" || r.Region == "" || r.IncomeType == "" {
		return errors.New("settlement: rule requires id, region and income type")
	}
	if r.RateBps < 0 || r.RateBps > 10000 {
		return errors.New("settlement: rate out of range")
	}
	if r.EffectiveTo.Before(r.EffectiveFrom) {
		return errors.New("settlement: rule effective_to before effective_from")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := r
	s.rules = append(s.rules, &cp)
	return nil
}

// AcceptSettlement 受理结算：为每条明细固定收款方身份版本、收入类型与交易发生日。
// 交易发生日缺少有效身份时直接报错，不得套用其他身份。
func (s *Service) AcceptSettlement(st Settlement) error {
	if st.ID == "" || len(st.Lines) == 0 {
		return errors.New("settlement: settlement requires id and lines")
	}
	for _, ln := range st.Lines {
		if ln.Gross <= 0 {
			return ErrInvalidAmount
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settlements[st.ID]; ok {
		return ErrSettlementExists
	}
	for _, ln := range st.Lines {
		if _, err := s.findIdentityLocked(ln.PayeeID, ln.TransactionDate); err != nil {
			return fmt.Errorf("line %s: %w", ln.LineID, err)
		}
	}
	cp := st
	cp.Lines = append([]SettlementLine(nil), st.Lines...)
	s.settlements[st.ID] = &cp
	return nil
}

// findIdentityLocked 取交易发生日有效的身份；存在多个时取版本最高者。
func (s *Service) findIdentityLocked(payeeID string, day time.Time) (*TaxIdentity, error) {
	var best *TaxIdentity
	for _, id := range s.identities {
		if id.PayeeID == payeeID && id.Contains(day) {
			if best == nil || id.Version > best.Version {
				best = id
			}
		}
	}
	if best == nil {
		return nil, ErrNoValidIdentity
	}
	return best, nil
}

// findRuleLocked 取身份地区 + 收入类型 + 金额区间 + 交易发生日生效的规则；
// 存在多个时取版本最高者。
func (s *Service) findRuleLocked(id *TaxIdentity, incomeType string, gross Money, day time.Time) (*TaxRateRule, error) {
	var best *TaxRateRule
	for _, r := range s.rules {
		if r.Region == id.Region && r.IncomeType == incomeType &&
			r.Covers(gross) && r.Effective(day) {
			if best == nil || r.Version > best.Version {
				best = r
			}
		}
	}
	if best == nil {
		return nil, ErrNoRateRule
	}
	return best, nil
}

// lineSnapshot 计算单条明细的身份/规则快照（不含税额分摊）。
func (s *Service) lineSnapshotLocked(ln SettlementLine) (LineResult, *TaxRateRule, error) {
	id, err := s.findIdentityLocked(ln.PayeeID, ln.TransactionDate)
	if err != nil {
		return LineResult{}, nil, err
	}
	rule, err := s.findRuleLocked(id, ln.IncomeType, ln.Gross, ln.TransactionDate)
	if err != nil {
		return LineResult{}, nil, err
	}
	return LineResult{
		LineID:          ln.LineID,
		PayeeID:         ln.PayeeID,
		Gross:           ln.Gross,
		IdentityID:      id.ID,
		IdentityVersion: id.Version,
		RuleID:          rule.ID,
		RuleVersion:     rule.Version,
		RateBps:         rule.RateBps,
	}, rule, nil
}

// allocateTax 对共享同一付款的明细按“分组汇总取整 + 最大余数法”分摊税额：
// 同一（身份版本, 规则版本, 税率）分组内，先按分组毛额合计计算税额并四舍五入到分，
// 再按各明细精确税额的最大余数分配到行，余数相同按 LineID 字典序（稳定规则）。
// 保证每行 Tax+Net=Gross，且合计税额等于分组级取整结果，不产生合计偏差。
func allocateTax(lines []LineResult, grosses []Money) {
	type groupKey struct {
		identityID string
		ruleID     string
		rateBps    int64
	}
	groups := map[groupKey][]int{}
	var order []groupKey
	for i, lr := range lines {
		k := groupKey{lr.IdentityID, lr.RuleID, lr.RateBps}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	for _, k := range order {
		idxs := groups[k]
		var groupGross Money
		for _, i := range idxs {
			groupGross += grosses[i]
		}
		// 分组级目标税额：对分组毛额合计四舍五入。
		target := roundBps(groupGross, k.rateBps)
		// 每行先向下取整，余数从大到小补 1 分。
		type rem struct {
			idx       int
			remainder int64
		}
		rems := make([]rem, 0, len(idxs))
		var floorSum Money
		floors := make([]Money, len(lines))
		for _, i := range idxs {
			exact := int64(grosses[i]) * k.rateBps // 单位：万分之一分
			floor := Money(exact / 10000)
			floors[i] = floor
			floorSum += floor
			rems = append(rems, rem{idx: i, remainder: exact % 10000})
		}
		sort.SliceStable(rems, func(a, b int) bool {
			if rems[a].remainder != rems[b].remainder {
				return rems[a].remainder > rems[b].remainder
			}
			return lines[rems[a].idx].LineID < lines[rems[b].idx].LineID
		})
		diff := int64(target - floorSum) // 需要补的分数量
		extra := make([]Money, len(lines))
		for j := 0; j < len(rems); j++ {
			if int64(j) < diff {
				extra[rems[j].idx] = 1
			}
		}
		for _, i := range idxs {
			lines[i].Tax = floors[i] + extra[i]
			lines[i].Net = lines[i].Gross - lines[i].Tax
		}
	}
}

// roundBps 按万分比税率对金额四舍五入到分。
func roundBps(amount Money, rateBps int64) Money {
	exact := int64(amount) * rateBps
	return Money((exact + 5000) / 10000)
}

// computeLocked 对结算全部明细计算扣税结果（试算与确认共用）。
func (s *Service) computeLocked(st *Settlement) (*WithholdingResult, error) {
	lines := make([]LineResult, len(st.Lines))
	grosses := make([]Money, len(st.Lines))
	for i, ln := range st.Lines {
		lr, _, err := s.lineSnapshotLocked(ln)
		if err != nil {
			return nil, fmt.Errorf("line %s: %w", ln.LineID, err)
		}
		lines[i] = lr
		grosses[i] = ln.Gross
	}
	allocateTax(lines, grosses)
	res := &WithholdingResult{SettlementID: st.ID, Lines: lines}
	for _, lr := range lines {
		res.TotalGross += lr.Gross
		res.TotalTax += lr.Tax
		res.TotalNet += lr.Net
	}
	return res, nil
}

// TrialCalculate 税额试算：不产生凭证、不改变任何状态。
func (s *Service) TrialCalculate(settlementID string) (*WithholdingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.settlements[settlementID]
	if !ok {
		return nil, ErrNotFound
	}
	return s.computeLocked(st)
}

// ConfirmPayment 付款确认：生成唯一有效扣税结果与凭证编号。
// 同一结算重复确认时返回既有结果（幂等），不会生成第二份凭证。
func (s *Service) ConfirmPayment(settlementID, paymentID string) (*WithholdingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.settlements[settlementID]
	if !ok {
		return nil, ErrNotFound
	}
	if existing, ok := s.results[settlementID]; ok {
		cp := *existing
		return &cp, nil
	}
	res, err := s.computeLocked(st)
	if err != nil {
		return nil, err
	}
	s.voucherSeq++
	res.PaymentID = paymentID
	res.VoucherNo = fmt.Sprintf("WHT-%06d", s.voucherSeq)
	res.ConfirmedAt = time.Now().UTC()
	s.results[settlementID] = res
	cp := *res
	return &cp, nil
}

// GetVoucher 按结算号查询有效扣税结果（完税凭证），
// 结果中包含每行采用的身份版本与税率规则版本。
func (s *Service) GetVoucher(settlementID string) (*WithholdingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.results[settlementID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *res
	return &cp, nil
}

// AddCorrection 针对已确认的结算增加独立更正记录（补扣或退回差额），
// 不修改历史扣税结果与凭证。
func (s *Service) AddCorrection(settlementID string, typ CorrectionType, amount Money, reason string) (*Correction, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	if typ != CorrectionAdditional && typ != CorrectionRefund {
		return nil, errors.New("settlement: unknown correction type")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.results[settlementID]
	if !ok {
		return nil, ErrNotFound
	}
	s.corrSeq++
	c := &Correction{
		ID:           fmt.Sprintf("CORR-%06d", s.corrSeq),
		SettlementID: settlementID,
		VoucherNo:    res.VoucherNo,
		Type:         typ,
		Amount:       amount,
		Reason:       reason,
		CreatedAt:    time.Now().UTC(),
	}
	s.corrections = append(s.corrections, c)
	cp := *c
	return &cp, nil
}

// ListCorrections 查询某结算的全部更正记录，按创建顺序返回。
func (s *Service) ListCorrections(settlementID string) []Correction {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Correction
	for _, c := range s.corrections {
		if c.SettlementID == settlementID {
			out = append(out, *c)
		}
	}
	return out
}
