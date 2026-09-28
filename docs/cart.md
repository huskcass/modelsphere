# CART: the cache-aware router

Each model's chart installs a CART in front of that model's engine replicas.
CART sends a request to the replica that already holds the longest matching
prompt prefix, so its KV cache is reused, and falls back to the least-loaded
replica when that one is busy. autoconfig keeps CART's worker list in step with
the engine pods.

With one engine replica there is nothing to choose between, so there is no
prefix routing to observe. Run two or more replicas to see it work.

## Settings

In the model's values file. Keys written as `cache.*`, `health.*`, `proxy.*`
and `circuit_breaker.*` live inside `cart.baseConfig`:

| Setting | Default | What it does |
| --- | --- | --- |
| `cart.enabled` | `true` | Installs CART and, with `modelRoute.enabled`, sends the model's route through it |
| `modelRoute.cart.maxLoad` | `20` | Most requests in flight to one engine replica. A replica at its limit is skipped; when all are, CART answers `503` at once instead of queuing. Set it to the engine's real concurrency |
| `cart.replicas` | `2` | One active pod and one standby; see [How it behaves](#how-it-behaves) |
| `cart.resources` | requests 200m / 256Mi, limits 4 CPU / 16Gi | CART needs no GPU. Memory grows with `max_tree_size` × the number of engine replicas |
| `cart.nodeSelector`, `tolerations`, `affinity` | none | Keep CART off GPU nodes if they are scarce. `affinity` is added to the rule that keeps the two pods on different nodes |
| `cart.terminationGracePeriodSeconds` | `3600` | Keep it at least as long as your longest response, or a CART restart cuts long streams |
| `cache.threshold` | `0.1` | Share of the prompt that must match for a request to follow its cached replica |
| `cache.match_abs_threshold` | `8192` | A request also follows its cached replica when this many characters match, whatever the share. Catches a long shared system prompt or tool list |
| `cache.balance_abs_threshold` | `10` | A cached replica counts as overloaded when it has this many more requests in flight than the least-loaded one… |
| `cache.balance_rel_threshold` | `1.25` | …and at least this many times as many. When both hold, the request goes to the least-loaded replica instead. Lower values give up cache affinity sooner |
| `cache.max_tree_size` | `5000000` | Prompt characters remembered per engine replica; the oldest are evicted every 60 s |
| `cache.daily_cleanup_hour_utc` | `21` | Hour (UTC) at which the whole prefix cache is emptied. `-1` turns it off |
| `health.interval_secs` | `10` | How often CART checks each replica's `/health` |
| `health.failure_threshold` | `3` | Failed checks before a replica is taken out, so a dead replica leaves after about 30 s |
| `proxy.request_timeout_secs` | `10000` | Total time for one request. Keep it above your longest generation |
| `proxy.connect_timeout_secs` | `2` | Time to open a connection. Keeps a dead pod IP from stalling requests; long streams are not affected |
| `proxy.max_retries` | `1` | Retries on another replica after a connection error, a timeout or a `408`, `429`, `500`, `502`–`504`, as long as no response has started |
| `proxy.max_body_size` | `10485760` | Largest request body (10 MiB). Raise it for large inline images |
| `proxy.remote_media_url_policy` | `400` | Refuses requests whose images or videos are URLs rather than inline `data:` URIs, so the engine never fetches URLs for clients. `200` allows them |
| `circuit_breaker.failure_threshold` | `5` | Failed requests in a row before a replica is taken out of rotation |
| `circuit_breaker.timeout_secs` | `30` | How long it stays out before CART tries it again |

**`cart.baseConfig` replaces the whole document**, so copy the default and edit
it rather than writing only the keys you change. Leave `workers` out:
autoconfig writes them into a separate key.

```yaml
cart:
  baseConfig: |
    server:
      host: "0.0.0.0"
      port: 8071
    cache:
      threshold: 0.1
      balance_abs_threshold: 10
      max_tree_size: 5000000
      daily_cleanup_hour_utc: 21
    proxy:
      add_routed_peer_header: true
      remote_media_url_policy: 400
      max_body_size: 33554432        # 32 MiB
    health:
      endpoint: "/health"
      interval_secs: 10
modelRoute:
  cart:
    maxLoad: 32
```

Leave `cart.configOverlays` as the chart sets it: that entry is what keeps
`helm upgrade` from overwriting the worker list.

## Changing a setting

**Restart CART after changing anything other than the worker list:**

```bash
kubectl -n <ns> rollout restart deploy/<release>-cart
```

A `helm upgrade` that changes `baseConfig` only updates the ConfigMap. CART
accepts a live reload of its worker list and nothing else. A reload that
changes any other setting is rejected, and every later worker update is then
rejected too, so CART stops following the engine pods until it restarts.

`helm upgrade` rewrites CART's config from values, so hand edits to the
`<release>-cart-config` ConfigMap are lost at the next upgrade. Make changes
in values.

## How the worker list follows the engine pods

Nobody edits the worker list by hand. When engine pods come and go (a rollout,
scaling, a restart), it updates itself:

1. **autoconfig** watches the model's engine pods and writes the Ready ones
   into the `workers.yaml` key of the `<release>-cart-config` ConfigMap. A pod
   that is terminating or not Ready is left out. It never writes an empty
   list: with no Ready pod it keeps the last one.
2. **Kubernetes** updates the mounted copy of that ConfigMap inside the CART
   pods. This is the slow step, typically up to a minute or more.
3. **The reload sidecar** in each CART pod sees the file change and sends CART
   a reload signal.
4. **CART** swaps in the new worker list without restarting and without
   dropping requests. The prefix cache starts empty.

End to end this took 65–80 s on the test cluster. Until then CART keeps
sending to the pods it knew, including one that is shutting down; the engine
chart's default drain keeps that pod serving through the gap (see
[rolling-updates.md](rolling-updates.md#engine)). In between, CART's own health
check stops sending to a pod that no longer answers after 3 failed checks, 10 s
apart.

## How it behaves

- **Every reload starts the prefix cache cold.** Each scale event, engine
  rollout or engine restart empties it. Frequent scaling
  ([autoscaling.md](autoscaling.md)) keeps hit rates low.
- **Two pods, one active.** The cache lives in process memory, so only the pod
  labelled `cart-active=true` takes traffic. Read logs from that pod; the
  standby's are empty. After a takeover the new active pod starts with an
  empty cache.
- **Restarts are safe.** A `rollout restart` of CART under load dropped no
  requests. If the active pod is lost abruptly (node failure, force delete),
  the standby takes over within about 8 s and only the requests in flight
  through the lost pod fail.

## Check that it works

```bash
NS=<model namespace>; REL=<release>

# both pods 3/3, exactly one with cart-active=true
kubectl -n $NS get pods -l app.kubernetes.io/name=cart -L cart-active

# the engine replicas CART routes to, as autoconfig wrote them
kubectl -n $NS get cm $REL-cart-config -o jsonpath='{.data.workers\.yaml}'

# live state of each replica: healthy, load, max_load, circuit breaker
kubectl -n $NS port-forward svc/$REL-cart 8071:8071 &
curl -s localhost:8071/workers

# routing decisions, from the active pod. Send the same long prompt twice:
# cache_miss the first time, cache_hit on the same replica the second
ACTIVE=$(kubectl -n $NS get pods -l app.kubernetes.io/name=cart,cart-active=true -o name)
kubectl -n $NS logs $ACTIVE -c cart --tail=20 | grep 'Route:'

# after a settings change or an engine pod change: was the reload accepted?
kubectl -n $NS logs $ACTIVE -c cart | grep -E 'Config reload|reload rejected'
```

| Symptom | Fix |
| --- | --- |
| `Config reload rejected` in the log; the worker list no longer follows the engine pods | A setting other than the workers changed. `kubectl rollout restart deploy/<release>-cart` |
| A new engine pod gets no traffic for about a minute after it is Ready | Expected; CART picks it up 65–80 s later |
| No `Route:` lines in the log | Only one engine replica is usable, or you are reading the standby. Read the pod with `cart-active=true` |
| Hit rate drops after scaling or an engine rollout | Expected: the cache starts cold after every reload |
| `503 No available workers` | Every replica is at `maxLoad`, unhealthy or taken out by the circuit breaker. Check `/workers`; raise `modelRoute.cart.maxLoad` if the engines can take more |
| A tuning change is gone after an upgrade | It was a hand edit to the ConfigMap. Put it in `cart.baseConfig` |
| CART pod stays in `Init` | It waits for autoconfig to write the first worker. Check that the engine pods are Ready and `modelRoute.enabled` is on |
| The Service has no endpoints after changing `cart.service.port` | Set `server.port` in `baseConfig` and `cart.ha.appTcp` to the same port |
| Requests with image URLs get `400` `remote_media_url_disallowed` | Send images inline as `data:` URIs, or set `proxy.remote_media_url_policy: 200` |
