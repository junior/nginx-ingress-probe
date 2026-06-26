# nginx-ingress-probe

A tiny diagnostics page to **verify the NGINX Plus Ingress Controller after an upgrade**.
Deploy it as a backend *behind* the controller, open it, and one beautiful page confirms the
controller is routing and shows — the request it forwarded, the Kubernetes version, and (when
pointed at the controller's NGINX Plus API) the nginx version, build, and cache zones. One
static Go binary, standard library only, distroless and non-root.

![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![image](https://img.shields.io/badge/image-distroless%20%C2%B7%20non--root-009639.svg)

![nginx-ingress-probe screenshot](docs/screenshot.png)

## What it shows

- **Request through the controller** — every header (so the controller's `X-Forwarded-*` /
  real-IP injections are visible and highlighted), scheme, client-IP chain, TLS. A green banner
  confirms the request actually arrived via the ingress.
- **Kubernetes** — the cluster version (from the in-cluster API `/version`), plus pod, namespace,
  node, and IPs from the downward API.
- **NGINX Plus Ingress Controller** — set `NGINX_PLUS_API_URL` to the controller's NGINX Plus
  API and it pulls `/nginx` (nginx version, build, generation, PID) and `/http/caches` (cache
  zones: used vs. max, cold/warm). This is the data plane you just upgraded.
- **Prometheus** — set `PROMETHEUS_URL` and it queries the controller's live metrics with **no
  RBAC**: controller version + git commit, last config-reload status, cache zone sizes, and
  request rate / active connections. Each row shows only if that metric is being scraped.
- **Facts** — any `PROBE_FACT_*` env var, for things the data-plane API doesn't expose — the
  **controller (NIC) version**, **NGINX Instance Manager (NIM) version**, etc.

Endpoints: **`/`** (page) · **`/api/info`** (everything as JSON, great for `curl`) · **`/healthz`**.

## Build & push to a private registry (Artifactory)

The probe is a normal OCI image built from the [`Dockerfile`](Dockerfile). Build it and push to
your private registry — override `REGISTRY` / `IMAGE` / `TAG`:

```bash
docker login artifactory.example.com
make buildx REGISTRY=artifactory.example.com/docker-local TAG=0.1.0   # multi-arch: builds + pushes
# …or single-arch:
make build push REGISTRY=artifactory.example.com/docker-local TAG=0.1.0
```

Without `make`:

```bash
REF=artifactory.example.com/docker-local/nginx-ingress-probe:0.1.0
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=0.1.0 -t "$REF" --push .
```

Then set that image in [`k8s/deployment.yaml`](k8s/deployment.yaml) and add a pull secret:

```bash
kubectl create secret docker-registry artifactory \
  --docker-server=artifactory.example.com --docker-username=… --docker-password=…
```

### Optional: GitHub Packages (GHCR) for testing / external consumption

[`.github/workflows/release.yml`](.github/workflows/release.yml) publishes the same multi-arch
image to GHCR on a tag (`git tag v0.1.0 && git push origin v0.1.0`) — handy for sharing or quick
tests without the private registry. Artifactory stays the primary home.

## Deploy & test the controller

```bash
kubectl apply -f k8s/deployment.yaml   # (edit the image first)
kubectl apply -f k8s/service.yaml
kubectl apply -f k8s/ingress.yaml      # set ingressClassName to the controller under test
```

Browse the Ingress host — the green banner + forwarded headers confirm the NGINX Plus Ingress
Controller routed to the pod. Or, quickly, without the ingress:

```bash
kubectl port-forward deploy/nginx-ingress-probe 8080:8080   # → http://localhost:8080
```

Point it at the controller's NGINX Plus API to see the upgraded nginx version + caches
(uncomment in `k8s/deployment.yaml`):

```yaml
- name: NGINX_PLUS_API_URL
  value: "http://nginx-ingress.nginx-ingress.svc:8080/api"   # the controller's NGINX Plus API
- name: PROBE_FACT_Controller_Version    # NIC version isn't in the data-plane API — inject it
  value: "4.0.1"
```

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8080` | listen port |
| `NGINX_PLUS_API_URL` | — | the controller's NGINX Plus API base; enables the version + cache-zone card |
| `NGINX_PLUS_API_INSECURE` | `false` | skip TLS verification for a self-signed Plus API |
| `PROMETHEUS_URL` | — | Prometheus base URL; pulls the controller's live metrics (version, reload, caches, traffic) — no RBAC |
| `PROMETHEUS_TOKEN` · `PROMETHEUS_INSECURE` | — | bearer auth / skip TLS verify for a secured Prometheus |
| `PROBE_FACT_*` | — | each becomes a row in the **Facts** card (e.g. `PROBE_FACT_NIM_Version`) |
| `PROBE_DEMO` | `false` | fill sample K8s / controller values for local previews (clearly flagged) |

Pod identity is wired from the Kubernetes downward API in the Deployment.

## Security

Single static binary on `distroless/static:nonroot` — **uid 65532**, **read-only root
filesystem**, **all capabilities dropped**, `RuntimeDefault` seccomp, no shell (~10 MB). The
cluster version uses the pod's service-account token against `/version` (readable by any
authenticated account — no extra RBAC).

## Develop

```bash
make run            # PROBE_DEMO=1 go run . → http://localhost:8080
make test
make lint
```

## License

[MIT](LICENSE) — built by [Adao Oliveira Jr](https://adao.dev).
