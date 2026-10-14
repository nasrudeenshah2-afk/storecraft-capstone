package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"storecraft/internal/api"
	"storecraft/internal/replica"
	"storecraft/internal/store"
)

const usage = `usage: storecraft <command> [flags]

commands:
  serve   run the HTTP server  (-role=primary|replica -primary=<url> -addr :8080 -db storecraft.db -poll-timeout 25s -sse-heartbeat 15s)
          admin token: STORECRAFT_ADMIN_TOKEN; a replica also needs the primary's admin token in STORECRAFT_REPLICATION_TOKEN
  seed    create 3 demo tenants and print their tokens once  (-db storecraft.db)
  verify  compare a replica with its primary  (-primary=<url> -replica=<url> -timeout 30s)
          admin token: STORECRAFT_ADMIN_TOKEN (STORECRAFT_REPLICA_ADMIN_TOKEN overrides it for the replica)
          exit code: 0 equal, 1 diverged, 2 timeout or error
  backfill-usage  recompute every tenant's usage and store old values in canonical form  (-db storecraft.db -limit 0 -restart)
          safe to run twice and while the server runs; a stopped run continues where it stopped
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "seed":
		err = seed(os.Args[2:], os.Stdout)
	case "verify":
		os.Exit(runVerify(os.Args[2:], os.Stdout, os.Stderr))
	case "backfill-usage":
		err = backfillUsage(os.Args[2:], os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	db := fs.String("db", "storecraft.db", "SQLite database file")
	pollTimeout := fs.Duration("poll-timeout", 25*time.Second, "how long GET /v1/changes holds an idle request open")
	sseHeartbeat := fs.Duration("sse-heartbeat", 15*time.Second, "idle interval between SSE heartbeat comments on /v1/changes/stream")
	role := fs.String("role", api.RolePrimary, "primary or replica (a replica is read-only and follows -primary)")
	primary := fs.String("primary", "", "replica only: base URL of the primary, e.g. http://primary:8080")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pollTimeout <= 0 {
		return errors.New("-poll-timeout must be positive")
	}
	if *sseHeartbeat <= 0 {
		return errors.New("-sse-heartbeat must be positive")
	}
	log := slog.Default()
	adminToken := os.Getenv("STORECRAFT_ADMIN_TOKEN")
	if adminToken == "" {
		log.Warn("STORECRAFT_ADMIN_TOKEN is not set; admin access disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, rep, err := openNode(ctx, nodeConfig{
		Role:             *role,
		DB:               *db,
		PrimaryURL:       *primary,
		ReplicationToken: os.Getenv("STORECRAFT_REPLICATION_TOKEN"),
		Log:              log,
	})
	if err != nil {
		return err
	}
	defer st.Close()

	opts := api.Options{
		AdminToken:   adminToken,
		Log:          log,
		PollTimeout:  *pollTimeout,
		SSEHeartbeat: *sseHeartbeat,
		Role:         *role,
	}
	repErr := make(chan error, 1)
	if rep != nil {
		opts.Replica = rep
		go func() { repErr <- rep.Run(ctx) }()
		// ctx is cancelled before this runs, so Run returns promptly; the
		// store (closed after, by the defer above) must outlive it.
		defer func() { <-repErr }()
	}

	srv := newHTTPServer(*addr, api.New(st, opts))
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", *addr, "db", *db, "role", *role)

	select {
	case err := <-errc:
		stop() // make the replica loop (if any) exit
		return err
	case err := <-repErr:
		stop()
		repErr <- nil // satisfy the deferred wait
		return fmt.Errorf("replication stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

type nodeConfig struct {
	Role             string // api.RolePrimary or api.RoleReplica
	DB               string
	PrimaryURL       string // replica only
	ReplicationToken string // replica only: the primary's admin token
	Log              *slog.Logger
}

// openNode validates the role flags and opens the database accordingly. A
// primary refuses a replica database, and a replica refuses a database that
// already holds data of its own, so neither can be turned into the other by
// accident. It returns a Replica only for the replica role.
func openNode(ctx context.Context, c nodeConfig) (*store.Store, *replica.Replica, error) {
	switch c.Role {
	case api.RolePrimary:
		if c.PrimaryURL != "" {
			return nil, nil, errors.New("-primary is only valid with -role=replica")
		}
		st, err := store.Open(ctx, c.DB)
		if err != nil {
			return nil, nil, err
		}
		isReplica, err := st.IsReplica(ctx)
		if err == nil && isReplica {
			err = fmt.Errorf("%s is a replica database; start it with -role=replica", c.DB)
		}
		if err != nil {
			st.Close()
			return nil, nil, err
		}
		return st, nil, nil

	case api.RoleReplica:
		if c.PrimaryURL == "" {
			return nil, nil, errors.New("-primary is required with -role=replica")
		}
		if c.ReplicationToken == "" {
			return nil, nil, errors.New("STORECRAFT_REPLICATION_TOKEN (the primary's admin token) must be set for -role=replica")
		}
		st, err := store.Open(ctx, c.DB, store.WithoutForeignKeys())
		if err != nil {
			return nil, nil, err
		}
		if err := st.EnsureReplica(ctx); err != nil {
			st.Close()
			return nil, nil, fmt.Errorf("cannot use %s as a replica database: %w", c.DB, err)
		}
		rep, err := replica.New(replica.Config{
			PrimaryURL: c.PrimaryURL,
			Token:      c.ReplicationToken,
			Store:      st,
			Log:        c.Log,
		})
		if err != nil {
			st.Close()
			return nil, nil, err
		}
		return st, rep, nil
	}
	return nil, nil, fmt.Errorf("-role must be %q or %q, got %q", api.RolePrimary, api.RoleReplica, c.Role)
}

// newHTTPServer deliberately sets no ReadTimeout/WriteTimeout: those are
// whole-request deadlines and would kill long-poll and SSE connections.
// Slowloris protection comes from ReadHeaderTimeout.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// seed implements `storecraft seed`. The plaintext tokens are written to
// stdout once; tests pass io.Discard so no token ever lands in a test log.
func seed(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	db := fs.String("db", "storecraft.db", "SQLite database file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *db)
	if err != nil {
		return err
	}
	defer st.Close()

	if isReplica, err := st.IsReplica(ctx); err != nil {
		return err
	} else if isReplica {
		return fmt.Errorf("%s is a replica database; seed the primary instead", *db)
	}
	res, err := seedTenants(ctx, st, 3)
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Token == "" {
			fmt.Fprintf(stdout, "%s: already exists (token was shown when created)\n", r.Name)
			continue
		}
		fmt.Fprintf(stdout, "%s: %s\n", r.Name, r.Token)
	}
	return nil
}
