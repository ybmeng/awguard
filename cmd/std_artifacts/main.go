// Command std_artifacts runs the std artifacts sync service: it watches
// <dir>/inbox and moves every file that appears there into <dir>/synced.
//
// Usage:
//
//	std_artifacts -dir /path/to/artifacts [-interval 1s] [-once]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"awguard/std/artifacts"
)

func main() {
	dir := flag.String("dir", "", "local root directory (inbox/ and synced/ live under it)")
	interval := flag.Duration("interval", artifacts.DefaultInterval, "poll interval")
	once := flag.Bool("once", false, "sync a single time and exit instead of running as a service")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "std_artifacts: -dir is required")
		flag.Usage()
		os.Exit(2)
	}

	svc, err := artifacts.New(artifacts.Config{
		Root:     *dir,
		Interval: *interval,
	})
	if err != nil {
		log.Fatalf("std_artifacts: %v", err)
	}

	if *once {
		moved, err := svc.SyncOnce()
		if err != nil {
			log.Fatalf("std_artifacts: %v", err)
		}
		log.Printf("std_artifacts: synced %d file(s)", moved)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("std_artifacts: watching %s/inbox every %s", svc.Root(), *interval)
	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("std_artifacts: %v", err)
	}
	log.Print("std_artifacts: shutting down")
}
