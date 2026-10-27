# test/e2e

End-to-end test of the stack on a CPU-only kind cluster. `make test-e2e`:

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

- all nine releases deployed and their workloads ready;
- swissd: `/healthz`, `/readyz`, login, the site profile (`testdata/profile.yaml`),
  `mock-llm` in the public catalog;
- console: OAuth login as the default admin;
- for each swissd backend (`helm`, `llmsvc`): deploy `mock-llm` (no GPU, no weights),
  `/v1/models` on its Service, a chat through openresty on the route autoconfig
  publishes, the request recorded by bodylog in victorialogs, then uninstall.

Not covered: kube-prometheus-stack and ServiceMonitors (off in the profile),
scaler decisions (need GPU nodes), CART, and the helmfile `models:` path.

## Run

Needs docker, kind, kubectl, helm with helm-diff (`make helm-deps`), helmfile and Go.

```sh
make test-e2e                          # create, install, test, delete
make test-e2e KEEP_CLUSTER=1           # keep the cluster afterwards
make test-e2e E2E_ARGS='-ginkgo.focus=llmsvc'
E2E_SKIP_INSTALL=1 make test-e2e       # reuse an installed cluster, specs only
make test-e2e KIND_PROXY_MODE=nftables # kernel without xt_statistic
make cleanup-test-e2e
```

Images are pulled by the cluster from Docker Hub and ghcr.io, so a cold run
spends most of its time there.
