// Command letsgoagain resumes OpenCode sessions stopped by a Claude usage limit once the limit resets.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Greqory/letsgoagain/internal/daemon"
	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/opencode"
)

const usage = `letsgoagain: let's go again after a Claude usage limit.

Usage:
  letsgoagain daemon [--dry-run] [--no-notice] [--meridian URL] [--message TEXT]
  letsgoagain status
  letsgoagain doctor [--meridian URL]
  letsgoagain resume-now [SESSION_ID | --all]
  letsgoagain install [--dry-run] [--no-notice]
  letsgoagain uninstall
  letsgoagain update [--check]
  letsgoagain version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon(os.Args[2:])
	case "status":
		err = runStatus(os.Stdout)
	case "doctor":
		err = runDoctor(os.Stdout, os.Args[2:])
	case "resume-now":
		err = runResumeNow(os.Stdout, os.Args[2:])
	case "install":
		err = runInstall(os.Stdout, os.Args[2:])
	case "uninstall":
		err = runUninstall(os.Stdout)
	case "update":
		err = runUpdate(os.Stdout, os.Args[2:])
	case "version", "--version":
		fmt.Println("letsgoagain", currentVersion())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "letsgoagain:", err)
		os.Exit(1)
	}
}

func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "letsgoagain"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "letsgoagain"), nil
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "record and log stops, never send anything")
	noNotice := fs.Bool("no-notice", false, "no \"continues automatically at …\" line in the TUI")
	meridianURL := fs.String("meridian", meridian.DefaultURL, "meridian proxy URL (for the quota)")
	message := fs.String("message", daemon.DefaultMessage, "text a resumed session receives")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "letsgoagain.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("log file: %w", err)
	}
	defer logFile.Close()
	logger := log.New(io.MultiWriter(logFile, os.Stderr), "", log.LstdFlags)

	servicePath, err := opencode.DefaultServicePath()
	if err != nil {
		return err
	}
	socket, err := daemon.DefaultSocketPath()
	if err != nil {
		return err
	}
	d, err := daemon.New(daemon.Config{
		StatePath: filepath.Join(dir, "state.json"),
		Version:   currentVersion(),
		Connect: func() (daemon.API, error) {
			s, err := opencode.ReadService(servicePath)
			if err != nil {
				return nil, err
			}
			return opencode.NewClient(s), nil
		},
		Quota:    meridian.NewClient(*meridianURL),
		Log:      logger,
		DryRun:   *dryRun,
		NoNotice: *noNotice,
		Message:  *message,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := d.Serve(ctx, socket); err != nil {
			logger.Printf("%v (status and resume-now will not work)", err)
		}
	}()
	return d.Run(ctx)
}

func runResumeNow(w io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("resume-now needs a session ID or --all")
	}
	id := args[0]
	if id == "--all" {
		id = ""
	}
	socket, err := daemon.DefaultSocketPath()
	if err != nil {
		return err
	}
	results, err := daemon.NewControl(socket).ResumeNow(id)
	if err != nil {
		return err
	}
	for _, r := range results {
		fmt.Fprintln(w, r)
	}
	return nil
}
