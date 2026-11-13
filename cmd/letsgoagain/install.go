package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitName = "letsgoagain.service"

func unitPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("unit path: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

func unitFile(binary string, dryRun bool) string {
	args := "daemon"
	if dryRun {
		args += " --dry-run"
	}
	return fmt.Sprintf(`[Unit]
Description=LetsGoAgain: resumes OpenCode sessions after a Claude usage limit

[Service]
ExecStart=%s %s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, systemdQuote(binary), args)
}

// systemdQuote quotes a path for ExecStart when it needs it.
func systemdQuote(p string) string {
	if !strings.ContainsAny(p, " \t\"'\\") {
		return p
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runInstall(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "install the daemon in dry-run mode (records, sends nothing)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	if binary, err = filepath.EvalSymlinks(binary); err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	if strings.HasPrefix(binary, os.TempDir()) {
		return errors.New("run install from a built binary (go build or go install), not go run")
	}
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unitFile(binary, *dryRun)), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", unitName); err != nil {
		return err
	}
	if err := systemctl("restart", unitName); err != nil {
		return err
	}
	mode := "resuming"
	if *dryRun {
		mode = "dry run: records, sends nothing"
	}
	fmt.Fprintf(w, "installed %s (%s), running %s\n", path, mode, binary)
	return nil
}

func runUninstall(w io.Writer) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(w, "not installed")
		return nil
	}
	if err := systemctl("disable", "--now", unitName); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove unit: %w", err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(w, "uninstalled %s (the state in ~/.local/state/letsgoagain stays)\n", path)
	return nil
}
