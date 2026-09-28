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
the model's pods as they come and go. 

## Route limits

Set these in the model's values file, under `modelRoute.nginx`:

```yaml
modelRoute:
  nginx:
    outputConfigMap: "llm-route/openresty-conf"   # required
    peers:                        # a list: write it whole, keep both entries
      - use: backend
        priority: 2
        maxConcurrency: 60        # per engine pod; the route's limit is about this x healthy pods
      - use: backend-svc
        priority: 1
    values:                       # all values are strings
      ttft_limit_ms: "20000"
      tps_limit_tps: "25"
      adaptive_cc_min_frac: "0.5"
```

| Setting | Default | What it does |
| --- | --- | --- |
| `peers[].maxConcurrency` (the `use: backend` entry) | `100` | Concurrency cap per engine pod. The route's static limit is about this × healthy pods |
| `ttft_limit_ms` | `30000` | When the route's average time to first token reaches this, new requests get `429`. A few requests still pass so the average can recover |
| `tps_limit_tps` | `20` | Decode-rate target, in tokens/s per request. Below it, adaptive concurrency lowers the concurrency limit |
| `adaptive_cc` | on | `"false"` turns adaptive concurrency off: the full static limit applies, and a decode rate below `tps_limit_tps` gives `429` instead |
| `adaptive_cc_min_frac` | `0.4` | Floor of the adaptive limit, as a fraction of the static limit (above 0, at most 1) |
| `adaptive_cc_min` | not set | Floor as an absolute number; when set, `adaptive_cc_min_frac` is ignored |
| `default_max` | `20` | Concurrency cap for a peer with no `maxConcurrency` of its own |

### SLO targets take precedence

If the model's `sloRequirement.extraSpec` declares `ttft` or `otps` targets,
they become the route's TTFT and decode-rate limits, and `ttft_limit_ms` /
`tps_limit_tps` are ignored for that metric. The same
targets drive autoscaling; the fields are described in
[autoscaling.md](autoscaling.md#model-settings). `modelRoute.slo.enabled:
false` keeps the static values instead.

A model installed with the chart defaults declares no targets, so the values
above apply.

## How rate limiting works

A route has three limits. A request that trips any of them gets `429` at once;
nothing is queued.

1. **Concurrency.** The number of requests in flight on the route is capped.
   The static cap is `maxConcurrency` × healthy pods. With adaptive concurrency
   (on by default) the cap in force moves between a floor
   (`adaptive_cc_min_frac` of the static cap) and the static cap: it drops when
   requests decode slower than `tps_limit_tps` or wait longer than
   `ttft_limit_ms` for their first token, and rises again only while requests
   are filling the cap. A new or idle route starts at the floor.
2. **Time to first token.** When the route's recent average TTFT reaches
   `ttft_limit_ms`, new requests are rejected. A few still get through, so the
   limit lifts by itself once the engines catch up.
3. **Decode rate.** `tps_limit_tps` is the output speed each request should
   get. With adaptive concurrency on, a slower rate only lowers the concurrency
   cap (1). With `adaptive_cc: "false"`, a slower rate rejects requests
   directly.

The decode rate is read from the `usage` in each response, so streaming
clients should send `stream_options.include_usage`.

## Change a limit

Edit the model's values file and upgrade the release:

```yaml
# values.yaml of the model
modelRoute:
  nginx:
    values:
      tps_limit_tps: "15"
```

```bash
helm upgrade <release> modelsphere/<engine> -n <ns> -f values.yaml
```

Always pass the whole values file with `-f`, not `--reuse-values`.

A change under `modelRoute` or `sloRequirement` does not restart the engine:
the route is rewritten and openresty reloads it gracefully, without cutting
requests in flight. It takes effect within about a minute. To confirm, check
`/<route>/_tps_status` or `_ttft_status` (see [Check that it works](#check-that-it-works)).

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

Body logging is optional. openresty sends each request and response to the
bodylog listener, and [autoscaling](autoscaling.md) reads its signals from
those records.

- **On** (the default, `enabled.bodylog: true`): openresty is pointed at
  `bodylog.<namespace>.svc.cluster.local`. If you set the address yourself
  (`bodylog.host` in `llmgateway/openresty.yaml.gotmpl`), use the fully
  qualified name; a shorter one never resolves inside nginx and records are
  dropped without an error.
- **Off**: set `enabled.bodylog: false` and `enabled.bodylogExporter: false`.
  openresty's address is then left empty, and it captures and sends nothing.
  Autoscaling has no signals and holds every model where it is.

If the address is set but no listener answers, requests are not affected: each
openresty worker keeps records in memory (up to 256 MB) and then drops them,
counting `drop_count`, and logs a warning every 1,000 drops.

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
`/<route>/_*` at the Gateway. `/_bodylog_status` accepts only `127.0.0.1`, so
query it from inside the active pod:

```bash
POD=$(kubectl -n llm-route get pod -l openresty-active=true -o name | head -1)
kubectl -n llm-route exec $POD -c openresty -- curl -s 127.0.0.1:8080/qwen/_bodylog_status
```

In `/_tps_status`, `tps_limit_source` says where the decode-rate limit comes
from: `declared` (SLO), `route` (values file) or `global_default` (20 tok/s).

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
