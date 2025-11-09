### Do

- Build the Docker plugin for the current architecture with `make clean rootfs create`; optionally set `ARCH=amd64|arm64` and `PLUGIN_TAG=<arch>-latest`.
- Push the current-arch plugin to Docker Hub using `make push`; set `PLUGIN_TAG=<arch>-latest` (e.g., `amd64-latest` or `arm64-latest`).
- Build and push the multi-arch Docker image with `make image-push`; it produces `jakubs22/juicefs:latest` and `jakubs22/juicefs:<JUICEFS_CE_VERSION>` for `linux/amd64,linux/arm64`.
- Log in before pushes using `make image-login` with env vars `DOCKER_USERNAME` and `DOCKER_PASSWORD` (Docker Hub access token preferred).
- Override JuiceFS CE version explicitly when needed: `JUICEFS_CE_VERSION=1.3.0 make image-push`.
- Perform a minimal sanity check after build/push: `docker run --rm jakubs22/juicefs:latest /usr/bin/juicefs version` and `docker buildx imagetools inspect jakubs22/juicefs:latest`.
- On branch `master`, rely on `.github/workflows/go.yml` to build per-arch and push plugin tags `<arch>-latest`; ensure repo secrets `DOCKER_USERNAME` and `DOCKER_PASSWORD` are set.
- For plugin E2E test, export `JFS_VOL`, `JFS_TOKEN`, `JFS_ACCESSKEY`, `JFS_SECRETKEY`, `JFS_SUBDIR` (load from `~/.secrets.zsh`; derive `JFS_VOL` from `VOL_NAME`/`JFS_VOLUME` when present), then run `make test`.
- Remove the temporary buildx builder when done with `make clean-buildx`.
- If the Go proxy is rate‑limited, set `GOPROXY='https://proxy.golang.org,direct'` for reproducible builds.
- Use a Python base with full stdlib in the final image stage (e.g., `python:3.12-alpine` or `python:3.12-slim`) so `/usr/bin/juicefs` (Python script) runs; sanity‑check with `test -f plugin/rootfs/usr/local/lib/python3.12/textwrap.py`.
- After `make rootfs`, manage the plugin on the target node explicitly via context: `docker --context $DOCKER_CONTEXT plugin create juicefs-volume:latest ./plugin && docker --context $DOCKER_CONTEXT plugin enable juicefs-volume:latest`.
- Run a smoke mount on the target node: `docker --context $DOCKER_CONTEXT volume create -d juicefs-volume:latest -o name=$JFS_VOL -o token=$JFS_TOKEN -o access-key=$JFS_ACCESSKEY -o secret-key=$JFS_SECRETKEY -o subdir=$JFS_SUBDIR jfs-smoke && docker --context $DOCKER_CONTEXT run --rm -v jfs-smoke:/mnt busybox sh -lc 'echo ok>/mnt/probe && cat /mnt/probe'`.
- For diagnostics, toggle DEBUG safely: `docker plugin disable -f juicefs-volume:latest && docker plugin set juicefs-volume:latest DEBUG=1 && docker plugin enable juicefs-volume:latest`.
- Prefer pinning JuiceFS binaries during build: set `JUICEFS_CE_VERSION`, and if using EE, pin the `/usr/bin/juicefs` source (avoid unversioned URLs).
- Bundle the mount helper and disable auto‑updates: place `jfsmount` in `/bin/jfsmount`, set `JFS_NO_UPDATE=1` and `JFS_MOUNT_BIN=/bin/jfsmount` in `config.json`, and verify via `docker plugin inspect`.
- Use `/bin/juicefs` for both CE/EE paths in code and always pass credentials (`token`, `access-key`, `secret-key`, optional `subdir`) to `juicefs mount`; invoke with `-d` to avoid orphan wait errors.
- Map `JFSMOUNT_URL` by arch at build time (arm64 → `mount.aarch64`, amd64 → `mount.x86_64`); overrideable via build arg.
- After build, smoke‑mount with decrypted secrets only (never `ENC[...]`): decrypt with `sops -d` and pass plain values to `docker volume create -d juicefs-volume ...`.
- After implementing plugin logic changes, run a Cloud happy‑path sanity check with `JFS_VOL=${VOL_NAME:-skinbase-juicefs-volume} JFS_TOKEN=$JFS_TOKEN JFS_ACCESSKEY=$JFS_ACCESSKEY JFS_SECRETKEY=$JFS_SECRETKEY make test` using secrets from `~/.secrets.zsh`.

### Don't

- Don’t run `make test` without valid `JFS_*` credentials; it will fail and may expose tokens.
- Don’t remove the `<arch>-latest` suffix from plugin tags; keep `amd64-latest` and `arm64-latest` to distinguish architectures.
- Don’t alter the builder name `juicefs-builder` across scripts; targets expect this identifier.
- Don’t run `docker plugin set` while the plugin is enabled; always disable first.
- Don’t manage plugins on a remote node without `docker --context $DOCKER_CONTEXT`; you’ll modify the local engine by mistake.
- Don’t rely on unpinned `/usr/bin/juicefs` downloads; runtime drift will break mounts when CLI flags change.
- Don’t rely on runtime auto‑download of `jfsmount` inside the plugin; missing DNS or checksum issues will break mounts.
- Don’t depend on persisted `juicefs auth` state; treat `token/access-key/secret-key` as mount‑time options and avoid requiring auth files.
