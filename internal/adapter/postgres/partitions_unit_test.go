package postgres

import (
	"strings"
	"testing"
)

func TestUnitPartitionDDLIsBoundedAndUTC(t *testing.T) {
	for _, year := range []int{-1, 0, 2009, 9999, 10000} {
		if _, err := operationPartitionDDL(year); err == nil {
			t.Fatal("invalid year accepted", year)
		}
		p := NewOperationPartitions(nil)
		if _, err := p.Exists(t.Context(), year); err == nil {
			t.Fatal("invalid existence year accepted")
		}
		if err := p.Ensure(t.Context(), year); err == nil {
			t.Fatal("invalid ensure year accepted")
		}
	}
	ddl, err := operationPartitionDDL(2028)
	if err != nil || ddl != "CREATE TABLE public.operation_y2028 PARTITION OF public.operation FOR VALUES FROM ('2028-01-01 00:00:00Z') TO ('2029-01-01 00:00:00Z')" {
		t.Fatal("wrong DDL", ddl, err)
	}
	for _, year := range []int{2010, 9998} {
		if _, err := operationPartitionDDL(year); err != nil {
			t.Fatal(err)
		}
	}
	if err = NewOperationPartitions(&DB{}).Ensure(t.Context(), 2028); err == nil || !strings.Contains(err.Error(), "transaction required") {
		t.Fatal("missing transaction accepted", err)
	}
}
