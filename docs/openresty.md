# Openresty (Routing and rate limiting)

Every model gets a route on openresty, the routing layer in front of the
engines. openresty decides whether a request is admitted and which engine pod
serves it. This page covers the limits you set per route, turning on API keys,
and checking that the routing layer is healthy.

Three things to know first:

- **Nothing is queued.** A request over a limit gets an immediate `429` with
  `Retry-After: 1`. Retrying is the client's job.
- **Every route has limits, even with no settings**: adaptive concurrency, a
  30 s time-to-first-token (TTFT) limit and a 20 tok/s decode-rate limit.
- **A stock install has no API-key authentication.** See
  [Turn on API keys](#turn-on-api-keys).

## Sending requests

**The first path segment is the route, not the model.** The route is the
model's release name (`qwen` for the example in
[deploy-a-model.md](deploy-a-model.md)):

```bash
curl -s http://llm.example.com/qwen/v1/chat/completions \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"user","content":"hi"}]}'
```

An unknown route returns `502` while everything is healthy.

The route is created from the engine chart's `modelRoute:` section and follows
the model's pods as they come and go. openresty reloads gracefully: streams in
flight are not cut.

## Route limits

Set these in the model's values file, under `modelRoute.nginx.values`. All
values are strings:

```yaml
modelRoute:
  nginx:
    outputConfigMap: "llm-route/openresty-conf"   # required
    values:
      ttft_limit_ms: "20000"
      tps_limit_tps: "25"
      default_max: "60"
```

| Setting | Default | What it does |
| --- | --- | --- |
| `ttft_limit_ms` | `30000` | When the route's average time to first token reaches this, new requests get `429`. A few requests still pass so the average can recover |
| `tps_limit_tps` | `20` | Decode-rate target, in tokens/s per request. Below it, adaptive concurrency lowers the concurrency limit |
| `adaptive_cc` | on | `"false"` turns adaptive concurrency off: the full static limit applies, and a decode rate below `tps_limit_tps` gives `429` instead |
| `adaptive_cc_min` | 40 % of the static limit | The lowest the adaptive limit goes |
| `default_max` | `20` | Concurrency cap for a peer with no `maxConcurrency` of its own |
| `expose_routed_peer` | `"true"` | Responses carry `X-Routed-Peer: <ip:port>\|<gpu>\|<node>`. Set `"false"` on routes reachable from outside the cluster |

The per-pod cap is not a `values` key: it is `maxConcurrency` (default `100`)
on the `use: backend` entry of `modelRoute.nginx.peers`. The route's static
limit is roughly that cap × healthy pods. The setting is a list, so copy the
chart's default list and edit it.

### SLO targets take precedence

If the model's `sloRequirement.extraSpec` declares `ttft` or `otps` targets,
they become the route's TTFT and decode-rate limits, and `ttft_limit_ms` /
`tps_limit_tps` and runtime overrides are ignored for that metric. The same
targets drive autoscaling; the fields are described in
[autoscaling.md](autoscaling.md#model-settings). `modelRoute.slo.enabled:
false` keeps the static values instead.

A model installed with the chart defaults declares no targets, so the values
above apply.

## How the limits behave

- **Adaptive concurrency starts low and grows only under pressure.** When it
  first applies, a route drops to about 40 % of its static limit. It grows 2 %
  every 20 s only while requests fill the limit or get `429`s, so a lightly
  loaded route sits at the floor; reaching the full limit takes about 15
  minutes of sustained load. It shrinks when the decode rate falls below
  `tps_limit_tps` or TTFT rises above its limit.
- **A `tps_limit_tps` the engine cannot reach keeps the limit at the floor**,
  and nothing alerts on it. Watch `/_tps_status`.
- **Decode-rate samples need `usage` in the response.** A streaming client
  that does not send `stream_options.include_usage` produces no sample; the
  counter `nousage_samples` grows instead and adaptive concurrency never gets a
  value. Make clients request usage, or set `adaptive_cc: "false"` on that
  route.
- **`GET /v1/models` is never rate-limited**, so health probes from outside
  always pass.

## Change a limit at runtime

For an immediate change without a reload, call the route's endpoints from
inside the active openresty pod. They accept only `127.0.0.1`:

```bash
POD=$(kubectl -n llm-route get pod -l openresty-active=true -o name | head -1)
kubectl -n llm-route exec $POD -c openresty -- \
  curl -s 'http://127.0.0.1:8080/qwen/_tps_limit?tps=15&ttl=3600'
```

| Endpoint | What it does |
| --- | --- |
| `/<route>/_ttft_limit?ms=N` | Set the TTFT limit; `ms=0` clears the override |
| `/<route>/_tps_limit?tps=N` | Set the decode-rate limit; `tps=0` clears the override |
| `/<route>/_ttft_toggle?on=0` | Turn TTFT limiting off (`on=1` turns it back on) |
| `/<route>/_tps_toggle?on=0` | Turn decode-rate handling and adaptive concurrency off; the full static limit applies |

Overrides expire after `ttl` seconds (default `7200`, `0` = never). They are
lost when the pod restarts or the standby takes over, and have no effect while
an SLO target is declared. For a lasting change, edit the values file.

## Turn on API keys

**No key file means no authentication**: openresty lets every request through
and reports `api_keys_configured=false` in `/_health_status`. Alert on it.

1. Create the Secret. The entry must be named `keys`, in the form
   `key:owner,key:owner`:

   ```bash
   kubectl -n llm-route create secret generic openresty-api-keys \
     --from-literal=keys='<key-1>:team-a,<key-2>:team-b'
   ```

2. Add this to `llmgateway/openresty.yaml.gotmpl` and apply:

   ```yaml
   existingSecret: openresty-api-keys
   ```

Clients then send `Authorization: Bearer <key>`; a missing or wrong key gets
`401`.

**To rotate**, edit the Secret: add the new key next to the old one, move the
callers over, then remove the old key. openresty reloads when the Secret
changes; no restart is needed.

## Body logging: the listener address

openresty sends request and response bodies to the bodylog listener, and
[autoscaling](autoscaling.md) depends on them. **The host must be the fully
qualified name `bodylog.<namespace>.svc.cluster.local`.** A shorter name never
resolves inside nginx, and the records are dropped with no error.

**Known issue:** `llmgateway/openresty.yaml.gotmpl` sets
`host: bodylog.llm-route.svc`. Change it to:

```yaml
bodylog:
  host: bodylog.llm-route.svc.cluster.local
```

**To check that records arrive**, query the listener's `/summary` on its HTTP
port `9998` (with its token, if one is set): `peers` must be non-empty. The
`write_count` in openresty's `/_bodylog_status` is not proof; it keeps growing
even when nothing arrives.

## Check that it works

```bash
kubectl get mr -A                          # every model's route: Ready=True
kubectl describe mr qwen -n llm-demo       # SLOSynced: whether SLO targets were applied

# status endpoints, per route
curl -s http://llm.example.com/qwen/_health_status   # each engine pod: active / banned
curl -s http://llm.example.com/qwen/_tps_status      # decode rate, adaptive limit, limit source
curl -s http://llm.example.com/qwen/_ttft_status     # TTFT average, whether it is enforcing
curl -s http://llm.example.com/qwen/_429_status      # 429 counts by reason
```

**The status endpoints have no authentication and list the engines' internal
addresses.** Do not expose them: if port 8080 is reachable from outside, block
`/<route>/_*` at the Gateway. `/_bodylog_status` accepts only `127.0.0.1`;
query it from inside the active pod as in
[Change a limit at runtime](#change-a-limit-at-runtime).

In `/_tps_status`, `tps_limit_source` says where the decode-rate limit comes
from: `declared` (SLO), `override` (runtime), `route` (values file) or
`global_default` (20 tok/s).

| Symptom | Do |
| --- | --- |
| `502` on every request to a route | The route name in the path is wrong, or `kubectl get mr` shows the route not `Ready` |
| `429 "concurrency limit exceeded"` at low load | Adaptive concurrency is at its floor; check `nousage_samples` in `/_tps_status` and whether `tps_limit_tps` is reachable |
| `429 "ttft limit exceeded"` | The engines are slower than the TTFT limit; raise the limit or add replicas |
| `503` with `Retry-After: 5` | Every engine pod failed its health check; check the pods |
| A limit in the values file has no effect | An SLO target is declared for it; check `tps_limit_source` and `SLOSynced` |
| `SLOSynced` reason `NothingApplicable` | The model declares no `ttft` / `otps` targets; the values file applies |
| Requests work without a key | No key Secret; see [Turn on API keys](#turn-on-api-keys) |
| Autoscaling holds with `hold-no-physical-data` | Body logging is not arriving; see [the listener address](#body-logging-the-listener-address) |
