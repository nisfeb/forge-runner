# forge-runner

forge-runner runs Forge CI jobs on a build machine. It polls a Forge ship over HTTPS for work, so the machine needs no inbound port and no SSH. It fetches the job's commit, runs its steps, streams the log back, and reports the result.

Forge is the CI control plane inside [grubbery](https://github.com/gwbtc/grubbery). The design is `.grubbery/docs/forge-ci-plan.md` in that repo. The runner lives in its own repo because Forge will not always ship inside a grubbery install. The two share only the runner protocol, whose version the runner sends in `hello`. Forge refuses a version it does not speak.

## Build

Go, standard library only. One source builds for every platform:

```sh
go build -o forge-runner .
GOOS=windows go build -o forge-runner.exe .
GOOS=darwin GOARCH=arm64 go build -o forge-runner .
```

`go test ./...` runs the runner against a fake Forge.

## Configure

In Forge's CI page, **Add runner** mints a key and shows it once. Put it in `forge-runner.json` beside the binary, readable only by the runner's user:

```json
{
  "ship": "https://your-ship.example.com",
  "key": "<id>.<secret>",
  "labels": ["linux", "x86_64", "android"],
  "workdir": "/var/lib/forge-runner/work",
  "env_file": "/etc/forge-runner/env"
}
```

- `labels` are what this machine can build. A job runs here only if the runner holds every label in its `runs-on`. Labels are written by hand.
- `workdir` holds one fresh directory per job, deleted when the job ends.
- `env_file` holds `KEY=VALUE` lines added to every job's environment. This is where secrets such as signing keys live. Forge never holds them. Every job on this runner sees them, so give a runner with secrets an allowlist of repos in Forge.

Each job also gets `CI=true`, `FORGE_REPO`, `FORGE_SHA`, `FORGE_BRANCH`, `FORGE_RUN` and `FORGE_JOB`.

## Run as a service

Run it as a dedicated unprivileged user, since it executes code from the repos it builds.

Linux, as a systemd unit:

```ini
[Unit]
Description=Forge CI runner
After=network-online.target

[Service]
User=forge-runner
WorkingDirectory=/opt/forge-runner
ExecStart=/opt/forge-runner/forge-runner -config /opt/forge-runner/forge-runner.json
Restart=always

[Install]
WantedBy=multi-user.target
```

macOS uses a launchd daemon with the same command. Windows uses a scheduled task that runs at startup.

## Getting the source

For a watched repo the runner fetches the job's commit from GitHub: `git init`, `git fetch --depth 1 <clone_url> <sha>`, then `git checkout FETCH_HEAD`. A private repo needs the runner's own read-only credential, such as a deploy key or a fine-grained token in its git config. Forge never hands out its GitHub token.

## Steps

A `run` step goes through `bash -eo pipefail -c` on Linux and macOS and `pwsh -NoProfile -Command` on Windows, or the step's `shell`. The first failing step fails the job. A cancel from Forge, or the job's timeout, kills the step's whole process group.
