package settlement

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func at(y, m, d, hh, mm int) time.Time {
	return time.Date(y, time.Month(m), d, hh, mm, 0, 0, time.UTC)
}

func day(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", s, time.UTC)
	return t
}

func newServiceWithCalendar() *Service {
	s := NewService()
	s.CreateCalendar("USD", "SWIFT", 16*time.Hour, map[string]bool{"2026-01-01": true})
	return s
}

// 截点边界：恰好等于截点按当日受理，超过一秒即顺延。
func TestCutoffBoundary(t *testing.T) {
	s := newServiceWithCalendar()
	// 2026-01-05 是周一，工作日。
	b1, _ := s.CreateBatch("B1", "USD", "SWIFT", at(2026, 1, 5, 16, 0))
	if got := b1.ValueDate; !got.Equal(day("2026-01-05")) {
		t.Fatalf("截点时刻应算当日，得到 %v", got)
	}
	b2, _ := s.CreateBatch("B2", "USD", "SWIFT", at(2026, 1, 5, 16, 0).Add(time.Second))
	if got := b2.ValueDate; !got.Equal(day("2026-01-06")) {
		t.Fatalf("超过截点应顺延到下一工作日，得到 %v", got)
	}
}

// 节假日与周末顺延。
func TestValueDateSkipsHolidayAndWeekend(t *testing.T) {
	s := newServiceWithCalendar()
	// 2025-12-31 周三 17:00 超过截点 -> 候选 2026-01-01（节假日）-> 01-02 周五。
	b, _ := s.CreateBatch("B1", "USD", "SWIFT", at(2025, 12, 31, 17, 0))
	if got := b.ValueDate; !got.Equal(day("2026-01-02")) {
		t.Fatalf("应跳过节假日，得到 %v", got)
	}
	// 2026-01-02 周五 17:00 超过截点 -> 候选周六 -> 周一 01-05。
	b2, _ := s.CreateBatch("B2", "USD", "SWIFT", at(2026, 1, 2, 17, 0))
	if got := b2.ValueDate; !got.Equal(day("2026-01-05")) {
		t.Fatalf("应跳过周末，得到 %v", got)
	}
}

// 日期计算依据可解释。
func TestExplainBatch(t *testing.T) {
	s := newServiceWithCalendar()
	s.CreateBatch("B1", "USD", "SWIFT", at(2025, 12, 31, 17, 0))
	d, err := s.ExplainBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || !d.ValueDate.Equal(day("2026-01-02")) || len(d.Steps) == 0 {
		t.Fatalf("推导结果不正确: %+v", d)
	}
}

// 调整预览 -> 确认：未提交批次一次性迁移，已送银行批次保留原价值日。
func TestAdjustmentMigratesOnlyPendingBatches(t *testing.T) {
	s := newServiceWithCalendar()
	s.CreateBatch("P1", "USD", "SWIFT", at(2026, 1, 5, 10, 0)) // 价值日 01-05
	s.CreateBatch("P2", "USD", "SWIFT", at(2026, 1, 5, 10, 0))
	s.CreateBatch("S1", "USD", "SWIFT", at(2026, 1, 5, 10, 0))
	if err := s.SendBatch("S1"); err != nil {
		t.Fatal(err)
	}
	// 调整：把 01-05 设为节假日。
	req := AdjustmentRequest{
		ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 16 * time.Hour,
		Holidays: map[string]bool{"2026-01-01": true, "2026-01-05": true},
	}
	report, err := s.PreviewAdjustment(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("影响清单应只含 2 个未提交批次，得到 %d", len(report.Items))
	}
	adj, err := s.ConfirmAdjustment(report.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	if adj.NewVersion.Version != 2 {
		t.Fatalf("应生成日历版本 2，得到 %d", adj.NewVersion.Version)
	}
	for _, id := range []string{"P1", "P2"} {
		b, _ := s.GetBatch(id)
		if !b.ValueDate.Equal(day("2026-01-06")) || b.CalVersion != 2 || b.AdjustmentID != "ADJ-1" {
			t.Fatalf("批次 %s 未正确迁移: %+v", id, b)
		}
	}
	sent, _ := s.GetBatch("S1")
	if !sent.ValueDate.Equal(day("2026-01-05")) || sent.CalVersion != 1 {
		t.Fatalf("已送银行批次必须保留原价值日: %+v", sent)
	}
}

// 预览后影响范围变化（批次被送出），旧清单不能继续使用。
func TestStalePreviewRejected(t *testing.T) {
	s := newServiceWithCalendar()
	s.CreateBatch("P1", "USD", "SWIFT", at(2026, 1, 5, 10, 0))
	req := AdjustmentRequest{
		ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 16 * time.Hour,
		Holidays: map[string]bool{"2026-01-01": true, "2026-01-05": true},
	}
	report, _ := s.PreviewAdjustment(req)
	if err := s.SendBatch("P1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmAdjustment(report.ReportID); !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("影响范围变化后旧清单应失效，得到 %v", err)
	}
}

// 相同调整号 + 相同内容重放返回原结果；内容变化返回冲突。
func TestAdjustmentIdempotency(t *testing.T) {
	s := newServiceWithCalendar()
	s.CreateBatch("P1", "USD", "SWIFT", at(2026, 1, 5, 10, 0))
	req := AdjustmentRequest{
		ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 16 * time.Hour,
		Holidays: map[string]bool{"2026-01-01": true, "2026-01-05": true},
	}
	r1, _ := s.PreviewAdjustment(req)
	a1, err := s.ConfirmAdjustment(r1.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	// 重放：相同内容 -> 返回原结果，不再迁移。
	r2, _ := s.PreviewAdjustment(req)
	a2, err := s.ConfirmAdjustment(r2.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	if a2.NewVersion.Version != a1.NewVersion.Version || !a2.ConfirmedAt.Equal(a1.ConfirmedAt) {
		t.Fatalf("重放应返回原结果: %+v vs %+v", a1, a2)
	}
	// 同号不同内容 -> 冲突。
	req.Holidays = map[string]bool{"2026-01-01": true, "2026-01-06": true}
	if _, err := s.PreviewAdjustment(req); !errors.Is(err, ErrAdjustmentExists) {
		t.Fatalf("内容变化应返回冲突，得到 %v", err)
	}
}

// 已生效调整不得删除。
func TestAppliedAdjustmentCannotBeDeleted(t *testing.T) {
	s := newServiceWithCalendar()
	req := AdjustmentRequest{
		ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 16 * time.Hour,
		Holidays: map[string]bool{"2026-01-01": true},
	}
	r, _ := s.PreviewAdjustment(req)
	if _, err := s.ConfirmAdjustment(r.ReportID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAdjustment("ADJ-1"); !errors.Is(err, ErrAdjustmentApplied) {
		t.Fatalf("已生效调整不得删除，得到 %v", err)
	}
}

// 日历确认与批次送出并发：送出批次保留原价值日，未送出批次全部迁移。
func TestConcurrentConfirmAndSend(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		s := newServiceWithCalendar()
		const n = 20
		for i := 0; i < n; i++ {
			s.CreateBatch(fmt.Sprintf("B%d", i), "USD", "SWIFT", at(2026, 1, 5, 10, 0))
		}
		req := AdjustmentRequest{
			ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 16 * time.Hour,
			Holidays: map[string]bool{"2026-01-01": true, "2026-01-05": true},
		}
		report, _ := s.PreviewAdjustment(req)
		var wg sync.WaitGroup
		sentValueDates := make([]time.Time, n)
		var mu sync.Mutex
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := s.SendBatch(fmt.Sprintf("B%d", i)); err == nil {
					b, _ := s.GetBatch(fmt.Sprintf("B%d", i))
					mu.Lock()
					sentValueDates[i] = b.ValueDate
					mu.Unlock()
				}
			}(i)
		}
		wg.Add(1)
		var confirmErr error
		go func() {
			defer wg.Done()
			_, confirmErr = s.ConfirmAdjustment(report.ReportID)
		}()
		wg.Wait()
		if confirmErr != nil && !errors.Is(confirmErr, ErrPreviewStale) {
			t.Fatalf("确认只允许成功或清单失效，得到 %v", confirmErr)
		}
		// 无论确认是否成功，最终状态必须一致：已送出批次保留送出时刻的价值日，
		// 未送出批次要么全部迁移到 01-06，要么（清单失效时）全部保持 01-05。
		migrated, kept := 0, 0
		for i := 0; i < n; i++ {
			b, _ := s.GetBatch(fmt.Sprintf("B%d", i))
			if b.Status == StatusSent {
				if !b.ValueDate.Equal(sentValueDates[i]) {
					t.Fatalf("已送出批次价值日被改动: %+v，送出时为 %v", b, sentValueDates[i])
				}
				continue
			}
			if b.ValueDate.Equal(day("2026-01-06")) {
				migrated++
			} else {
				kept++
			}
		}
		if migrated > 0 && kept > 0 {
			t.Fatalf("未送出批次必须一次性迁移，migrated=%d kept=%d", migrated, kept)
		}
	}
}

// 日历版本历史可查询。
func TestCalendarVersionHistory(t *testing.T) {
	s := newServiceWithCalendar()
	req := AdjustmentRequest{
		ID: "ADJ-1", Currency: "USD", Channel: "SWIFT", Cutoff: 15 * time.Hour,
		Holidays: map[string]bool{"2026-01-01": true},
	}
	r, _ := s.PreviewAdjustment(req)
	if _, err := s.ConfirmAdjustment(r.ReportID); err != nil {
		t.Fatal(err)
	}
	vs := s.CalendarVersions("USD", "SWIFT")
	if len(vs) != 2 || vs[0].Version != 1 || vs[1].Version != 2 || vs[1].Cutoff != 15*time.Hour {
		t.Fatalf("版本历史不正确: %+v", vs)
	}
	adj, err := s.GetAdjustment("ADJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if adj.OldVersion.Cutoff != 16*time.Hour || adj.NewVersion.Cutoff != 15*time.Hour {
		t.Fatalf("调整前后对比不正确: %+v", adj)
	}
}
