// Package marking delivers durable recalculation markers independently of financial snapshots.
package marking

import (
	"context"
	"errors"
	"time"
)

// Job identifies one immutable true marker; acknowledgement completes the job.
type Job struct {
	User     string
	Version  int32
	Attempts int
}

// Repository retains markers across request cancellation and process restarts.
type Repository interface {
	Find(context.Context, string, int32) (Job, bool, error)
	Pending(context.Context, time.Time, int) ([]Job, error)
	Ack(context.Context, Job) error
	Retry(context.Context, Job, time.Time) error
}

// Send delivers a system-only marker outside a database transaction.
type Send func(context.Context, string, int32) error

// Service owns bounded synchronous attempts and independent durable retries.
type Service struct {
	repo Repository
	send Send
	now  func() time.Time
}

// New requires explicit storage, network and clock dependencies.
func New(repo Repository, send Send, now func() time.Time) (*Service, error) {
	if repo == nil || send == nil || now == nil {
		return nil, errors.New("invalid marker dependencies")
	}
	return &Service{repo, send, now}, nil
}

// Notify attempts delivery after the caller committed; failure never reverses that commit.
func (s *Service) Notify(ctx context.Context, user string, version int32) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	job, found, err := s.repo.Find(ctx, user, version)
	if err == nil && found {
		_ = s.deliver(ctx, job)
	}
}

func (s *Service) deliver(ctx context.Context, job Job) error {
	if err := s.send(ctx, job.User, job.Version); err != nil {
		// Failure after cancellation leaves the original due row for the next worker.
		delay := time.Second * time.Duration(1<<min(job.Attempts, 8))
		return errors.Join(err, s.repo.Retry(ctx, job, s.now().Add(delay)))
	}
	return s.repo.Ack(ctx, job)
}

// Tick sends at most sixteen due markers, with a separate one-second budget each.
func (s *Service) Tick(ctx context.Context) error {
	jobs, err := s.repo.Pending(ctx, s.now(), 16)
	if err != nil {
		return err
	}
	var failures []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		work, cancel := context.WithTimeout(ctx, time.Second)
		err := s.deliver(work, job)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
