package settlement

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service 结算服务。所有余额与台账变更在同一把互斥锁内完成，
// 保证退汇确认、账户冻结、提现等并发操作原子生效。
type Service struct {
	mu        sync.Mutex
	transfers map[string]*Transfer
	accounts  map[string]*Account // key: merchantID
	returns   map[string]*ReturnReceipt
	reversals map[string]*Reversal
	ledger    []*LedgerEntry
	seq       int64
	now       func() time.Time
}

func NewService() *Service {
	return &Service{
		transfers: make(map[string]*Transfer),
		accounts:  make(map[string]*Account),
		returns:   make(map[string]*ReturnReceipt),
		reversals: make(map[string]*Reversal),
		now:       time.Now,
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%06d", prefix, s.seq)
}

// CreateTransfer 创建银行转账指令（PENDING）。
func (s *Service) CreateTransfer(id, merchantID string, amount int64, currency string) (*Transfer, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.transfers[id]; ok {
		return nil, fmt.Errorf("transfer %s already exists", id)
	}
	t := &Transfer{
		ID:         id,
		MerchantID: merchantID,
		Amount:     amount,
		Currency:   currency,
		Status:     TransferStatusPending,
		CreatedAt:  s.now(),
	}
	s.transfers[id] = t
	cp := *t
	return &cp, nil
}

// ConfirmTransferSuccess 确认转账成功，保存银行成功回执。
// 成功结果一旦写入不可更改、不可删除。
func (s *Service) ConfirmTransferSuccess(transferID, successReceipt string) (*Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[transferID]
	if !ok {
		return nil, ErrTransferNotFound
	}
	if t.Status == TransferStatusSuccess {
		if t.SuccessReceipt == successReceipt {
			cp := *t
			return &cp, nil // 幂等
		}
		return nil, fmt.Errorf("transfer %s already succeeded with receipt %s", transferID, t.SuccessReceipt)
	}
	t.Status = TransferStatusSuccess
	t.SuccessReceipt = successReceipt
	t.SucceededAt = s.now()
	cp := *t
	return &cp, nil
}

// OpenAccount 创建（或返回已有）业务方账户。
func (s *Service) OpenAccount(merchantID, currency string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openAccountLocked(merchantID, currency)
}

func (s *Service) openAccountLocked(merchantID, currency string) *Account {
	a, ok := s.accounts[merchantID]
	if !ok {
		a = &Account{MerchantID: merchantID, Currency: currency}
		s.accounts[merchantID] = a
	}
	return a
}

// Deposit 业务方账户充值（初始化可用余额）。
func (s *Service) Deposit(merchantID string, amount int64) (*Account, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[merchantID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	a.Available += amount
	cp := *a
	return &cp, nil
}

// Freeze 冻结可用余额。
func (s *Service) Freeze(merchantID string, amount int64) (*Account, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[merchantID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	if a.Available < amount {
		return nil, ErrInsufficientBalance
	}
	a.Available -= amount
	a.Frozen += amount
	cp := *a
	return &cp, nil
}

// Withdraw 业务方提现，从可用余额扣减。
func (s *Service) Withdraw(merchantID string, amount int64) (*Account, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[merchantID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	if a.Available < amount {
		return nil, ErrInsufficientBalance
	}
	a.Available -= amount
	cp := *a
	return &cp, nil
}

// GetAccount 查询账户。
func (s *Service) GetAccount(merchantID string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[merchantID]
	if !ok {
		return Account{}, ErrAccountNotFound
	}
	return *a, nil
}

// ConfirmReturn 受理银行退汇回执。
//
// 幂等规则：银行退汇号全局唯一；相同退汇号且关键内容（原指令、金额、币种）
// 完全一致时返回首次结果；任一关键内容不同返回冲突错误。
//
// 校验规则：原指令必须已成功；币种必须与原转账一致；
// 累计退回金额不得超过原转账金额。
//
// 入账与台账在同一临界区内完成，要么同时生效要么同时失败。
func (s *Service) ConfirmReturn(bankReturnNo, transferID string, amount int64, currency, reason string, receiptAt time.Time) (*ReturnReceipt, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.returns[bankReturnNo]; ok {
		switch {
		case existing.TransferID != transferID:
			return nil, &ConflictError{BankReturnNo: bankReturnNo, Field: "transfer_id"}
		case existing.Amount != amount:
			return nil, &ConflictError{BankReturnNo: bankReturnNo, Field: "amount"}
		case existing.Currency != currency:
			return nil, &ConflictError{BankReturnNo: bankReturnNo, Field: "currency"}
		}
		cp := *existing
		return &cp, nil
	}

	t, ok := s.transfers[transferID]
	if !ok {
		return nil, ErrTransferNotFound
	}
	if t.Status != TransferStatusSuccess {
		return nil, ErrTransferNotSucceeded
	}
	if currency != t.Currency {
		return nil, ErrCurrencyMismatch
	}
	if s.totalReturnedLocked(transferID)+amount > t.Amount {
		return nil, ErrReturnAmountExceeded
	}

	a := s.openAccountLocked(t.MerchantID, t.Currency)
	a.Available += amount

	r := &ReturnReceipt{
		BankReturnNo: bankReturnNo,
		TransferID:   transferID,
		Amount:       amount,
		Currency:     currency,
		Reason:       reason,
		ReceiptAt:    receiptAt,
		CreditedAmt:  amount,
		CreatedAt:    s.now(),
	}
	entry := &LedgerEntry{
		ID:         s.nextID("L"),
		MerchantID: t.MerchantID,
		Type:       LedgerTypeReturnCredit,
		Amount:     amount,
		Currency:   currency,
		TransferID: transferID,
		RefNo:      bankReturnNo,
		CreatedAt:  s.now(),
	}
	r.LedgerID = entry.ID
	s.returns[bankReturnNo] = r
	s.ledger = append(s.ledger, entry)
	cp := *r
	return &cp, nil
}

// ReverseReturn 冲正错误退汇，从业务方可用余额扣回。
// 累计冲正金额不得超过该退汇已入账金额。
func (s *Service) ReverseReturn(bankReturnNo string, amount int64, reason string) (*Reversal, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.returns[bankReturnNo]
	if !ok {
		return nil, ErrReturnNotFound
	}
	if r.ReversedAmt+amount > r.CreditedAmt {
		return nil, ErrReversalAmountExceed
	}
	t := s.transfers[r.TransferID]
	a, ok := s.accounts[t.MerchantID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	if a.Available < amount {
		return nil, ErrInsufficientBalance
	}
	a.Available -= amount
	r.ReversedAmt += amount

	rev := &Reversal{
		ID:           s.nextID("RV"),
		BankReturnNo: bankReturnNo,
		Amount:       amount,
		Reason:       reason,
		CreatedAt:    s.now(),
	}
	entry := &LedgerEntry{
		ID:         s.nextID("L"),
		MerchantID: t.MerchantID,
		Type:       LedgerTypeReversalDebit,
		Amount:     -amount,
		Currency:   r.Currency,
		TransferID: r.TransferID,
		RefNo:      rev.ID,
		CreatedAt:  s.now(),
	}
	rev.LedgerID = entry.ID
	s.reversals[rev.ID] = rev
	s.ledger = append(s.ledger, entry)
	cp := *rev
	return &cp, nil
}

func (s *Service) totalReturnedLocked(transferID string) int64 {
	var total int64
	for _, r := range s.returns {
		if r.TransferID == transferID {
			total += r.Amount
		}
	}
	return total
}

// GetTransferView 查询原转账、全部退汇、冲正关联、可用余额与可退剩余金额。
func (s *Service) GetTransferView(transferID string) (*TransferView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[transferID]
	if !ok {
		return nil, ErrTransferNotFound
	}
	view := &TransferView{Transfer: *t}
	var returned int64
	var nos []string
	for no, r := range s.returns {
		if r.TransferID == transferID {
			nos = append(nos, no)
			returned += r.Amount
		}
	}
	sort.Strings(nos)
	for _, no := range nos {
		r := s.returns[no]
		rv := ReturnView{Receipt: *r}
		for _, rev := range s.reversals {
			if rev.BankReturnNo == no {
				rv.Reversals = append(rv.Reversals, *rev)
			}
		}
		view.Returns = append(view.Returns, rv)
	}
	view.TotalReturned = returned
	view.RemainingAmount = t.Amount - returned
	if a, ok := s.accounts[t.MerchantID]; ok {
		view.AvailableBalance = a.Available
	}
	return view, nil
}

// Ledger 返回业务方全部资金台账（按发生顺序）。
func (s *Service) Ledger(merchantID string) []LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LedgerEntry
	for _, e := range s.ledger {
		if e.MerchantID == merchantID {
			out = append(out, *e)
		}
	}
	return out
}
