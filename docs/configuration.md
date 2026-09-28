# Configuring ModelSphere

What to change once the stack is running, and where.

## Where a setting lives

| What you are changing | File | Apply with |
|---|---|---|
| The cluster: which components run, registries, openresty, bodylog | `environments/<env>.yaml` | `make helm-apply ENV=<env>` |
| One model: engine, its router (CART), its route, its scaling | that model's values file | `helm upgrade --install <model> modelsphere/<engine> -n <ns> -f <values.yaml>` |

Most day-to-day changes are to a model's values file.

## Guides

| To | Read |
|---|---|
| Deploy a new model, or remove one | [deploy-a-model.md](deploy-a-model.md) |
| Set rate limits on a model's route, turn on API keys | [routing-and-rate-limiting.md](routing-and-rate-limiting.md) |
| Tune the cache-aware router (CART) | [cart.md](cart.md) |
| Let a model scale with load, or pin its replica count | [autoscaling.md](autoscaling.md) |
| Upgrade or reconfigure anything without dropping requests | [rolling-updates.md](rolling-updates.md) |


## Before changing a live cluster

- Preview first: `make helm-diff ENV=<env>`, or `helm diff upgrade` for a
  model. Neither changes anything.
- Check that `ENV` and `kubectl config current-context` point at the same
  cluster; nothing checks this for you.
- Upgrade with the full values file (`-f`), not `--reuse-values`.
