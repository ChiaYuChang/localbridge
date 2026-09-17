# C1 container envelope (no live serving).
# Build stage tracks go.mod `go` directive exactly (verified: go 1.27.0).
FROM golang:1.27.0-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/gateway ./cmd/

# Runtime: node LTS slim (Debian trixie).
FROM node:lts-trixie-slim
# One apt layer: git (VCS tooling) + tini (PID 1) + pipx (classic
# entry-point installs) + curl/ca-certificates (jj tarball fetch only).
RUN apt-get update \
 && apt-get install -y --no-install-recommends git tini pipx curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# uv/uvx for modern PEP 723/pyproject flows (joint decision: both uv and
# pipx are carried — neither is an MCP installer, both are runners).
RUN pipx install uv
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
# HOME-less operation is forbidden.
RUN useradd -u 10001 -U --no-create-home --shell /usr/sbin/nologin mcp \
 && mkdir -p /workspace /var/lib/mcp/npm /var/lib/mcp/uv /var/lib/mcp/pipx /opt/mcp/bin /opt/mcp/config \
 && chown -R mcp:mcp /workspace /var/lib/mcp /opt/mcp
ENV HOME=/tmp \
    NPM_CONFIG_CACHE=/var/lib/mcp/npm \
    UV_CACHE_DIR=/var/lib/mcp/uv \
    PIPX_HOME=/var/lib/mcp/pipx \
    PATH="/opt/mcp/bin:${PATH}"
# Frozen COPY: EXACTLY TWO build-context artifacts (gateway binary via the
# build stage + entrypoint script). tini/jj arrive via their own install
# steps, never via COPY.
COPY --from=build /out/gateway /opt/mcp/bin/gateway
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh
USER mcp
# Build-time assertions (parsed comparisons, never print-only):
# git >= 2.41, jj == 0.41.0 exact, gateway executes (serving needs creds —
# out of envelope scope; assert it starts and reports config, no crash).
RUN set -eu; \
  GV=$(git --version | awk '{print $3}'); \
  LOWEST=$(printf '2.41.0\n%s\n' "$GV" | sort -V | head -n1); \
  [ "$LOWEST" = "2.41.0" ] || { echo "git $GV < 2.41"; exit 1; }; \
  case "$(jj --version)" in "jj 0.41.0"*) ;; *) echo "jj version mismatch: $(jj --version)"; exit 1 ;; esac; \
  set +e; timeout 10 /opt/mcp/bin/gateway > /tmp/smoke.log 2>&1; CODE=$?; set -e; \
  cat /tmp/smoke.log; rm /tmp/smoke.log; \
  if [ "$CODE" -eq 124 ]; then echo "gateway hung (timeout kill)"; exit 1; fi; \
  if [ "$CODE" -eq 127 ] || [ "$CODE" -gt 128 ]; then echo "gateway crash/not-found ($CODE)"; exit 1; fi; \
  echo "gateway smoke exit: $CODE (serving needs creds — out of scope)"
ENTRYPOINT ["tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["/opt/mcp/bin/gateway"]
# No VOLUME declared (image stays plain); persistence is a RUNTIME
# contract (:ro, --read-only, --tmpfs /tmp, volume mounts) enforced by
# scripts/container-proof.sh + documented run flags.
