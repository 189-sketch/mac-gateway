# Fleet

Run jobs on idle home computers through one cloud API. An agent calls the gateway on your cloud server, the gateway hands the job to a machine that can run it, and the agent gets progress and result files back.

```text
Agent ──HTTPS──▶ Gateway (cloud server) ◀──outbound WSS + HTTPS── Worker (Mac mini) ──▶ local tools
```

Workers only make outbound connections, so your home network needs no open ports. Each worker can run only what its local manifest declares. The gateway sends a capability name and input, never a command.

This is step 1 of the design: the gateway, the worker and the CLI adapter. The tests run on Linux with a stand-in tool. Nothing has run on a real Mac yet, so the Mac steps below, including `say` and launchd, are untested.

**Not built yet:**
- SSE progress streaming
- A cancel endpoint
- Input file uploads
- A client CLI
- The HTTP adapter
- LLM streaming
- MCP
- A Windows worker
- A dashboard

## Build

This needs Go 1.27 or newer. The `make` targets are shortcuts, so if `make` isn't installed, run the matching commands from the `Makefile` directly.

```sh
make build   # Binaries for this machine, in bin/
make dist    # Gateway for Linux and workers for both Mac types, in dist/
make test    # All tests, including the end-to-end suite (a few minutes)
```

## Set up the gateway

1. Copy `dist/fleet-gateway-linux-amd64` to `/usr/local/bin/fleet-gateway` on the server.
2. Create the service user and install the service:

   ```sh
   sudo useradd --system --no-create-home fleet
   sudo cp deploy/fleet-gateway.service /etc/systemd/system/
   sudo systemctl enable --now fleet-gateway
   ```

3. Put a TLS proxy in front of it. `deploy/Caddyfile.example` is a complete Caddy config once you replace the domain.
4. Create the credentials. Each secret is printed once and only its hash is stored:

   ```sh
   sudo -u fleet fleet-gateway node add mac-mini-1 --data /var/lib/fleet
   sudo -u fleet fleet-gateway key add my-agent --capabilities 'tts.*' --data /var/lib/fleet
   ```

Use `node revoke <name>` or `key revoke <name>` to withdraw a credential. `--capabilities` takes comma-separated patterns, and `'*'` allows everything.

## Set up a Mac

1. Check the chip with `uname -m`: `arm64` needs `fleet-worker-darwin-arm64`, and `x86_64` needs `fleet-worker-darwin-amd64`.
2. Copy that binary to `~/fleet/fleet-worker`. If you downloaded it through a browser, run `xattr -d com.apple.quarantine ~/fleet/fleet-worker`.
3. Copy `deploy/worker.example.yaml` to `~/fleet/worker.yaml`, then set `gateway_url` and `node.name`. Copy `deploy/com.fleet.worker.plist` to `~/fleet/` as well.
4. Save the node token and check the config:

   ```sh
   cd ~/fleet
   (umask 077; pbpaste > token)   # With the token on the clipboard
   ./fleet-worker --config worker.yaml --check
   ```

5. Run it once in the foreground and submit a test job, as shown below:

   ```sh
   ./fleet-worker --config worker.yaml
   ```

6. Stop it, then install it to start automatically. Replace `YOUR_USER` in the plist first:

   ```sh
   cp com.fleet.worker.plist ~/Library/LaunchAgents/
   launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.fleet.worker.plist
   ```

7. Keep the machine available:

   ```sh
   sudo pmset -a sleep 0 disablesleep 1 autorestart 1
   ```

The plist is a LaunchAgent, so the worker runs inside a logged-in user session, which tools like `say` may need. For the worker to start after a power cut, turn on automatic login for that user.

Use a dedicated standard (non-admin) macOS account with none of your personal files. Commands run as that user, so they can read anything it can, including the token file.

## Call it

```sh
export FLEET=https://fleet.example.com
export KEY=fk_...

# See what's available, with input schemas and online status
curl -H "Authorization: Bearer $KEY" $FLEET/v1/capabilities

# Submit a job
curl -X POST -H "Authorization: Bearer $KEY" $FLEET/v1/jobs -d '{
  "capability": "tts.say",
  "input": { "text": "Hello from the Mac mini" },
  "idempotency_key": "agent-run-42-step-3",
  "queue_timeout": "10m"
}'

# Wait up to 60 seconds for it to finish, then read its status
curl -H "Authorization: Bearer $KEY" "$FLEET/v1/jobs/JOB_ID?wait=60"

# Download a result, using the url from the job's artifacts
curl -H "Authorization: Bearer $KEY" -o audio.aiff $FLEET/v1/files/FILE_ID
```

| Route | Purpose |
|---|---|
| `GET /v1/capabilities` | Capabilities this key may use |
| `POST /v1/jobs` | Create a job. Returns 202, or 200 when the idempotency key repeats an identical request |
| `GET /v1/jobs/{id}` | Job status. `?wait=N` waits up to 60 seconds for it to finish, and `?include=events` adds its event history |
| `GET /v1/files/{id}` | Download a result file. Files expire after 24 hours by default |
| `GET /healthz` | Liveness check, no key needed |

Keys can see only their own jobs and files. Errors have a stable `code`, a `message` and a `retryable` flag.

### Job states

| Status | Meaning | Final? |
|---|---|---|
| `queued` | Waiting for a node | No |
| `assigned` | Sent to a node, not yet started | No |
| `running` | The command is running or its results are uploading | No |
| `lost` | The node went quiet; it may still return | No |
| `succeeded` | Finished, with results available | Yes |
| `failed` | Check `error.code` | Yes |
| `timed_out` | Passed its timeout | Yes |

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

Only set `retry_if_lost` for work that is safe to repeat.

## Add a service

Add a capability to the machine's `worker.yaml` and restart the worker. The gateway needs no changes.

- **Placeholders:** `command` and `stdin` can use `{{workdir}}` and `{{input.field}}`. `command[0]` can't contain any.
- **Option injection:** an argument starting with a placeholder is refused if its value begins with `-`. Prefer `stdin` for free text.
- **Outputs:** `outputs` maps result names to paths inside the working directory. Symlinks are refused.
- **Progress:** `progress.regex` reads command output using the named groups `percent`, `current`, `total` and `message`.
- **Capacity:** `requires` reserves units from `node.resources`, so heavy jobs don't overlap. `node.max_jobs` caps the total.
- **Environment:** commands get only `PATH`, `HOME`, `LANG`, `LC_ALL`, `TMPDIR` and whatever the capability's `env` sets.

Unknown fields, missing programs and invalid schemas stop the worker at startup. Check a config with `fleet-worker --config worker.yaml --check`.

## Layout

| Path | Contents |
|---|---|
| `cmd/` | The two programs |
| `internal/gateway/lifecycle.go` | Every job state transition: leases, fencing, revival and retries |
| `internal/gateway/` | API, scheduler, worker connections and storage |
| `internal/worker/` | Manifest, command execution, event spool and connection |
| `internal/protocol/` | Messages between gateway and worker |
| `e2e/` | Tests that run the real binaries, with a proxy that simulates network cuts |
| `deploy/` | Example worker manifest, launchd, systemd and Caddy files |
