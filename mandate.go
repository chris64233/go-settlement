package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// VersionStatus describes the lifecycle state of an account version.
type VersionStatus string

const (
	StatusPending        VersionStatus = "PENDING"         // submitted, awaiting reviews
	StatusAwaitingEffect VersionStatus = "AWAITING_EFFECT" // approved, waiting for the safety waiting period
	StatusActive         VersionStatus = "ACTIVE"          // effective and usable for settlement
	StatusRejected       VersionStatus = "REJECTED"        // terminated by a reviewer rejection
	StatusRevoked        VersionStatus = "REVOKED"         // withdrawn before/after activation
	StatusSuperseded     VersionStatus = "SUPERSEDED"      // replaced by a newer activated version
)

var (
	ErrVersionNotFound   = errors.New("account version not found")
	ErrVersionNotPending = errors.New("account version is not pending review")
	ErrDuplicateReviewer = errors.New("reviewer has already decided on this version")
	ErrEffectiveTooEarly = errors.New("effective time is before the end of the safety waiting period")
	ErrNoActiveVersion   = errors.New("account has no active version")
)

// Application is the content of an account creation or change request.
// The account number itself is never stored; only AccountDigest (a salted
// hash supplied by the caller via DigestAccount) is kept. Once submitted the
// content digest is frozen: any field change requires a new version.
type Application struct {
	AccountID     string    // empty for a brand-new account
	HolderName    string    // account holder
	BankID        string    // bank identifier (e.g. SWIFT/BIC or clearing code)
	AccountDigest string    // security digest of the account number, never plaintext
	Currencies    []string  // applicable currencies
	EffectiveAt   time.Time // requested effective time
}

// DigestAccount computes the security digest for a plaintext account number.
// The plaintext must not be stored or logged; only the digest is persisted.
func DigestAccount(accountNumber string) string {
	sum := sha256.Sum256([]byte("settlement-account\x00" + accountNumber))
	return hex.EncodeToString(sum[:])
}

// MaskDigest renders a digest safe for ordinary queries and logs.
func MaskDigest(digest string) string {
	if len(digest) <= 8 {
		return "****"
	}
	return digest[:4] + "****" + digest[len(digest)-4:]
}

// contentDigest freezes the application content into a tamper-evident digest.
func contentDigest(app Application) string {
	currencies := append([]string(nil), app.Currencies...)
	sort.Strings(currencies)
	h := sha256.New()
	h.Write([]byte(app.HolderName))
	h.Write([]byte{0})
	h.Write([]byte(app.BankID))
	h.Write([]byte{0})
	h.Write([]byte(app.AccountDigest))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(currencies, ",")))
	h.Write([]byte{0})
	h.Write([]byte(app.EffectiveAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))
}

// Review records a single reviewer's decision on a version.
type Review struct {
	Reviewer  string
	EventID   string
	Approved  bool
	Reason    string
	DecidedAt time.Time
}

// Version is one immutable version of an account application.
type Version struct {
	AccountID     string
	VersionNo     int
	ContentDigest string
	Application   Application
	Status        VersionStatus
	Reviews       []Review
	ActivatedAt   time.Time
	ClosedReason  string
}

// VersionView is the masked representation returned by ordinary queries.
// It never exposes sensitive material beyond the masked digest.
type VersionView struct {
	AccountID     string
	VersionNo     int
	HolderName    string
	BankID        string
	MaskedAccount string
	Currencies    []string
	EffectiveAt   time.Time
	Status        VersionStatus
	Reviewers     []string
	ActivatedAt   time.Time
	ClosedReason  string
}

func toView(v *Version) VersionView {
	reviewers := make([]string, 0, len(v.Reviews))
	for _, r := range v.Reviews {
		reviewers = append(reviewers, r.Reviewer)
	}
	return VersionView{
		AccountID:     v.AccountID,
		VersionNo:     v.VersionNo,
		HolderName:    v.Application.HolderName,
		BankID:        v.Application.BankID,
		MaskedAccount: MaskDigest(v.Application.AccountDigest),
		Currencies:    append([]string(nil), v.Application.Currencies...),
		EffectiveAt:   v.Application.EffectiveAt,
		Status:        v.Status,
		Reviewers:     reviewers,
		ActivatedAt:   v.ActivatedAt,
		ClosedReason:  v.ClosedReason,
	}
}

// MandateConfig tunes approval and waiting-period policy.
type MandateConfig struct {
	RequiredApprovals int           // number of distinct approvers (default 2)
	WaitingPeriod     time.Duration // safety waiting period after final approval
}

// MandateStore manages account applications, reviews and version history.
// It is safe for concurrent use.
type MandateStore struct {
	mu       sync.Mutex
	cfg      MandateConfig
	versions map[string][]*Version // accountID -> ordered versions
	events   map[string]bool       // processed review event IDs (idempotency)
	seq      int
}

func NewMandateStore(cfg MandateConfig) *MandateStore {
	if cfg.RequiredApprovals < 2 {
		cfg.RequiredApprovals = 2 // dual control is the minimum
	}
	return &MandateStore{
		cfg:      cfg,
		versions: make(map[string][]*Version),
		events:   make(map[string]bool),
	}
}

// Submit creates a new account application version. The content digest is
// frozen at creation; changing any field requires submitting a new version.
func (s *MandateStore) Submit(app Application, now time.Time) (VersionView, error) {
	if app.HolderName == "" || app.BankID == "" || app.AccountDigest == "" {
		return VersionView{}, errors.New("holder name, bank ID and account digest are required")
	}
	if len(app.Currencies) == 0 {
		return VersionView{}, errors.New("at least one currency is required")
	}
	if app.EffectiveAt.Before(now) {
		return VersionView{}, errors.New("effective time must not be in the past")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if app.AccountID == "" {
		s.seq++
		app.AccountID = fmt.Sprintf("acct-%d", s.seq)
	}
	// A pending or awaiting-effect version must be decided or revoked before
	// a new change can be submitted for the same account.
	for _, v := range s.versions[app.AccountID] {
		if v.Status == StatusPending || v.Status == StatusAwaitingEffect {
			return VersionView{}, fmt.Errorf("account %s already has an undecided version %d", app.AccountID, v.VersionNo)
		}
	}
	v := &Version{
		AccountID:     app.AccountID,
		VersionNo:     len(s.versions[app.AccountID]) + 1,
		ContentDigest: contentDigest(app),
		Application:   app,
		Status:        StatusPending,
	}
	s.versions[app.AccountID] = append(s.versions[app.AccountID], v)
	return toView(v), nil
}

func (s *MandateStore) findVersion(accountID string, versionNo int) (*Version, error) {
	for _, v := range s.versions[accountID] {
		if v.VersionNo == versionNo {
			return v, nil
		}
	}
	return nil, ErrVersionNotFound
}

// review applies a reviewer's decision. Review event IDs are idempotent:
// re-submitting the same event is a no-op. A reviewer may decide on a given
// version at most once.
func (s *MandateStore) review(accountID string, versionNo int, reviewer, eventID string, approved bool, reason string, now time.Time) error {
	if reviewer == "" || eventID == "" {
		return errors.New("reviewer and event ID are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.events[eventID] {
		return nil // idempotent replay
	}
	v, err := s.findVersion(accountID, versionNo)
	if err != nil {
		return err
	}
	if v.Status != StatusPending {
		return ErrVersionNotPending
	}
	for _, r := range v.Reviews {
		if r.Reviewer == reviewer {
			return ErrDuplicateReviewer
		}
	}
	s.events[eventID] = true
	v.Reviews = append(v.Reviews, Review{
		Reviewer:  reviewer,
		EventID:   eventID,
		Approved:  approved,
		Reason:    reason,
		DecidedAt: now,
	})

	if !approved {
		// Any single rejection terminates the version.
		v.Status = StatusRejected
		v.ClosedReason = reason
		return nil
	}
	approvals := 0
	for _, r := range v.Reviews {
		if r.Approved {
			approvals++
		}
	}
	if approvals >= s.cfg.RequiredApprovals {
		// The safety waiting period starts at the final approval; the
		// version cannot take effect before it elapses.
		earliest := now.Add(s.cfg.WaitingPeriod)
		if v.Application.EffectiveAt.Before(earliest) {
			return fmt.Errorf("%w: effective %s, earliest %s",
				ErrEffectiveTooEarly,
				v.Application.EffectiveAt.UTC().Format(time.RFC3339),
				earliest.UTC().Format(time.RFC3339))
		}
		v.Status = StatusAwaitingEffect
	}
	return nil
}

// Approve records an approval decision by reviewer on the given version.
func (s *MandateStore) Approve(accountID string, versionNo int, reviewer, eventID string, now time.Time) error {
	return s.review(accountID, versionNo, reviewer, eventID, true, "", now)
}

// Reject records a rejection; a single rejection terminates the version.
func (s *MandateStore) Reject(accountID string, versionNo int, reviewer, eventID, reason string, now time.Time) error {
	return s.review(accountID, versionNo, reviewer, eventID, false, reason, now)
}

// ActivateDue activates every approved version whose effective time and
// waiting period have both elapsed, superseding the previously active
// version of the same account. Returns the activated versions.
func (s *MandateStore) ActivateDue(now time.Time) []VersionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var activated []VersionView
	for _, vs := range s.versions {
		for _, v := range vs {
			if v.Status != StatusAwaitingEffect {
				continue
			}
			if now.Before(v.Application.EffectiveAt) {
				continue
			}
			for _, other := range vs {
				if other.Status == StatusActive {
					other.Status = StatusSuperseded
					other.ClosedReason = fmt.Sprintf("superseded by version %d", v.VersionNo)
				}
			}
			v.Status = StatusActive
			v.ActivatedAt = now
			activated = append(activated, toView(v))
		}
	}
	return activated
}

// Revoke withdraws a version. Pending, awaiting-effect and active versions
// can be revoked; terminal versions cannot.
func (s *MandateStore) Revoke(accountID string, versionNo int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.findVersion(accountID, versionNo)
	if err != nil {
		return err
	}
	switch v.Status {
	case StatusPending, StatusAwaitingEffect, StatusActive:
		v.Status = StatusRevoked
		v.ClosedReason = reason
		return nil
	default:
		return fmt.Errorf("version in status %s cannot be revoked", v.Status)
	}
}

// ActiveVersion returns the currently active version of an account.
func (s *MandateStore) ActiveVersion(accountID string) (VersionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions[accountID] {
		if v.Status == StatusActive {
			return toView(v), nil
		}
	}
	return VersionView{}, ErrNoActiveVersion
}

// GetVersion returns the masked view of one specific version.
func (s *MandateStore) GetVersion(accountID string, versionNo int) (VersionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.findVersion(accountID, versionNo)
	if err != nil {
		return VersionView{}, err
	}
	return toView(v), nil
}

// History returns the masked version history of an account, oldest first.
func (s *MandateStore) History(accountID string) []VersionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []VersionView
	for _, v := range s.versions[accountID] {
		out = append(out, toView(v))
	}
	return out
}

// snapshot is the internal lookup used by the instruction store.
func (s *MandateStore) snapshot(accountID string, versionNo int) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findVersion(accountID, versionNo)
}
