package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greqory/letsgoagain/internal/fakemeridian"
	"github.com/Greqory/letsgoagain/internal/state"
)

type doctorWorld struct {
	d       doctor
	service *httptest.Server
	events  int // status code of /api/event
}

func newDoctorWorld(t *testing.T) *doctorWorld {
	t.Helper()
	dir := t.TempDir()
	w := &doctorWorld{events: http.StatusOK}
	w.service = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, p, _ := r.BasicAuth(); p != "pw" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/session/active":
			fmt.Fprint(rw, `{"data":{}}`)
		case "/api/event":
			rw.WriteHeader(w.events)
			if w.events == http.StatusOK {
				rw.(http.Flusher).Flush()
				<-r.Context().Done()
			}
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.service.Close)
	servicePath := filepath.Join(dir, "service.json")
	writeFile(t, servicePath, fmt.Sprintf(`{"url":%q,"version":"2.3.0","pid":1,"password":"pw"}`, w.service.URL))

	m := &fakemeridian.Server{}
	util, resets := 0.52, time.Date(2026, 11, 14, 3, 30, 0, 0, time.Local).UnixMilli()
	m.SetBuckets([]fakemeridian.Bucket{{Type: "five_hour", Status: "allowed", Utilization: util, ResetsAt: resets}})
	ms := httptest.NewServer(m)
	t.Cleanup(ms.Close)

	unit := filepath.Join(dir, "letsgoagain.service")
	writeFile(t, unit, unitFile("/opt/bin/letsgoagain", false))
	w.d = doctor{
		self: "/opt/bin/letsgoagain", servicePath: servicePath, meridianURL: ms.URL, unitPath: unit,
		timeout:  2 * time.Second,
		isActive: func() (string, error) { return "active", nil },
		status: func() ([]state.Stop, error) {
			return []state.Stop{{Status: state.Waiting}, {Status: state.Done}}, nil
		},
	}
	return w
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *doctorWorld) run(t *testing.T) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	n := w.d.run(context.Background(), &buf)
	return buf.String(), n
}

func TestDoctorAllGood(t *testing.T) {
	w := newDoctorWorld(t)
	out, failures := w.run(t)
	if failures != 0 {
		t.Fatalf("failures %d:\n%s", failures, out)
	}
	for _, want := range []string{
		"ok    letsgoagain", "ok    OpenCode         " + w.service.URL + ", OpenCode 2.3.0", "ok    event stream     subscribed",
		"ok    meridian         meridian 1.79.0 (tested: 1.79); five_hour 52% (resets Sat 03:30)", "ok    service          active\n",
		"ok    daemon           answering, 1 open stops, 2 tracked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pw") {
		t.Errorf("password in the output:\n%s", out)
	}
}

func TestDoctorProblems(t *testing.T) {
	tests := []struct {
		name     string
		break_   func(w *doctorWorld)
		want     string
		failures int
	}{
		{"no service file", func(w *doctorWorld) { w.d.servicePath += ".missing" }, "FAIL  OpenCode         read service file", 2},
		{"wrong password", func(w *doctorWorld) {
			writeFile(t, w.d.servicePath, fmt.Sprintf(`{"url":%q,"password":"nope"}`, w.service.URL))
		}, "not answering", 2},
		{"newer OpenCode", func(w *doctorWorld) {
			writeFile(t, w.d.servicePath, fmt.Sprintf(`{"url":%q,"version":"2.4.0","password":"pw"}`, w.service.URL))
		}, "OpenCode 2.4.0 is newer than the tested 2.3", 0},
		{"event stream refused", func(w *doctorWorld) { w.events = http.StatusInternalServerError }, "FAIL  event stream     event stream: status 500", 1},
		{"meridian down", func(w *doctorWorld) { w.d.meridianURL = "http://127.0.0.1:1" }, "WARN  meridian", 0},
		{"not installed", func(w *doctorWorld) { w.d.unitPath += ".missing" }, "FAIL  service          not installed; run: letsgoagain install", 1},
		{"installed, stopped", func(w *doctorWorld) {
			w.d.isActive = func() (string, error) { return "inactive", errors.New("exit 3") }
		}, "FAIL  service          installed but inactive", 1},
		{"other binary", func(w *doctorWorld) { w.d.self = "/home/me/go/bin/letsgoagain" }, "WARN  service          active, but runs /opt/bin/letsgoagain", 0},
		{"daemon silent", func(w *doctorWorld) { w.d.status = func() ([]state.Stop, error) { return nil, errors.New("refused") } }, "FAIL  daemon", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newDoctorWorld(t)
			tt.break_(w)
			out, failures := w.run(t)
			if !strings.Contains(out, tt.want) || failures != tt.failures {
				t.Fatalf("want %q and %d failures, got %d:\n%s", tt.want, tt.failures, failures, out)
			}
			if strings.Contains(out, "nope") {
				t.Errorf("password in the output:\n%s", out)
			}
		})
	}
}

func TestDoctorDryRunUnit(t *testing.T) {
	w := newDoctorWorld(t)
	writeFile(t, w.d.unitPath, unitFile("/opt/bin/letsgoagain", true))
	if out, _ := w.run(t); !strings.Contains(out, "ok    service          active, dry run: records only, sends nothing") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestUnitExec(t *testing.T) {
	for _, tt := range []struct {
		unit   string
		bin    string
		dryRun bool
	}{
		{unitFile("/opt/bin/letsgoagain", false), "/opt/bin/letsgoagain", false},
		{unitFile("/opt/bin/letsgoagain", true), "/opt/bin/letsgoagain", true},
		{unitFile(`/home/me/my "bins"/letsgoagain`, true), `/home/me/my "bins"/letsgoagain`, true},
		{"[Service]\nRestart=always\n", "", false},
	} {
		if bin, dry := unitExec(tt.unit); bin != tt.bin || dry != tt.dryRun {
			t.Errorf("got %q %v, want %q %v for:\n%s", bin, dry, tt.bin, tt.dryRun, tt.unit)
		}
	}
}
