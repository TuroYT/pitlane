# Troubleshooting

**Jobs stay queued.**
- Check the label: `runs-on: pitlane` must match the label shown in **Repositories**.
- Check the target status in **Repositories**. `error` shows the GitHub error, often that the App is not installed on that repository.
- All slots busy? The dashboard shows jobs waiting for a slot. Raise **Parallel jobs**.
- `waiting for memory`: a slot is free but the job's predicted memory does not fit next to the running jobs (see [Scheduling](how-it-works.md#scheduling)). It starts when one of them finishes. Add RAM, lower the **Memory reserve**, or turn memory admission off if the predictions are too high (the dashboard shows each slot's prediction next to its real use).
- `waiting for a strict slot`: a strict job (public repository) waits for a slot reserved for strict jobs or an empty one instead of wiping a warm cache. Raise **Reserved slots for strict jobs**, or set it to 0 to let strict jobs take any slot.

**Jobs show no CPU or memory usage.**
Pitlane reads usage from cgroup v2, so some hosts have no data. Check with
`stat -fc %T /sys/fs/cgroup`: `cgroup2fs` is v2, `tmpfs` is v1. Nothing is recorded on:
- a host on cgroup v1;
- Docker Desktop or rootless Docker;
- Pitlane running in a container that does not see the host's `/sys/fs/cgroup`;
- runners adopted after a Pitlane restart.

Jobs still run normally. See [Resource usage per job](how-it-works.md#resource-usage-per-job).

**A slot is in `error`.**
Usually privileged containers are not allowed. On Proxmox, the LXC needs
`nesting=1,keyctl=1`. Check `journalctl -u pitlane` and `docker logs pitlane-slot-0`.

**`services:` or `container:` jobs fail.**
Make sure you did not override `DOCKER_HOST` in the workflow. Service ports are
reachable on `localhost`.

**Disk fills up.**
Lower **Build cache kept per slot**, or reduce the number of slots. To reset a
slot's cache entirely: `docker rm -f pitlane-slot-N && docker volume rm pitlane-slot-N-docker`, then restart Pitlane.

**The runner version is too old.**
GitHub refuses runners that are too old. Upgrade Pitlane: each release pins a
recent runner image. With a custom runner image, rebuild it on a recent
`ghcr.io/actions/actions-runner` and update **Runner image** in the settings.

**Sign-in fails with a redirect error.**
The **Public URL** must match the GitHub App's callback URL
(`<base url>/auth/callback`). Edit it in the App settings on GitHub if you changed the address.

**Logs.** `journalctl -u pitlane -f` (systemd) or `docker compose logs -f pitlane`.
Use `PITLANE_LOG_LEVEL=debug` for more detail.

**Multi-architecture builds (`docker/setup-qemu-action`) fail with `exec format error`.**
Pitlane mounts `binfmt_misc` and registers the QEMU emulators when it starts
(this is needed in unprivileged LXC containers, where nothing else keeps
`binfmt_misc` mounted). If the log says `binfmt_misc not mountable`, Pitlane
runs in a container that may not mount it (e.g. Docker Compose without extra
privileges): run it with systemd on the host, or keep multi-arch jobs on
GitHub-hosted runners.

**Windows or macOS jobs.** Pitlane only provides Linux runners; keep
`windows-latest` / `macos-latest` jobs on GitHub-hosted runners.
