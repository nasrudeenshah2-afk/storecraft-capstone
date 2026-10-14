package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"storecraft/internal/store"
)

// backfillUsage runs `storecraft backfill-usage` (STORE-210, AC-API-13).
func backfillUsage(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("backfill-usage", flag.ContinueOnError)
	db := fs.String("db", "storecraft.db", "SQLite database file")
	limit := fs.Int("limit", 0, "stop after this many tenants (0 = all); run again to continue")
	restart := fs.Bool("restart", false, "forget the progress of an earlier unfinished run and start from the first tenant")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 0 {
		return errors.New("-limit must be 0 or more")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	st, err := store.Open(ctx, *db)
	if err != nil {
		return err
	}
	defer st.Close()

	// The one documented exception to "replica writes go through
	// ApplyReplicated" (CLAUDE.md): only the spelling of values and the usage
	// counters change, never a JSON value or a version. Say so.
	if isReplica, err := st.IsReplica(ctx); err != nil {
		return err
	} else if isReplica {
		fmt.Fprintln(stdout, "note: this is a replica database. backfill-usage rewrites only the spelling of values (canonical JSON) and the usage counters; JSON values and versions are not changed, so the replica stays in step with its primary.")
	}

	res, err := st.BackfillUsage(ctx, store.BackfillOptions{Restart: *restart, Limit: *limit})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted after %d tenants; run again to continue: %w", res.Processed, err)
		}
		return err
	}
	fmt.Fprintf(stdout, "backfill-usage: %d tenants processed, %d already done by an earlier run, %d values rewritten in canonical form\n",
		res.Processed, res.AlreadyDone, res.Rewritten)
	if res.Remaining > 0 {
		fmt.Fprintf(stdout, "stopped after %d tenants (-limit); %d remaining; run again to continue\n", res.Processed, res.Remaining)
	} else {
		fmt.Fprintln(stdout, "complete: every tenant's usage equals its data")
	}
	return nil
}
