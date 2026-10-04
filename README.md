# kube-ups-taint

A Kubernetes controller that reads UPS state from [Network UPS Tools](https://networkupstools.org/) (NUT) servers and taints nodes so pods are shed gracefully during a power outage.

## Overview

The controller polls one or more NUT servers for `ups.status`, `battery.charge` and `battery.runtime`. It maps each UPS to the nodes it powers and keeps their taints in sync. Pods choose how far into an outage they keep running through ordinary tolerations. A pod without tolerations stops scheduling onto a node as soon as its UPS goes on battery and is evicted at the first battery step. A pod that should run until the end tolerates every step.

It augments, and does not replace, NUT's own shutdown handling (`upsmon`).

## Taints

All keys are prefixed with `kube-ups-taint.josh.github.io/`.

| Taint | When |
| ----- | ---- |
| `status=on-battery:NoSchedule` | UPS reports `OB` |
| `status=low-battery:NoExecute` | UPS reports `OB LB` |
| `status=unknown:NoSchedule` | UPS reports neither `OL` nor `OB` (e.g. `OFF`, `BYPASS`), or has been unreachable for longer than `staleAfter` |
| `battery-below-<N>:NoExecute` | Charge below `N`% for each `N` in `batterySteps` |
| `runtime-below-<S>:NoExecute` | Runtime below `S` seconds for each `S` in `runtimeSteps` |
| `battery-charge=<0-100>:NoExecute` | Charge percentage, when `batteryChargeTaint` is enabled |
| `battery-runtime=<seconds>:NoExecute` | Runtime in seconds, when `batteryRuntimeTaint` is enabled |

- **When taints apply:** level taints only apply while the UPS is not online.
- **During an outage they only ratchet down:**
  - A `battery-below-50` taint stays even if the charge reading jitters back to 51%.
  - The numeric values only decrease.
- **When power returns:** as soon as the UPS reports `OL` (including `OL CHRG` at low charge), every taint under the prefix is removed.
- **If a NUT server is unreachable:**
  - The existing taints are left alone.
  - Once the UPS has gone unobserved for `staleAfter`, `status=unknown:NoSchedule` is added. Taints are never removed while the UPS can't be read.
- **Shutdown:** taints are left in place when the controller exits.

## Annotations and events

Each mapped node is annotated with the latest reading:

- `kube-ups-taint.josh.github.io/ups`: UPS entry name
- `kube-ups-taint.josh.github.io/ups-status`: raw `ups.status`
- `kube-ups-taint.josh.github.io/battery-charge`: charge percentage
- `kube-ups-taint.josh.github.io/battery-runtime`: runtime in seconds
- `kube-ups-taint.josh.github.io/observed-at`: time of the last successful read

`observed-at` is refreshed at least every `staleAfter / 4` and is shared by all replicas. A single replica that loses its connection to NUT won't mark a UPS stale while another replica can still read it.

Whenever a node's taints change, an Event is recorded on the Node with one of these reasons: `OnBattery`, `LowBattery`, `BatteryLevel`, `Online` or `UPSUnreachable`.

```bash
kubectl get events --field-selector involvedObject.kind=Node,source=kube-ups-taint
```

## Tolerating battery levels

A pod that should keep running until the battery is below 25%, with `batterySteps: [25, 50, 75]`:

```yaml
tolerations:
  - key: kube-ups-taint.josh.github.io/status
    operator: Equal
    value: on-battery
  - key: kube-ups-taint.josh.github.io/battery-below-75
    operator: Exists
  - key: kube-ups-taint.josh.github.io/battery-below-50
    operator: Exists
```

The same pattern works with runtime. With `runtimeSteps: [600, 300]`, this pod runs until less than 5 minutes remain:

```yaml
tolerations:
  - key: kube-ups-taint.josh.github.io/status
    operator: Equal
    value: on-battery
  - key: kube-ups-taint.josh.github.io/runtime-below-600
    operator: Exists
```

### Numeric tolerations

`batteryChargeTaint` and `batteryRuntimeTaint` publish the raw level as a numeric taint value. That suits the `Gt` and `Lt` toleration operators ([KEP-5471](https://github.com/kubernetes/enhancements/issues/5471)). `Gt` matches when the taint value is *greater than* the toleration value. This pod runs while charge is above 30%:

```yaml
tolerations:
  - key: kube-ups-taint.josh.github.io/status
    operator: Equal
    value: on-battery
  - key: kube-ups-taint.josh.github.io/battery-charge
    operator: Gt
    value: "30"
    effect: NoExecute
```

Things to know before using it:

- **It's alpha.** The `TaintTolerationComparisonOperators` feature gate is alpha and off by default as of Kubernetes 1.37.
- **Enable the gate everywhere.** It has to be on in kube-apiserver, kube-scheduler, kube-controller-manager *and* the kubelet. Otherwise matching pods are rejected or evicted immediately.
- **Leave out `tolerationSeconds`.** A matching toleration with `tolerationSeconds` still evicts the pod after that many seconds. Pods are evicted immediately once the value stops matching.
- **Pods without a matching toleration.** Once the UPS goes on battery, a pod with no `Gt`/`Lt` toleration for this key is evicted, unless it tolerates the key with `operator: Exists`.

## Surviving the outage

The controller has to outlive the pods it is evicting.

- **Self-tolerations.** The chart tolerates all of the controller's own taints with `operator: Exists`, so it is never evicted by them and can be rescheduled onto nodes that are on battery.
- **Fast failover.** `node.kubernetes.io/not-ready` and `unreachable` are tolerated for `nodeFailureTolerationSeconds` (5s) instead of the default 300s. When a node loses power, the replacement pod starts after the node controller's grace period (about 40–50s) plus 5s.
- **Spreading.** Replicas prefer different hosts and are spread across the `kube-ups-taint.josh.github.io/ups` label, so with `replicaCount: 2` they land behind different UPS units when possible.
- **Priority.** The pod runs as `system-cluster-critical` so it can preempt other workloads when it needs to reschedule.

Replicas don't use leader election. Every replica computes the same taints, and node updates use optimistic concurrency, so running several is safe.

## Installation

1. Label the nodes powered by each UPS:

```bash
kubectl label node node-1 node-2 node-3 kube-ups-taint.josh.github.io/ups=rack-a
```

2. If the NUT server requires a login, create a secret with `username` and `password` keys:

```bash
kubectl create secret generic nut-rack-a \
 --from-literal=username="<nut-user>" \
 --from-literal=password="<nut-password>"
```

3. Install the chart:

```bash
helm install kube-ups-taint ./charts/kube-ups-taint \
 --set 'ups[0].name=rack-a' \
 --set 'ups[0].address=ups@192.0.2.10:3493' \
 --set 'ups[0].secretName=nut-rack-a'
```

Several UPS units are listed as separate entries. An entry can match nodes with `nodeSelector` instead of the default `kube-ups-taint.josh.github.io/ups=<name>` label:

```yaml
replicaCount: 2
ups:
  - name: rack-a
    address: ups@192.0.2.10
  - name: rack-b
    address: eaton@192.0.2.11:3493
    nodeSelector: topology.kubernetes.io/zone=b
```

A node that matches more than one entry is skipped and an error is logged.

## Configuration

| Value                                     | Description                                                             | Default                       |
| ----------------------------------------- | ----------------------------------------------------------------------- | ----------------------------- |
| `image.repository`                        | Container image repository                                              | `ghcr.io/josh/kube-ups-taint` |
| `image.tag`                               | Container image tag                                                     | Chart `appVersion`            |
| `image.pullPolicy`                        | Image pull policy                                                       | `IfNotPresent`                |
| `replicaCount`                            | Number of controller replicas                                           | `1`                           |
| `ups[].name`                              | UPS entry name, used as the node label value                            | (required)                    |
| `ups[].address`                           | NUT address as `upsname@host[:port]`                                    | (required)                    |
| `ups[].nodeSelector`                      | Label selector for nodes instead of the default label                   | `""`                          |
| `ups[].secretName`                        | Secret with `username` and `password` keys for NUT login                | `""`                          |
| `controller.interval`                     | Polling interval                                                        | `15s`                         |
| `controller.staleAfter`                   | How long a UPS can go unread before `status=unknown`                    | `2m`                          |
| `controller.maxConsecutiveFailures`       | Exit after this many consecutive failed runs                            | `10`                          |
| `controller.batterySteps`                 | Charge percentages that get `battery-below-<N>` taints                  | `[50]`                        |
| `controller.runtimeSteps`                 | Runtime seconds that get `runtime-below-<S>` taints                     | `[]`                          |
| `controller.batteryChargeTaint`           | Publish numeric `battery-charge` taint                                  | `false`                       |
| `controller.batteryRuntimeTaint`          | Publish numeric `battery-runtime` taint                                 | `false`                       |
| `controller.debug`                        | Enable debug logging                                                    | `false`                       |
| `serviceAccount.create`                   | Create a ServiceAccount                                                 | `true`                        |
| `serviceAccount.name`                     | ServiceAccount name                                                     | Generated                     |
| `priorityClassName`                       | Pod priority class                                                      | `system-cluster-critical`     |
| `nodeFailureTolerationSeconds`            | Seconds to tolerate `not-ready` and `unreachable` nodes                 | `5`                           |
| `tolerations`                             | Additional tolerations                                                  | `[]`                          |
| `affinity`                                | Pod affinity, replaces the default hostname anti-affinity               | `{}`                          |
| `topologySpreadConstraints`               | Replaces the default spread across UPS labels                           | `[]`                          |
| `networkPolicy.ingress.enabled`           | Restrict ingress traffic                                                | `false`                       |
| `networkPolicy.ingress.rules`             | Ingress rules                                                           | `[]`                          |
| `networkPolicy.egress.enabled`            | Restrict egress traffic                                                 | `false`                       |
| `networkPolicy.egress.dns.enabled`        | Allow DNS on port 53                                                    | `true`                        |
| `networkPolicy.egress.dns.to`             | DNS peers                                                               | `[]` (any)                    |
| `networkPolicy.egress.kubeApiServer.enabled` | Allow the Kubernetes API server                                      | `true`                        |
| `networkPolicy.egress.kubeApiServer.hosts`   | API server addresses                                                 | `[]`                          |
| `networkPolicy.egress.kubeApiServer.port`    | API server port                                                      | `6443`                        |
| `networkPolicy.egress.nut.enabled`        | Allow NUT servers                                                       | `true`                        |
| `networkPolicy.egress.nut.hosts`          | NUT server addresses                                                    | `[]`                          |
| `networkPolicy.egress.nut.port`           | NUT server port                                                         | `3493`                        |
| `networkPolicy.egress.rules`              | Additional egress rules                                                 | `[]`                          |
| `resources`                               | Container resource requests and limits                                  | See values                    |

The controller re-reads its config every interval, so changing values only requires a `helm upgrade`. The pod does not restart. A change to `batterySteps` or `runtimeSteps` also updates the pod's self-tolerations, which does roll the pod.

See [values.yaml](./charts/kube-ups-taint/values.yaml) for all options.

## Network policy

Network policy is disabled by default. Ingress and egress are controlled independently. If you enable a direction with an empty `rules` list, all traffic in that direction is denied.

The controller serves nothing, so ingress can be denied entirely.

Egress is described as peers. The chart knows which port each peer uses; you supply only the addresses:

```yaml
networkPolicy:
  ingress:
    enabled: true
  egress:
    enabled: true
    kubeApiServer:
      hosts:
        - 192.0.2.1
    nut:
      hosts:
        - 192.0.2.10
```

Bare IPv4 and IPv6 addresses get `/32` and `/128` appended, bracketed IPv6 is accepted, and CIDRs pass through. Enabling a peer without addresses fails the render.

- **`kubeApiServer`:** the API server's real address and port (usually 6443), not the `kubernetes` Service's 443. Network policy is evaluated after the Service is DNAT'd.
- **`nut`:** each NUT server, on port 3493.
- **`dns`:** port 53, to any destination unless `to` is set.

## Requirements

- One or more NUT servers (`upsd`) reachable from the cluster on port 3493
- Nodes labeled with their UPS entry name, or an entry with a `nodeSelector`
- Kubernetes 1.29 or newer
