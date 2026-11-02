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

// Notify shares one total budget across lookup, send and acknowledgement.
// An exhausted request leaves its durable job for the independent worker.
func (s *Service) Notify(ctx context.Context, user string, version int32) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	job, found, err := s.attempt(ctx, user, version)
	if found {
		_ = s.finish(ctx, job, err)
	}
}

// attempt rechecks the row immediately before sending, including after batch selection.
func (s *Service) attempt(ctx context.Context, user string, version int32) (Job, bool, error) {
	job, found, err := s.repo.Find(ctx, user, version)
	if err != nil || !found {
		return job, false, err
	}
	return job, true, s.send(ctx, job.User, job.Version)
}

func (s *Service) finish(ctx context.Context, job Job, sendErr error) error {
	if sendErr != nil {
		delay := time.Second * time.Duration(1<<min(job.Attempts, 8))
		return errors.Join(sendErr, s.repo.Retry(ctx, job, s.now().Add(delay)))
	}
	return s.repo.Ack(ctx, job)
}

// Tick sends at most sixteen due markers. Each attempt has one second for lookup
// and network, then a separate bounded second to persist its outcome even after
// cancellation. Shutdown finishes only the current job before stopping the batch.
func (s *Service) Tick(ctx context.Context) error {
	jobs, err := s.repo.Pending(ctx, s.now(), 16)
	if err != nil {
		return err
	}
	var failures []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		work, cancel := context.WithTimeout(ctx, time.Second)
		current, found, err := s.attempt(work, job.User, job.Version)
		cancel()
		if found {
			finish, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			err = s.finish(finish, current, err)
			stop()
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
