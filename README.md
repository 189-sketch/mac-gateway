# Fleet

Run jobs on idle home computers through one cloud API. An agent calls the gateway on your cloud server, the gateway hands the job to a machine that can run it, and the agent gets progress and result files back.

```text
Agent ──HTTPS──▶ Gateway (cloud server) ◀──outbound WSS + HTTPS── Worker (Mac mini) ──▶ local tools
```

Workers only make outbound connections, so your home network needs no open ports. Each worker can run only what its local manifest declares. The gateway sends a capability name and input, never a command.

This covers steps 1 to 3 of the design: the gateway, the worker, adapters for command-line tools and HTTP services, progress streaming, cancellation, file transfer, the `fleet` command, a status dashboard and a Windows worker.

**What has been tested:** the suite runs the real gateway and worker on Linux, with a stand-in tool and a stand-in HTTP service, and the dashboard has been checked in headless Chromium.

**What hasn't:**
- Nothing has run on a real Mac or through a real server install, so `say`, launchd, systemd and Caddy are untested.
- The Windows worker and its installer have been compiled but have never run on Windows.
- The HTTP adapters haven't been used with a real service such as Ollama or ComfyUI.

**Not built yet:**
- LLM streaming
- An OpenAI-compatible endpoint
- MCP

## Build

This needs Go 1.27 or newer. The `make` targets are shortcuts, so if `make` isn't installed, run the matching commands from the `Makefile` directly.

```sh
make build   # Binaries for this machine, in bin/
make dist    # Gateway for Linux, workers for Mac and Windows, and the CLI, in dist/
make test    # All tests, including the end-to-end suite (a few minutes)
./deploy/test-install.sh   # Tests the install scripts without changing this machine
```

## Install

Prebuilt binaries are on the [Releases page](https://github.com/189-sketch/mac-gateway/releases). Releases are marked as pre-releases until they've been run on a real Mac.

Mac installs use a one-line command rather than a double-click download. A file downloaded through a browser is blocked by Gatekeeper unless it's signed with a paid Apple Developer ID, and these aren't. Files fetched with `curl` aren't blocked this way.

### What you need

- **Server:** Debian or Ubuntu on x86-64 or ARM64, with a normal user who has `sudo`. Installing from a release doesn't need Go.
- **Domain:** a domain name with its DNS pointing at the server.
- **Firewall:** ports 80 and 443 open on the server, including in any cloud security group. Caddy needs both to get its HTTPS certificate.
- **Worker machine:** macOS on Apple silicon or Intel, or Windows 10 or 11 on x64 or ARM64. Your home network doesn't need any open ports.

Run the commands below in your own terminal. They print secrets that are shown only once, so don't paste the output into chats or tickets.

### 1. Install the gateway

Run one of these on the server, replacing `fleet.example.com` with your domain and `mac-mini-1` with a name for your first machine.

**One line, if the repo is public:**

```sh
curl -fsSL https://github.com/189-sketch/mac-gateway/releases/download/v0.2.0/install-gateway.sh | bash -s -- fleet.example.com mac-mini-1
```

**From a downloaded bundle, which also works while the repo is private.** Download `fleet-server-linux-amd64.tar.gz` (or `-arm64`) from the [Releases page](https://github.com/189-sketch/mac-gateway/releases) while logged in, or use the GitHub CLI as below:

```sh
gh release download v0.2.0 -R 189-sketch/mac-gateway -p 'fleet-server-linux-amd64.tar.gz'
tar -xzf fleet-server-linux-amd64.tar.gz
./fleet-server-linux-amd64/install-gateway.sh fleet.example.com mac-mini-1
```

**From source, which needs Go 1.27 or newer:**

```sh
./deploy/install-gateway.sh fleet.example.com mac-mini-1
```

Whichever you use, the script does the following:

1. Gets the binaries, and verifies their checksums if it downloaded them.
2. Installs the gateway as the `fleet-gateway` service, with its data in `/var/lib/fleet`.
3. Installs Caddy if it's missing and sets up HTTPS. Your existing Caddy sites are kept.
4. Publishes the worker installer and worker binaries at `https://your-domain/install/`. They contain no secrets, and anyone can download them.
5. Prints an API key for your agent and the command to run on your Mac.

It stops before changing anything if another program is using port 80 or 443. It exits with a warning if the server isn't reachable over HTTPS yet, but still prints your credentials. In that case, fix DNS or the firewall; Caddy keeps retrying, so you don't need to run the script again.

### 2. Install the worker

Use a dedicated standard (non-admin) macOS account with none of your personal files, because jobs run as that user and can read anything it can, including the token file.

Log in to that account on the Mac, open Terminal and paste the command the gateway script printed. It looks like this:

```sh
curl -fsSL https://fleet.example.com/install/worker.sh | FLEET_NODE_NAME=mac-mini-1 FLEET_NODE_TOKEN=fn_... sh
```

The script does the following:

1. Downloads the worker for your chip and verifies its checksum.
2. Installs into `~/fleet`, with a `worker.yaml` that offers one service, `tts.say`.
3. Starts the worker as a LaunchAgent, then waits until it has connected.

The command contains the machine's token, so your shell history will too. To remove it, close Terminal, then delete that line from `~/.zsh_history`.

### 3. Keep the Mac available

The script doesn't change your power settings, so do these yourself:

```sh
sudo pmset -a sleep 0 disablesleep 1 autorestart 1
```

Then turn on automatic login for the worker's account in System Settings. The worker runs inside a logged-in session, so without this it won't come back after a reboot or power cut.

### 4. Test it

You need the `fleet` command on the machine you test from. If the repo is public, this installs it to `~/.local/bin`:

```sh
curl -fsSL https://github.com/189-sketch/mac-gateway/releases/download/v0.2.0/install-cli.sh | sh
```

Otherwise, download `fleet-linux-amd64`, `fleet-darwin-arm64` or the one for your machine from the Releases page, or build it with `go build -o fleet ./cmd/fleet`. Then:

```sh
export FLEET_URL=https://fleet.example.com
export FLEET_API_KEY=fk_...

fleet capabilities
fleet run tts.say --input '{"text":"Hello from the Mac mini"}' -o hello.aiff
```

You should get `hello.aiff` on the machine that ran the command. To confirm jobs survive an outage, unplug the Mac's network during a longer job, then plug it back in. The job should finish with one result.

### Add another machine

On the server:

```sh
fleet-add-worker mac-mini-2
```

It prints one command for a Mac and one for Windows. Run the one that fits the new machine.

### Install on Windows

This hasn't been run on real Windows yet, so expect to find rough edges.

Run `fleet-add-worker windows-pc-1` on the server. Then, on the PC, log in as the account the worker should use, open PowerShell with **Run as administrator**, and paste the Windows command it printed:

```powershell
[Net.ServicePointManager]::SecurityProtocol = 'Tls12'; $env:FLEET_NODE_NAME = 'windows-pc-1'; $env:FLEET_NODE_TOKEN = 'fn_...'; irm https://fleet.example.com/install/worker.ps1 | iex
```

The script does the following:

1. Downloads the worker for your processor and verifies its checksum.
2. Installs into `%LOCALAPPDATA%\Fleet`, with a `worker.yaml` that offers one test service, `system.hostname`, and limits the token file to this user.
3. Registers a scheduled task named **Fleet Worker**, which runs at boot as this user with limited rights, with nobody logged in, and comes back within 5 minutes after a crash.

It needs administrator rights only to register the task. A scheduled task is used rather than a Windows service so the worker runs in your account, where your tools and models are installed. It can't reach network shares when started this way.

The script doesn't change your power settings, so do this yourself to keep the PC from sleeping on mains power:

```powershell
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0
```

To upgrade, run the same command again without the node name and token. To uninstall, run `Unregister-ScheduledTask -TaskName 'Fleet Worker'`, then delete `%LOCALAPPDATA%\Fleet`.

Some things work differently on Windows:

- **Stopping a job is immediate.** Windows has no gentle stop signal for a program with no console, so a command gets no chance to clean up.
- **Batch files are refused.** Windows runs `.bat` and `.cmd` files through `cmd.exe`, which would reparse job input as commands. Point `command` at the real program.
- **Write paths in single quotes in YAML,** such as `command: ['C:\tools\ffmpeg.exe', ...]`, or the backslashes are read as escapes.

### Upgrade

Upgrade the server first, by running the install command from a newer release without a node name. Then run this on each Mac, which needs no node name or token. Windows is covered under [Install on Windows](#install-on-windows).

```sh
curl -fsSL https://fleet.example.com/install/worker.sh | sh
```

Data, API keys, tokens and each Mac's `worker.yaml` are kept. Running jobs on a Mac are interrupted during its upgrade, and rerun only if their capability sets `retry_if_lost`.

### Manage credentials

On the server:

```sh
sudo -u fleet fleet-gateway key add my-agent --capabilities 'tts.*,video.*' --data /var/lib/fleet
sudo -u fleet fleet-gateway key revoke my-agent --data /var/lib/fleet
sudo -u fleet fleet-gateway node revoke mac-mini-1 --data /var/lib/fleet
```

`--capabilities` takes comma-separated patterns, and `'*'` allows everything. A key made with `--dashboard` instead can only view [the dashboard](#dashboard). Revoking takes effect within a heartbeat. A revoked name can't be registered again, so a replacement machine needs a new name.

### Troubleshooting

| Problem | What to check |
|---|---|
| `https://your-domain` isn't reachable | DNS, ports 80 and 443 in the cloud security group, then `sudo journalctl -u caddy -n 50` |
| The gateway won't start | `sudo journalctl -u fleet-gateway -n 50` |
| A Mac says `launchd refused to start it` | You're probably connected over SSH. Log in to that user at the screen or through Screen Sharing, then run the command again |
| A Mac starts but doesn't connect | `tail ~/fleet/worker.log`. `the gateway rejected this node's token` means the token is wrong or revoked |
| A job fails with `NO_NODE_ONLINE` | `fleet capabilities` shows which nodes are online. Check that the Mac is awake and logged in |
| You changed `worker.yaml` | Restart the worker: `launchctl kickstart -k gui/$(id -u)/com.fleet.worker` |

### Uninstall

On a Mac:

```sh
launchctl bootout "gui/$(id -u)/com.fleet.worker"
rm -rf ~/fleet ~/Library/LaunchAgents/com.fleet.worker.plist
```

On the server, this also deletes all jobs, results and credentials:

```sh
sudo systemctl disable --now fleet-gateway
sudo rm -rf /var/lib/fleet /srv/fleet /etc/systemd/system/fleet-gateway.service /usr/local/bin/fleet-gateway /usr/local/bin/fleet-add-worker /etc/caddy/fleet.caddy
sudo sed -i '/^import fleet.caddy$/d' /etc/caddy/Caddyfile
sudo systemctl reload caddy
sudo userdel fleet
```

Caddy itself stays installed, in case other sites use it.

## Call it

### With the `fleet` command

This is the simplest way for an agent or a script to use the gateway. Copy the binary for your machine from `dist/`, then set:

```sh
export FLEET_URL=https://fleet.example.com
export FLEET_API_KEY=fk_...
```

```sh
# See what's available, with input schemas and online status
fleet capabilities

# Submit a job, follow it and save its result
fleet run tts.say --input '{"text":"Hello from the Mac mini"}' -o hello.aiff

# Send a local file as an input file
fleet run video.transcode -f source=clip.mov -o out.mp4

# Start a job without waiting, then check on it
fleet run tts.say --input @request.json --detach
fleet status JOB_ID
fleet watch JOB_ID
fleet download JOB_ID -o hello.aiff
fleet cancel JOB_ID
```

The final result goes to stdout as JSON and progress goes to stderr. `fleet run` reconnects if the gateway or network drops, and Ctrl-C stops following the job without cancelling it. Run `fleet help` for all options.

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | The job or command failed, and running it again won't help |
| 2 | Invalid usage or configuration |
| 75 | A temporary failure, such as no node being online; running it again may work |
| 130 | Interrupted; a submitted job keeps running |

If a job succeeds but its result can't be saved, `fleet run` exits with 1 rather than 75, because running it again would repeat the job. Use `fleet download` instead.

### With the HTTP API

```sh
export FLEET=https://fleet.example.com
export KEY=fk_...

curl -H "Authorization: Bearer $KEY" $FLEET/v1/capabilities

# Submit a job
curl -X POST -H "Authorization: Bearer $KEY" $FLEET/v1/jobs -d '{
  "capability": "tts.say",
  "input": { "text": "Hello from the Mac mini" },
  "idempotency_key": "agent-run-42-step-3",
  "queue_timeout": "10m"
}'

# Follow it until it ends
curl -N -H "Authorization: Bearer $KEY" $FLEET/v1/jobs/JOB_ID/events

# Or wait up to 60 seconds for it to finish, then read its status
curl -H "Authorization: Bearer $KEY" "$FLEET/v1/jobs/JOB_ID?wait=60"

# Download a result, using the url from the job's artifacts
curl -H "Authorization: Bearer $KEY" -o audio.aiff $FLEET/v1/files/FILE_ID

# Upload an input file, then pass its id in the job's "files"
curl -X POST -H "Authorization: Bearer $KEY" --data-binary @clip.mov "$FLEET/v1/files?name=clip.mov"
```

| Route | Purpose |
|---|---|
| `GET /v1/capabilities` | Capabilities this key may use, with `input_files` listing the files each one needs |
| `POST /v1/jobs` | Create a job. Returns 202, or 200 when the idempotency key repeats an identical request |
| `GET /v1/jobs/{id}` | Job status. `?wait=N` waits up to 60 seconds for it to finish, and `?include=events` adds its event history |
| `GET /v1/jobs/{id}/events` | Follow a job as Server-Sent Events |
| `POST /v1/jobs/{id}/cancel` | Stop a job. Safe to repeat, and changes nothing if the job has finished |
| `POST /v1/files?name=NAME` | Upload an input file. The body is the raw file |
| `GET /v1/files/{id}` | Download a result or your own input file, with `Range` support. Files expire after 24 hours by default |
| `GET /healthz` | Liveness check, no key needed |

Keys can see only their own jobs and files. Errors have a stable `code`, a `message` and a `retryable` flag.

### Event stream

The stream sends these events:

| Event | Meaning |
|---|---|
| `status` | The job's state. Sent on connect and whenever it changes, including changes no worker reported, such as `lost` |
| `started`, `progress`, `completed`, `failed` | Reported by the worker, each with an `id` |
| `end` | The final job, including its artifacts. The stream then closes |

To resume after a disconnect, send the last `id` you saw as `Last-Event-ID`, or as `?after=ID`. You'll miss nothing and see nothing twice. Decide the outcome from `status` and `end`: a `failed` event can be followed by a rerun if the job has `retry_if_lost`.

A job that used an HTTP service may also have a `result` holding the service's answer, as described under [Add a service](#add-a-service). `fleet run` and `fleet status` print it with the rest of the job.

### Job states

| Status | Meaning | Final? |
|---|---|---|
| `queued` | Waiting for a node | No |
| `assigned` | Sent to a node, not yet started | No |
| `running` | The command or service is working, or its results are uploading | No |
| `lost` | The node went quiet; it may still return | No |
| `succeeded` | Finished, with results available | Yes |
| `failed` | Check `error.code` | Yes |
| `timed_out` | Passed its timeout | Yes |
| `cancelled` | Stopped by `POST /v1/jobs/{id}/cancel` | Yes |

A job is finished when `terminal` is `true`. If a job is rerun, `attempts` goes up and `progress` starts again.

### What happens when things fail

| Situation | Result |
|---|---|
| Network drops briefly | Nothing visible. The worker keeps running and resends its events on reconnect |
| Silent for longer than the lease (2 minutes) with `retry_if_lost: false` | `lost`. Resumes if the worker returns within 30 minutes, otherwise fails with `NODE_LOST` |
| Silent for longer than the lease with `retry_if_lost: true` | `queued`. If the worker returns first, its work continues. If another node starts first, the old attempt is stopped and its results are rejected |
| Worker crashes or restarts | The leftover command is killed, then the job reruns if `retry_if_lost` is set. Otherwise it fails with `WORKER_RESTARTED` |
| Gateway restarts | Jobs continue, and workers reconnect and catch up |
| No capable node in time | Fails with `NO_NODE_ONLINE`, or `QUEUE_TIMEOUT` if capable nodes were busy |
| Network drops during a file transfer | The worker retries for up to `upload_retry_window` (6 hours by default) and continues from where it stopped |
| Job cancelled while its worker is offline | It shows `cancelled` straight away, and the command is stopped when the worker reconnects |

Only set `retry_if_lost` for work that is safe to repeat.

HTTP services add these error codes:

| Code | Meaning | `retryable` |
|---|---|---|
| `SERVICE_ERROR` | The service refused the job or said it failed. The message has its reply | No |
| `SERVICE_UNAVAILABLE` | The service couldn't be reached, or answered 408, 429, 502, 503 or 504 | Yes |
| `BAD_RESPONSE` | The reply wasn't usable: not JSON, a missing field, or too large for a `result` | No |

`retryable` is advice for the agent. The gateway doesn't resubmit these by itself.

## Add a service

Add a capability to the machine's `worker.yaml` and restart the worker. The gateway needs no changes.

- **Placeholders:** `command` and `stdin` can use `{{workdir}}`, `{{input.field}}` and `{{files.field}}`. `command[0]` can't contain any.
- **Input files:** `input_files` lists the files a job must supply, and all are required. The worker downloads and verifies them first, then `{{files.field}}` gives the local path.
- **Option injection:** an argument starting with a placeholder is refused if its value begins with `-`. Prefer `stdin` for free text.
- **Outputs:** `outputs` maps result names to paths inside the working directory. Symlinks are refused.
- **Progress:** `progress.regex` reads command output using the named groups `percent`, `current`, `total` and `message`.
- **Capacity:** `requires` reserves units from `node.resources`, so heavy jobs don't overlap. `node.max_jobs` caps the total.
- **Environment:** commands get only `PATH`, `HOME`, `LANG`, `LC_ALL`, `TMPDIR` and whatever the capability's `env` sets.

Unknown fields, missing programs and invalid schemas stop the worker at startup. Check a config with `fleet-worker --config worker.yaml --check`.

### Call an HTTP service

For a service already running on the machine, use `adapter: http` instead of a command:

```yaml
- name: llm.generate
  adapter: http
  request:
    url: http://127.0.0.1:11434/api/generate
    json:
      model: llama3
      prompt: "{{input.prompt}}"
      temperature: "{{input.temperature}}" # Left out if the job doesn't send it.
      stream: false
  response:
    result: json
    result_field: response
  input_schema:
    type: object
    required: [prompt]
    properties:
      prompt: { type: string }
      temperature: { type: number }
  timeout: 10m
```

- **Where it can connect:** the scheme and host are fixed in the manifest, and must be this machine unless the capability sets `allow_remote: true`. Job input can only fill in the path, the query and the body. A redirect to a different host isn't followed.
- **URLs:** `{{input.field}}` in a URL is escaped for its position. A value used in the path must stay one segment, so one containing `/` or equal to `..` is refused.
- **Bodies:** set one of `json`, `body` or `body_file`.
  - In `json`, a string that's only a placeholder keeps the value's type, so input can't alter the document's structure, and `{{input}}` is the whole input. A key whose field wasn't sent is left out.
  - `body` is raw text, with no escaping.
  - `body_file: "{{files.field}}"` sends an input file as the whole body. Multipart uploads aren't supported.
  - `{{workdir}}` and `{{files.field}}` also work in `json` and `body`, for a local service that reads and writes files itself.
- **Headers:** values can use `{{env.NAME}}`, read from the worker's environment at startup, so secrets can stay out of the manifest. Job input can't reach a header.
- **The answer:** with no `response`, only success or failure is kept. There are two ways to keep the reply:
  - `result: json` or `result: text` puts it in the job's `result`, up to 1 MB. `result_field` picks out one field, using a path such as `choices.0.text`.
  - `file: out.wav` saves it. The same path must be listed in `outputs`, which is what delivers it.
- **Progress and timeouts:** `timeout` covers the whole call. A plain `http` capability reports no progress. Cancelling a job drops the connection, and the service decides what that means for its work.

A capability is advertised even while its service is down, so jobs sent to it fail with `SERVICE_UNAVAILABLE` rather than moving to another machine.

### Call a service with its own job queue

Use `adapter: http_async` for a service that answers with a job ID and works in the background:

```yaml
- name: image.generate
  adapter: http_async
  request:
    url: http://127.0.0.1:8188/jobs
    json: "{{input}}"
  async:
    job_id_field: id
    poll:
      url: http://127.0.0.1:8188/jobs/{{remote.id}}
      every: 2s
      status_field: state
      succeeded: [done]
      failed: [error]
      percent_field: fraction
      percent_scale: 100 # This service reports 0 to 1.
      message_field: stage
      error_field: detail
    fetch:
      url: http://127.0.0.1:8188/jobs/{{remote.id}}/image
    cancel:
      method: DELETE
      url: http://127.0.0.1:8188/jobs/{{remote.id}}
  response:
    file: out.png
  outputs:
    image: out.png
  input_schema: { type: object }
  timeout: 1h
```

- **Finishing:** a status that isn't listed, or isn't there yet, means keep waiting until `timeout`.
- **Collecting the result:** `response` applies to the `fetch` reply. With no `fetch`, it applies to the poll reply that reported success.
- **Outages:** while checking on a job, the worker waits out network errors and "try later" statuses for `give_up_after` (2 minutes by default). Any other error status, such as a 404 from a service that has forgotten the job, fails it straight away.
- **Stopping:** `cancel` is sent when the job is cancelled, times out or is abandoned, and when a restarted worker finds a job left over from a crash. It's a best effort, so without it, or if it fails, the service's job keeps running. It also won't be sent if the worker crashes in the moment between submitting a job and recording its ID.
- **Placeholders:** `poll`, `fetch` and `cancel` can use only `{{remote.id}}`, because after a crash the job's input is gone.

## Dashboard

The gateway serves a status page at `https://fleet.example.com/dashboard/`, showing machines, their load, and jobs with their progress and failures. It refreshes every 3 seconds while its tab is visible.

It needs its own kind of key. On the server:

```sh
sudo -u fleet fleet-gateway key add ops --dashboard --data /var/lib/fleet
```

A dashboard key can see the status of every job from every agent, but can't run or cancel anything, and can't read job input, `result` values or result files. Agent keys can't open the dashboard. The key is kept only in the browser tab and is forgotten when the tab closes.

**Error messages are visible to anyone with a dashboard key.** They can include what a command printed or a service replied, and that may quote part of a job's input.

## Publish a release

From a clean checkout, build the files and upload them. Nothing is published until the last command:

```sh
./deploy/test-install.sh
./deploy/build-release.sh v0.3.0
gh release create v0.3.0 --prerelease --target "$(git rev-parse HEAD)" --title v0.3.0 dist/release/*
```

The version is built into the binaries and into the download address in the install scripts, so each release's scripts fetch that release's files. Update the version in this README's install commands too.

## Layout

| Path | Contents |
|---|---|
| `cmd/` | The three programs: `fleet-gateway`, `fleet-worker` and `fleet` |
| `internal/gateway/lifecycle.go` | Every job state transition: leases, fencing, revival and retries |
| `internal/gateway/` | API, scheduler, worker connections and storage |
| `internal/gateway/dashboard.go`, `internal/gateway/web/` | Dashboard API, and its page, which is built into the binary |
| `internal/worker/` | Manifest, command execution, event spool and connection |
| `internal/worker/service.go`, `service_config.go` | The HTTP adapters |
| `internal/worker/proc_unix.go`, `proc_windows.go` | Stopping commands, per operating system |
| `internal/transfer/` | Resumable, verified file downloads, shared by the worker and CLI |
| `internal/client/` | Client for the agent API, including stream reconnects |
| `internal/protocol/` | Messages between gateway and worker |
| `e2e/` | Tests that run the real binaries, with a proxy that simulates network cuts |
| `deploy/` | Install scripts for Mac and Windows, plus example worker manifest, launchd, systemd and Caddy files |
