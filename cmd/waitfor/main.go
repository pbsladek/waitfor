package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pbsladek/wait-for/internal/cli"
)

func main() {
	// Register a buffered channel so neither signal is dropped while Execute runs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Suppress SIGPIPE so a broken pipe on stdout/stderr doesn't crash the
	// process; write errors are already handled via fmt.Fprintf return values.
	signal.Ignore(syscall.SIGPIPE)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := cli.Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	// Use conventional 128+signal exit codes: SIGINT→130, SIGTERM→143.
	// Reset to the default handler and re-raise the signal so that parent
	// shells see the process as signal-terminated (affects $? and job control)
	// rather than a plain exit with code 130/143.
	select {
	case sig := <-sigCh:
		signal.Reset(sig)
		if s, ok := sig.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
		}
		// Fallback if the re-raised signal isn't delivered before we reach here.
		if sig == syscall.SIGTERM {
			os.Exit(143)
		}
		os.Exit(130)
	default:
		os.Exit(code)
	}
}
