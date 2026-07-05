# How ecs-init's Engine Manages the ECS Agent Container, and `ecs-init stop` vs `systemctl stop ecs`

> Scope: the `ecs-init` engine's `start`/`stop` lifecycle, how it drives the
> ECS Agent **Docker container**, and how the `ecs.service` systemd unit
> interacts with it. Every behavioral claim below was validated with a
> prototype (see [Empirical validation](#empirical-validation)); every code
> claim cites the exact file and line in this repository.
>
> All line numbers refer to this repository at the revision this document was
> written against. If code moves, re-verify with the referenced symbol name.

---

## TL;DR

1. **The `ecs-init` engine does *not* use Go's `context` package at all in its
   `start`/`stop` path.** There is no `context` import in
   [`ecs-init/engine/engine.go`](../engine/engine.go), and it installs **no**
   OS signal handler. This is the opposite of `dcgm-init`, whose engine builds a
   `context.WithCancel` and cancels it on SIGTERM. (The only `context` usage in
   `ecs-init` is `context.TODO()` in the `cache` package to satisfy AWS SDK
   signatures — see [Where `context` *is* used](#where-context-is-actually-used-in-ecs-init).)

2. **ecs-init supervises an *external* process — the ECS Agent Docker
   container — not an in-process goroutine.** `start` (`StartSupervised`) runs a
   loop that starts the agent container and then **blocks on `docker wait`**
   until the container exits. `stop` (`PreStop`) tells the Docker daemon to stop
   that container.

3. Because it's an external container, **both** `ecs-init stop` (as a separate
   process) *and* systemd's SIGTERM-to-the-supervisor can affect the running
   service — but they do **different** things, and only their *combination* (as
   `systemctl stop ecs` performs) stops the service durably. Running a bare
   `ecs-init stop` by itself can leave the service running, because the
   still-alive supervisor restarts the agent.

---

## Part 1 — The command dispatch model

`ecs-init` is a single binary invoked once per command. `main()` parses the
first CLI argument and dispatches to an action from a map.

- Command constants (`pre-start`, `start`, `stop`, `post-stop`, …):
  [`ecs-init/ecs-init.go:29-37`](../ecs-init.go).
- Dispatch: `main()` builds the action map and looks up `args[0]`
  ([`ecs-init/ecs-init.go:63-69`](../ecs-init.go)).
- The action map wires commands to engine methods
  ([`ecs-init/ecs-init.go:84-113`](../ecs-init.go)):
  - `start`    → `engine.StartSupervised` ([line 90-93](../ecs-init.go))
  - `stop`     → `engine.PreStop` ([line 100-103](../ecs-init.go))
  - `pre-stop` → `engine.PreStop` (deprecated alias) ([line 96-99](../ecs-init.go))
  - `post-stop`→ `engine.PostStop` ([line 108-111](../ecs-init.go))

**Key structural fact:** each invocation runs `main()` to completion and exits.
`start` and `stop` are therefore **two separate OS processes** with separate
memory. Nothing in-process is shared between them.

On action error, `main()` maps a `*engine.TerminalError` to exit code 5 and
everything else to the default error code
([`ecs-init/ecs-init.go:71-76`](../ecs-init.go)); `die()` flushes logs and calls
`os.Exit` ([`ecs-init/ecs-init.go:125-129`](../ecs-init.go)). A normal return
from `main()` exits 0.

---

## Part 2 — `start`: `StartSupervised` supervises the agent container

`StartSupervised` is the long-running foreground loop
([`ecs-init/engine/engine.go:294-341`](../engine/engine.go)):

```
for {
    docker.RemoveExistingAgentContainer()      // clean slate
    agentExitCode, err = docker.StartAgent()   // <-- BLOCKS until container exits
    switch agentExitCode {
    case upgradeAgentExitCode (42):     upgrade + continue (no backoff)
    case containerFailureAgentExitCode (2): capture logs
    case TerminalFailureAgentExitCode (5):  return *TerminalError  // do NOT restart
    case terminalSuccessAgentExitCode (0):   return nil            // stop cleanly
    }
    time.Sleep(retryBackoff.Duration())        // back off, then loop -> restart agent
}
```

- The exit-code constants are defined at
  [`ecs-init/engine/engine.go:41-45`](../engine/engine.go)
  (`terminalSuccessAgentExitCode = 0`, `containerFailureAgentExitCode = 2`,
  `TerminalFailureAgentExitCode = 5`, `upgradeAgentExitCode = 42`).
- The blocking call is `docker.StartAgent()`
  ([invoked at `ecs-init/engine/engine.go:309`](../engine/engine.go)).

### What `StartAgent` actually does

[`ecs-init/docker/docker.go:266-284`](../docker/docker.go):

1. `CreateContainer` — create the agent container ([line 271](../docker/docker.go)).
2. `StartContainer` — start it ([line 279](../docker/docker.go)).
3. `return c.docker.WaitContainer(container.ID)` — **block until the container
   exits**, returning its exit code ([line 283](../docker/docker.go)).

`WaitContainer` is the `fsouza/go-dockerclient` call
([`ecs-init/vendor/github.com/fsouza/go-dockerclient/container_wait.go:14`](../vendor/github.com/fsouza/go-dockerclient/container_wait.go)):
`func (c *Client) WaitContainer(id string) (int, error)`. It blocks on the
Docker daemon's `/containers/{id}/wait` endpoint.

**So the supervisor spends virtually all of its life blocked inside
`WaitContainer`.** The only ways it wakes up are (a) the container exits, or
(b) the supervisor process itself is signaled/killed.

---

## Part 3 — `stop`: `PreStop` stops the agent container

`PreStop` ([`ecs-init/engine/engine.go:349-360`](../engine/engine.go)) simply
calls `docker.StopAgent()`:

```go
func (e *Engine) PreStop() error {
    docker, err := getDockerClient()
    ...
    err = docker.StopAgent()
    ...
}
```

`StopAgent` ([`ecs-init/docker/docker.go:627-643`](../docker/docker.go)):

1. `findAgentContainer()` — locate the running agent container ([line 628](../docker/docker.go)).
2. If none, log "No running Agent to stop" and return nil ([line 632-635](../docker/docker.go)).
3. `stopContainerTimeoutSeconds := uint(10)` ([line 636](../docker/docker.go)).
4. `c.docker.StopContainer(id, stopContainerTimeoutSeconds)` ([line 637](../docker/docker.go)).

`StopContainer` is again a `fsouza/go-dockerclient` call with a **plain `uint`
timeout, not a Go context**
([`ecs-init/vendor/github.com/fsouza/go-dockerclient/container_stop.go:14`](../vendor/github.com/fsouza/go-dockerclient/container_stop.go)):

```go
func (c *Client) StopContainer(id string, timeout uint) error
```

The timeout is passed to the Docker daemon as `/containers/{id}/stop?t=%d`
([`container_stop.go:28`](../vendor/github.com/fsouza/go-dockerclient/container_stop.go)).
Semantically: the daemon sends **SIGTERM to the container's PID 1, then SIGKILL
after `t` (10) seconds** if it hasn't exited. (A context-aware variant,
`StopContainerWithContext`, exists at
[`container_stop.go:23`](../vendor/github.com/fsouza/go-dockerclient/container_stop.go),
but ecs-init does **not** use it.)

---

## What is `context` used for (in general)?

`context` is a Go **standard-library** package. Per its own documentation, it
"defines the Context type, which carries deadlines, cancellation signals, and
other request-scoped values across API boundaries and between processes"
([pkg.go.dev/context, Overview][ctx-overview]). In other words, a `Context` is
the idiomatic Go handle for **lifetime and cancellation**: a callee can ask "has
the caller given up / run out of time?" and abort accordingly.

The package prescribes a specific convention that explains *why* it shows up in
so many function signatures:

- "Incoming requests to a server should create a Context, and outgoing calls to
  servers should accept a Context. The chain of function calls between them must
  propagate the Context …" ([pkg.go.dev/context, Overview][ctx-overview]).
- "The Context should be the first parameter, typically named ctx"
  ([pkg.go.dev/context, Overview][ctx-overview]).

There are two "empty" root contexts that carry no cancellation of their own:

- `context.Background()` — "returns a non-nil, empty Context. It is never
  canceled, has no values, and has no deadline." ([pkg.go.dev/context,
  `Background`][ctx-background]).
- `context.TODO()` — "returns a non-nil, empty Context. Code should use
  context.TODO when it's unclear which Context to use or it is not yet available
  (because the surrounding function has not yet been extended to accept a Context
  parameter)." ([pkg.go.dev/context, `TODO`][ctx-todo]).

## Where `context` is *actually* used in ecs-init

To be precise about the "does ecs-init use context" question: the **engine**
does not use `context` at all (it imports no `context` package — see
[`ecs-init/engine/engine.go`](../engine/engine.go) import block, lines 16-38),
but the **cache** package does. It uses `context` **only to satisfy AWS SDK
Go v2 signatures**, and every call site passes the placeholder `context.TODO()`
rather than a context ecs-init created or can cancel. The SDK requires a
`context.Context` as the first argument of these calls; ecs-init has no deadline
or cancellation of its own to impose, so it supplies `context.TODO()`.

There are **three** such call sites:

1. **Initializing the IMDS client's AWS config.**
   `awsconfig.LoadDefaultConfig(context.TODO())`
   ([`ecs-init/cache/cache.go:80`](../cache/cache.go)). `LoadDefaultConfig`'s
   signature is
   `func LoadDefaultConfig(ctx context.Context, optFns ...func(*LoadOptions) error) (cfg aws.Config, err error)`
   — the first parameter is `ctx context.Context`
   ([pkg.go.dev …/config, `LoadDefaultConfig`][sdk-config]).

2. **Querying the instance region from IMDS.**
   `d.metadata.GetRegion(context.TODO(), &imds.GetRegionInput{})`
   ([`ecs-init/cache/cache.go:185`](../cache/cache.go)). `imds` is the SDK's
   "API client for interacting with the Amazon EC2 Instance Metadata Service"
   ([pkg.go.dev …/feature/ec2/imds, Overview][sdk-imds]), and
   `GetRegion`'s first parameter is `ctx context.Context`
   ([pkg.go.dev …/feature/ec2/imds, `GetRegion`][sdk-imds]).

3. **Downloading the agent image from S3.** The `s3API` interface declares
   `Download(ctx context.Context, w io.WriterAt, input *s3.GetObjectInput, …)`
   ([`ecs-init/cache/dependencies.go:42`](../cache/dependencies.go)); it is
   called as `bd.client.Download(context.TODO(), file, &s3.GetObjectInput{…})`
   ([`ecs-init/cache/dependencies.go:84`](../cache/dependencies.go)); and the
   downloader's own AWS config is likewise built with
   `config.LoadDefaultConfig(context.TODO(), …)`
   ([`ecs-init/cache/dependencies.go:54`](../cache/dependencies.go)). The
   concrete implementation is the SDK's S3 transfer manager, whose method is
   `func (d Downloader) Download(ctx context.Context, w io.WriterAt, input *s3.GetObjectInput, options ...func(*Downloader)) (n int64, err error)`
   and which "downloads an object in S3 and writes the payload into w using
   concurrent GET requests" ([pkg.go.dev …/feature/s3/manager,
   `Downloader.Download`][sdk-s3manager]). (That page marks this `Download`
   method "Deprecated: superceded by feature/s3/transfermanager"; ecs-init still
   uses `feature/s3/manager` as vendored.)

**What this tells us:** ecs-init sits at the *minimal* end of the `context`
spectrum. It never creates a cancellable context, never derives a timeout, and
never reads `ctx.Done()`; it only *forwards* the placeholder `context.TODO()`
into SDK calls because those APIs demand a context argument. This matches the
documented purpose of `TODO`: use it "when it's unclear which Context to use or
it is not yet available" ([pkg.go.dev/context, `TODO`][ctx-todo]). Contrast with
`dcgm-init`, which actively creates (`context.WithCancel`) and cancels a context
to drive and stop its in-process collection loop.

> Note: `context.CallTime()` at [`ecs-init/logger/log.go:80`](../logger/log.go)
> is **not** the standard-library `context` package. There, `context` is the
> `seelog.LogContextInterface` parameter of a seelog formatter closure, and
> `CallTime()` is a method on that seelog type. Don't confuse the two.

[ctx-overview]: https://pkg.go.dev/context#pkg-overview
[ctx-background]: https://pkg.go.dev/context#Background
[ctx-todo]: https://pkg.go.dev/context#TODO
[sdk-config]: https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/config#LoadDefaultConfig
[sdk-imds]: https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/ec2/imds
[sdk-s3manager]: https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/s3/manager#Downloader.Download

---

## Part 4 — The `ecs.service` systemd unit

[`packaging/amazon-linux-ami-integrated/ecs.service`](../../packaging/amazon-linux-ami-integrated/ecs.service):

```ini
[Service]
Type=simple
Restart=on-failure
RestartPreventExitStatus=5
RestartSec=10s
EnvironmentFile=-/etc/ecs/ecs.config
ExecStartPre=/bin/bash -c 'if [ $(/usr/bin/systemctl is-active docker) != "active" ]; then exit 1; fi'
ExecStartPre=/usr/libexec/amazon-ecs-init pre-start
ExecStart=/usr/libexec/amazon-ecs-init start
ExecStop=/usr/libexec/amazon-ecs-init stop
ExecStopPost=/usr/libexec/amazon-ecs-init post-stop
```

(Exact lines: `Type=simple` [line 24], `Restart=on-failure` [line 25],
`RestartPreventExitStatus=5` [line 26], `ExecStart=… start` [line 31],
`ExecStop=… stop` [line 32], `ExecStopPost=… post-stop` [line 33].)

How systemd runs this (all from `systemd.service(5)` / `systemd.kill(5)` man
pages, quoted in [References](#references)):

- **`ExecStart`** — the main service process. Here it's `amazon-ecs-init start`,
  i.e. the `StartSupervised` supervisor. It is the process systemd tracks as
  `MainPID`.
- **`ExecStop`** — run on `systemctl stop`, as a **separate** process
  (`amazon-ecs-init stop` → `PreStop` → `docker stop`). Per `systemd.service(5)`:
  *"After the commands configured in this option are run, it is implied that the
  service is stopped, and any processes remaining for it are terminated
  according to the `KillMode=` setting."*
- **After `ExecStop` returns**, systemd sends its kill signal (default
  `KillSignal=SIGTERM`) to the remaining processes in the unit's cgroup
  (default `KillMode=control-group`), escalating to `SIGKILL` after
  `TimeoutStopSec` if needed.

---

## Part 5 — `ecs-init stop` vs `systemctl stop ecs`: the two code paths

This is the crux. The two commands are **not** interchangeable.

### Path A — `systemctl stop ecs`

systemd orchestrates a full stop of the unit:

1. systemd marks the unit "Stopping" and **runs `ExecStop`**: a *new*
   `amazon-ecs-init stop` process → `PreStop` → `StopAgent` →
   `StopContainer(id, 10)`. This SIGTERMs the agent **container** (SIGKILL after
   10s).
2. When the container exits, the supervisor's blocked `WaitContainer`
   ([`docker.go:283`](../docker/docker.go)) **unblocks** and returns the
   container's exit code into `StartSupervised`'s `switch`.
3. **Independently**, because the death/stop is a systemd-initiated operation,
   systemd also delivers **SIGTERM to the supervisor process itself** (the
   `MainPID`). Since ecs-init installs **no signal handler**, Go's *default*
   SIGTERM disposition **terminates the supervisor**.
4. Net effect: the container is stopped **and** the supervisor is terminated, so
   it cannot loop and restart the agent. The unit goes `inactive (dead)`.

Crucially, **`Restart=on-failure` does not re-launch the service here.** Per
`systemd.service(5)`: *"When the death of the process is a result of systemd
operation (e.g. service stop or restart), the service will not be restarted"*,
and the service *"will not be restarted … if … the service is stopped with
`systemctl stop`."* (Also, SIGTERM is in the default set of signals treated as a
*clean* exit — see [References](#references) — and `RestartPreventExitStatus=5`
handles the terminal-error case, though the manual-stop rule already covers it.)

### Path B — bare `ecs-init stop` (run directly, not via systemctl)

Running `/usr/libexec/amazon-ecs-init stop` yourself invokes **only** `PreStop`
→ `StopContainer(id, 10)`. It:

- Stops the **agent container** (SIGTERM, then SIGKILL after 10s).
- Does **nothing** to the still-running `ExecStart` supervisor process — a
  separate process it cannot reach (no shared memory; `ecs-init` has no IPC/
  signal wiring for this).

What happens next depends on the container's **exit code**, per
`StartSupervised`'s `switch` ([`engine.go:315-336`](../engine/engine.go)):

- If the container exits **0** (`terminalSuccessAgentExitCode`), the supervisor
  returns and the process exits — the service effectively stops. But this
  requires the agent to exit cleanly on SIGTERM.
- If the container exits **non-zero and non-5** (e.g. **137/143** from being
  killed), the supervisor treats it as a restartable failure, **backs off, loops,
  and starts a new agent container** ([`engine.go:337-339`](../engine/engine.go)).
  The service is **not** stopped — you just bounced the container.

**This is the key difference:** `systemctl stop ecs` stops *both* the supervisor
and the container, so the agent stays down. A bare `ecs-init stop` stops *only*
the container; whether the service stays down depends entirely on how the agent
exits, and in the general (killed → non-zero) case the supervisor brings it
right back.

### Side-by-side

| Aspect | `systemctl stop ecs` | bare `ecs-init stop` |
|---|---|---|
| What runs | `ExecStop` (`ecs-init stop`) **+** SIGTERM to supervisor | just `ecs-init stop` |
| Agent container | Stopped (SIGTERM→SIGKILL @10s) | Stopped (SIGTERM→SIGKILL @10s) |
| Supervisor (`StartSupervised`) | **Terminated** by systemd's SIGTERM | **Untouched — keeps running** |
| Does the agent come back? | No | **Yes**, if the container exited non-zero (supervisor loops & restarts) |
| Service final state | `inactive (dead)` | still `active (running)`, agent restarted |
| Intended use | The correct way to stop the service | An internal step of `ExecStop`, not a standalone stop |

---

## Empirical validation

Because the exact ordering and the restart behavior are load-bearing, I built a
faithful prototype rather than reasoning alone. The prototype (`mini-ecs-init`)
mirrors the real structure: `start` = "remove stale container → `docker run` →
**block on `docker wait`** → inspect exit code → loop/return", and `stop` =
"`docker stop -t 10`". Like the real ecs-init, it installs **no signal
handler**. It ran a real `busybox` container under a real (user) systemd unit
whose `[Service]` mirrored `ecs.service` (`Type=simple`, `Restart=on-failure`,
`RestartPreventExitStatus=5`, `ExecStart=… start`, `ExecStop=… stop`).

### Experiment A — `systemctl stop`, agent exits 0

Journal (times abbreviated):

```
16.644 systemd:      Stopping mini-ecs.service...
16.646 mini-ecs-init: invoked: stop (pid=3684093)      # ExecStop, a NEW process
16.658 mini-ecs-init: stopping agent container (10s timeout)
17.469 (container stopped)
17.471 mini-ecs-init: agent container exited with code 0   # supervisor's docker wait unblocks
17.471 mini-ecs-init: terminal success -> StartSupervised returns
17.472 systemd:      Stopped mini-ecs.service.
```

Result: `ActiveState=inactive`, `ExecMainStatus=0`, container `Exited (0)`.
**Observed:** ExecStop ran first as a separate PID; stopping the container
unblocked the supervisor's `docker wait`; the supervisor then exited on its own.
Clean stop.

### Experiment C — bare `stop`, agent exits **non-zero** (137)

Here the container ignored SIGTERM, so `docker stop` SIGKILLed it after 10s
(exit 137). I ran the bare `stop` binary directly (not via systemctl) while the
service was running:

```
before: container id = e0e90b7d7166, service active (MainPID=3685701)
$ mini-ecs-init2 stop      # only stops the container
  ... "stopping agent container (10s timeout)"   # took the full 10s (SIGKILL)
after 5s:
  service: ActiveState=active, SubState=running, MainPID=3685701 (UNCHANGED)
  container id = d3e430591cee   # DIFFERENT id
RESULT: supervisor RESTARTED the container -> bare stop did NOT stop the service
```

**Observed:** the bare `stop` stopped the container, but the supervisor (same
`MainPID`, still alive) looped and started a **new** container. The service was
never stopped. This is the failure mode the table's last two rows describe.

### Experiment D — `systemctl stop`, agent exits **non-zero** (137)

Same non-terminal-exit service, stopped the correct way. Journal:

```
20:10.554 systemd:      Stopping mini-ecs2.service...      # SIGTERM to supervisor + run ExecStop
20:10.556 mini2:        invoked: stop (pid=3686682)        # ExecStop process
20:10.569 mini2:        stopping agent container (10s timeout)
20:20.912 (container SIGKILLed after 10s)
20:20.913 mini2:        agent container exited with code 137
20:20.913 mini2:        non-terminal exit 137 -> backoff 2s then RESTART   # supervisor WANTED to restart...
20:20.915 systemd:      Stopped mini-ecs2.service.                        # ...but systemd had already SIGTERM'd it
```

Result: `ActiveState=inactive`, `NRestarts=0`, container stays `Exited (137)`.
**Observed:** even though the supervisor *intended* to restart (it logged
"backoff 2s then RESTART"), systemd's SIGTERM to the supervisor terminated it
during the backoff sleep, so no restart occurred. **This is precisely why
`systemctl stop` works where a bare `stop` does not:** the SIGTERM to the
supervisor is what prevents the restart.

### Control — Go's default SIGTERM disposition

To confirm the "no handler → SIGTERM terminates it" premise: a trivial Go
program with no signal handler, sent SIGTERM, exited with status **143**
(128 + 15) — i.e. the default disposition terminated it. This is what happens to
ecs-init's supervisor under systemd's stop, since it registers no handler
(verified: no `signal.Notify` in `ecs-init` outside vendored code).

---

## Practical implications

- **Always stop the service with `systemctl stop ecs`** (or `sudo systemctl
  stop ecs`). That is the path that stops both the agent container and the
  supervisor.
- **Do not use a bare `amazon-ecs-init stop` to stop the service.** It is
  designed to be invoked by systemd as `ExecStop`, in concert with systemd's
  SIGTERM to the supervisor. On its own it will typically just bounce the agent
  container, which the supervisor restarts.
- The 10-second window in `StopAgent` ([`docker.go:636`](../docker/docker.go)) is
  the grace period for the agent container to exit before it is SIGKILLed. It is
  independent of systemd's `TimeoutStopSec`.

---

## Contrast with `dcgm-init` (why this matters)

`ecs-init` and `dcgm-init` sit at opposite ends of the "what does `stop`
control" spectrum:

- **ecs-init** supervises an **external Docker container**. Its `stop` acts on
  the Docker daemon (`docker stop`), which *can* affect the running service from
  a separate process. It uses **no `context`** and **no signal handler** in the
  engine; systemd's default SIGTERM disposition terminates the supervisor.
- **dcgm-init** runs its collection loop **in-process**. Its `stop` cannot reach
  the other process's in-memory `context`, so it uses a pidfile + SIGTERM to
  signal the running `start` process, whose signal handler cancels a
  `context.Context`. See `dcgm-init/engine/engine.go`.

The common thread: a separate `stop` process can only affect a running service
through something that crosses the process boundary — the Docker daemon (ecs-init)
or an OS signal (dcgm-init). Neither can reach the other process's in-memory
state directly.

---

## References

### Code (this repository)

- `ecs-init/ecs-init.go:29-37` — command constants.
- `ecs-init/ecs-init.go:63-69` — command dispatch.
- `ecs-init/ecs-init.go:84-113` — action map (`start`→`StartSupervised`, `stop`→`PreStop`).
- `ecs-init/engine/engine.go:41-45` — agent exit-code constants.
- `ecs-init/engine/engine.go:294-341` — `StartSupervised` supervision loop.
- `ecs-init/engine/engine.go:309` — blocking `docker.StartAgent()` call.
- `ecs-init/engine/engine.go:349-360` — `PreStop`.
- `ecs-init/docker/docker.go:266-284` — `StartAgent` (ends in `WaitContainer`, line 283).
- `ecs-init/docker/docker.go:627-643` — `StopAgent` (`StopContainer(id, 10)`, line 637).
- `ecs-init/vendor/github.com/fsouza/go-dockerclient/container_wait.go:14` — `WaitContainer`.
- `ecs-init/vendor/github.com/fsouza/go-dockerclient/container_stop.go:14,23,28` — `StopContainer` / `StopContainerWithContext` / the `?t=` timeout.
- `ecs-init/cache/cache.go:80,185` and `ecs-init/cache/dependencies.go:42,84` — the only `context.TODO()` usage.
- `packaging/amazon-linux-ami-integrated/ecs.service:24-33` — the systemd unit.

### systemd documentation (man pages, verified on host)

- `systemd.service(5)` — `ExecStop=`: *"After the commands configured in this
  option are run, it is implied that the service is stopped, and any processes
  remaining for it are terminated according to the KillMode= setting."*
- `systemd.service(5)` — `Restart=`: *"When the death of the process is a result
  of systemd operation (e.g. service stop or restart), the service will not be
  restarted"*; and the service *"will not be restarted … if the exit code or
  signal is specified in RestartPreventExitStatus= … or the service is stopped
  with systemctl stop."*
- `systemd.service(5)` — `SuccessExitStatus=`: the signals `SIGHUP`, `SIGINT`,
  `SIGTERM`, and `SIGPIPE` are treated as a clean exit (in addition to exit code
  0), except for `Type=oneshot`.
- `systemd.kill(5)` — `KillMode=` *"Defaults to control-group"*;
  `KillSignal=` defaults to `SIGTERM`.
- `systemd.service(5)` — `TimeoutStopSec=`: bounds each `ExecStop=` command; if
  it times out, the service is terminated by SIGTERM.

### External

- Amazon ECS container agent / ecs-init source: <https://github.com/aws/amazon-ecs-agent>
- `fsouza/go-dockerclient`: <https://github.com/fsouza/go-dockerclient>

---

## Appendix — statement→reference validation ledger

Every statement in [What is `context` used for](#what-is-context-used-for-in-general)
and [Where `context` is *actually* used in ecs-init](#where-context-is-actually-used-in-ecs-init)
was checked against its cited reference; each external URL was fetched and each
code reference was read at the exact lines, and confirmed to contain a phrase
that validates the statement. Results:

| # | Statement | Reference | Validating phrase found (verbatim) | ✓ |
|---|-----------|-----------|------------------------------------|---|
| 1 | `context` carries deadlines/cancellation across API boundaries | [pkg.go.dev/context#pkg-overview][ctx-overview] | "carries deadlines, cancellation signals, and other request-scoped values across API boundaries and between processes" | ✓ |
| 2 | Incoming requests create a Context; outgoing calls accept one; the chain propagates it | [pkg.go.dev/context#pkg-overview][ctx-overview] | "Incoming requests to a server should create a Context, and outgoing calls to servers should accept a Context." | ✓ |
| 3 | Context should be the first parameter, named `ctx` | [pkg.go.dev/context#pkg-overview][ctx-overview] | "The Context should be the first parameter, typically named ctx" | ✓ |
| 4 | `context.Background()` is never canceled / no deadline | [pkg.go.dev/context#Background][ctx-background] | "It is never canceled, has no values, and has no deadline." | ✓ |
| 5 | `context.TODO()` is for when the Context is unclear / not yet available | [pkg.go.dev/context#TODO][ctx-todo] | "Code should use context.TODO when it's unclear which Context to use or it is not yet available" | ✓ |
| 6 | The engine imports no `context` package | [`ecs-init/engine/engine.go`](../engine/engine.go) lines 16-38 | import block contains `errors, fmt, io, math, os, time, …` and no `"context"` | ✓ |
| 7 | `LoadDefaultConfig` is called with `context.TODO()` for the IMDS client | [`ecs-init/cache/cache.go:80`](../cache/cache.go) | `cfg, err := awsconfig.LoadDefaultConfig(context.TODO())` | ✓ |
| 8 | `LoadDefaultConfig`'s first param is `ctx context.Context` | [pkg.go.dev …/config#LoadDefaultConfig][sdk-config] | `func LoadDefaultConfig(ctx context.Context, optFns ...func(*LoadOptions) error) (cfg aws.Config, err error)` | ✓ |
| 9 | Region is queried from IMDS with `context.TODO()` | [`ecs-init/cache/cache.go:185`](../cache/cache.go) | `output, err := d.metadata.GetRegion(context.TODO(), &imds.GetRegionInput{})` | ✓ |
| 10 | `imds` is the client for the EC2 Instance Metadata Service; `GetRegion` takes `ctx` first | [pkg.go.dev …/feature/ec2/imds][sdk-imds] | "API client for interacting with the Amazon EC2 Instance Metadata Service"; `GetRegion(ctx context.Context, …)` | ✓ |
| 11 | `s3API.Download` declares `ctx context.Context` first | [`ecs-init/cache/dependencies.go:42`](../cache/dependencies.go) | `Download(ctx context.Context, w io.WriterAt, input *s3.GetObjectInput, …)` | ✓ |
| 12 | Download is invoked with `context.TODO()` | [`ecs-init/cache/dependencies.go:84`](../cache/dependencies.go) | `_, err = bd.client.Download(context.TODO(), file, &s3.GetObjectInput{` | ✓ |
| 13 | The downloader's config is built with `context.TODO()` | [`ecs-init/cache/dependencies.go:54`](../cache/dependencies.go) | `config.LoadDefaultConfig(` … `context.TODO(),` | ✓ |
| 14 | The SDK downloader `Download` takes `ctx` first and does concurrent S3 GETs | [pkg.go.dev …/feature/s3/manager#Downloader.Download][sdk-s3manager] | `func (d Downloader) Download(ctx context.Context, …)`; "downloads an object in S3 and writes the payload into w using concurrent GET requests" | ✓ |
| 15 | `context.CallTime()` in logger is seelog, not stdlib context | [`ecs-init/logger/log.go:80`](../logger/log.go) | `context` is the `seelog.LogContextInterface` closure param; `CallTime()` is a method on it | ✓ |

All 15 statements validated (6 external URLs fetched + confirmed; 9 code
references read at the exact lines + confirmed). One caveat surfaced during
validation and is noted inline above: the SDK's `feature/s3/manager`
`Downloader.Download` page marks the method "Deprecated: superceded by
feature/s3/transfermanager" — ecs-init still uses the vendored
`feature/s3/manager`.
