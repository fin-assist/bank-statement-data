package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnitCommandDispatchRejectsUnsupportedInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "extra"}, {"migrate"}} {
		err := run(context.Background(), args)
		var usage usageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "service serve") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}
