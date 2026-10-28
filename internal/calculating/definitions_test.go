package calculating

import (
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
)

func TestUnitCalculatedPeriodsCaptureEveryDefinitionInBothModes(t *testing.T) {
	seen := make(map[string]bool)
	for _, kind := range []string{"month", "quarter", "year"} {
		in := fixtureInput()
		in.Kind = kind
		result := mustBuild(t, in)
		if len(result.Periods) != 2 {
			t.Fatal("period lost a transfer mode")
		}
		for _, period := range result.Periods {
			for id, metric := range period.Metrics {
				definition, ok := accounting.DefinitionFor(id)
				if !ok || metric.DefinitionVersion != definition.Version {
					t.Fatal("result lacks its exact definition identity", kind, period.Mode, id, metric.DefinitionVersion)
				}
				seen[id] = true
				if other, ok := result.Periods[1].Metrics[id]; !ok || other.DefinitionVersion != metric.DefinitionVersion {
					t.Fatal("transfer modes used different definition versions", kind, id)
				}
			}
		}
	}
	for _, definition := range accounting.Definitions() {
		if !seen[definition.ID] {
			t.Fatal("catalog definition is not computed by any supported period", definition.ID)
		}
	}
	// Locked and no-data results retain definition identity just like ready values.
	in := fixtureInput()
	in.Snapshot.Operations, in.Snapshot.Uploads = nil, nil
	for _, period := range mustBuild(t, in).Periods {
		for id, metric := range period.Metrics {
			if metric.DefinitionVersion < 1 {
				t.Fatal("no-data/locked result lost version", id)
			}
		}
	}
}
