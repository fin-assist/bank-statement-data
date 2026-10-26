package importing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Run processes the durable upload queue with bounded per-process parallelism.
// Other pods may parse the same source, but only the guarded processing-state
// transaction can publish operations and a result once.
func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	jobs := make(chan Upload, 2)
	type outcome struct {
		key string
		err error
	}
	results := make(chan outcome, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case upload := <-jobs:
					err := s.processSafely(ctx, upload.UserID, upload.ID)
					select {
					case results <- outcome{upload.UserID + "/" + upload.ID, err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	active := make(map[string]bool)
	for {
		read, stop := context.WithTimeout(ctx, 5*time.Second)
		pending, err := s.repo.Pending(read, 16)
		stop()
		if err != nil {
			return err
		}
		for _, upload := range pending {
			key := upload.UserID + "/" + upload.ID
			if active[key] {
				continue
			}
			select {
			case jobs <- upload:
				active[key] = true
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			delete(active, result.key)
			if result.err != nil && !errors.Is(result.err, ErrDeleted) && !errors.Is(result.err, ErrNotFound) {
				return result.err
			}
		case <-ticker.C:
		}
	}
}

func (s *Service) processSafely(ctx context.Context, user, id string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("statement worker stopped unexpectedly")
		}
	}()
	return s.Process(ctx, user, id)
}

// RunCleanup resumes failed/cancelled erasures and retires abandoned staged PUTs.
// The global orphan audit never touches a key with an active primary reference.
func (s *Service) RunCleanup(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	orphans := time.NewTicker(time.Minute)
	defer orphans.Stop()
	for {
		read, stop := context.WithTimeout(ctx, 5*time.Second)
		candidates, err := s.repo.Cleanup(read, 16, s.now().Add(-time.Minute))
		stop()
		if err != nil {
			return err
		}
		for _, upload := range candidates {
			work, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := s.cleanupFile(work, upload.UserID, upload.ID)
			cancel()
			if err != nil && !errors.Is(err, ErrDeleted) && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-orphans.C:
			work, cancel := context.WithTimeout(ctx, time.Minute)
			err := s.SweepOrphans(work)
			cancel()
			if err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}

// SweepOrphans installs permanent empty fences only for this service's generated
// key namespace, older than the conservative one-hour admission grace period.
func (s *Service) SweepOrphans(ctx context.Context) error {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	cursor := s.sweepCursor
	seen := make(map[string]bool)
	for {
		page, err := s.objects.List(ctx, "", cursor)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			parts := strings.Split(object.Key, "/")
			if len(parts) != 2 || object.ModifiedAt.IsZero() || object.ModifiedAt.After(s.now().Add(-time.Hour)) {
				continue
			}
			actor, err := uuid.Parse(parts[0])
			if err != nil || actor == uuid.Nil || actor.String() != parts[0] || !ownedObjectKey(parts[0], object.Key) {
				continue
			}
			release, err := s.barrier.Exclusive(ctx, parts[0])
			if err != nil {
				return err
			}
			err = s.deleteUnreferenced(ctx, parts[0], object.Key)
			unlocked := release()
			if err != nil {
				return err
			}
			if unlocked != nil {
				return unlocked
			}
		}
		if page.Next == "" {
			s.sweepCursor = ""
			return nil
		}
		if page.Next == cursor || seen[page.Next] {
			return errAccounting
		}
		seen[page.Next], cursor = true, page.Next
		s.sweepCursor = cursor
	}
}
