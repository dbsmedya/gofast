package contracttest

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const slowLogTS = "2006-01-02T15:04:05.000000Z"

// WriteFixtureLogs writes two MySQL slow-log files into dir with timestamps
// relative to now, so /sql/queries' default now-15d window always includes them.
// Deterministic except for the sliding timestamps.
func WriteFixtureLogs(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	d := func(days int) string { return now.AddDate(0, 0, -days).Format(slowLogTS) }

	db1 := fmt.Sprintf(`# Time: %s
# User@Host: app[app] @ web1 [10.0.0.1]  Id: 42
# Schema: shop  Query_time: 2.500000  Lock_time: 0.000100  Rows_sent: 1  Rows_examined: 100000
use shop;
SELECT * FROM orders WHERE customer_id = 7 AND status = 'paid';
# Time: %s
# User@Host: app[app] @ web1 [10.0.0.1]  Id: 42
# Schema: shop  Query_time: 3.100000  Lock_time: 0.000000  Rows_sent: 20  Rows_examined: 500000
SELECT * FROM orders WHERE customer_id = 99 AND status = 'pending';
# Time: %s
# User@Host: reporting[reporting] @ web2 [10.0.0.2]  Id: 7
# Schema: shop  Query_time: 0.900000  Lock_time: 0.000000  Rows_sent: 5  Rows_examined: 5
SELECT id, total FROM invoices WHERE created_at > '2026-01-01';
`, d(3), d(3), d(4))

	db2 := fmt.Sprintf(`# Time: %s
# User@Host: etl[etl] @ batch1 [10.0.0.9]  Id: 11
# Schema: analytics  Query_time: 5.000000  Lock_time: 0.001000  Rows_sent: 1000  Rows_examined: 2000000
use analytics;
SELECT country, COUNT(*) FROM events GROUP BY country;
`, d(5))

	if err := os.WriteFile(filepath.Join(dir, "shop-slow.log"), []byte(db1), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "analytics-slow.log"), []byte(db2), 0o644)
}
