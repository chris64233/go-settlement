package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrScopeNotFound       = errors.New("settlement: no calendar for scope")
	ErrBatchNotFound       = errors.New("settlement: batch not found")
	ErrBatchAlreadySent    = errors.New("settlement: batch already sent to bank")
	ErrReportNotFound      = errors.New("settlement: impact report not found")
	ErrReportStale         = errors.New("settlement: impact report is stale, impact scope changed")
	ErrAdjustmentConflict  = errors.New("settlement: adjustment number replayed with different content")
	ErrAdjustmentEffective = errors.New("settlement: effective adjustment cannot be deleted")
)

// AdjustmentInput 一次日历临时调整的内容。
type AdjustmentInput struct {
	AdjustmentNo string
	Scope        Scope
	Cutoff       string
	Holidays     map[string]bool
	ExtraWorkday map[string]bool
}

func (in AdjustmentInput) fingerprint() string {
	h := sha256.New()
	h.Write([]byte(in.Scope.Currency + "|" + in.Scope.Channel + "|" + in.Cutoff))
	keys := make([]string, 0, len(in.Holidays)+len(in.ExtraWorkday))
	for k, v := range in.Holidays {
		if v {
			keys = append(keys, "H:"+k)
		}
	}
	for k, v := range in.ExtraWorkday {
		if v {
			keys = append(keys, "W:"+k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte("|" + k))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ImpactItem 影响清单中的一条：某未提交批次价值日将发生变化。
type ImpactItem struct {
	BatchID      string
	OldValueDate string
	NewValueDate string
}

// ImpactReport 调整前生成的影响清单。确认调整时会重新校验，
// 影响范围发生变化后旧清单不能继续使用。
type ImpactReport struct {
	ID           string
	AdjustmentNo string
	Scope        Scope
	BaseVersion  int
	NewVersion   int
	Items        []ImpactItem
	Diffs        []string
	Confirmed    bool
	CreatedAt    time.Time
}

// adjustmentRecord 记录一次调整号对应的内容指纹与结果，用于幂等重放。
type adjustmentRecord struct {
	fingerprint string
	reportID    string
}

// Service 结算日历与批次排期服务。所有方法并发安全。
type Service struct {
	mu          sync.Mutex
	calendars   map[Scope][]*Calendar
	batches     map[string]*Batch
	reports     map[string]*ImpactReport
	drafts      map[string]*Calendar
	adjustments map[string]*adjustmentRecord
	seq         int
}

func NewService() *Service {
	return &Service{
		calendars:   map[Scope][]*Calendar{},
		batches:     map[string]*Batch{},
		reports:     map[string]*ImpactReport{},
		drafts:      map[string]*Calendar{},
		adjustments: map[string]*adjustmentRecord{},
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

func copyBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCalendar(c *Calendar) *Calendar {
	cp := *c
	cp.Holidays = copyBoolMap(c.Holidays)
	cp.ExtraWorkday = copyBoolMap(c.ExtraWorkday)
	return &cp
}

func cloneReport(r *ImpactReport) *ImpactReport {
	cp := *r
	cp.Items = append([]ImpactItem(nil), r.Items...)
	cp.Diffs = append([]string(nil), r.Diffs...)
	return &cp
}

func impactEqual(a, b []ImpactItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CreateCalendar 为指定范围创建首个生效的日历版本。
func (s *Service) CreateCalendar(scope Scope, cutoff string, holidays, extraWorkday map[string]bool) (*Calendar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := time.Parse("15:04", cutoff); err != nil {
		return nil, fmt.Errorf("invalid cutoff %q: %w", cutoff, err)
	}
	if len(s.calendars[scope]) > 0 {
		return nil, fmt.Errorf("settlement: calendar for scope %+v already exists, use adjustments", scope)
	}
	cal := &Calendar{
		Version:      1,
		Scope:        scope,
		Cutoff:       cutoff,
		Holidays:     copyBoolMap(holidays),
		ExtraWorkday: copyBoolMap(extraWorkday),
		Effective:    true,
		CreatedAt:    time.Now(),
	}
	s.calendars[scope] = []*Calendar{cal}
	return cloneCalendar(cal), nil
}

func (s *Service) currentCalendar(scope Scope) (*Calendar, error) {
	versions := s.calendars[scope]
	if len(versions) == 0 {
		return nil, ErrScopeNotFound
	}
	return versions[len(versions)-1], nil
}

// CalendarVersions 返回某范围的全部日历版本（含历史版本）。
func (s *Service) CalendarVersions(scope Scope) ([]Calendar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.calendars[scope]
	if len(versions) == 0 {
		return nil, ErrScopeNotFound
	}
	out := make([]Calendar, 0, len(versions))
	for _, c := range versions {
		out = append(out, *cloneCalendar(c))
	}
	return out, nil
}

// CreateBatch 创建结算批次：按当前生效日历计算价值日，
// 并记录命中的日历版本与受理时间。
func (s *Service) CreateBatch(scope Scope, receivedAt time.Time) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cal, err := s.currentCalendar(scope)
	if err != nil {
		return nil, err
	}
	valueDate, _, err := cal.ComputeValueDate(receivedAt)
	if err != nil {
		return nil, err
	}
	b := &Batch{
		ID:              s.nextID("B"),
		Scope:           scope,
		ReceivedAt:      receivedAt,
		CalendarVersion: cal.Version,
		ValueDate:       valueDate,
		Status:          StatusPending,
	}
	s.batches[b.ID] = b
	cp := *b
	return &cp, nil
}

// SendBatch 将批次送往银行。送出后价值日永久固定。
func (s *Service) SendBatch(id string, sentAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[id]
	if !ok {
		return ErrBatchNotFound
	}
	if b.Status == StatusSent {
		return ErrBatchAlreadySent
	}
	b.Status = StatusSent
	b.SentAt = sentAt
	return nil
}

// GetBatch 查询批次。
func (s *Service) GetBatch(id string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[id]
	if !ok {
		return nil, ErrBatchNotFound
	}
	cp := *b
	return &cp, nil
}

// ExplainBatchValueDate 解释某批次价值日的计算依据：
// 使用批次创建时命中的日历版本与受理时间重新推演。
func (s *Service) ExplainBatchValueDate(id string) (DateExplanation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[id]
	if !ok {
		return DateExplanation{}, ErrBatchNotFound
	}
	var cal *Calendar
	for _, c := range s.calendars[b.Scope] {
		if c.Version == b.CalendarVersion {
			cal = c
			break
		}
	}
	if cal == nil {
		return DateExplanation{}, fmt.Errorf("settlement: calendar version %d not found", b.CalendarVersion)
	}
	_, exp, err := cal.ComputeValueDate(b.ReceivedAt)
	if err != nil {
		return DateExplanation{}, err
	}
	exp.ValueDate = b.ValueDate
	return exp, nil
}

// PrepareAdjustment 生成临时调整的影响清单（草稿）。
// 相同调整号 + 相同内容重放返回原结果；内容不同返回冲突。
func (s *Service) PrepareAdjustment(in AdjustmentInput) (*ImpactReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := in.fingerprint()
	if rec, ok := s.adjustments[in.AdjustmentNo]; ok {
		if rec.fingerprint != fp {
			return nil, ErrAdjustmentConflict
		}
		return cloneReport(s.reports[rec.reportID]), nil
	}
	base, err := s.currentCalendar(in.Scope)
	if err != nil {
		return nil, err
	}
	if _, err := time.Parse("15:04", in.Cutoff); err != nil {
		return nil, fmt.Errorf("invalid cutoff %q: %w", in.Cutoff, err)
	}
	draft := &Calendar{
		Version:      base.Version + 1,
		Scope:        in.Scope,
		Cutoff:       in.Cutoff,
		Holidays:     copyBoolMap(in.Holidays),
		ExtraWorkday: copyBoolMap(in.ExtraWorkday),
		AdjustmentNo: in.AdjustmentNo,
	}
	items, err := s.computeImpactLocked(draft)
	if err != nil {
		return nil, err
	}
	report := &ImpactReport{
		ID:           s.nextID("R"),
		AdjustmentNo: in.AdjustmentNo,
		Scope:        in.Scope,
		BaseVersion:  base.Version,
		NewVersion:   draft.Version,
		Items:        items,
		Diffs:        DiffCalendars(base, draft),
		CreatedAt:    time.Now(),
	}
	s.reports[report.ID] = report
	s.drafts[report.ID] = draft
	s.adjustments[in.AdjustmentNo] = &adjustmentRecord{fingerprint: fp, reportID: report.ID}
	return cloneReport(report), nil
}

// computeImpactLocked 计算新日历会改变哪些未提交批次的价值日。
// 已送银行的批次不参与重新排期。
func (s *Service) computeImpactLocked(newCal *Calendar) ([]ImpactItem, error) {
	var items []ImpactItem
	ids := make([]string, 0, len(s.batches))
	for id := range s.batches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := s.batches[id]
		if b.Scope != newCal.Scope || b.Status != StatusPending {
			continue
		}
		newDate, _, err := newCal.ComputeValueDate(b.ReceivedAt)
		if err != nil {
			return nil, err
		}
		if newDate != b.ValueDate {
			items = append(items, ImpactItem{
				BatchID:      b.ID,
				OldValueDate: b.ValueDate,
				NewValueDate: newDate,
			})
		}
	}
	return items, nil
}

// ConfirmAdjustment 确认调整：重新校验基准日历版本与影响范围，
// 若与清单不一致则拒绝；校验通过后原子地生效新日历版本，
// 并一次性迁移所有受影响的未提交批次。已送银行的批次保留原价值日。
func (s *Service) ConfirmAdjustment(reportID string) (*ImpactReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.reports[reportID]
	if !ok {
		return nil, ErrReportNotFound
	}
	if report.Confirmed {
		return cloneReport(report), nil
	}
	base, err := s.currentCalendar(report.Scope)
	if err != nil {
		return nil, err
	}
	if base.Version != report.BaseVersion {
		return nil, fmt.Errorf("%w: base version %d -> %d", ErrReportStale, report.BaseVersion, base.Version)
	}
	draft, ok := s.drafts[report.ID]
	if !ok {
		return nil, ErrReportNotFound
	}
	fresh, err := s.computeImpactLocked(draft)
	if err != nil {
		return nil, err
	}
	if !impactEqual(report.Items, fresh) {
		return nil, fmt.Errorf("%w: impact scope changed since report %s", ErrReportStale, report.ID)
	}
	finalCal := cloneCalendar(draft)
	finalCal.Effective = true
	finalCal.CreatedAt = time.Now()
	s.calendars[report.Scope] = append(s.calendars[report.Scope], finalCal)
	for _, item := range report.Items {
		b := s.batches[item.BatchID]
		if b.Status != StatusPending {
			continue
		}
		b.ValueDate = item.NewValueDate
		b.CalendarVersion = finalCal.Version
	}
	report.Confirmed = true
	return cloneReport(report), nil
}

// DeleteAdjustment 删除未生效的调整草稿；已生效的日历调整不得删除，
// 只能通过新的版本恢复或再次调整。
func (s *Service) DeleteAdjustment(adjustmentNo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.adjustments[adjustmentNo]
	if !ok {
		return ErrReportNotFound
	}
	if s.reports[rec.reportID].Confirmed {
		return ErrAdjustmentEffective
	}
	delete(s.reports, rec.reportID)
	delete(s.drafts, rec.reportID)
	delete(s.adjustments, adjustmentNo)
	return nil
}

// AffectedBatches 查询某调整号影响清单中的批次。
func (s *Service) AffectedBatches(adjustmentNo string) ([]ImpactItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.adjustments[adjustmentNo]
	if !ok {
		return nil, ErrReportNotFound
	}
	return append([]ImpactItem(nil), s.reports[rec.reportID].Items...), nil
}
