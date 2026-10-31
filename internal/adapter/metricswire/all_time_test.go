package metricswire

import (
	"testing"
	"time"

	common "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
)

func TestUnitAllTimeWirePublishesBothModesAndRemovesSingleton(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	value := recalculating.Publication{User: "user", Version: 3, State: calculating.State{CalculatedAt: at, TimeZone: "UTC", Revisions: map[string]int64{"uploads": 4}}}
	for _, mode := range []string{"with", "without"} {
		value.Periods = append(value.Periods, calculating.Period{Kind: "all_time", Key: "all_time", Mode: mode, CalculatedAt: at})
	}
	requests, err := publications(value, maxPublicationBytes)
	if err != nil || len(requests) != 2 || len(requests[0].Periods) != 2 {
		t.Fatal(requests, err)
	}
	for i, p := range requests[0].Periods {
		if p.Period != common.PeriodType_PERIOD_TYPE_ALL_TIME || p.PeriodKey != "all_time" || p.TransferMode != []common.TransferMode{common.TransferMode_TRANSFER_MODE_WITH, common.TransferMode_TRANSFER_MODE_WITHOUT}[i] || len(p.DataRevisions) != 1 {
			t.Fatal("typed all-time pair lost", p)
		}
	}
	if len(requests[0].State.DataRevisions) != 0 || len(requests[1].State.DataRevisions) != 1 {
		t.Fatal("partial inventory certified")
	}
	value.Periods = nil
	value.Removed = []recalculating.Period{{Kind: "all_time", Key: "all_time"}}
	requests, err = publications(value, maxPublicationBytes)
	if err != nil || len(requests) != 2 || len(requests[0].RemovedPeriods) != 1 || requests[0].RemovedPeriods[0].Period != common.PeriodType_PERIOD_TYPE_ALL_TIME || requests[0].RemovedPeriods[0].PeriodKey != "all_time" {
		t.Fatal("singleton removal lost", requests, err)
	}
}
