// Package covering restores only missing source coverage metadata from retained OFX files.
package covering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// ErrCoverage hides object keys, original contents and provider diagnostics.
var ErrCoverage = errors.New("original statement coverage unavailable")

// Upload identifies the exact immutable object associated with a completed upload.
type Upload struct {
	ID, User, Key string
	Size          int64
	Digest        [32]byte
}

// Repository reads bounded missing-metadata work outside transactions and updates
// exactly the same completed object under the caller's tombstone/user locks.
type Repository interface {
	PendingCoverage(context.Context, string, int) ([]Upload, error)
	LockUser(context.Context, string, bool) (string, error)
	SaveCoverage(context.Context, Upload, *importing.Period, string) (bool, error)
}

// Objects supplies only original bytes; metadata recovery never writes object data.
type Objects interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

// Service repairs up to sixteen files per attempt with durable progress per file.
// It does not rewrite operations, categories, unread rows, import results or rules.
type Service struct {
	tx      importing.Transactions
	repo    Repository
	deleted importing.Tombstones
	objects Objects
	parser  importing.Parser
}

// New requires explicit storage, admission, object and parsing boundaries.
func New(tx importing.Transactions, repo Repository, deleted importing.Tombstones, objects Objects, parser importing.Parser) (*Service, error) {
	if tx == nil || repo == nil || deleted == nil || objects == nil || parser == nil {
		return nil, ErrCoverage
	}
	return &Service{tx, repo, deleted, objects, parser}, nil
}

// Recover returns ready only after every currently completed legacy upload has
// exact source metadata. Errors preserve pending work instead of guessing days.
func (s *Service) Recover(ctx context.Context, user string) (bool, error) {
	rows, err := s.repo.PendingCoverage(ctx, user, 16)
	if err != nil {
		return false, err
	}
	for _, upload := range rows {
		if upload.User != user {
			return false, ErrCoverage
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		period, err := s.read(ctx, upload)
		if err != nil {
			return false, err
		}
		if err = s.tx.Run(ctx, func(ctx context.Context) error {
			live, err := s.deleted.Guard(ctx, user)
			if err != nil || !live {
				return err
			}
			zone, err := s.repo.LockUser(ctx, user, false)
			if errors.Is(err, importing.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = s.repo.SaveCoverage(ctx, upload, period, zone)
			return err
		}); err != nil {
			return false, err
		}
	}
	remaining, err := s.repo.PendingCoverage(ctx, user, 1)
	return len(remaining) == 0, err
}

func (s *Service) read(ctx context.Context, upload Upload) (period *importing.Period, resultErr error) {
	if upload.Size <= 0 || upload.Size > importing.MaxFileBytes || upload.Key == "" {
		return nil, ErrCoverage
	}
	body, err := s.objects.Open(ctx, upload.Key)
	if err != nil {
		return nil, ErrCoverage
	}
	defer func() {
		if err := body.Close(); err != nil && resultErr == nil {
			resultErr = ErrCoverage
		}
	}()
	digest := sha256.New()
	source := &io.LimitedReader{R: io.TeeReader(body, digest), N: importing.MaxFileBytes + 1}
	document, err := s.parser.Parse(ctx, source)
	if err != nil {
		return nil, ErrCoverage
	}
	if _, err = io.Copy(io.Discard, source); err != nil {
		return nil, ErrCoverage
	}
	if source.N == 0 || importing.MaxFileBytes+1-source.N != upload.Size || !bytes.Equal(digest.Sum(nil), upload.Digest[:]) {
		return nil, ErrCoverage
	}
	return importing.SourcePeriod(document, time.UTC), nil
}
