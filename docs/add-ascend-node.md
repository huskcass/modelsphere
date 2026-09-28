# Adding an Ascend NPU node

How to join a node that is **not** an Ubuntu/amd64/NVIDIA box — an Ascend NPU
host, aarch64, vendor OS — to one of our kubeadm clusters. The Ubuntu path
(`setup_k8s.yaml`, `network_accesslator.yaml`, `nvidia_audit.yaml`) assumes apt
and the NVIDIA operators; these nodes come with a vendor OS, a vendor driver and
a vendor container-runtime hook that we keep as they are.

**Read [the node prerequisites](install.md#1-prerequisites) first.** The
kernel one (5.10+) is the one that actually stops nodes: an older vendor kernel
has to be replaced or reinstalled before anything here applies, and that is a
host-lifecycle job outside this repo.

There is no separate tooling for this. Node prep is the playbook every node
runs; the rest is kubeadm and kubectl, spelled out below so the reasons travel
with the commands. [`ascend/`](../ascend) holds only what is genuinely vendor
specific: the MindCluster chart, its values, and a smoke pod.

| Step | How | Runs on | Mutates |
|---|---|---|---|
| 0. Requirements | kernel 5.10+, and the rest of the table below | node | no |
| 1. Prep node | `make setup-k8s-*` (Ansible, `k8s_install_method=binary`) | control machine | yes |
| 2. Join | `kubeadm join --config` (below) | node | yes |
| 3. Label the node, then the device plugin | `kubectl label`, then helmfile | cluster | yes |
| 4. Verify | `kubectl` + the smoke pod (below) | cluster | no |

Worked example throughout: one **Atlas 800T A2** (8× Ascend 910B3, Kylin V10,
kernel 4.19.90, aarch64) joining a cluster running k8s v1.36.3, Cilium 1.20 and
containerd 2.x on its other nodes. Where this document says a thing was
measured, that is the host it was measured on.

---

## 0. Requirements

| Requirement | Detail |
|---|---|
| **kernel 5.10+** | Cilium refuses to start below it, and no flag, `CiliumNodeConfig` or per-node override skips the check. **A vendor 4.19 with eBPF backports is still a fail** -- Kylin V10's passes every generic eBPF probe and misses six of the exact (program type, helper) pairs Cilium asks for, so the node joins and cilium-agent then crash-loops. Replace the kernel first; that is a host-lifecycle job outside this repo |
| aarch64 | fine: cilium, cilium-envoy, node-problem-detector and node-exporter are multi-arch |
| cgroup v1 | fine, with `failCgroupV1: false` in step 2's join patches; kubelet only warns |
| containerd 1.7 | works on k8s 1.36, and kubeadm warns the fallback goes away in **1.37** -- upgrade containerd before the cluster does |
| NVIDIA DaemonSets | do not land here: gated by `nvidia.com/gpu.deploy.*` labels and `pci-15b3`, and Huawei NICs (`19e5`) match neither |

`uname -r` settles the kernel. To check it exactly rather than by version number,
run the bpftool in the cilium image your cluster runs -- an empty output is a
fail, not a pass -- and compare against a node that already runs cilium:

```bash
docker run --rm --privileged --net host quay.io/cilium/cilium:<your version> \
    bpftool feature probe kernel > /tmp/probe.txt
```

The pairs it must list are in Cilium's own `pkg/datapath/linux/requirements.go`.

---

## 1. Prep the node

The same Ansible play every other node goes through — `setup_k8s.yaml` — with
the install method switched over. There is no separate script for this: node
prep had one implementation already, and a second one in bash would have drifted
from it the first time someone fixed a containerd setting in only one place.

Ansible needs a Python on the target that ansible-core supports — 3.8 or newer
for ansible-core 2.18. This is where a vendor OS bites: Kylin V10 ships 3.7.9 and
its repo has nothing newer, while CANN and `npu-smi` are built against that very
interpreter, so replacing it is out of the question. Install a self-contained one
next to it and point Ansible at that; nothing else on the host sees it.

```bash
# on the node, once -- chicken-and-egg: ansible cannot install what it needs to run
VER=3.11.16 TAG=20260901
curl -fsSL \
  "https://github.com/astral-sh/python-build-standalone/releases/download/$TAG/cpython-$VER+$TAG-aarch64-unknown-linux-gnu-install_only.tar.gz" \
  | tar -xz -C /opt/ansible-python --strip-components=1   # mkdir -p it first
/opt/ansible-python/bin/python3 -V
```

Removing `/opt/ansible-python` reverts it completely. An Ubuntu node needs none
of this; its system python is already new enough.

Put the node in an inventory. [`inventory-ascend.ini.example`](../inventory-ascend.ini.example)
is the shape, with every variable that differs from an Ubuntu node and why it is
there; copy it and fill in the host.

Do not skip `http_proxy` there if your nodes need one. A network that reaches
dl.k8s.io but not github fails in a way that misleads: kubeadm, kubelet and
kubectl install fine and only the crictl download times out, which reads like a
flaky network rather than a missing setting.

Then, against whichever inventory holds it:

```bash
make setup-k8s-online  INVENTORY=<inventory> LIMIT=<node>
make setup-k8s-offline INVENTORY=<inventory> LIMIT=<node>
make setup-mirror      INVENTORY=<inventory> LIMIT=<node>   # certs.d mirrors
```

What the `binary` path does differently, and nothing else does:

- `kubeadm`/`kubelet`/`kubectl` from `dl.k8s.io` into `/usr/local/bin` (any distro,
  any arch — `k8s_arch` comes from `ansible_architecture`), crictl from GitHub,
  and the kubelet unit + `10-kubeadm.conf` the deb would have provided. Any other
  drop-in is removed: KubeKey leaves `--hostname-override` in one, which silently
  overrides what `kubeadm join` writes.
- containerd's `sandbox_image` set from `kubeadm config images list` (the vendor
  host's built-in pause is usually older than the cluster's).
- `k8s_accel_runtime=ascend` registers `ascend-docker-runtime` as a runc.v2
  runtime and makes it `default_runtime_name`.

Everything else — swap, modules, sysctl, `/etc/hosts`, `SystemdCgroup`, the 20m
pull timeout, `certs.d` — is the shared path, unchanged from the Ubuntu nodes.

> The containerd config is regenerated from `containerd config default` on every
> run, which is deliberate here: Ascend's installer had pointed runc at the
> removed v1 shim (`io.containerd.runtime.v1.linux`), which containerd 2.x will
> not start with. Patching the vendor's file would have kept that.

## 2. Join

Not the bare `kubeadm join` line from `kubeadm-cluster-init.md` — an older host
needs kubelet overrides, and they have to be in place *during* the join. Get a
token on the control plane (`kubeadm token create --ttl 1h --print-join-command`),
then on the node:

```bash
mkdir -p /etc/kubernetes/join-patches
cat > /etc/kubernetes/join-patches/kubeletconfiguration+strategic.yaml <<'EOF'
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failCgroupV1: false                 # only on a cgroup v1 host
resolvConf: /etc/resolv.conf        # only without systemd-resolved
EOF

cat > /etc/kubernetes/join.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "<cp-endpoint:6443>"
    token: "<token>"
    caCertHashes: ["sha256:<hash>"]
nodeRegistration:
  name: "<node>"                    # see the naming note below
  criSocket: unix:///run/containerd/containerd.sock
  taints:
    - {key: "huawei.com/Ascend910", value: "compute-only", effect: "NoSchedule"}
  kubeletExtraArgs:
    - name: node-ip
      value: "<node ip on the cluster network>"
  ignorePreflightErrors:            # only on a cgroup v1 host
    - SystemVerification
patches:
  directory: /etc/kubernetes/join-patches
EOF

kubeadm join --config /etc/kubernetes/join.yaml && rm -f /etc/kubernetes/join.yaml
```

Why each piece:

- **node name** follows `<accelerator>-<last IP octet>` -- `ascend910b-207`, say --
  instead of the vendor hostname, which on these hosts is whatever the vendor set.
- **taint at registration**, so no general pod lands in the window before the
  labels go on. Same idea as `script/taint_gpu_nodes.sh` for NVIDIA nodes.
- **kubelet overrides via `patches:`, not by editing afterwards.** kubeadm
  downloads the cluster-wide KubeletConfiguration during the join; by the time
  `/var/lib/kubelet/config.yaml` exists, kubelet is already crash-looping.
  - `failCgroupV1: false` — kubelet ≥ 1.35 refuses to start on cgroup v1 otherwise.
  - `resolvConf: /etc/resolv.conf` — the cluster config points at
    `/run/systemd/resolve/resolv.conf`, which a host without systemd-resolved does
    not have, and then **every** pod sandbox fails with
    `open /run/systemd/resolve/resolv.conf: no such file`. The error names DNS,
    the cause is the file.

If the node is already joined, do not join again — put those same two keys into
`/var/lib/kubelet/config.yaml` and restart kubelet.

## 3. Label the node and deploy the device plugin

Labels first — the device plugin's DaemonSet selects on `workerselector`, and
the `nvidia.com/gpu.deploy.*` ones keep the GPU operator's DaemonSets off a node
that has no NVIDIA card (its validator and toolkit pods otherwise land here and
fail):

```bash
kubectl label node <node> --overwrite \
    accelerator=huawei-Ascend910 \
    workerselector=dls-worker-node \
    node.modelsphere.dev/accelerator=ascend-910b \
    nvidia.com/gpu.deploy.operator-validator=false \
    nvidia.com/gpu.deploy.container-toolkit=false \
    nvidia.com/gpu.deploy.device-plugin=false \
    nvidia.com/gpu.deploy.gpu-feature-discovery=false \
    nvidia.com/gpu.deploy.dcgm-exporter=false \
    nvidia.com/gpu.deploy.mig-manager=false
```

The compute-only taint is set at registration (step 2's `JoinConfiguration`); to
reconcile it later use the same script the NVIDIA nodes use, pointed at this resource:

```bash
RESOURCE=huawei.com/Ascend910 bash script/taint_gpu_nodes.sh          # dry-run
RESOURCE=huawei.com/Ascend910 bash script/taint_gpu_nodes.sh --apply
```

Then the plugin itself:

```bash
make ENV=<cluster> helm-diff SELECTOR=name=ascend-device-plugin
make ENV=<cluster> helm-apply SELECTOR=name=ascend-device-plugin
```

The plugin is a helmfile release like every other component, and the chart is
**Huawei's own** (`ascend/mindcluster-deploy-tool-26.1.0.tgz`, MindCluster 26.1.0):

- It is vendored into the repo because it ships as a tgz attached to a GitCode
  release, not from a helm repo — `Ascend-helm-deploy-tool_<ver>_linux.zip`.
  It is kept **as that .tgz**, byte for byte, so "we run the vendor chart
  unmodified" is something a reviewer can check rather than take on trust:

      shasum -a 256 ascend/mindcluster-deploy-tool-26.1.0.tgz
      # 966e9abc74c8dce157fc70c48f99ab563eaa3245a4d5600d0d9f9215b07b2440

  Unpacked it is 131 files nobody reviews in a diff, and a local edit would be
  invisible. To inspect it: `tar xzf ascend/mindcluster-deploy-tool-26.1.0.tgz`.
- It creates the `mindx-dl` and `cluster-system` namespaces unconditionally, with
  `helm.sh/resource-policy: keep` so they survive an uninstall. With only the
  device plugin enabled they just sit there empty. Upstream behaviour; left alone
  on purpose, since patching it would forfeit the point above.
- It is an umbrella over all of MindCluster (device plugin, NodeD, ClusterD,
  NPU-Exporter, Ascend Operator, Infer Operator, its own Volcano build, RDMA
  plugin). `ascend/overrides.yaml` enables **only the device plugin**. Note that
  `ascend-for-volcano` would replace the cluster's existing Volcano release, so
  it is not something to switch on casually.
- `volcanoType: false`, so whole cards are allocated by the default
  kube-scheduler; the chart defaults to `true`, i.e. expecting Volcano.
- The image is pulled from **Docker Hub** (`ascendai/ascend-k8sdeviceplugin`,
  multi-arch). AscendHub's public SWR only serves old tags — v7.1.RC1 and
  v6.0.0 pull anonymously, the v26.1.0 this chart wants does not.
- The chart's DaemonSet selects on `workerselector=dls-worker-node`, Huawei's
  own label convention — hence that label above, next to our own `accelerator`.
- `enabled.ascendDevicePlugin` is **off by default**; turn it on in the
  environment of a cluster that has Ascend nodes.

`smoke-pod.yaml` requests one card and prints `ASCEND_VISIBLE_DEVICES`,
`/dev/davinci*` and `npu-smi` from inside the container — a pod gets one card,
with only that `/dev/davinciN` and `/dev/davinci_manager` mounted.

## 4. Verify

```bash
kubectl get node <node>                            # Ready
kubectl get pods -A -o wide --field-selector spec.nodeName=<node>
kubectl get node <node> -o jsonpath='{.status.allocatable}' | jq '."huawei.com/Ascend910"'
kubectl -n kube-system exec <cilium-pod-on-the-node> -c cilium-agent -- cilium-dbg status
```

Three things worth checking explicitly -- each has looked fine while being wrong:

- `cilium-dbg status` reporting `Cluster health 0/0 reachable` is **not** health —
  it means cilium-health has not probed yet. Require a non-zero denominator.
- Node Ready says nothing about the datapath. Run a pod **on this node** and have
  it reach the `kubernetes` Service ClusterIP (kube-proxy replacement) and a pod
  on another node (tunnel). Pick the peer from the pod CIDR — a hostNetwork pod
  carries the node IP and testing against it proves nothing.
- Reading allocatable with jsonpath needs the dots in `huawei.com/Ascend910`
  escaped; unescaped it returns empty, which reads exactly like "no cards".

Then the card itself:

```bash
kubectl apply -f ascend/smoke-pod.yaml && kubectl logs ascend-smoke
```
