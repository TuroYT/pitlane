# Troubleshooting

**Jobs stay queued.**
- Check the label: `runs-on: pitlane` must match the label shown in **Repositories**.
- Check the target status in **Repositories**. `error` shows the GitHub error, often that the App is not installed on that repository.
- All slots busy? The dashboard shows jobs waiting for a slot. Raise **Parallel jobs**.

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
GitHub refuses runners that are too old. Pull the latest runner image
(`docker pull ghcr.io/actions/actions-runner:latest`) and restart Pitlane.

**Sign-in fails with a redirect error.**
The **Public URL** must match the GitHub App's callback URL
(`<base url>/auth/callback`). Edit it in the App settings on GitHub if you changed the address.

**Logs.** `journalctl -u pitlane -f` (systemd) or `docker compose logs -f pitlane`.
Use `PITLANE_LOG_LEVEL=debug` for more detail.
