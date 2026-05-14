# SKY-254 runsc-on-Fly validation

Validation probe that proved gVisor `runsc` works on Fly.io for per-delegation
sandboxing of TF agent runs. Run 2026-05-14 against an empty Fly App
(`tf-runsc-probe-39b851`, now destroyed).

This directory is **architectural reference, not product code**. It exists so
the eventual D10 implementation has the working recipe in hand and so future
"why did we pick Fly?" questions don't require re-running the experiment.

The pivot context: the original SKY-242 plan targeted Railway for the hosted
deployment. Railway turned out to be a dead end for gVisor — its container
runtime applies seccomp + AppArmor profiles that block the `/proc/self/exe`
re-exec at the heart of `runsc --rootless`. Fly Machines are themselves
Firecracker microVMs with no such restrictions, so runsc runs inside them
with the standard CNI-style setup.

## What's here

| File | What it does |
| --- | --- |
| `Dockerfile` | Image that bundles runsc + the network-plumbing deps (`iptables`, `iproute2`, `procps`). What TF's production image will look like. |
| `fly.toml` | Fly App config for the probe. Mirrors what `D13` (containerization) will produce. |
| `probe.sh` | Sets up the test environment: parent-side state (env vars, fake services, secret file), the OCI bundle (alpine rootfs + `runsc spec`-generated config), then invokes `runsc run`. This is the "naive" path — the `runsc run` step fails for networking. |
| `precns-test.sh` | The **production-mode invocation**. Pre-creates the netns + veth + iptables, patches the OCI bundle to point at the netns, then invokes `runsc run`. **This is the recipe D10 implements in Go.** |

## To reproduce

```bash
fly apps create tf-runsc-probe-XXX --org personal
cd docs/specs/sky-254-runsc-validation
# edit fly.toml: change `app = "tf-runsc-probe-39b851"` to your app name
fly deploy --remote-only --ha=false
# Once the Machine is up:
fly ssh sftp shell <<< "put precns-test.sh /tmp/precns-test.sh"
fly ssh console --command "sh /tmp/precns-test.sh"
# Expect: sandbox sees only /work + alpine rootfs; parent localhost services
# blocked; outbound HTTPS to api.github.com works.
# Cleanup:
fly apps destroy tf-runsc-probe-XXX --yes
```

## What was learned

1. **`runsc do --network=sandbox`** (the convenience mode) works on Fly but
   inherits the host filesystem — not production-safe.
2. **`runsc run` against an OCI bundle** is the production mode. The bundle
   ships its own rootfs (alpine minirootfs, ~5MB) and bind-mounts only the
   per-run worktree at `/work`. Everything else of the host is invisible.
3. **`runsc run` does NOT auto-set-up the netns/veth** the way `runsc do`
   does. The caller owns network plumbing — this is the CNI pattern that
   containerd/Docker implement around runc/runsc.
4. **`--ignore-cgroups`** is required on Fly because Fly Machines use cgroup
   v2 unified hierarchy and the bundled runsc release defaults to v1 paths.
5. **DNS** in the sandbox needs explicit public resolvers (1.1.1.1 / 8.8.8.8).
   Fly's internal `fdaa::3` resolver isn't reachable through the sandbox's
   IPv4 NAT.
6. **Image dependencies**: `runsc` binary + `iptables` + `iproute2` + `procps`.
   The TF image bundles all four; customer's host needs no separate install.

## Acceptance criteria the probe confirmed

| Property | Method | Result |
| --- | --- | --- |
| Filesystem isolation | Sandbox attempts `cat /tmp/parent-secret.txt` | `No such file or directory` |
| Bind-mount works | Sandbox reads `/work/agent-input.md` | `worktree-content-from-host` |
| Env curation | Spec's `process.env` has only `AGENT_CURATED_KEY` | Confirmed via `env` inside sandbox |
| Loopback isolation | Sandbox tries `wget http://127.0.0.1:5432` (parent's fake-pg) | `Connection refused` (sandbox loopback ≠ host) |
| Outbound HTTPS by IP | Sandbox `wget https://1.1.1.1` | Cloudflare HTML returned |
| Outbound HTTPS + DNS | Sandbox `wget https://api.github.com/zen` | "Non-blocking is better than blocking." |
| Sandbox kernel | `uname -a` inside | `Linux tf-sandbox 4.19.0-gvisor` (gVisor's user-mode kernel) |

See `docs/multi-tenant-architecture.html` §6 for the architectural framing.
