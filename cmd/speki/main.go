// Command speki runs the speki knowledge base server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eve-online-tools/speki/service"
	"github.com/eve-online-tools/speki/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // a second signal during the drain ends the process at once
	}()
	err := run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "speki:", err)
		os.Exit(1)
	}
}

// run parses flags, loads the configuration, builds the logger and runs the service until ctx ends.
func run(ctx context.Context, args, environ []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("speki", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the YAML configuration file (default "+defaultConfigPath+")")
	showVersion := flags.Bool("version", false, "print the version and exit")
	err := flags.Parse(args)
	if err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}

	cfg, err := loadConfig(*configPath, environ)
	if err != nil {
		return err
	}

	level, err := telemetry.ParseLevel(cfg.Telemetry.LogLevel)
	if err != nil {
		level = slog.LevelInfo // service.New reports it, with any other problems
	}
	logger := telemetry.NewLogger(stderr, level)

	cfg.Version = version
	svc, err := service.New(cfg, logger)
	if err != nil {
		return err
	}
	return svc.Run(ctx)
}
