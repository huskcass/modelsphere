# Rolling updates

How to upgrade or reconfigure one component of a running stack without
dropping requests. Every component except the model engine rolls cleanly with
its chart defaults. The engine needs two extra values, set once per model.

## Summary

| Component | How to update | Drops requests? | What to do |
| --- | --- | --- | --- |
| Engine (SGLang / vLLM) | `make helm-apply SELECTOR=name=<model> MODELS=models/<file>.yaml` | **Yes, with chart defaults** | Set the [shutdown values](#engine) in every model |
| CART | Same command as the engine (it is part of the model release) | No; a force-deleted active pod loses its in-flight requests | `rollout restart` after changing CART settings |
| openresty | `make helm-apply SELECTOR=name=openresty` | No | Bump tags in `llmgateway/openresty.yaml.gotmpl` |
| autoconfig | `make helm-apply SELECTOR=name=autoconfig` | No | Apply CRDs first; don't roll it together with a model |
| bodylog, bodylog-exporter | `make helm-apply SELECTOR=name=bodylog` (or `bodylog-exporter`) | No (records can be lost, not requests) | Nothing |
| llmscaleoperator, llm-slo | `make helm-apply SELECTOR=name=<release>` | Not on the request path | Apply llm-slo CRDs first; avoid traffic peaks |

`make helm-apply` renders the full values every time. If you run helm
yourself, pass the values file with `-f`, not `--reuse-values`: that replays
only last time's values and leaves out keys a newer chart added.

## Before you roll

1. **Diff.** `make helm-diff SELECTOR=name=<release>`. For a model release add
   `MODELS=models/<file>.yaml`; without it the release is not in the helmfile
   state and nothing matches. A pod-template change means a rollout.
2. **Check the release.** `make helm-status` (same `SELECTOR` and `MODELS`)
   should report `ok`, not `failed` or `pending-upgrade`.
3. **Check the engine's shutdown values** (see [Engine](#engine)).
4. **Check GPUs for the surge**: one replica's worth of `model.gpus` free.
5. **Check the routers.** autoconfig is Running, `kubectl get mr -A` shows the
   ModelRoute Ready, and exactly one pod carries `openresty-active=true` and one
   `cart-active=true`.
6. **Apply CRDs** if the autoconfig or llm-slo chart version changed (see
   [CRDs](#crds)).
7. **Start the [request loop](#verify)** and roll one component at a time.

## Engine

```bash
make helm-diff  SELECTOR=name=<model> MODELS=models/<file>.yaml
make helm-apply SELECTOR=name=<model> MODELS=models/<file>.yaml
kubectl -n <ns> rollout status deploy/<model> --timeout=30m
```

`helm-apply` returns before the model has loaded; wait on `rollout status`.
Changes to `hangWatcher.config.*` and `modelRoute.*` reload without restarting
the engine.

**Why the defaults drop requests.** CART picks up a new engine pod 65–80 s after
the rollout (openresty about 16 s), and until then keeps sending requests to the
old pod. With the chart defaults the old pod stops waiting for its requests
after 30 s and is shut down, which cut 6 in-flight streams on the test cluster.

**Set this in every SGLang model.** A pod being replaced then keeps serving
until it has no requests left, for up to 10 minutes (`drainSeconds`), which
covers both the router delay and long responses:

```yaml
terminationGracePeriodSeconds: 3600
lifecycle:
  forceShutdown: true
  preStop:
    drainSeconds: 600
    pollIntervalSeconds: 5
  preStopKill: true
```

**vLLM:**

```yaml
terminationGracePeriodSeconds: 3600
lifecycle:
  preStop:
    drainSeconds: 600
    pollIntervalSeconds: 5
```

A response still running after `drainSeconds` is cut. An idle pod exits within
seconds.

**No spare GPU for the surge** (not measured). The default is `maxSurge: 1,
maxUnavailable: 0`, so without a free replica's worth of GPUs the new pod stays
Pending.

| Replicas | Do |
| --- | --- |
| 2 or more | Set `strategy.rollingUpdate` to `maxSurge: 0, maxUnavailable: 1`; you lose one replica's capacity during the roll |
| 1 | Free a spare GPU, or schedule an outage window for the full model load |

Keep the `backend-svc` peer in `modelRoute.nginx.peers` (the chart renders it by
default): it is the route that still works if autoconfig is down while an engine
rolls. For LeaderWorkerSet models (not measured), surge needs a whole spare
group; use the same shutdown values.

## CART

CART rolls with its model release. It does not drop requests on a normal
rollout. Two things to know:

- **Changing CART settings** (`cart.baseConfig`: anything other than the worker
  list) is not picked up on reload, and blocks later worker-list updates until
  the pod restarts. After every such change run:

  ```bash
  kubectl -n <ns> rollout restart deploy/<release>-cart
  kubectl -n <ns> rollout status  deploy/<release>-cart
  ```

- **Don't force-delete the active CART pod** (`--grace-period=0 --force`). Its
  in-flight requests are lost and hang until an idle timeout (~300 s).

Expect the cache hit rate to dip after any CART restart or engine rollout. See
[cart.md](cart.md).

## openresty

```bash
make helm-diff  SELECTOR=name=openresty
make helm-apply SELECTOR=name=openresty
kubectl -n llm-route rollout status deploy/openresty
```

Image tags are pinned in `llmgateway/openresty.yaml.gotmpl` (`image.tag`,
`reload.image`, `ha.image`); bump them there. Route config and API keys reload
on their own; changing `bodylog.host` needs a rollout. Test through the Gateway,
not `kubectl port-forward` (it pins you to one pod).

## autoconfig

```bash
kubectl apply --server-side -f <autoconfig chart>/crds/   # see CRDs
make helm-apply SELECTOR=name=autoconfig
```

It is not on the request path; the routers keep their last config while it
restarts. Don't roll it and a model at the same time. When tearing down, delete
model releases before uninstalling autoconfig, or their ModelRoutes stay stuck
in `Terminating`.

## CRDs

Helm does not upgrade CRDs in a chart's `crds/` directory. If you skip this, new
fields are silently dropped. Apply them before upgrading the chart:

| Chart | CRDs in `crds/` |
| --- | --- |
| autoconfig | `modelroutes.routing.modelsphere.dev` |
| llm-slo-decision-gen (release `llm-slo`) | `llmslorequirements`, `jobslorequirements` |

```bash
helm pull modelsphere/autoconfig --version <new> --untar -d /tmp/ac
kubectl apply --server-side -f /tmp/ac/autoconfig/crds/
```

The llmscaleoperator CRD ships in `templates/` and helm upgrades it.

## Verify

Send traffic through the Gateway during the rollout and until about two minutes
after `rollout status` returns. Count non-200 responses **and** streams missing
`data: [DONE]`: a cut stream can still return 200.

```bash
URL=http://<gateway-address>:<nodeport>/<route>/v1/chat/completions
HOST=<gateway hostname>
AUTH="Authorization: Bearer <key>"
worker() {   # $1 = id, $2 = max_tokens
  while :; do
    code=$(curl -sS -N -m 600 -o /tmp/b.$1 -w '%{http_code}' \
      -H "Host: $HOST" -H "$AUTH" -H 'Content-Type: application/json' \
      -d "{\"model\":\"<served-model-name>\",\"stream\":true,\"max_tokens\":$2,\"ignore_eos\":true,
           \"messages\":[{\"role\":\"user\",\"content\":\"Count upwards.\"}]}" "$URL")
    echo "$(date +%s) w$1 code=$code done=$(grep -c '\[DONE\]' /tmp/b.$1)" >> /tmp/roll.log
  done
}
for w in 1 2 3 4; do worker $w 200 & done
for w in 5 6;     do worker $w 3000 & done
# roll the component, wait for rollout status + ~2 min, then:
kill $(jobs -p)
grep -v 'code=200 done=1' /tmp/roll.log | wc -l    # 0 = nothing dropped
```

## Measured

Test cluster: one node with 2× A10, sglang chart 0.8.0 with a small model and
one engine replica; CART, openresty and autoconfig with two replicas each. Load
was the loop above.

| Component | Action | Failed |
| --- | --- | --- |
| Engine | rollout, chart defaults (grace 60, endpointSync 5, drain 30) | **6** of ~1,500 |
| CART | `rollout restart` | 0 |
| CART | force-delete the active pod | 0 new; its 4 in-flight requests lost |
| openresty | `rollout restart` (long streams included) | 0 |
| autoconfig | `rollout restart` | 0 |
| bodylog | `rollout restart` | 0 |

Not measured: vLLM, LeaderWorkerSet, rollouts with no spare GPU, CRD upgrades,
API key rotation.
