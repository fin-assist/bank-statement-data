package importing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Process recovers one admitted job; source I/O and parsing happen before the
// atomic user transaction which links operations, rules, result and recalculation.
func (s *Service) Process(ctx context.Context, user, id string) error {
	parent := ctx
	ctx, envelope := context.WithTimeout(parent, 3*time.Minute)
	defer envelope()
	var upload Upload
	err := s.tx.Read(ctx, func(read context.Context) error {
		var err error
		upload, err = s.repo.Upload(read, user, id)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if upload.Status != "processing" {
		return nil
	}
	remaining := 3*time.Minute - s.now().Sub(upload.UploadedAt)
	if remaining <= 0 {
		return s.failProcessingWorker(parent, user, id)
	}
	if remaining > 3*time.Minute {
		remaining = 3 * time.Minute
	}
	work, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	if err = s.progress(work, user, id, 20); err != nil {
		if errors.Is(err, ErrDeleted) || errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	body, err := s.objects.Open(work, upload.ObjectKey)
	var document Document
	if err == nil {
		document, err = func() (parsed Document, resultErr error) {
			defer func() {
				if closed := body.Close(); resultErr == nil {
					resultErr = closed
				}
			}()
			digest := sha256.New()
			source := &io.LimitedReader{R: io.TeeReader(body, digest), N: MaxFileBytes + 1}
			parsed, resultErr = s.parser.Parse(work, source)
			if resultErr == nil {
				_, resultErr = io.Copy(io.Discard, source)
			}
			if resultErr == nil && (MaxFileBytes+1-source.N != upload.SizeBytes || source.N == 0 || !bytes.Equal(digest.Sum(nil), upload.Digest[:])) {
				resultErr = errAccounting
			}
			return parsed, resultErr
		}()
	}
	if err == nil {
		err = s.progress(work, user, id, 60)
	}
	if err == nil {
		err = s.tx.Run(work, func(txCtx context.Context) error {
			if err := s.guard(txCtx, user, false); err != nil {
				return err
			}
			current, err := s.repo.Upload(txCtx, user, id)
			if err != nil {
				return err
			}
			if current.Status != "processing" {
				return ErrNotFound
			}
			stage, err := s.repo.Stage(txCtx, user, id, document, s.now())
			if err != nil {
				return err
			}
			zone, err := time.LoadLocation(stage.Snapshot.TimeZone)
			if err != nil {
				return errAccounting
			}
			operations, err := ApplyRules(txCtx, stage.Snapshot.Operations, stage.Snapshot.Categories, s.newID)
			if err != nil {
				return err
			}
			period := statementPeriod(document, zone)
			before := completedPeriods(stage.Snapshot.Uploads)
			after := append([]Period(nil), before...)
			if period != nil {
				after = append(after, *period)
			}
			first := true
			for _, old := range stage.Snapshot.Uploads {
				if old.Status == "done" {
					first = false
					break
				}
			}
			result, err := BuildResult(txCtx, ResultInput{Document: document, Operations: operations, NewIDs: stage.NewIDs, Before: before, After: after, Now: s.now(), Zone: zone, FirstImport: first})
			if err != nil {
				return err
			}
			if err := s.repo.Complete(txCtx, user, id, stage, operations, result, period, s.now()); err != nil {
				return err
			}
			return s.resolveUnread(txCtx, user)
		})
	}
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrDeleted) {
		return nil
	}
	// A graceful pod stop leaves the durable job to a surviving/restarted worker.
	if parent.Err() != nil {
		return parent.Err()
	}
	return s.failProcessingWorker(parent, user, id)
}

func (s *Service) progress(ctx context.Context, user, id string, percent int) error {
	return s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, false); err != nil {
			return err
		}
		return s.repo.Progress(txCtx, user, id, percent)
	})
}

// Workers may race with completed user erasure; streams must report revocation.
func (s *Service) failProcessingWorker(ctx context.Context, user, id string) error {
	err := s.failProcessing(ctx, user, id)
	if errors.Is(err, ErrDeleted) {
		return nil
	}
	return err
}

func (s *Service) failProcessing(ctx context.Context, user, id string) error {
	work, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	changed := false
	err := s.tx.Run(work, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, false); err != nil {
			return err
		}
		var err error
		changed, err = s.repo.Fail(txCtx, user, id, "processing_failed", s.now())
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil || !changed {
		return err
	}
	return s.cleanupFile(work, user, id)
}

func (s *Service) resolveUnread(ctx context.Context, user string) error {
	state, err := s.repo.Snapshot(ctx, user)
	if err != nil {
		return err
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return errAccounting
	}
	rows, err := ResolveUnread(ctx, state.Uploads, state.Unread, state.Sources, zone)
	if err != nil {
		return err
	}
	return s.repo.SaveUnread(ctx, user, rows)
}

// Watch starts from the current durable state, including reconnects after a crash.
// A failed result is emitted only after confirmed erasure of its original file.
func (s *Service) Watch(ctx context.Context, user, id string, send func(Event) error) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	last := -1
	for {
		if err := s.watchAdmission(ctx, user); err != nil {
			return err
		}
		var upload Upload
		var zone string
		err := s.tx.Read(ctx, func(read context.Context) error {
			var err error
			upload, err = s.repo.Upload(read, user, id)
			if err != nil {
				return err
			}
			zone, err = s.repo.TimeZone(read, user)
			return err
		})
		// A tombstone can commit while the coherent read is in flight. Recheck
		// outside its RR snapshot, including NOT_FOUND after completed purge.
		if admissionErr := s.watchAdmission(ctx, user); admissionErr != nil {
			return admissionErr
		}
		if err != nil {
			return err
		}
		switch upload.Status {
		case "done":
			if upload.Result == nil {
				return errAccounting
			}
			return send(Event{TimeZone: zone, Result: upload.Result})
		case "failed":
			if upload.FileDeletedAt == nil {
				if err := s.cleanupFile(ctx, user, id); err != nil {
					return err
				}
				continue
			}
			if upload.ErrorCode != "cancelled" && upload.ErrorCode != "processing_failed" {
				return errAccounting
			}
			return send(Event{TimeZone: zone, Error: upload.ErrorCode})
		case "processing":
			if s.now().Sub(upload.UploadedAt) >= 3*time.Minute {
				if err := s.failProcessing(ctx, user, id); err != nil {
					return err
				}
				continue
			}
			if upload.Percent != last {
				if err := send(Event{TimeZone: zone, Percent: upload.Percent}); err != nil {
					return err
				}
				last = upload.Percent
			}
		case "staging", "deleted":
			return ErrNotFound
		default:
			return errAccounting
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// watchAdmission uses the published primary reader, never Guard inside RR or
// a lock held across network delivery. A response already in flight is not atomic
// with revocation; this check admits each poll and each newly prepared event.
func (s *Service) watchAdmission(ctx context.Context, user string) error {
	deleted, err := s.deleted.Deleted(ctx, user)
	if err != nil {
		return err
	}
	if deleted {
		return ErrDeleted
	}
	return nil
}

// Delete cancels active processing or removes only this upload's operation sources.
// It retains the successful original-key response, so replay never recreates a deleted upload.
func (s *Service) Delete(ctx context.Context, user, id string) (err error) {
	release, err := s.barrier.Shared(ctx, user)
	if err != nil {
		return err
	}
	defer func() {
		if e := release(); err == nil {
			err = e
		}
	}()
	err = s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, false); err != nil {
			return err
		}
		upload, err := s.repo.Upload(txCtx, user, id)
		if err != nil {
			return err
		}
		switch upload.Status {
		case "staging":
			return ErrNotFound
		case "failed", "deleted":
			return nil
		case "processing":
			_, err := s.repo.Fail(txCtx, user, id, "cancelled", s.now())
			return err
		case "done":
			state, err := s.repo.Snapshot(txCtx, user)
			if err != nil {
				return err
			}
			remaining, err := s.repo.Remove(txCtx, user, id, s.now())
			if err != nil {
				return err
			}
			rules, err := ApplyRules(txCtx, remaining, state.Categories, s.newID)
			if err != nil {
				return err
			}
			if err := s.repo.SaveRules(txCtx, user, rules, s.now()); err != nil {
				return err
			}
			return s.resolveUnread(txCtx, user)
		default:
			return errAccounting
		}
	})
	if err != nil {
		return err
	}
	// Already holding a shared barrier: avoid acquiring a second session while
	// a waiting purge holds the exclusive lock's queue position.
	return s.cleanupFileHeld(ctx, user, id)
}

func (s *Service) cleanupFile(ctx context.Context, user, id string) (err error) {
	release, err := s.barrier.Shared(ctx, user)
	if err != nil {
		return err
	}
	defer func() {
		if e := release(); err == nil {
			err = e
		}
	}()
	return s.cleanupFileHeld(ctx, user, id)
}

func (s *Service) cleanupFileHeld(ctx context.Context, user, id string) error {
	var current Upload
	err := s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, false); err != nil {
			return err
		}
		var err error
		current, err = s.repo.Upload(txCtx, user, id)
		if err != nil {
			return err
		}
		if current.Status == "staging" {
			if err := s.repo.AbandonStaging(txCtx, user, id, s.now()); err != nil {
				return err
			}
			current.Status = "failed"
		}
		return nil
	})
	if err != nil {
		return err
	}
	if current.FileDeletedAt != nil || current.Status != "failed" && current.Status != "deleted" {
		return nil
	}
	if err := s.objects.Delete(ctx, current.ObjectKey); err != nil {
		return err
	}
	return s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, false); err != nil {
			return err
		}
		return s.repo.FileDeleted(txCtx, user, id, s.now())
	})
}

// SetTimezone applies a newer system event; tombstoned users acknowledge it without resurrection.
func (s *Service) SetTimezone(ctx context.Context, user, zone string, version int32) error {
	if zone == "" || zone == "Local" || strings.Contains(zone, "..") || strings.HasPrefix(zone, "/") {
		return &InputError{"timezone", "INVALID_FORMAT"}
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return &InputError{"timezone", "INVALID_FORMAT"}
	}
	return s.tx.Run(ctx, func(txCtx context.Context) error {
		alive, err := s.deleted.Guard(txCtx, user)
		if err != nil || !alive {
			return err
		}
		if _, err := s.repo.LockUser(txCtx, user, true); err != nil {
			return err
		}
		return s.repo.SetTimezone(txCtx, user, zone, version, s.now())
	})
}

// Purge retires admission first, then fences every possible late PUT, and only
// after confirmed content erasure removes personal database rows. Retries resume
// durable per-key confirmations; a failed phase never reports deletion complete.
func (s *Service) Purge(ctx context.Context, user string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	actor, parseErr := uuid.Parse(user)
	if parseErr != nil || actor == uuid.Nil {
		return ErrInput
	}
	user = actor.String()
	err = s.tx.Run(ctx, func(txCtx context.Context) error {
		// Store.Purge intentionally skips its callback for an existing tombstone.
		// Phase two must therefore be a separate privileged transaction.
		return s.deleted.Purge(txCtx, user, func(context.Context, string) error { return nil })
	})
	if err != nil {
		return err
	}
	release, err := s.barrier.Exclusive(ctx, user)
	if err != nil {
		return err
	}
	defer func() {
		if e := release(); err == nil {
			err = e
		}
	}()
	var keys []string
	err = s.tx.Read(ctx, func(read context.Context) error {
		var err error
		keys, err = s.repo.IntentKeys(read, user)
		return err
	})
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !ownedObjectKey(user, key) {
			return errAccounting
		}
		if err := s.fenceForPurge(ctx, user, key); err != nil {
			return err
		}
	}
	cursor := ""
	seen := make(map[string]bool)
	for {
		page, err := s.objects.List(ctx, user+"/", cursor)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if !ownedObjectKey(user, object.Key) {
				return errAccounting
			}
			if err := s.fenceForPurge(ctx, user, object.Key); err != nil {
				return err
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next == cursor || seen[page.Next] {
			return errAccounting
		}
		seen[page.Next], cursor = true, page.Next
	}
	return s.tx.Run(ctx, func(txCtx context.Context) error { return s.repo.Purge(txCtx, user) })
}

func (s *Service) fenceForPurge(ctx context.Context, user, key string) error {
	if err := s.objects.Delete(ctx, key); err != nil {
		return err
	}
	return s.tx.Run(ctx, func(txCtx context.Context) error { return s.repo.MarkFenced(txCtx, user, key, s.now()) })
}

func ownedObjectKey(user, key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || parts[0] != user {
		return false
	}
	id, err := uuid.Parse(parts[1])
	return err == nil && id != uuid.Nil && id.String() == parts[1]
}

func statementPeriod(document Document, zone *time.Location) *Period {
	var from, to time.Time
	source := "tags"
	for _, period := range document.Periods {
		if from.IsZero() || period.From.Before(from) {
			from = period.From
		}
		if to.IsZero() || period.To.After(to) {
			to = period.To
		}
	}
	if from.IsZero() {
		source = "operations"
		for _, line := range document.Lines {
			if line.Reason != "" || line.OccurredAt == nil {
				continue
			}
			if from.IsZero() || line.OccurredAt.Before(from) {
				from = *line.OccurredAt
			}
			if to.IsZero() || line.OccurredAt.After(to) {
				to = *line.OccurredAt
			}
		}
	}
	if from.IsZero() || to.IsZero() {
		return nil
	}
	return &Period{From: startDay(from, zone), To: startDay(to, zone), RawFrom: &from, RawTo: &to, Source: source}
}

func completedPeriods(uploads []Upload) []Period {
	var periods []Period
	for _, upload := range uploads {
		if upload.Status == "done" && upload.Period != nil {
			periods = append(periods, *upload.Period)
		}
	}
	return periods
}
