package importing

import (
	"errors"
	"testing"
)

func TestUnitWatchFreshAdmissionRejectsRevocationAndPrimaryFailure(t *testing.T) {
	failure := errors.New("primary unavailable")
	for _, phase := range []string{"before-poll", "done-before-emit", "missing-after-purge", "primary-before", "primary-after", "genuine-missing"} {
		t.Run(phase, func(t *testing.T) {
			f := newLife(t)
			f.upload.Status = "done"
			f.upload.Result = &Result{NewCount: 1}
			want := ErrDeleted
			switch phase {
			case "before-poll":
				f.deleted.alive = false
			case "done-before-emit":
				f.repo.upload = func() (Upload, error) { f.deleted.alive = false; return f.upload, nil }
			case "missing-after-purge":
				f.repo.upload = func() (Upload, error) { f.deleted.alive = false; return Upload{}, ErrNotFound }
			case "primary-before":
				f.deleted.err = failure
				want = failure
			case "primary-after":
				f.repo.upload = func() (Upload, error) { f.deleted.err = failure; return f.upload, nil }
				want = failure
			case "genuine-missing":
				f.repo.errs["upload"] = ErrNotFound
				want = ErrNotFound
			}
			err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { t.Error("event emitted after revocation/failure"); return nil })
			if !errors.Is(err, want) || f.trace.count("guard") != 0 || f.trace.count("fence") != 0 {
				t.Fatal(err, f.trace)
			}
			if (phase == "before-poll" || phase == "primary-before") && f.trace.count("upload") != 0 {
				t.Fatal("state read before admission")
			}
		})
	}
}

func TestUnitWatchRechecksPrimaryOnUnchangedProgressPoll(t *testing.T) {
	f := newLife(t)
	events := 0
	err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { events++; f.deleted.alive = false; return nil })
	if !errors.Is(err, ErrDeleted) || events != 1 || f.trace.count("upload") != 1 || f.trace.count("guard") != 0 {
		t.Fatal(err, events, f.trace)
	}
}
