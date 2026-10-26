package statements

import (
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
)

func TestUnitUploadIDBoundaryRejectsBeforeUseCases(t *testing.T) {
	h := streamHandler(&streamUseCases{})
	for _, raw := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000"} {
		if response, err := h.DeleteUpload(unaryContext(), &pb.DeleteUploadRequest{UploadId: raw}); response != nil {
			t.Fatal(response)
		} else {
			unaryReason(t, err, "VALIDATION_ERROR")
		}
		if response, err := h.ListUnreadLines(unaryContext(), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: raw}}); response != nil {
			t.Fatal(response)
		} else {
			unaryReason(t, err, "VALIDATION_ERROR")
		}
		unaryReason(t, h.WatchUpload(&pb.WatchUploadRequest{UploadId: raw}, &streamWatchServer{ctx: unaryContext()}), "VALIDATION_ERROR")
	}
	if got, err := requestUploadID(unaryRawUser); err != nil || got != unaryUser {
		t.Fatal("valid ID was not canonicalized", got, err)
	}
}
