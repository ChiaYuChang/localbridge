# C1 container envelope (no live serving).
# Build stage tracks go.mod `go` directive exactly (verified: go 1.27.0).
FROM golang:1.27.0-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/localbridge ./cmd/

# Test stage (explicit chain: builder -> test -> runtime). Installs the
# SAME pinned jj 0.41.0 tarball by SHA + distro git, then runs the
# gateway composition suite at build time (real git/jj integration
# in-container: composition, natives, integration, sanitized child env,
# proxy child start/stop). Build-stage tests prove composition ONLY —
# never mount/PID1/stop semantics (those belong to container-proof.sh
# runtime rows; never credited across).
FROM build AS test
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates nodejs npm python3 python3-pip && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
RUN set -eu; \
  case "${TARGETARCH:-amd64}" in \
    amd64) JJ_TRIPLE=x86_64-unknown-linux-musl; JJ_SHA=42181a80d316ac157874c817c9945e104275114fb461d99e06e2312502f08f99 ;; \
    arm64) JJ_TRIPLE=aarch64-unknown-linux-musl; JJ_SHA=cd75d0f920b2674147a48eac84ee4594f476fc8f98cd7e358b25750a51622d91 ;; \
    *) echo "unsupported arch ${TARGETARCH}"; exit 1 ;; \
  esac; \
  curl -fsSL -o /tmp/jj.tar.gz "https://github.com/jj-vcs/jj/releases/download/v0.41.0/jj-v0.41.0-${JJ_TRIPLE}.tar.gz"; \
  echo "${JJ_SHA}  /tmp/jj.tar.gz" | sha256sum -c -; \
  tar -xzf /tmp/jj.tar.gz -C /usr/local/bin --strip-components=1 --wildcards '*/jj'; \
  chmod 755 /usr/local/bin/jj; \
  rm /tmp/jj.tar.gz
# Phase 2a prefetch (network at build only): warm the npx cache and
# install the time module so the driver runs cached/offline. The `|| true`
# only tolerates the expected bad-dir exit; an actually missing server
# still fails the suite below as SERVER_MISSING (fail-closed).
RUN npx -y @modelcontextprotocol/server-filesystem@2026.8.31 --help >/dev/null 2>&1 || true
RUN python3 -m pip install --break-system-packages mcp-server-time==2026.8.18
RUN go test ./internal/gateway/ -count=1

# Runtime: node LTS slim (Debian trixie).
FROM node:lts-trixie-slim
# One apt layer: git (VCS tooling) + tini (PID 1) + pipx (classic
# entry-point installs) + curl/ca-certificates (jj tarball fetch only)
# + python3/pip (Phase 2b live downstreams; node base lacks them)
# + golang-go/gcc (Phase-1 installer toolchains: go + rustup cargo join
# the npm/node + uv/python already present — installer-enabled implies
# executable-present, no runtime toolchain bootstrapping in v1; gcc +
# libc6-dev are the crate linker rustup-minimal does not ship).
RUN apt-get update \
  && apt-get install -y --no-install-recommends git tini pipx curl ca-certificates python3 python3-pip golang-go gcc libc6-dev \
  && rm -rf /var/lib/apt/lists/*
# uv/uvx for modern PEP 723/pyproject flows (joint decision: both uv and
# pipx are carried — neither is an MCP installer, both are runners).
# Image-resident by design (round 3): /opt/mcp/pipx, NEVER the persistent
# /var/lib/mcp tree — all four installer executables must belong to the
# immutable image so installed shims cannot shadow them.
RUN mkdir -p /opt/mcp/pipx && PIPX_HOME=/opt/mcp/pipx PIPX_BIN_DIR=/opt/mcp/pipx/bin pipx install uv
# Rust 1.89.0 via rustup (apt's 1.85.1 is too old for modern crate
# deps): toolchain image-resident under /opt/mcp/rustup (the persistent
# volume overlays /var/lib/mcp/cache at runtime, so a volume-homed
# toolchain would vanish on fresh volumes); registry/git caches stay on
# the volume via CARGO_HOME. /opt/mcp/bin symlinks keep cargo/rustc
# resolvable as absolute image paths (installer PATH scrub skips
# state-root entries by design).
RUN set -eu; \
  export RUSTUP_HOME=/opt/mcp/rustup CARGO_HOME=/opt/mcp/cargo-tmp; \
  mkdir -p /opt/mcp/bin; \
  curl -fsSL --proto '=https' --tlsv1.2 https://sh.rustup.rs -o /tmp/rustup-init.sh; \
  sh /tmp/rustup-init.sh -y --default-toolchain 1.89.0 --profile minimal --no-modify-path; \
  rm -f /tmp/rustup-init.sh; \
  TC=$(ls /opt/mcp/rustup/toolchains); \
  ln -s "/opt/mcp/rustup/toolchains/$TC/bin/cargo" /opt/mcp/bin/cargo; \
  ln -s "/opt/mcp/rustup/toolchains/$TC/bin/rustc" /opt/mcp/bin/rustc; \
  rm -rf /opt/mcp/cargo-tmp; \
  /opt/mcp/bin/cargo --version; /opt/mcp/bin/rustc --version
# No global downstream installs in the runtime image (joint defense):
# the gateway installs its declared downstreams at startup into the
# persistent volume (installer); baking servers globally would bypass
# receipts/reconciliation. (Test-stage fixtures stay — build-time
# suite needs them.)
# jj 0.41.0 official prebuilt linux-musl tarballs, SHA256 verified.
# Source: jj-vcs/jj release 318923928 asset digests (musl static, no glibc).
#   amd64: sha256:42181a80d316ac157874c817c9945e104275114fb461d99e06e2312502f08f99
#   arm64: sha256:cd75d0f920b2674147a48eac84ee4594f476fc8f98cd7e358b25750a51622d91
ARG TARGETARCH
RUN set -eu; \
  case "${TARGETARCH:-amd64}" in \
    amd64) JJ_TRIPLE=x86_64-unknown-linux-musl; JJ_SHA=42181a80d316ac157874c817c9945e104275114fb461d99e06e2312502f08f99 ;; \
    arm64) JJ_TRIPLE=aarch64-unknown-linux-musl; JJ_SHA=cd75d0f920b2674147a48eac84ee4594f476fc8f98cd7e358b25750a51622d91 ;; \
    *) echo "unsupported arch ${TARGETARCH}"; exit 1 ;; \
  esac; \
  curl -fsSL -o /tmp/jj.tar.gz "https://github.com/jj-vcs/jj/releases/download/v0.41.0/jj-v0.41.0-${JJ_TRIPLE}.tar.gz"; \
  echo "${JJ_SHA}  /tmp/jj.tar.gz" | sha256sum -c -; \
  tar -xzf /tmp/jj.tar.gz -C /usr/local/bin --strip-components=1 --wildcards '*/jj'; \
  chmod 755 /usr/local/bin/jj; \
  rm /tmp/jj.tar.gz
# Layout + explicit non-1000 UID (auto-1000 commonly matches the host and
# would make ownership tests vacuous). HOME is ALWAYS /tmp (tmpfs);
# HOME-less operation is forbidden. /var/lib/mcp split (blueprint):
# bin/ derived executables, state.json snapshot, cache/ toolchain homes.
# /opt/mcp holds image executables (localbridge, uv via pipx).
RUN useradd -u 10001 -U --no-create-home --shell /usr/sbin/nologin mcp \
  && mkdir -p /workspace /var/lib/mcp/bin /var/lib/mcp/cache/npm /var/lib/mcp/cache/uv /var/lib/mcp/cache/uvtools /var/lib/mcp/cache/go /var/lib/mcp/cache/cargo /opt/mcp/bin /opt/mcp/config \
  && chown -R mcp:mcp /workspace /var/lib/mcp /opt/mcp
ENV HOME=/tmp \
    NPM_CONFIG_CACHE=/var/lib/mcp/cache/npm \
    UV_CACHE_DIR=/var/lib/mcp/cache/uv \
    UV_TOOL_DIR=/var/lib/mcp/cache/uvtools \
    UV_TOOL_BIN_DIR=/var/lib/mcp/bin \
    CARGO_HOME=/var/lib/mcp/cache/cargo \
    RUSTUP_HOME=/opt/mcp/rustup \
    GOBIN=/var/lib/mcp/bin \
    GOCACHE=/var/lib/mcp/cache/go/build \
    GOMODCACHE=/var/lib/mcp/cache/go/mod \
    GOPATH=/var/lib/mcp/cache/go/path \
    PIPX_HOME=/opt/mcp/pipx \
    PIPX_BIN_DIR=/opt/mcp/pipx/bin \
    PATH="/opt/mcp/bin:/opt/mcp/pipx/bin:/var/lib/mcp/bin:${PATH}"
# Frozen COPY: EXACTLY TWO build-context artifacts (localbridge binary via the
# build stage + entrypoint script). tini/jj arrive via their own install
# steps, never via COPY. Runtime-needed files stay world-executable
# (arbitrary runtime UIDs must exec them: 0755 pinned here).
COPY --from=build --chmod=755 /out/localbridge /opt/mcp/bin/localbridge
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh
USER mcp
# Phase-1 installer contract proof (row 7, in-image): runs AFTER the
# ENV block so the pipx bin dir (uv) is on PATH. Manager presence (one
# check each — multi-arg command -v is shell-dependent) + one real
# install per manager into a throwaway prefix, argv mirroring
# internal/installer argv() exactly (pinned version FORMS are
# unit-asserted in Go; fixtures prove the prefix/cache/bin mechanics).
# cargo --path is fixture-only (production installs registry specs).
# uv path-installs download a build backend (network at build only).
RUN set -eu; \
  command -v npm >/dev/null || { echo "npm missing"; exit 1; }; \
  command -v uv >/dev/null || { echo "uv missing"; exit 1; }; \
  command -v cargo >/dev/null || { echo "cargo missing"; exit 1; }; \
  command -v go >/dev/null || { echo "go missing"; exit 1; }; \
  P=/tmp/contract; rm -rf "$P"; mkdir -p "$P"; \
  export NPM_CONFIG_CACHE="$P/cache/npm" UV_CACHE_DIR="$P/cache/uv" UV_TOOL_DIR="$P/uvtools" UV_TOOL_BIN_DIR="$P/pfx/bin" CARGO_HOME="$P/cargohome" GOBIN="$P/pfx/bin" GOCACHE="$P/cache/go/build" GOMODCACHE="$P/cache/go/mod" GOPATH="$P/cache/go/path"; \
  mkdir -p "$P/npmfix"; \
  printf '{"name":"fixnpm","version":"0.1.0","bin":{"fixnpm":"cli.js"}}\n' > "$P/npmfix/package.json"; \
  printf '#!/usr/bin/env node\nconsole.log("fixnpm");\n' > "$P/npmfix/cli.js"; \
  npm install -g --prefix "$P/pfx" --no-audit --no-fund "$P/npmfix"; \
  [ -x "$P/pfx/bin/fixnpm" ] || { echo "npm contract: binary missing"; exit 1; }; \
  mkdir -p "$P/uvfix"; \
  printf '[project]\nname = "fixuv"\nversion = "0.1.0"\n[project.scripts]\nfixuv = "fixuv:main"\n[build-system]\nrequires = ["setuptools>=61"]\nbuild-backend = "setuptools.build_meta"\n' > "$P/uvfix/pyproject.toml"; \
  printf 'def main():\n    print("fixuv")\n' > "$P/uvfix/fixuv.py"; \
  uv tool install --force "$P/uvfix" --color never; \
  [ -x "$P/pfx/bin/fixuv" ] || { echo "uv contract: binary missing"; exit 1; }; \
  mkdir -p "$P/cargofix/src"; \
  printf '[package]\nname = "fixcargo"\nversion = "0.1.0"\nedition = "2021"\n' > "$P/cargofix/Cargo.toml"; \
  printf 'fn main() { println!("fixcargo"); }\n' > "$P/cargofix/src/main.rs"; \
  cargo install --root "$P/pfx" --path "$P/cargofix" --color never; \
  [ -x "$P/pfx/bin/fixcargo" ] || { echo "cargo contract: binary missing"; exit 1; }; \
  mkdir -p "$P/gofix"; \
  printf 'module example.com/fixgo\n\ngo 1.21\n' > "$P/gofix/go.mod"; \
  printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("fixgo") }\n' > "$P/gofix/main.go"; \
  (cd "$P/gofix" && GOPROXY=off go install .); \
  [ -x "$P/pfx/bin/fixgo" ] || { echo "go contract: binary missing"; exit 1; }; \
  rm -rf "$P"; \
  echo "installer contract ok: npm/uv/cargo/go"
# Build-time assertions (parsed comparisons, never print-only):
# git >= 2.41, jj == 0.41.0-prefix exact, 0755 on localbridge/entrypoint/
# tini/jj, localbridge executes (serving needs creds — out of envelope
# scope; assert it starts and reports config, no crash/hang).
RUN set -eu; \
  GV=$(git --version | awk '{print $3}'); \
  LOWEST=$(printf '2.41.0\n%s\n' "$GV" | sort -V | head -n1); \
  [ "$LOWEST" = "2.41.0" ] || { echo "git $GV < 2.41"; exit 1; }; \
  case "$(jj --version)" in "jj 0.41.0"*) ;; *) echo "jj version mismatch: $(jj --version)"; exit 1 ;; esac; \
  [ "$(stat -c %a /opt/mcp/bin/localbridge)" = "755" ] || { echo "localbridge mode"; exit 1; }; \
  [ "$(stat -c %a /usr/local/bin/entrypoint.sh)" = "755" ] || { echo "entrypoint mode"; exit 1; }; \
  [ "$(stat -c %a "$(command -v tini)")" = "755" ] || { echo "tini mode"; exit 1; }; \
  [ "$(stat -c %a /usr/local/bin/jj)" = "755" ] || { echo "jj mode"; exit 1; }; \
  set +e; timeout 10 /opt/mcp/bin/localbridge > /tmp/smoke.log 2>&1; CODE=$?; set -e; \
  cat /tmp/smoke.log; rm /tmp/smoke.log; \
  if [ "$CODE" -eq 124 ]; then echo "localbridge hung (timeout kill)"; exit 1; fi; \
  if [ "$CODE" -eq 127 ] || [ "$CODE" -gt 128 ]; then echo "localbridge crash/not-found ($CODE)"; exit 1; fi; \
  echo "localbridge smoke exit: $CODE (serving needs creds — out of scope)"
ENTRYPOINT ["tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["/opt/mcp/bin/localbridge"]
# No VOLUME declared (image stays plain); persistence is a RUNTIME
# contract (:ro, --read-only, --tmpfs /tmp, volume mounts) enforced by
# scripts/container-proof.sh + documented run flags.
