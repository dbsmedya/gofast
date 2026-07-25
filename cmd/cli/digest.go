package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/dbsmedya/gofast/internal/digest"
	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/storage"
	"github.com/spf13/cobra"
)

// digestFlags is the raw flag input, before parsing and validation.
type digestFlags struct {
	groupBy string
	orderBy string
	limit   int
	filters []string
	since   string
	until   string
	json    bool
}

// buildDigestOptions parses raw flags into validated Options. now is injected
// so relative --since/--until values are testable.
func buildDigestOptions(f digestFlags, now time.Time) (digest.Options, error) {
	o := digest.DefaultOptions()

	g, err := digest.ParseGroupKey(f.groupBy)
	if err != nil {
		return o, err
	}
	o.GroupBy = g

	ord, err := digest.ParseOrderBy(f.orderBy)
	if err != nil {
		return o, err
	}
	o.OrderBy = ord
	o.Limit = f.limit

	filter, err := digest.ParseFilters(f.filters)
	if err != nil {
		return o, err
	}
	if err := digest.ApplyTimeBounds(&filter, f.since, f.until, now); err != nil {
		return o, err
	}
	o.Filter = filter

	if err := o.Validate(); err != nil {
		return o, err
	}
	return o, nil
}

// runDigest opens the store read-only, runs the digest, and renders it.
//
// Nothing is written to out until the report is complete, so a failure never
// leaves a truncated JSON document on stdout.
func runDigest(cfg *config.Config, o digest.Options, asJSON bool, out io.Writer) error {
	store, err := storage.NewStorage(cfg.GetDSN(), true)
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer store.Close()

	rep, err := digest.Run(context.Background(), store, o)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return renderMarkdown(out, rep)
}

func digestCmd() *cobra.Command {
	var f digestFlags

	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Report slow queries in pt-query-digest's format",
		Long: `Aggregate stored slow queries into a pt-query-digest-style report.

Flag names and defaults follow pt-query-digest so existing habits transfer:

  gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10

--order-by takes ATTRIBUTE:AGGREGATE. Attributes are Query_time, Lock_time,
Rows_sent and Rows_examined; aggregates are sum, min, max, avg and cnt. The
"cnt" aggregate counts rows and ignores the attribute, so Query_time:cnt orders
by call frequency. Results are always ordered worst-first.

--filter takes "KEY OP VALUE" and may be repeated; all filters are ANDed:

  gofast-cli digest --filter "fingerprint = FDEA8D2993C9CAF3"
  gofast-cli digest --filter "db = shop" --filter "query_time > 2"

String keys (user, host, db, source_host, table) accept a comma list or a
repeated flag, both producing an IN match. Numeric keys are query_time and
lock_time (> and <) and rows_examined (>). rows_sent is reported but cannot be
filtered.

Quote filters that contain > or <, or the shell will redirect output:
  --filter "query_time > 2"    not    --filter query_time > 2

--since and --until accept a relative value (7d, 90m, 2h, 30s; a bare integer
means seconds) or an absolute "YYYY-MM-DD" / "YYYY-MM-DD HH:MM:SS", read in the
machine's local timezone.

Output is markdown by default. With --json, stdout carries only JSON, so it can
be piped straight into jq.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true, // keep usage off stdout so --json | jq stays valid
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}
			o, err := buildDigestOptions(f, time.Now())
			if err != nil {
				return err
			}
			// OutOrStdout, not os.Stdout: writing to the process stream
			// directly would make the command untestable. SilenceUsage keeps
			// cobra's own output off this writer on the error path.
			return runDigest(configLoaded, o, f.json, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&f.groupBy, "group-by", "fingerprint",
		"group by fingerprint, user, db, host, source_host, or tables")
	cmd.Flags().StringVar(&f.orderBy, "order-by", "Query_time:sum",
		"order by ATTRIBUTE:AGGREGATE, e.g. Query_time:sum")
	cmd.Flags().IntVar(&f.limit, "limit", 20, "number of query classes to report")
	cmd.Flags().StringArrayVar(&f.filters, "filter", nil,
		`filter as "KEY OP VALUE"; repeatable and ANDed`)
	cmd.Flags().StringVar(&f.since, "since", "", "only queries at or after this time")
	cmd.Flags().StringVar(&f.until, "until", "", "only queries at or before this time")
	cmd.Flags().BoolVar(&f.json, "json", false, "emit JSON instead of markdown")

	return cmd
}
