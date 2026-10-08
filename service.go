package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// AdjustmentRequest 描述一次日历临时调整的内容。
type AdjustmentRequest struct {
	ID       string // 调整号，幂等键
	Currency string
	Channel  string
	Cutoff   time.Duration   // 新截点
	Holidays map[string]bool // 新节假日集合
}

func (r AdjustmentRequest) hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|", r.ID, r.Currency, r.Channel, r.Cutoff)
	for _, d := range sortedHolidays(r.Holidays) {
		fmt.Fprintf(h, "%s;", d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ImpactItem 影响清单中的一项：会被改变价值日的未提交批次。
type ImpactItem struct {
	BatchID     string
	OldValueDay time.Time
	NewValueDay time.Time
}

// ImpactReport 调整预览产生的影响清单。确认调整时必须携带 ReportID，
// 若确认时影响范围发生变化，旧清单作废。
type ImpactReport struct {
	ReportID  string
	Request   AdjustmentRequest
	Items     []ImpactItem
	CreatedAt time.Time
}

// fingerprint 影响范围指纹，用于确认时校验清单是否仍然有效。
func (r *ImpactReport) fingerprint() string {
	h := sha256.New()
	for _, it := range r.Items {
		fmt.Fprintf(h, "%s:%s>%s;", it.BatchID,
			it.OldValueDay.Format(time.RFC3339), it.NewValueDay.Format(time.RFC3339))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Adjustment 一次已确认的日历调整。
type Adjustment struct {
	Request     AdjustmentRequest
	OldVersion  CalendarVersion
	NewVersion  CalendarVersion
	Applied     []ImpactItem // 实际迁移的未提交批次
	ConfirmedAt time.Time
}

// Service 结算日历与批次排期服务。所有状态变更都在同一把锁下完成，
// 保证日历确认、批次送出、批次创建并发时的原子性。
type Service struct {
	mu          sync.Mutex
	calendars   map[string][]*CalendarVersion // key: currency|channel，按版本升序
	batches     map[string]*Batch
	previews    map[string]*ImpactReport // reportID -> 预览
	adjustments map[string]*Adjustment   // 调整号 -> 已确认调整
	adjHashes   map[string]string        // 调整号 -> 内容指纹
	seq         int
}

func NewService() *Service {
	return &Service{
		calendars:   make(map[string][]*CalendarVersion),
		batches:     make(map[string]*Batch),
		previews:    make(map[string]*ImpactReport),
		adjustments: make(map[string]*Adjustment),
		adjHashes:   make(map[string]string),
	}
}

func scopeKey(currency, channel string) string { return currency + "|" + channel }

// CreateCalendar 创建某个适用范围的首个日历版本。
func (s *Service) CreateCalendar(currency, channel string, cutoff time.Duration, holidays map[string]bool) *CalendarVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scopeKey(currency, channel)
	v := &CalendarVersion{
		ID:        fmt.Sprintf("CAL-%d", len(s.calendars)+1),
		Version:   1,
		Currency:  currency,
		Channel:   channel,
		Cutoff:    cutoff,
		Holidays:  copyHolidays(holidays),
		Effective: true,
		CreatedAt: time.Now(),
	}
	s.calendars[key] = append(s.calendars[key], v)
	return v
}

func (s *Service) currentCalendar(currency, channel string) *CalendarVersion {
	vs := s.calendars[scopeKey(currency, channel)]
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

// CalendarVersions 返回某适用范围的全部日历版本（按版本升序）。
func (s *Service) CalendarVersions(currency, channel string) []CalendarVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CalendarVersion
	for _, v := range s.calendars[scopeKey(currency, channel)] {
		out = append(out, *v)
	}
	return out
}

// CreateBatch 创建结算批次：命中当前日历版本，记录受理时间与计算出的价值日。
func (s *Service) CreateBatch(id, currency, channel string, acceptedAt time.Time) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cal := s.currentCalendar(currency, channel)
	if cal == nil {
		return nil, ErrCalendarNotFound
	}
	b := &Batch{
		ID:         id,
		Currency:   currency,
		Channel:    channel,
		CalendarID: cal.ID,
		CalVersion: cal.Version,
		AcceptedAt: acceptedAt,
		ValueDate:  cal.ValueDate(acceptedAt),
		Status:     StatusPending,
	}
	s.batches[id] = b
	cp := *b
	return &cp, nil
}

// ExplainBatch 解释某批次价值日的计算依据（按创建时命中的日历版本）。
func (s *Service) ExplainBatch(batchID string) (Derivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return Derivation{}, ErrBatchNotFound
	}
	cal := s.findCalendar(b.CalendarID, b.CalVersion)
	if cal == nil {
		return Derivation{}, ErrCalendarNotFound
	}
	return cal.Explain(b.AcceptedAt), nil
}

func (s *Service) findCalendar(id string, version int) *CalendarVersion {
	for _, vs := range s.calendars {
		for _, v := range vs {
			if v.ID == id && v.Version == version {
				return v
			}
		}
	}
	return nil
}

// SendBatch 将批次送往银行；送出后价值日冻结，不再参与任何调整迁移。
func (s *Service) SendBatch(batchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return ErrBatchNotFound
	}
	if b.Status == StatusSent {
		return ErrBatchAlreadySent
	}
	b.Status = StatusSent
	return nil
}

// GetBatch 查询批次。
func (s *Service) GetBatch(batchID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	cp := *b
	return &cp, nil
}

// PreviewAdjustment 生成调整预览：按调整内容构造候选新版本，
// 列出所有会改变价值日的未提交批次及其旧/新价值日。
func (s *Service) PreviewAdjustment(req AdjustmentRequest) (*ImpactReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.adjHashes[req.ID]; ok && h != req.hash() {
		return nil, ErrAdjustmentExists
	}
	cur := s.currentCalendar(req.Currency, req.Channel)
	if cur == nil {
		return nil, ErrCalendarNotFound
	}
	candidate := &CalendarVersion{
		Currency: req.Currency, Channel: req.Channel,
		Cutoff: req.Cutoff, Holidays: copyHolidays(req.Holidays),
	}
	report := &ImpactReport{
		ReportID:  fmt.Sprintf("RPT-%s-%d", req.ID, len(s.previews)+1),
		Request:   req,
		Items:     s.impactOf(cur, candidate),
		CreatedAt: time.Now(),
	}
	s.previews[report.ReportID] = report
	cp := *report
	return &cp, nil
}

// impactOf 计算从 old 版本切换到 candidate 版本时受影响的未提交批次。
func (s *Service) impactOf(old, candidate *CalendarVersion) []ImpactItem {
	var items []ImpactItem
	for _, b := range s.batches {
		if b.Status != StatusPending || b.CalendarID != old.ID || b.CalVersion != old.Version {
			continue
		}
		nv := candidate.ValueDate(b.AcceptedAt)
		if !nv.Equal(b.ValueDate) {
			items = append(items, ImpactItem{BatchID: b.ID, OldValueDay: b.ValueDate, NewValueDay: nv})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].BatchID < items[j].BatchID })
	return items
}

// ConfirmAdjustment 确认调整：重新校验影响范围，与预览不一致则拒绝；
// 相同调整号 + 相同内容重放直接返回原结果。确认后未送出的受影响批次
// 在同一把锁内一次性迁移，已送银行的批次保留原价值日。
func (s *Service) ConfirmAdjustment(reportID string) (*Adjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.previews[reportID]
	if !ok {
		return nil, ErrPreviewStale
	}
	req := report.Request
	// 幂等重放：调整号已确认且内容一致，返回原结果。
	if h, ok := s.adjHashes[req.ID]; ok {
		if h != req.hash() {
			return nil, ErrAdjustmentExists
		}
		adj := *s.adjustments[req.ID]
		return &adj, nil
	}
	cur := s.currentCalendar(req.Currency, req.Channel)
	if cur == nil {
		return nil, ErrCalendarNotFound
	}
	// 重新校验：基于当前版本重算影响范围，与预览不符则旧清单作废。
	candidate := &CalendarVersion{
		Currency: req.Currency, Channel: req.Channel,
		Cutoff: req.Cutoff, Holidays: copyHolidays(req.Holidays),
	}
	fresh := &ImpactReport{Items: s.impactOf(cur, candidate)}
	if fresh.fingerprint() != report.fingerprint() {
		delete(s.previews, reportID)
		return nil, ErrPreviewStale
	}
	// 生效新版本。
	newVersion := &CalendarVersion{
		ID:        cur.ID,
		Version:   cur.Version + 1,
		Currency:  req.Currency,
		Channel:   req.Channel,
		Cutoff:    req.Cutoff,
		Holidays:  copyHolidays(req.Holidays),
		Effective: true,
		CreatedAt: time.Now(),
	}
	key := scopeKey(req.Currency, req.Channel)
	s.calendars[key] = append(s.calendars[key], newVersion)
	// 一次性迁移所有受影响的未提交批次。
	for _, it := range report.Items {
		b := s.batches[it.BatchID]
		b.ValueDate = it.NewValueDay
		b.CalendarID = newVersion.ID
		b.CalVersion = newVersion.Version
		b.AdjustmentID = req.ID
	}
	adj := &Adjustment{
		Request:     req,
		OldVersion:  *cur,
		NewVersion:  *newVersion,
		Applied:     append([]ImpactItem(nil), report.Items...),
		ConfirmedAt: time.Now(),
	}
	s.adjustments[req.ID] = adj
	s.adjHashes[req.ID] = req.hash()
	delete(s.previews, reportID)
	cp := *adj
	return &cp, nil
}

// GetAdjustment 查询已确认调整（含调整前后版本与受影响批次）。
func (s *Service) GetAdjustment(adjustmentID string) (*Adjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	adj, ok := s.adjustments[adjustmentID]
	if !ok {
		return nil, ErrCalendarNotFound
	}
	cp := *adj
	return &cp, nil
}

// DeleteAdjustment 已生效的调整不得删除。
func (s *Service) DeleteAdjustment(adjustmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.adjustments[adjustmentID]; ok {
		return ErrAdjustmentApplied
	}
	return ErrCalendarNotFound
}

func copyHolidays(h map[string]bool) map[string]bool {
	out := make(map[string]bool, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}
