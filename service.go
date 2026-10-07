package settlement

import (
	"sort"
	"sync"
	"time"
)

// Service 退汇处理服务。所有余额与台账变更在同一把互斥锁内完成，
// 保证退汇确认、账户冻结、提现并发时余额变化与台账在同一结果中生效。
type Service struct {
	mu        sync.Mutex
	accounts  map[string]*Account
	transfers map[string]*Transfer
	returns   map[string]*ReturnReceipt // key: 银行退汇号（全局唯一）
	reversals map[string]*Reversal      // key: 冲正号（全局唯一）
	ledger    []LedgerEntry
	seq       int64
	now       func() time.Time
}

func NewService() *Service {
	return &Service{
		accounts:  make(map[string]*Account),
		transfers: make(map[string]*Transfer),
		returns:   make(map[string]*ReturnReceipt),
		reversals: make(map[string]*Reversal),
		now:       time.Now,
	}
}

func (s *Service) appendLedger(e LedgerEntry) {
	s.seq++
	e.Seq = s.seq
	e.CreatedAt = s.now()
	s.ledger = append(s.ledger, e)
}

// OpenAccount 创建业务方账户。
func (s *Service) OpenAccount(id, currency string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; ok {
		return ErrTransferExists
	}
	s.accounts[id] = &Account{ID: id, Currency: currency}
	return nil
}

// Deposit 充值，增加可用余额。
func (s *Service) Deposit(accountID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	acc.Available += amount
	s.appendLedger(LedgerEntry{Type: LedgerDeposit, AccountID: accountID, Amount: amount, Currency: acc.Currency, BalanceAfter: acc.Available})
	return nil
}

// CreateTransfer 创建银行转账指令（PENDING）。
func (s *Service) CreateTransfer(id, accountID string, amount int64, currency string) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.transfers[id]; ok {
		return ErrTransferExists
	}
	if _, ok := s.accounts[accountID]; !ok {
		return ErrAccountNotFound
	}
	s.transfers[id] = &Transfer{ID: id, AccountID: accountID, Amount: amount, Currency: currency, Status: TransferStatusPending}
	return nil
}

// ConfirmTransferSuccess 确认转账成功：扣减可用余额并生成台账。
// 成功状态与成功回执号一旦写入不可变，退汇不会改回失败。
func (s *Service) ConfirmTransferSuccess(id, bankReceiptNo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[id]
	if !ok {
		return ErrTransferNotFound
	}
	if t.Status == TransferStatusSuccess {
		if t.BankReceiptNo == bankReceiptNo {
			return nil // 幂等
		}
		return ErrReturnConflict
	}
	acc := s.accounts[t.AccountID]
	if acc.Available < t.Amount {
		return ErrInsufficientBalance
	}
	acc.Available -= t.Amount
	t.Status = TransferStatusSuccess
	t.BankReceiptNo = bankReceiptNo
	t.SuccessAt = s.now()
	s.appendLedger(LedgerEntry{Type: LedgerTransferOut, AccountID: t.AccountID, TransferID: id, Amount: t.Amount, Currency: t.Currency, BalanceAfter: acc.Available})
	return nil
}

// SubmitReturn 受理银行退汇回执。银行退汇号全局唯一并支持幂等：
// 相同内容重复提交返回首次结果；原指令、金额或币种变化返回冲突。
// 只有原指令成功且币种匹配时才能受理，累计退回不得超过原转账金额。
// 确认成功即增加业务方可用余额并生成独立台账，原成功回执保持不变。
func (s *Service) SubmitReturn(bankReturnNo, transferID string, amount int64, currency, reason string, receiptAt time.Time) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.returns[bankReturnNo]; ok {
		if existing.TransferID == transferID && existing.Amount == amount && existing.Currency == currency {
			return nil // 幂等重放，返回首次结果
		}
		return ErrReturnConflict
	}
	t, ok := s.transfers[transferID]
	if !ok {
		return ErrTransferNotFound
	}
	if t.Status != TransferStatusSuccess {
		return ErrTransferNotSuccess
	}
	if currency != t.Currency {
		return ErrCurrencyMismatch
	}
	if t.ReturnedAmount+amount > t.Amount {
		return ErrReturnExceeded
	}
	acc := s.accounts[t.AccountID]
	t.ReturnedAmount += amount
	acc.Available += amount
	s.returns[bankReturnNo] = &ReturnReceipt{
		BankReturnNo: bankReturnNo,
		TransferID:   transferID,
		Amount:       amount,
		Currency:     currency,
		Reason:       reason,
		ReceiptAt:    receiptAt,
	}
	s.appendLedger(LedgerEntry{Type: LedgerReturnIn, AccountID: t.AccountID, TransferID: transferID, BankReturnNo: bankReturnNo, Amount: amount, Currency: currency, BalanceAfter: acc.Available})
	return nil
}

// ReverseReturn 冲正错误退汇：独立冲正号，扣回业务方可用余额并生成台账。
// 累计冲正不能超过对应退汇已入账金额；冲正号幂等，内容变化返回冲突。
func (s *Service) ReverseReturn(reversalNo, bankReturnNo string, amount int64, reason string) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.reversals[reversalNo]; ok {
		if existing.BankReturnNo == bankReturnNo && existing.Amount == amount {
			return nil
		}
		return ErrReversalConflict
	}
	ret, ok := s.returns[bankReturnNo]
	if !ok {
		return ErrReturnNotFound
	}
	if ret.ReversedAmt+amount > ret.Amount {
		return ErrReversalExceeded
	}
	t := s.transfers[ret.TransferID]
	acc := s.accounts[t.AccountID]
	if acc.Available < amount {
		return ErrInsufficientBalance
	}
	ret.ReversedAmt += amount
	t.ReturnedAmount -= amount
	acc.Available -= amount
	s.reversals[reversalNo] = &Reversal{ReversalNo: reversalNo, BankReturnNo: bankReturnNo, Amount: amount, Reason: reason, CreatedAt: s.now()}
	s.appendLedger(LedgerEntry{Type: LedgerReversalOut, AccountID: t.AccountID, TransferID: ret.TransferID, BankReturnNo: bankReturnNo, ReversalNo: reversalNo, Amount: amount, Currency: ret.Currency, BalanceAfter: acc.Available})
	return nil
}

// Freeze 冻结业务方可用余额。
func (s *Service) Freeze(accountID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	if acc.Available < amount {
		return ErrFrozenExceeded
	}
	acc.Available -= amount
	acc.Frozen += amount
	s.appendLedger(LedgerEntry{Type: LedgerFreeze, AccountID: accountID, Amount: amount, Currency: acc.Currency, BalanceAfter: acc.Available})
	return nil
}

// Unfreeze 解冻余额。
func (s *Service) Unfreeze(accountID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	if acc.Frozen < amount {
		return ErrInsufficientBalance
	}
	acc.Frozen -= amount
	acc.Available += amount
	s.appendLedger(LedgerEntry{Type: LedgerUnfreeze, AccountID: accountID, Amount: amount, Currency: acc.Currency, BalanceAfter: acc.Available})
	return nil
}

// Withdraw 业务方提现，扣减可用余额。
func (s *Service) Withdraw(accountID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	if acc.Available < amount {
		return ErrInsufficientBalance
	}
	acc.Available -= amount
	s.appendLedger(LedgerEntry{Type: LedgerWithdraw, AccountID: accountID, Amount: amount, Currency: acc.Currency, BalanceAfter: acc.Available})
	return nil
}

// GetAccount 查询账户余额快照。
func (s *Service) GetAccount(id string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[id]
	if !ok {
		return Account{}, ErrAccountNotFound
	}
	return *acc, nil
}

// GetTransferDetail 查询原转账、全部退汇、冲正关联、台账与当前可退剩余金额。
func (s *Service) GetTransferDetail(transferID string) (TransferDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[transferID]
	if !ok {
		return TransferDetail{}, ErrTransferNotFound
	}
	d := TransferDetail{Transfer: *t, Remaining: t.RemainingReturnable()}
	returnNos := make([]string, 0, len(s.returns))
	for no, r := range s.returns {
		if r.TransferID == transferID {
			returnNos = append(returnNos, no)
		}
	}
	sort.Strings(returnNos)
	for _, no := range returnNos {
		d.Returns = append(d.Returns, *s.returns[no])
	}
	reversalNos := make([]string, 0)
	returnSet := make(map[string]bool, len(returnNos))
	for _, no := range returnNos {
		returnSet[no] = true
	}
	for no, rev := range s.reversals {
		if returnSet[rev.BankReturnNo] {
			reversalNos = append(reversalNos, no)
		}
	}
	sort.Strings(reversalNos)
	for _, no := range reversalNos {
		d.Reversals = append(d.Reversals, *s.reversals[no])
	}
	for _, e := range s.ledger {
		if e.TransferID == transferID {
			d.Ledger = append(d.Ledger, e)
		}
	}
	return d, nil
}
