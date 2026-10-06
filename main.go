// forge-runner polls a Forge CI ship for jobs, runs their steps, and
// reports back. It speaks only the runner protocol over HTTPS, so Forge
// and the runner ship separately. See the plan in the grubbery repo,
// .grubbery/docs/forge-ci-plan.md.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// protocol is the runner protocol version this runner speaks. Forge
// answers 426 to one it does not speak.
const protocol = 1

var version = "dev"

type config struct {
	Ship    string   `json:"ship"`     // e.g. https://ship.example.com
	Key     string   `json:"key"`      // <id>.<secret>, shown once by Forge
	Labels  []string `json:"labels"`   // e.g. ["linux", "x86_64", "android"]
	Workdir string   `json:"workdir"`  // jobs run in fresh directories under it
	EnvFile string   `json:"env_file"` // KEY=VALUE lines added to every job
}

type client struct {
	cfg  config
	http *http.Client
}

// errStatus is a non-2xx answer from Forge.
type errStatus struct {
	code int
	body string
}

func (e errStatus) Error() string { return fmt.Sprintf("forge answered %d: %s", e.code, e.body) }

// post sends body as JSON to runner/<route> and decodes a 200 answer
// into out. A 204 leaves out alone and returns false.
func (c *client) post(route string, body, out any) (bool, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(c.cfg.Ship, "/")+"/grubbery/forge/ci/runner/"+route, bytes.NewReader(buf))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	switch {
	case res.StatusCode == http.StatusNoContent:
		return false, nil
	case res.StatusCode != http.StatusOK:
		return false, errStatus{res.StatusCode, strings.TrimSpace(string(raw))}
	case out == nil:
		return true, nil
	}
	return true, json.Unmarshal(raw, out)
}

// fatal is an answer no retry will change: a bad key or a protocol Forge
// does not speak.
func fatal(err error) bool {
	var s errStatus
	return errors.As(err, &s) && (s.code == 401 || s.code == 426)
}

func loadConfig(path string) (config, error) {
	var cfg config
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Ship == "" || cfg.Key == "" {
		return cfg, fmt.Errorf("%s: ship and key are required", path)
	}
	if cfg.Workdir == "" {
		cfg.Workdir = os.TempDir()
	}
	return cfg, nil
}

func main() {
	path := flag.String("config", "forge-runner.json", "config file")
	once := flag.Bool("once", false, "run at most one job, then exit")
	showVersion := flag.Bool("version", false, "print the version and protocol, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("forge-runner %s, runner protocol %d\n", version, protocol)
		return
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	c := &client{cfg: cfg, http: &http.Client{Timeout: 2 * time.Minute}}
	if err := c.run(*once); err != nil {
		log.Fatal(err)
	}
}

// run says hello, then polls for jobs until a fatal answer.
func (c *client) run(once bool) error {
	var hi struct {
		Poll int `json:"poll"`
	}
	hello := map[string]any{
		"protocol": protocol,
		"labels":   c.cfg.Labels,
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
		"version":  version,
	}
	for {
		_, err := c.post("hello", hello, &hi)
		if err == nil {
			break
		}
		if fatal(err) {
			return err
		}
		log.Printf("hello: %v; retrying in 30s", err)
		time.Sleep(30 * time.Second)
	}
	every := time.Duration(max(hi.Poll, 5)) * time.Second
	log.Printf("connected to %s as %v, polling every %s", c.cfg.Ship, c.cfg.Labels, every)
	for {
		var j job
		got, err := c.post("poll", map[string]any{}, &j)
		switch {
		case err != nil && fatal(err):
			return err
		case err != nil:
			log.Printf("poll: %v", err)
		case got:
			c.runJob(j)
			if once {
				return nil
			}
			continue
		}
		time.Sleep(every)
	}
}
