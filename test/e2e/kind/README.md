# test/e2e/kind

End-to-end test of the stack on a CPU-only kind cluster. `make test-e2e-kind`:

1. creates kind cluster `modelsphere-e2e` from `kind-config.yaml` (skipped if it exists);
2. installs the stack with `helmfile -e kind apply` (`environments/kind.yaml`);
3. waits for every workload in `console`, `llm-route`, `llm-scaler` and
   `llmscaleoperator-system`, then runs the Ginkgo specs;
4. deletes the cluster on success. A failure leaves it up for a look.

The `kind` environment turns off everything that needs GPUs, NICs, ceph disks or
the Prometheus operator, and shrinks openresty to one small replica. The whole
stack then requests about 2.5 CPU / 3 GiB and fits a 4 vCPU / 16 GB GitHub runner.
swissd is installed at cluster scope (the default `swiss.rbacScope: cluster`), and it creates
the model namespace (`llm-demo`) on the first install.

## Covered

17 specs. Every release the `kind` environment installs must be `deployed`
and its workloads ready; beyond that:

| Component | Status | Notes |
|---|---|---|
| swissd | tested | `/healthz`, `/readyz`, login, site profile, `mock-llm` in the catalog; plans, installs and uninstalls the model with both backends (`helm`, `llmsvc`) at cluster scope, creating `llm-demo` |
| console | tested | OAuth password login as the default admin |
| openresty | tested | `/v1/models` and a chat at `/<release>/v1/...` for each backend; one small replica |
| autoconfig | tested | the model's ModelRoute turns Ready and openresty serves its route |
| bodylog, bodylog-victorialogs | tested | the chat request (by nonce and model) is found in victorialogs |
| llmscaleoperator | tested | its LLMService CRD is available to swissd; the `llmsvc` backend deploys through it |
| bodylog-exporter | installed only | ServiceMonitor off |
| llm-slo | installed only | no scaling decision is checked |
| kube-prometheus-stack, alerts, dashboards, alert-webhook | off | no ServiceMonitors |
| rook-ceph, rook-ceph-cluster, ceph-csi-drivers | off | kind's `standard` local-path storage, no persistence |
| gpu-operator, dcgm metrics, network-operator, NicClusterPolicy, ascend-device-plugin | off | CPU-only node |
| cilium | off | kind's own CNI |
| lws, volcano, descheduler, node-problem-detector, condition2taint, llm-canary-operator | off | |

Not covered either: real GPUs and real engines (the model is `mock-llm`, no
weights), autoscaling, CART, and the helmfile `models:` path.

## Run

Needs docker, kind, kubectl, helm with helm-diff (`make helm-deps`), helmfile and Go.

```sh
make test-e2e-kind                          # create, install, test, delete
make test-e2e-kind KEEP_CLUSTER=1           # keep the cluster afterwards
make test-e2e-kind E2E_ARGS='-ginkgo.focus=llmsvc'
E2E_SKIP_INSTALL=1 make test-e2e-kind       # reuse an installed cluster, specs only
make test-e2e-kind KIND_PROXY_MODE=nftables # kernel without xt_statistic
make cleanup-test-e2e-kind
```

Images are pulled by the cluster from Docker Hub and ghcr.io, so a cold run
spends most of its time there.
