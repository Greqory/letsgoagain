package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Greqory/letsgoagain/internal/daemon"
	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/opencode"
	"github.com/Greqory/letsgoagain/internal/state"
)

// doctor checks everything LetsGoAgain depends on, one line each.
type doctor struct {
	self        string // own executable
	servicePath string
	meridianURL string
	unitPath    string
	isActive    func() (string, error)       // systemctl --user is-active
	status      func() ([]state.Stop, error) // the daemon's control socket
	timeout     time.Duration
}

type verdict int

const (
	pass verdict = iota
	warn
	fail
)

func (v verdict) String() string { return [...]string{"ok  ", "WARN", "FAIL"}[v] }

func runDoctor(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	meridianURL := fs.String("meridian", meridian.DefaultURL, "meridian proxy URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	servicePath, err := opencode.DefaultServicePath()
	if err != nil {
		return err
	}
	unit, err := unitPath()
	if err != nil {
		return err
	}
	d := doctor{
		self: self, servicePath: servicePath, meridianURL: *meridianURL, unitPath: unit, timeout: 5 * time.Second,
		isActive: func() (string, error) {
			out, err := exec.Command("systemctl", "--user", "is-active", unitName).Output()
			return strings.TrimSpace(string(out)), err
		},
		status: func() ([]state.Stop, error) {
			socket, err := daemon.DefaultSocketPath()
			if err != nil {
				return nil, err
			}
			return daemon.NewControl(socket).Status()
		},
	}
	if failures := d.run(context.Background(), w); failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	return nil
}

func (d doctor) run(ctx context.Context, w io.Writer) (failures int) {
	report := func(name string, v verdict, detail string) {
		fmt.Fprintf(w, "%s  %-16s %s\n", v, name, detail)
		if v == fail {
			failures++
		}
	}
	report("letsgoagain", pass, fmt.Sprintf("%s (%s)", currentVersion(), d.self))

	svc, err := opencode.ReadService(d.servicePath)
	if err != nil {
		report("OpenCode", fail, fmt.Sprintf("%v; is OpenCode v2 running?", err))
		report("event stream", fail, "needs the OpenCode service")
	} else {
		d.checkOpenCode(ctx, svc, report)
	}
	d.checkMeridian(ctx, report)
	d.checkService(report)
	stops, err := d.status()
	switch {
	case err != nil:
		report("daemon", fail, "not reachable; see: systemctl --user status letsgoagain")
	default:
		open := 0
		for _, s := range stops {
			if s.Open() {
				open++
			}
		}
		report("daemon", pass, fmt.Sprintf("answering, %d open stops, %d tracked", open, len(stops)))
	}
	return failures
}

func (d doctor) checkOpenCode(ctx context.Context, svc opencode.Service, report func(string, verdict, string)) {
	c := opencode.NewClient(svc)
	version := svc.Version
	if version == "" {
		version = "unknown version"
	}
	ctx1, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if _, err := c.Running(ctx1, ""); err != nil {
		report("OpenCode", fail, fmt.Sprintf("%s (%s) not answering: %v", svc.URL, version, err))
		report("event stream", fail, "needs the OpenCode service")
		return
	}
	report("OpenCode", pass, fmt.Sprintf("%s, OpenCode %s", svc.URL, version))

	ctx2, cancel2 := context.WithTimeout(ctx, d.timeout)
	defer cancel2()
	connected := false
	err := c.Events(ctx2, []string{opencode.EventExecutionFailed}, func() { connected = true; cancel2() },
		func(opencode.Event) error { return nil })
	if connected {
		report("event stream", pass, "subscribed")
		return
	}
	if errors.Is(ctx2.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("no answer within %s", d.timeout)
	}
	report("event stream", fail, err.Error())
}

func (d doctor) checkMeridian(ctx context.Context, report func(string, verdict, string)) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	q, err := meridian.NewClient(d.meridianURL).Quota(ctx)
	if err != nil {
		report("meridian", warn, fmt.Sprintf("%v; without the quota, reset times come from retry_after or probing", err))
		return
	}
	var parts []string
	for _, b := range q.Buckets {
		if b.Utilization == nil || b.ResetsAt == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d%% (resets %s)", b.Type, int(*b.Utilization*100+0.5),
			time.UnixMilli(*b.ResetsAt).Local().Format("Mon 15:04")))
	}
	report("meridian", pass, strings.Join(parts, ", "))
}

func (d doctor) checkService(report func(string, verdict, string)) {
	raw, err := os.ReadFile(d.unitPath)
	if err != nil {
		report("service", fail, "not installed; run: letsgoagain install")
		return
	}
	active, _ := d.isActive()
	if active != "active" {
		report("service", fail, fmt.Sprintf("installed but %s; see: systemctl --user status letsgoagain", orUnknown(active)))
		return
	}
	bin, dryRun := unitExec(string(raw))
	mode := ""
	if dryRun {
		mode = ", dry run: records only, sends nothing"
	}
	if bin != d.self {
		report("service", warn, fmt.Sprintf("active, but runs %s, not this binary (%s); run: letsgoagain install", bin, d.self))
		return
	}
	report("service", pass, "active"+mode)
}

// unitExec reads the binary and the dry-run flag from the unit's ExecStart line.
func unitExec(unit string) (binary string, dryRun bool) {
	for _, line := range strings.Split(unit, "\n") {
		rest, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(rest, `"`) {
			binary, rest, _ = strings.Cut(rest, " ")
			return binary, strings.Contains(rest, "--dry-run")
		}
		var b strings.Builder
		for i := 1; i < len(rest); i++ {
			switch c := rest[i]; {
			case c == '\\' && i+1 < len(rest):
				i++
				b.WriteByte(rest[i])
			case c == '"':
				return b.String(), strings.Contains(rest[i+1:], "--dry-run")
			default:
				b.WriteByte(c)
			}
		}
		return b.String(), false
	}
	return "", false
}

func orUnknown(s string) string {
	if s == "" {
		return "not running"
	}
	return s
}
