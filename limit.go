package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// LimitRule 描述一条限额规则在某个版本下的具体阈值。
// 值为 0 表示该层限额未设置（不约束）。
type LimitRule struct {
	// MaxSingleAmount 单笔金额上限。
	MaxSingleAmount int64
	// MaxNetExposure 双边净敞口上限：|付款方累计 - 收款方累计| 不得超过该值。
	MaxNetExposure int64
	// MaxGrossExposure 单方总敞口上限：参与方作为付款方的当日累计不得超过该值。
	MaxGrossExposure int64
}

// RuleVersion 是限额规则的一个不可变版本。
type RuleVersion struct {
	Version       int
	Rule          LimitRule
	EffectiveFrom time.Time
	PublishedAt   time.Time
}

// RuleKey 标识一组限额规则的主体。
type RuleKey struct {
	Participant  string
	Counterparty string
	Currency     string
}

// ErrRuleNotFound 表示未找到对应主体与生效时间的限额规则。
var ErrRuleNotFound = errors.New("settlement: limit rule not found")

// ruleSeries 是同一主体下按版本递增的规则历史。
type ruleSeries struct {
	key      RuleKey
	versions []RuleVersion
}

// versionAt 返回在 t 时刻生效的版本：取 EffectiveFrom <= t 的最大版本。
func (s *ruleSeries) versionAt(t time.Time) (RuleVersion, bool) {
	var best *RuleVersion
	for i := range s.versions {
		v := &s.versions[i]
		if !v.EffectiveFrom.After(t) && (best == nil || v.Version > best.Version) {
			best = v
		}
	}
	if best == nil {
		return RuleVersion{}, false
	}
	return *best, true
}

func (s *ruleSeries) versionByNumber(n int) (RuleVersion, bool) {
	for _, v := range s.versions {
		if v.Version == n {
			return v, true
		}
	}
	return RuleVersion{}, false
}

// RuleStore 维护限额规则及其版本历史。每次发布生成一个新版本，
// 历史版本保持不变，供已受理指令继续引用。
type RuleStore struct {
	mu     sync.RWMutex
	series map[RuleKey]*ruleSeries
	now    func() time.Time
}

// NewRuleStore 创建空的规则存储。
func NewRuleStore() *RuleStore {
	return &RuleStore{
		series: make(map[RuleKey]*ruleSeries),
		now:    time.Now,
	}
}

// Publish 为指定主体发布新版本的限额规则，返回新版本号（从 1 开始递增）。
func (s *RuleStore) Publish(key RuleKey, rule LimitRule, effectiveFrom time.Time) (int, error) {
	if key.Participant == "" || key.Counterparty == "" || key.Currency == "" {
		return 0, errors.New("settlement: rule key fields must not be empty")
	}
	if rule.MaxSingleAmount < 0 || rule.MaxNetExposure < 0 || rule.MaxGrossExposure < 0 {
		return 0, errors.New("settlement: limit values must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ser, ok := s.series[key]
	if !ok {
		ser = &ruleSeries{key: key}
		s.series[key] = ser
	}
	v := RuleVersion{
		Version:       len(ser.versions) + 1,
		Rule:          rule,
		EffectiveFrom: effectiveFrom,
		PublishedAt:   s.now(),
	}
	ser.versions = append(ser.versions, v)
	return v.Version, nil
}

// Resolve 返回指定主体在 t 时刻生效的规则版本。
func (s *RuleStore) Resolve(key RuleKey, t time.Time) (RuleVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ser, ok := s.series[key]
	if !ok {
		return RuleVersion{}, fmt.Errorf("%w: %+v", ErrRuleNotFound, key)
	}
	v, ok := ser.versionAt(t)
	if !ok {
		return RuleVersion{}, fmt.Errorf("%w: no effective version for %+v", ErrRuleNotFound, key)
	}
	return v, nil
}

// Version 返回指定主体的特定版本。
func (s *RuleStore) Version(key RuleKey, version int) (RuleVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ser, ok := s.series[key]
	if !ok {
		return RuleVersion{}, fmt.Errorf("%w: %+v", ErrRuleNotFound, key)
	}
	v, ok := ser.versionByNumber(version)
	if !ok {
		return RuleVersion{}, fmt.Errorf("%w: version %d for %+v", ErrRuleNotFound, version, key)
	}
	return v, nil
}
