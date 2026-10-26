package app_test

import (
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIPendingBacklogDoesNotConsumeBothWorkersBeforeAnotherUser(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	entered := make(chan struct{}, 8)
	resume := make(chan struct{})
	env.Objects.PauseReads(entered, resume)
	defer close(resume)
	for range 5 {
		if _, err := apiUpload(t, c, apiUser, uuid.NewString(), "queued.ofx", apiOFX); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first durable head did not start")
	}
	// With a full same-user backlog, the other processing slot remains available.
	select {
	case <-entered:
		t.Fatal("another job from the same user consumed the second worker")
	case <-time.After(750 * time.Millisecond):
	}
	// Existing GET keeps its captured pause; later reads can complete normally.
	env.Objects.PauseReads(nil, nil)
	other, err := apiUpload(t, c, apiOther, uuid.NewString(), "another-user.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	result := apiComplete(t, c, other.UploadId, apiOther)
	if result.NewCount != 1 {
		t.Fatal("another user did not finish while the first head remained pending", result)
	}
}
