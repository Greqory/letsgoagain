package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/Greqory/letsgoagain/internal/update"
)

// version is set by the release build (-ldflags "-X main.version=v1.2.3").
var version = ""

func currentVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version // go install …@v1.2.3
	}
	return "dev"
}

func runUpdate(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only tell whether a newer version exists")
	force := fs.Bool("force", false, "replace a development build too")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	u := update.New()
	cur := currentVersion()
	latest, err := u.Latest(ctx)
	if err != nil {
		return err
	}
	if !update.Newer(cur, latest) {
		fmt.Fprintf(w, "letsgoagain %s is up to date\n", cur)
		return nil
	}
	if *check {
		fmt.Fprintf(w, "letsgoagain %s → %s available, run: letsgoagain update\n", cur, latest)
		return nil
	}
	if cur == "dev" && !*force {
		return fmt.Errorf("this is a development build; update it from source, or replace it with %s using --force", latest)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	bin, err := u.Fetch(ctx, latest)
	if err != nil {
		return err
	}
	if err := update.Replace(exe, bin); err != nil {
		return err
	}
	fmt.Fprintf(w, "updated %s: %s → %s\n", exe, cur, latest)
	path, err := unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := systemctl("restart", unitName); err != nil {
		return err
	}
	fmt.Fprintln(w, "restarted the letsgoagain service")
	return nil
}
