package sourcerevision

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestUnitRevisionDecodeRejectsNullAndPreservesExactCounters(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `{"uploads":null}`, `{"uploads":0.0}`, `{"uploads":true}`, `{"uploads":"0"}`, `{"uploads":-1}`, `{"uploads":9223372036854775808}`, `{"UPPER":0}`} {
		if _, err := Decode([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"", `null`, `{}`} {
		if values, err := Decode([]byte(raw)); err != nil || len(values) != 0 {
			t.Fatal(values, err)
		}
	}
	got, err := Decode([]byte(`{"uploads":0,"future_source":9223372036854775807}`))
	if err != nil || len(got) != 2 || got["future_source"] != math.MaxInt64 {
		t.Fatal(got, err)
	}
	many := map[string]int64{}
	for i := range 33 {
		many[fmt.Sprintf("kind_%d", i)] = 0
	}
	if Valid(many) || Valid(map[string]int64{strings.Repeat("a", 65): 0}) {
		t.Fatal("unbounded map accepted")
	}
}
