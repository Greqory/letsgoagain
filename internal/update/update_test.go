package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGitHub(t *testing.T, bin []byte, sums string) *Updater {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v0.2.0","name":"v0.2.0"}`)
		case "/" + Repo + "/releases/download/v0.2.0/checksums.txt":
			fmt.Fprint(w, sums)
		case "/" + Repo + "/releases/download/v0.2.0/letsgoagain_linux_amd64":
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Updater{API: srv.URL, Download: srv.URL, Arch: "amd64", HTTP: srv.Client()}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestFetch(t *testing.T) {
	bin := []byte("#!/bin/sh\necho new\n")
	ctx := context.Background()

	u := fakeGitHub(t, bin, sum(bin)+"  letsgoagain_linux_amd64\n"+sum([]byte("x"))+"  letsgoagain_linux_arm64\n")
	tag, err := u.Latest(ctx)
	if err != nil || tag != "v0.2.0" {
		t.Fatalf("latest: %q %v", tag, err)
	}
	got, err := u.Fetch(ctx, tag)
	if err != nil || string(got) != string(bin) {
		t.Fatalf("fetch: %q %v", got, err)
	}

	bad := fakeGitHub(t, bin, sum([]byte("other"))+"  letsgoagain_linux_amd64\n")
	if _, err := bad.Fetch(ctx, "v0.2.0"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum mismatch, got %v", err)
	}
	missing := fakeGitHub(t, bin, sum(bin)+"  letsgoagain_linux_arm64\n")
	if _, err := missing.Fetch(ctx, "v0.2.0"); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("want a missing entry, got %v", err)
	}
	if _, err := u.Fetch(ctx, "v9.9.9"); err == nil {
		t.Fatal("want an error for a release that does not exist")
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "letsgoagain")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	entries, _ := os.ReadDir(dir)
	if string(got) != "new" || fi.Mode().Perm() != 0o755 || len(entries) != 1 {
		t.Fatalf("content %q, mode %v, entries %d", got, fi.Mode(), len(entries))
	}
}

func TestNewer(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.10.0", "v0.9.0", false},
		{"v1.0.0", "v1.0.1", true},
		{"v0.1.0-rc1", "v0.1.0", false},
		{"dev", "v0.1.0", true},
	}
	for _, tt := range tests {
		if got := Newer(tt.current, tt.latest); got != tt.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}
