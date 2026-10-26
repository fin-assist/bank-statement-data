package importing

import (
	"context"
	"errors"
	"testing"
)

func TestUnitReceiverAdmissionReleasesUserEntries(t *testing.T) {
	s := &Service{}
	release, err := s.acquireReceiver(t.Context(), "one-user")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.acquireReceiver(cancelled, "one-user"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Repeated cancellations cannot leave one entry per attempt after the holder ends.
	release()
	release()
	if len(s.receivers) != 0 {
		t.Fatal("receiving users leaked after all callers left")
	}
	fresh, err := s.acquireReceiver(t.Context(), "one-user")
	if err != nil {
		t.Fatal(err)
	}
	fresh()
	if len(s.receivers) != 0 {
		t.Fatal("fresh admission leaked")
	}
}
