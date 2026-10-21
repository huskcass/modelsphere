# Console: opening the portal

ModelSphere Console is the web UI for the stack: users, roles and login, plus
the Model Serving pages backed by swissd, which the helmfile installs as its
own release beside it. The portal is on by default (`enabled.console` in
`environments/default.yaml`).

## First visit

The Service is a `NodePort`, so the UI is reachable as soon as the release is
up — no ingress needed:

```bash
NODE_PORT=$(kubectl -n console get svc console-console -o jsonpath='{.spec.ports[0].nodePort}')
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
echo "http://$NODE_IP:$NODE_PORT/"
```

(That is the chart's own address recipe; `helm status console -n console` prints
it too. Behind a firewall, `kubectl -n console port-forward svc/console-console
8080:8080` and open `http://localhost:8080/` instead.)

Sign in as `admin` with the password `P@88w0rd`. The first login asks you to
set a new password — do it; the default is public knowledge. (If your cluster
renamed the namespace, use `namespaces.console` from your environment file in
place of `console` above.)

## What you get

- **Identity**: users, roles, login records — the admin you just set a password
  for, and any further users you create.
- **Model Serving pages**: deploying a model (`docs/deploy-a-model.md`) lights
  them up — console talks to the `swissd` release over the cluster DNS, no
  extra wiring.

## Storage

swissd keeps its sqlite database — drafts, plans, the audit log — on a
PersistentVolumeClaim `swissd-data` (10Gi). Since chart 0.7.0 that claim is
always rendered; emptyDir is not an option. With the defaults
(`swissd.persistence.storageClass` and `existingClaim` empty, and
`storage.persistence` false) the claim carries no `storageClassName` and binds
the cluster's default StorageClass. Set `swissd.persistence.storageClass` to
name a class, or `swissd.persistence.existingClaim` to a PVC you created (no
class is needed then, and the size is ignored). The release's presync hook
runs only when `enabled.console` is true. It refuses to install when that
claim or class is missing; on the default path an already-Bound `swissd-data`
claim is enough. See [Storage](helmfile-deploy.md#storage).

## Swiss running elsewhere

If your swissd lives outside this helmfile, the bundled `swissd` release is
the wrong one to keep: turn the portal off (`enabled.console: false`) and set
`swiss.enabled: false` plus `externalSwiss` (url, proxyKey secret, profile) for
a console release of your own — the console chart's
`values-existing-stack.example.yaml` shows the shape.
