package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Greqory/letsgoagain/internal/state"
)

// DefaultSocketPath is $XDG_RUNTIME_DIR/letsgoagain.sock.
func DefaultSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(dir, "letsgoagain.sock"), nil
}

type request struct {
	kind  string // "status" or "resume"
	id    string // resume: a session ID, or "" for all waiting stops
	reply chan reply
}

type reply struct {
	Stops   []state.Stop `json:"stops,omitempty"`
	Results []string     `json:"results,omitempty"`
	Error   string       `json:"error,omitempty"`
}

func (d *Daemon) control(ctx context.Context, r request) reply {
	switch r.kind {
	case "status":
		var out []state.Stop
		for _, st := range d.st.Sorted() {
			out = append(out, *st)
		}
		return reply{Stops: out}
	case "resume":
		if d.api == nil {
			return reply{Error: "the OpenCode service is not reachable"}
		}
		var targets []*state.Stop
		for _, st := range d.st.Sorted() {
			if st.Status == state.Waiting && (r.id == "" || st.SessionID == r.id) {
				targets = append(targets, st)
			}
		}
		if len(targets) == 0 && r.id == "" {
			return reply{Error: "no waiting stops"}
		}
		if len(targets) == 0 {
			return reply{Error: "no waiting stop for " + r.id}
		}
		var out reply
		for _, st := range targets {
			res := "resumed"
			if d.cfg.DryRun {
				res = "dry run, not sent"
			}
			if err := d.resume(ctx, st, "resume-now"); err != nil {
				res = "not resumed: " + err.Error()
			}
			out.Results = append(out.Results, fmt.Sprintf("%s: %s", d.name(st), res))
		}
		return out
	}
	return reply{Error: "unknown request " + r.kind}
}

// Serve answers control requests on a Unix socket until ctx ends.
func (d *Daemon) Serve(ctx context.Context, socketPath string) error {
	_ = os.Remove(socketPath) // a socket left by a crashed daemon
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("control socket: %w", err)
	}
	ask := func(w http.ResponseWriter, r request) {
		r.reply = make(chan reply, 1)
		if !d.send(ctx, r) {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		select {
		case rep := <-r.reply:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rep)
		case <-ctx.Done():
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { ask(w, request{kind: "status"}) })
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		ask(w, request{kind: "resume", id: r.URL.Query().Get("session")})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control socket: %w", err)
	}
	return nil
}

// Control is the CLI's side of the control socket.
type Control struct {
	http *http.Client
}

// NewControl talks to the daemon at socketPath.
func NewControl(socketPath string) *Control {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return &Control{http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

// Status returns the daemon's stops.
func (c *Control) Status() ([]state.Stop, error) {
	rep, err := c.call(http.MethodGet, "/status")
	return rep.Stops, err
}

// ResumeNow resumes one waiting stop, or all with id "".
func (c *Control) ResumeNow(id string) ([]string, error) {
	rep, err := c.call(http.MethodPost, "/resume?"+url.Values{"session": {id}}.Encode())
	return rep.Results, err
}

func (c *Control) call(method, path string) (reply, error) {
	req, err := http.NewRequest(method, "http://letsgoagain"+path, nil)
	if err != nil {
		return reply{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("daemon not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return reply{}, fmt.Errorf("daemon: status %d: %s", resp.StatusCode, b)
	}
	var rep reply
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return reply{}, fmt.Errorf("daemon: %w", err)
	}
	if rep.Error != "" {
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}
