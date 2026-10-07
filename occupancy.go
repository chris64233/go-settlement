package settlement

import "time"

// OccupancyRecord 是一条占用明细，对应一笔指令对某参与方的占用。
type OccupancyRecord struct {
	InstructionID string
	RequestID     string
	Payer         string
	Payee         string
	Currency      string
	Amount        int64
	ValueDay      string
	RuleVersion   int
	State         State
}

// OccupancyDetail 是按参与方、对手方、币种与价值日查询到的占用明细。
type OccupancyDetail struct {
	Participant  string
	Counterparty string
	Currency     string
	ValueDay     string
	// HeldAmount 处于持有状态（尚未完成）的占用。
	HeldAmount int64
	// UsedAmount 已结算完成、转为已用额度的占用。
	UsedAmount int64
	// UnilateralTotal 该参与方当日全部对手方的总敞口占用（持有+已用）。
	UnilateralTotal int64
	// NetPosition 该参与方对对手方的净敞口（付款方向为正）。
	NetPosition int64
	Records     []OccupancyRecord
}

// QueryOccupancy 查询参与方对某对手方在指定价值日的占用明细。
func (s *Service) QueryOccupancy(participant, counterparty, currency string, valueDate time.Time) OccupancyDetail {
	s.mu.Lock()
	defer s.mu.Unlock()

	day := dayOf(valueDate)
	detail := OccupancyDetail{
		Participant:     participant,
		Counterparty:    counterparty,
		Currency:        currency,
		ValueDay:        day,
		UnilateralTotal: s.unilateral[occKey{participant, currency, day}],
		NetPosition: s.net[flowKey{participant, counterparty, currency, day}] -
			s.net[flowKey{counterparty, participant, currency, day}],
	}
	for _, ins := range s.instructions {
		if ins.Currency != currency || dayOf(ins.ValueDate) != day {
			continue
		}
		if !((ins.Payer == participant && ins.Payee == counterparty) ||
			(ins.Payer == counterparty && ins.Payee == participant)) {
			continue
		}
		rec := OccupancyRecord{
			InstructionID: ins.ID,
			RequestID:     ins.RequestID,
			Payer:         ins.Payer,
			Payee:         ins.Payee,
			Currency:      ins.Currency,
			Amount:        ins.Amount,
			ValueDay:      day,
			RuleVersion:   ins.RuleVersion,
			State:         ins.State,
		}
		detail.Records = append(detail.Records, rec)
		if ins.Payer == participant {
			switch ins.State {
			case StateHeld:
				detail.HeldAmount += ins.Amount
			case StateUsed:
				detail.UsedAmount += ins.Amount
			}
		}
	}
	return detail
}
