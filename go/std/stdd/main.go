// Command stdd is the std tooling supervisor: a single binary, run as a
// macOS LaunchAgent, that hosts every std background service.
//
// Usage:
//
//	stdd run -dir DIR [-interval D]   run all services in the foreground (what launchd executes)
//	stdd insert -dir DIR FILE...      move files into a managed artifact dir, print its id
//	stdd verify                       fast self-check of every service, then exit
//	stdd install -dir DIR             install + start the macOS LaunchAgent
//	stdd uninstall                    stop + remove the LaunchAgent
//	stdd start | stop | restart       control the installed service
//	stdd status                       show launchd state for the service
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	bgservices "awguard/go/std/bg_services"
	"awguard/go/std/bg_services/artifacts"
)

func usage() {
	fmt.Fprintln(os.Stderr, `stdd — std background services supervisor

Commands:
  run -dir DIR [-interval D]   run all services in the foreground
  insert -dir DIR FILE...      move files into a managed artifact dir, print its id
  verify                       fast self-check of every service
  install -dir DIR             install + start the macOS LaunchAgent
  uninstall                    stop + remove the LaunchAgent
  start | stop | restart       control the installed service
  status                       show launchd state for the service`)
	os.Exit(2)
}

// services builds the full roster of std background services.
func services(root string, interval time.Duration) ([]bgservices.Service, error) {
	art, err := artifacts.New(artifacts.Config{Root: root, Interval: interval})
	if err != nil {
		return nil, err
	}
	return []bgservices.Service{art}, nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "insert":
		err = cmdInsert(args)
	case "verify":
		err = cmdVerify(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = uninstallService()
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	case "restart":
		err = restartService()
	case "status":
		err = serviceStatus()
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("stdd %s: %v", cmd, err)
	}
}

func runFlags(name string) (*flag.FlagSet, *string, *time.Duration) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dir := fs.String("dir", "", "root directory for the artifacts service (inbox/ and synced/ live under it)")
	interval := fs.Duration("interval", artifacts.DefaultInterval, "artifacts poll interval")
	return fs, dir, interval
}

func cmdRun(args []string) error {
	fs, dir, interval := runFlags("run")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}

	svcs, err := services(*dir, *interval)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, svc := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("stdd: starting %s", svc.Name())
			bgservices.Supervise(ctx, svc, log.Default())
		}()
	}
	wg.Wait()
	log.Print("stdd: all services stopped")
	return nil
}

// cmdVerify self-checks every service against a throwaway root, so it can run
// anywhere without touching real data.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	fs.Parse(args)

	tmp, err := os.MkdirTemp("", "stdd_verify_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	svcs, err := services(tmp, artifacts.DefaultInterval)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	failed := 0
	for _, svc := range svcs {
		started := time.Now()
		if err := svc.Verify(ctx); err != nil {
			failed++
			fmt.Printf("FAIL  %-12s %v\n", svc.Name(), err)
			continue
		}
		fmt.Printf("ok    %-12s (%s)\n", svc.Name(), time.Since(started).Round(time.Millisecond))
	}
	if failed > 0 {
		return fmt.Errorf("%d service(s) failed verification", failed)
	}
	return nil
}

// cmdInsert moves the given files into a fresh managed dir and prints the
// managed dir id the files are now referenced by.
func cmdInsert(args []string) error {
	fs, dir, interval := runFlags("insert")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	if fs.NArg() == 0 {
		return errors.New("insert needs at least one file")
	}

	svc, err := artifacts.New(artifacts.Config{Root: *dir, Interval: *interval})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	id, err := svc.Insert(ctx, fs.Args()...)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func cmdInstall(args []string) error {
	fs, dir, interval := runFlags("install")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	return installService(*dir, *interval)
}
