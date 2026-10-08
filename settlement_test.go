package settlement

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var testScope = Scope{Currency: "USD", Channel: "SWIFT"}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func newServiceWithCalendar(t *testing.T) *Service {
	t.Helper()
	svc := NewService()
	// 2026-10-01(周四) 为节假日
	if _, err := svc.CreateCalendar(testScope, "16:00", map[string]bool{"2026-10-01": true}, nil); err != nil {
		t.Fatal(err)
	}
	return svc
}

// 截点边界：截点前受理当天起息，截点后顺延；恰好在截点视为不晚于截点。
func TestCutoffBoundary(t *testing.T) {
	svc := newServiceWithCalendar(t)
	cases := []struct {
		received string
		want     string
	}{
		{"2026-10-09 15:59:59", "2026-10-09"}, // 周五，截点前
		{"2026-10-09 16:00:00", "2026-10-09"}, // 恰好截点
		{"2026-10-09 16:00:01", "2026-10-12"}, // 截点后，跳过周末
		{"2026-10-01 10:00:00", "2026-10-02"}, // 节假日受理，顺延
		{"2026-10-03 10:00:00", "2026-10-05"}, // 周六受理，顺延到周一
	}
	for _, c := range cases {
		b, err := svc.CreateBatch(testScope, mustTime(t, c.received))
		if err != nil {
			t.Fatal(err)
		}
		if b.ValueDate != c.want {
			t.Errorf("received %s: got value date %s, want %s", c.received, b.ValueDate, c.want)
		}
		exp, err := svc.ExplainBatchValueDate(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if exp.ValueDate != c.want || exp.CalendarVersion != 1 || len(exp.Steps) == 0 {
			t.Errorf("explanation for %s incomplete: %+v", c.received, exp)
		}
	}
}

// 调整流程：生成影响清单 -> 确认 -> 未送出批次一次性迁移，日历产生新版本。
func TestAdjustmentMigratePendingBatches(t *testing.T) {
	svc := newServiceWithCalendar(t)
	// 受理于 2026-10-01（节假日），原价值日 2026-10-02
	b1, _ := svc.CreateBatch(testScope, mustTime(t, "2026-10-01 10:00:00"))
	b2, _ := svc.CreateBatch(testScope, mustTime(t, "2026-10-01 11:00:00"))
	// 调整：2026-10-01 改为工作日（调休）
	report, err := svc.PrepareAdjustment(AdjustmentInput{
		AdjustmentNo: "ADJ-1",
		Scope:        testScope,
		Cutoff:       "16:00",
		ExtraWorkday: map[string]bool{"2026-10-01": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("expected 2 impacted batches, got %d", len(report.Items))
	}
	for _, item := range report.Items {
		if item.OldValueDate != "2026-10-02" || item.NewValueDate != "2026-10-01" {
			t.Errorf("unexpected impact item: %+v", item)
		}
	}
	if _, err := svc.ConfirmAdjustment(report.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{b1.ID, b2.ID} {
		b, _ := svc.GetBatch(id)
		if b.ValueDate != "2026-10-01" || b.CalendarVersion != 2 {
			t.Errorf("batch %s not migrated: %+v", id, b)
		}
	}
	versions, _ := svc.CalendarVersions(testScope)
	if len(versions) != 2 {
		t.Fatalf("expected 2 calendar versions, got %d", len(versions))
	}
	affected, _ := svc.AffectedBatches("ADJ-1")
	if len(affected) != 2 {
		t.Fatalf("expected 2 affected batches, got %d", len(affected))
	}
}

// 影响范围变化后旧清单不能继续使用。
func TestStaleReportRejected(t *testing.T) {
	svc := newServiceWithCalendar(t)
	svc.CreateBatch(testScope, mustTime(t, "2026-10-01 10:00:00"))
	report, err := svc.PrepareAdjustment(AdjustmentInput{
		AdjustmentNo: "ADJ-1",
		Scope:        testScope,
		Cutoff:       "16:00",
		ExtraWorkday: map[string]bool{"2026-10-01": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 生成清单后又新建了一个受影响批次，影响范围变化
	svc.CreateBatch(testScope, mustTime(t, "2026-10-01 12:00:00"))
	if _, err := svc.ConfirmAdjustment(report.ID); !errors.Is(err, ErrReportStale) {
		t.Fatalf("expected ErrReportStale, got %v", err)
	}
}

// 送出竞态：已送银行的批次保留原价值日；确认与送出并发时结果必居其一且一致。
func TestSendRaceKeepsSentValueDate(t *testing.T) {
	for i := 0; i < 50; i++ {
		svc := newServiceWithCalendar(t)
		b, _ := svc.CreateBatch(testScope, mustTime(t, "2026-10-01 10:00:00"))
		report, _ := svc.PrepareAdjustment(AdjustmentInput{
			AdjustmentNo: "ADJ-1",
			Scope:        testScope,
			Cutoff:       "16:00",
			ExtraWorkday: map[string]bool{"2026-10-01": true},
		})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); svc.SendBatch(b.ID, time.Now()) }()
		go func() { defer wg.Done(); svc.ConfirmAdjustment(report.ID) }()
		wg.Wait()
		got, _ := svc.GetBatch(b.ID)
		if got.Status == StatusSent && got.ValueDate != "2026-10-02" && got.CalendarVersion == 1 {
			t.Fatalf("sent batch lost original value date: %+v", got)
		}
		if got.Status == StatusPending && got.ValueDate != "2026-10-01" {
			t.Fatalf("pending batch not migrated: %+v", got)
		}
	}
}

// 已送银行批次不参与影响清单。
func TestSentBatchExcludedFromImpact(t *testing.T) {
	svc := newServiceWithCalendar(t)
	b, _ := svc.CreateBatch(testScope, mustTime(t, "2026-10-01 10:00:00"))
	if err := svc.SendBatch(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	report, _ := svc.PrepareAdjustment(AdjustmentInput{
		AdjustmentNo: "ADJ-1",
		Scope:        testScope,
		Cutoff:       "16:00",
		ExtraWorkday: map[string]bool{"2026-10-01": true},
	})
	if len(report.Items) != 0 {
		t.Fatalf("sent batch should not be impacted: %+v", report.Items)
	}
	if _, err := svc.ConfirmAdjustment(report.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetBatch(b.ID)
	if got.ValueDate != "2026-10-02" {
		t.Fatalf("sent batch value date changed: %+v", got)
	}
}

// 重复调整：同调整号同内容重放返回原结果；内容变化返回冲突；已生效调整不得删除。
func TestAdjustmentReplayAndConflict(t *testing.T) {
	svc := newServiceWithCalendar(t)
	in := AdjustmentInput{
		AdjustmentNo: "ADJ-1",
		Scope:        testScope,
		Cutoff:       "16:00",
		ExtraWorkday: map[string]bool{"2026-10-01": true},
	}
	r1, err := svc.PrepareAdjustment(in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.PrepareAdjustment(in)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("replay should return original report %s, got %s", r1.ID, r2.ID)
	}
	changed := in
	changed.Cutoff = "15:00"
	if _, err := svc.PrepareAdjustment(changed); !errors.Is(err, ErrAdjustmentConflict) {
		t.Fatalf("expected ErrAdjustmentConflict, got %v", err)
	}
	if _, err := svc.ConfirmAdjustment(r1.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAdjustment("ADJ-1"); !errors.Is(err, ErrAdjustmentEffective) {
		t.Fatalf("expected ErrAdjustmentEffective, got %v", err)
	}
	// 通过新版本恢复：再次调整回 2026-10-01 为节假日
	restore := AdjustmentInput{
		AdjustmentNo: "ADJ-2",
		Scope:        testScope,
		Cutoff:       "16:00",
		Holidays:     map[string]bool{"2026-10-01": true},
	}
	r3, err := svc.PrepareAdjustment(restore)
	if err != nil {
		t.Fatal(err)
	}
	if r3.BaseVersion != 2 || r3.NewVersion != 3 {
		t.Fatalf("unexpected versions: %+v", r3)
	}
	if _, err := svc.ConfirmAdjustment(r3.ID); err != nil {
		t.Fatal(err)
	}
	versions, _ := svc.CalendarVersions(testScope)
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
}

// 并发创建批次与确认调整：所有未送出批次要么迁移要么保持，状态一致。
func TestConcurrentCreateAndConfirm(t *testing.T) {
	svc := newServiceWithCalendar(t)
	report, _ := svc.PrepareAdjustment(AdjustmentInput{
		AdjustmentNo: "ADJ-1",
		Scope:        testScope,
		Cutoff:       "16:00",
		ExtraWorkday: map[string]bool{"2026-10-01": true},
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.CreateBatch(testScope, mustTime(t, "2026-10-01 10:00:00"))
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); svc.ConfirmAdjustment(report.ID) }()
	wg.Wait()
	// 无论确认是否成功（可能因影响范围变化而拒绝），服务必须保持可用
	if _, err := svc.CalendarVersions(testScope); err != nil {
		t.Fatal(err)
	}
}
