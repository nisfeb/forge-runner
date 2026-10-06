package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// job is what poll answers: one job of one run, held by lease.
type job struct {
	Job      string `json:"job"` // <run>/<job>
	Lease    string `json:"lease"`
	Repo     string `json:"repo"`
	Sha      string `json:"sha"`
	Branch   string `json:"branch"`
	Source   string `json:"source"` // github: clone_url at sha
	CloneURL string `json:"clone_url"`
	Timeout  int    `json:"timeout"` // minutes
	Steps    []step `json:"steps"`
}

type step struct {
	Run    string `json:"run"`
	Upload string `json:"upload"`
	Shell  string `json:"shell"`
}

const (
	flushEvery = 5 * time.Second
	beatEvery  = 10 * time.Second // a quiet build still hears a cancel within 10 s
	chunkMax   = 16 * 1024        // Forge keeps at most 16 KB per chunk
)

// logger buffers output and posts it to Forge in chunks: every 5 seconds,
// at 16 KB, and empty every 60 seconds so a quiet build stays alive. Each
// answer may carry a cancel, which cancels the job's context.
type logger struct {
	c      *client
	j      job
	cancel context.CancelFunc
	mu     sync.Mutex
	buf    []byte
	seq    int
	last   time.Time
	cut    bool // cancel seen
}

func (l *logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	full := len(l.buf) >= chunkMax
	l.mu.Unlock()
	if full {
		l.flush(false)
	}
	return len(p), nil
}

func (l *logger) printf(format string, a ...any) {
	fmt.Fprintf(l, format+"\n", a...)
}

// flush posts what is buffered, or an empty heartbeat when beat is set.
func (l *logger) flush(beat bool) {
	l.mu.Lock()
	n := min(len(l.buf), chunkMax)
	// the second's margin keeps a 5 s ticker from missing the 10 s beat by
	// the length of a round trip
	if n == 0 && !(beat && time.Since(l.last) >= beatEvery-time.Second) {
		l.mu.Unlock()
		return
	}
	text := string(l.buf[:n])
	l.buf = l.buf[n:]
	// an empty post is a heartbeat: Forge keeps no chunk for it, so it
	// takes no sequence number
	seq := l.seq
	if n > 0 {
		l.seq++
	}
	l.last = time.Now()
	l.mu.Unlock()
	var ans struct {
		Cancel bool `json:"cancel"`
	}
	body := map[string]any{"job": l.j.Job, "lease": l.j.Lease, "seq": seq, "text": text}
	var err error
	for try := 0; try < 3; try++ {
		if _, err = l.c.post("log", body, &ans); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Printf("%s: log chunk %d: %v", l.j.Job, seq, err)
		return
	}
	l.mu.Lock()
	first := ans.Cancel && !l.cut
	if first {
		l.cut = true
	}
	l.mu.Unlock()
	if first {
		l.cancel()
	}
}

func (l *logger) cancelled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cut
}

// pump flushes on a timer until stop closes, then flushes the rest.
func (l *logger) pump(stop <-chan struct{}, done chan<- struct{}) {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.flush(true)
		case <-stop:
			for {
				l.mu.Lock()
				left := len(l.buf)
				l.mu.Unlock()
				if left == 0 {
					break
				}
				l.flush(false)
			}
			close(done)
			return
		}
	}
}

// runJob fetches the source, runs each step, streams the log, and reports
// the outcome. The job's directory is removed afterwards.
func (c *client) runJob(runner context.Context, j job) {
	log.Printf("%s: %s at %.12s", j.Job, j.Repo, j.Sha)
	limit := time.Duration(max(j.Timeout, 1)) * time.Minute
	ctx, cancel := context.WithTimeout(runner, limit)
	defer cancel()
	l := &logger{c: c, j: j, cancel: cancel, last: time.Now()}
	stop, flushed := make(chan struct{}), make(chan struct{})
	go l.pump(stop, flushed)

	status, why := "succeeded", ""
	dir, err := os.MkdirTemp(c.cfg.Workdir, "forge-job-")
	if err != nil {
		status, why = "failed", err.Error()
	} else {
		defer os.RemoveAll(dir)
		status, why = c.steps(ctx, l, j, dir)
	}
	switch {
	case l.cancelled():
		status, why = "cancelled", "cancelled from Forge"
	case runner.Err() != nil:
		status, why = "failed", "the runner was stopped"
	}
	l.printf("==> %s %s", status, why)
	close(stop)
	<-flushed
	body := map[string]any{"job": j.Job, "lease": j.Lease, "status": status, "error": why, "artifacts": []any{}}
	for try := 0; try < 5; try++ {
		if _, err = c.post("done", body, nil); err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		log.Printf("%s: reporting %s: %v", j.Job, status, err)
	}
	log.Printf("%s: %s %s", j.Job, status, why)
}

// steps runs the source fetch and each step, stopping at the first
// failure. It answers the job's status and why.
func (c *client) steps(ctx context.Context, l *logger, j job, dir string) (string, string) {
	env, err := c.jobEnv(j)
	if err != nil {
		return "failed", err.Error()
	}
	if j.Source != "github" {
		return "failed", "source " + j.Source + " is not supported by this runner yet"
	}
	l.printf("==> fetching %s at %s", j.Repo, j.Sha)
	for _, args := range [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", j.CloneURL, j.Sha},
		{"checkout", "-q", "FETCH_HEAD"},
	} {
		if err := run(ctx, l, dir, env, "git", args...); err != nil {
			return "failed", "fetching the source: " + err.Error()
		}
	}
	for i, s := range j.Steps {
		switch {
		case s.Run != "":
			sh := s.Shell
			if sh == "" {
				sh = defaultShell()
			}
			l.printf("==> step %d: %s", i+1, s.Run)
			var err error
			if sh == "pwsh" {
				err = run(ctx, l, dir, env, "pwsh", "-NoProfile", "-NonInteractive", "-Command", s.Run)
			} else {
				err = run(ctx, l, dir, env, "bash", "-eo", "pipefail", "-c", s.Run)
			}
			if err != nil {
				return "failed", fmt.Sprintf("step %d: %v", i+1, err)
			}
		case s.Upload != "":
			// ponytail: artifact storage arrives in phase 2; until then
			// an upload step fails instead of pretending.
			return "failed", fmt.Sprintf("step %d: upload %s: artifact storage is not set up yet", i+1, s.Upload)
		}
	}
	return "succeeded", ""
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "pwsh"
	}
	return "bash"
}

// run runs one command in its own process group, output to the log. A
// cancelled or timed-out context kills the whole group.
func run(ctx context.Context, l *logger, dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, l, l
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
		killGroup(cmd)
		<-waited
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %d minutes", max(l.j.Timeout, 1))
		}
		return ctx.Err()
	}
}

// jobEnv is the runner's environment, the job's FORGE_ variables, and the
// env_file's lines, which is where runner-local secrets live.
func (c *client) jobEnv(j job) ([]string, error) {
	run, name, _ := strings.Cut(j.Job, "/")
	env := append(os.Environ(),
		"CI=true",
		"FORGE_REPO="+j.Repo,
		"FORGE_SHA="+j.Sha,
		"FORGE_BRANCH="+j.Branch,
		"FORGE_RUN="+run,
		"FORGE_JOB="+name,
	)
	if c.cfg.EnvFile == "" {
		return env, nil
	}
	f, err := os.Open(filepath.Clean(c.cfg.EnvFile))
	if err != nil {
		return nil, fmt.Errorf("env_file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		env = append(env, line)
	}
	return env, sc.Err()
}
