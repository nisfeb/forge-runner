package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeForge answers the runner protocol with one job, then nothing. It
// records the log and the outcome.
type fakeForge struct {
	t      *testing.T
	job    job
	cancel bool // answer log chunks with cancel
	mu     sync.Mutex
	given  bool
	log    strings.Builder
	status string
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer abc123.sekrit" {
		w.WriteHeader(401)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.TrimPrefix(r.URL.Path, "/grubbery/forge/ci/runner/") {
	case "hello":
		if body["protocol"] != float64(protocol) {
			w.WriteHeader(426)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"poll": 5})
	case "poll":
		if f.given {
			w.WriteHeader(204)
			return
		}
		f.given = true
		json.NewEncoder(w).Encode(f.job)
	case "log":
		if body["lease"] != f.job.Lease {
			w.WriteHeader(409)
			return
		}
		f.log.WriteString(body["text"].(string))
		json.NewEncoder(w).Encode(map[string]any{"cancel": f.cancel})
	case "done":
		f.status = body["status"].(string)
		w.WriteHeader(200)
	default:
		w.WriteHeader(404)
	}
}

// repo makes a one-commit git repo holding hello.txt and returns its
// file:// URL and the commit's hash.
func repo(t *testing.T) (string, string) {
	dir := t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "hello.txt")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q",
		"-m", "test: add the hello fixture file", "-m", "The runner tests fetch this commit.")
	return "file://" + dir, git("rev-parse", "HEAD")
}

func runAgainst(t *testing.T, f *fakeForge) {
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &client{
		cfg:  config{Ship: srv.URL, Key: "abc123.sekrit", Labels: []string{"linux"}, Workdir: t.TempDir()},
		http: srv.Client(),
	}
	done := make(chan error, 1)
	go func() { done <- c.run(true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the job did not finish")
	}
}

func TestJobRunsAndReports(t *testing.T) {
	url, sha := repo(t)
	f := &fakeForge{t: t, job: job{
		Job: "1791-ab/build", Lease: "L1", Repo: "o/r", Sha: sha, Branch: "main",
		Source: "github", CloneURL: url, Timeout: 1,
		Steps: []step{{Run: "cat hello.txt"}, {Run: "echo on $FORGE_BRANCH"}},
	}}
	runAgainst(t, f)
	if f.status != "succeeded" {
		t.Fatalf("status %q, log:\n%s", f.status, f.log.String())
	}
	for _, want := range []string{"hello from the repo", "on main", "==> step 2"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, f.log.String())
		}
	}
}

func TestFailingStepFails(t *testing.T) {
	url, sha := repo(t)
	f := &fakeForge{t: t, job: job{
		Job: "1791-ab/build", Lease: "L1", Sha: sha, Source: "github", CloneURL: url, Timeout: 1,
		Steps: []step{{Run: "exit 3"}, {Run: "echo never"}},
	}}
	runAgainst(t, f)
	if f.status != "failed" || strings.Contains(f.log.String(), "never") {
		t.Fatalf("status %q, log:\n%s", f.status, f.log.String())
	}
}

func TestCancelKillsTheStep(t *testing.T) {
	url, sha := repo(t)
	f := &fakeForge{t: t, cancel: true, job: job{
		Job: "1791-ab/build", Lease: "L1", Sha: sha, Source: "github", CloneURL: url, Timeout: 5,
		Steps: []step{{Run: "sleep 300"}},
	}}
	start := time.Now()
	runAgainst(t, f)
	if f.status != "cancelled" || time.Since(start) > 30*time.Second {
		t.Fatalf("status %q after %s, log:\n%s", f.status, time.Since(start), f.log.String())
	}
}
