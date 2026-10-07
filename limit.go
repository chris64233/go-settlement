package settlement

import "time"

// LimitRule 是一条限额规则的某个版本。
// 三个限额字段中，值为 0 表示该层限额不启用。
type LimitRule struct {
	Participant  string
	Counterparty string
	Currency     string
	Version      int
	EffectiveAt  time.Time
	// MaxSingleAmount 单笔上限。
	MaxSingleAmount int64
	// MaxBilateralNet 双边净敞口上限（绝对值）。
	MaxBilateralNet int64
	// MaxUnilateralTotal 单方总敞口上限，对付款方与收款方分别生效。
	MaxUnilateralTotal int64
}

type ruleKey struct {
	participant  string
	counterparty string
	currency     string
}

// PublishRule 发布一条新的限额规则，形成新的版本号并返回。
// 同一（参与方, 对手方, 币种）下版本号单调递增。
func (s *Service) PublishRule(rule LimitRule) LimitRule {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ruleKey{rule.Participant, rule.Counterparty, rule.Currency}
	versions := s.rules[key]
	rule.Version = 1
	if n := len(versions); n > 0 {
		rule.Version = versions[n-1].Version + 1
	}
	stored := rule
	s.rules[key] = append(versions, &stored)
	return rule
}

// resolveRule 返回在 at 时刻已生效的最高版本规则。
func (s *Service) resolveRule(participant, counterparty, currency string, at time.Time) *LimitRule {
	versions := s.rules[ruleKey{participant, counterparty, currency}]
	var best *LimitRule
	for _, r := range versions {
		if !r.EffectiveAt.After(at) && (best == nil || r.Version > best.Version) {
			best = r
		}
	}
	return best
}

// CurrentRule 返回当前已生效的规则版本，未发布过时返回 nil。
func (s *Service) CurrentRule(participant, counterparty, currency string) *LimitRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.resolveRule(participant, counterparty, currency, s.now()); r != nil {
		cpy := *r
		return &cpy
	}
	return nil
}
