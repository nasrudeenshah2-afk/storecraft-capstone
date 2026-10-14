package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"storecraft/internal/verify"
)

// runVerify implements `storecraft verify` and returns the process exit code:
// 0 the replica equals the primary, 1 they diverged, 2 no verdict (timeout,
// unreachable node, bad token, bad flags).
//
// The admin token comes from STORECRAFT_ADMIN_TOKEN and is used for both
// nodes; STORECRAFT_REPLICA_ADMIN_TOKEN overrides it for the replica when the
// replica has a different admin token.
func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	primary := fs.String("primary", "", "base URL of the primary, e.g. http://primary:8080")
	replica := fs.String("replica", "", "base URL of the replica, e.g. http://replica:8081")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait for the replica to catch up and for a consistent comparison")
	if err := fs.Parse(args); err != nil {
		return 2 // bad flags, or -h: either way not a verdict
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "error: "+format+"\n", a...)
		return 2
	}
	if *primary == "" || *replica == "" {
		return fail("-primary and -replica are required")
	}
	if *timeout <= 0 {
		return fail("-timeout must be positive")
	}
	token := os.Getenv("STORECRAFT_ADMIN_TOKEN")
	if token == "" {
		return fail("STORECRAFT_ADMIN_TOKEN must be set to the admin token")
	}
	replicaToken := os.Getenv("STORECRAFT_REPLICA_ADMIN_TOKEN")
	if replicaToken == "" {
		replicaToken = token
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := verify.Run(ctx, verify.Config{
		PrimaryURL:   *primary,
		ReplicaURL:   *replica,
		PrimaryToken: token,
		ReplicaToken: replicaToken,
		Timeout:      *timeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
	} else {
		res.Format(stdout)
	}
	return verify.ExitCode(res, err)
}
