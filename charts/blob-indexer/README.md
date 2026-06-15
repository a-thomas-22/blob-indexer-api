# blob-indexer Helm chart

Deploys the Blob Indexer API (Deployment) and indexer (StatefulSet) against an
externally provided PostgreSQL database. Database migrations run from a
pre-install/pre-upgrade hook Job.

See `values.yaml` for the full, commented set of values. This README focuses on
the security-sensitive bits.

## Secret handling

Two credentials are involved, and **neither is ever written to the ConfigMap**:

| Credential | Env var | Source |
|------------|---------|--------|
| Database URL | `DB_URL` | `databaseSecret` (chart-managed or `existingSecret`-style ref) |
| Per-network RPC URL (embeds a provider API key, e.g. Alchemy/Infura) | `NETWORK_<NAME>_RPC_URL` | `rpcSecret` (chart-managed or `existingSecret`) |

`<NAME>` is the upper-cased network name with any character outside
`[A-Za-z0-9_]` normalised to `_`. The application reads these env overrides (see
`internal/config/config.go`).

### RPC URLs are credentials — never put them in the ConfigMap/values for non-local installs

`appConfig.networks[].rpc_url` is **stripped from the rendered ConfigMap**. The
chart only ever exposes it through a Kubernetes Secret.

**Production (recommended): provision the Secret out-of-band**

Create a Secret with one key per network, named exactly
`NETWORK_<UPPER(name)>_RPC_URL`, using sealed-secrets / external-secrets / vault,
then reference it:

```yaml
rpcSecret:
  existingSecret: blob-indexer-rpc   # keys: NETWORK_MAINNET_RPC_URL, ...
appConfig:
  networks:
    - name: mainnet
      chain_id: 1
      rpc_url: ""        # leave empty — the URL comes from the Secret
```

This keeps the credential out of Helm values and release history entirely.

**Local/dev only: let the chart manage the Secret**

Setting `appConfig.networks[].rpc_url` renders a chart-managed Secret
(`<release>-rpc`). This is convenient locally but **stores the credential in
Helm release history and any values file** — do not use it for shared installs.

## Health probes

- **API Deployment** — `livenessProbe` → `healthCheck.livenessPath`,
  `readinessProbe` → `healthCheck.readinessPath`. Both default to **empty**, so
  they fall back to `healthCheck.path` (`/api/v1/status`) and stay compatible
  with app images that predate the dedicated health endpoints. Once the app
  image exposes `/api/v1/healthz` (DB-independent, so a DB blip never restarts
  the pod) and `/api/v1/readyz` (pings the DB), set `livenessPath: /api/v1/healthz`
  and `readinessPath: /api/v1/readyz` in a release coordinated with that app
  version.
- **Indexer StatefulSet** — `indexer.livenessProbe` is a deliberately
  RPC-independent exec probe on a heartbeat file (`heartbeatPath`), so an
  upstream provider outage does not cause a restart storm. It is **disabled by
  default** because the indexer binary does not yet refresh the heartbeat file;
  enable it once that wiring (or a local health port) exists. See the TODO in
  `templates/statefulset.yaml`.
