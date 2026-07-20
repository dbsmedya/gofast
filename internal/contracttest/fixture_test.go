package contracttest

import (
	"context"
	"testing"
)

func TestBuildFixtureDB(t *testing.T) {
	fx := BuildFixtureDB(t)
	res, err := fx.Store.Query(context.Background(),
		`SELECT COUNT(DISTINCT db) FROM slow_logs`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got, _ := res.Rows[0][0].(int64)
	if got != 2 {
		t.Fatalf("want 2 distinct databases, got %v", res.Rows[0][0])
	}
}
