---
title: Requesting queues
weight: 1
description: >
  ResourceClaims, CEL selectors, constraints, and claim configuration
---

## Claims and templates

Workloads request queues with the standard dynamic resource allocation (DRA) objects:

- A **ResourceClaimTemplate** stamps out a fresh claim per pod or VMI.
  This is the common case: each VM instance gets its own queue allocation.
- A **ResourceClaim** is a standalone object with its own lifecycle, shared by every pod that references it.

All examples below use templates.
The `spec.spec.devices` block is the same in both.

## Selecting devices

Requests draw from a DeviceClass (see [Concepts](../concepts.md)) and narrow the candidate set with Common Expression Language (CEL) selectors over the published attributes.
Select by card mode:

```yaml
selectors:
  - cel:
      expression: device.attributes["cex.ibm.com"].type == "cca"
```

Note: The type can be cca, ep11 or accel.

By card generation:

```yaml
selectors:
  - cel:
      expression: device.attributes["cex.ibm.com"].generation >= 8
```

By configured master key, pinning the claim to queues whose current AES master key matches a known verification pattern:

```yaml
selectors:
  - cel:
      expression: >-
        device.attributes["cex.ibm.com"].type == "cca" &&
        device.attributes["cex.cca.domain.ibm.com"].mkvp_aes_cur == "7f902215156ccaec"
```

## Co-locating queues in one domain

When a claim asks for several queues, a `matchAttribute` constraint forces them to agree on an attribute.
Requiring the same `apqi` places all queues in the same crypto domain, whichever domain has capacity - no hardcoded domain number:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: default
  name: dual-cca-ap-queue-vm
spec:
  spec:
    devices:
      requests:
        - name: cca-ap-queues
          exactly:
            deviceClassName: ap-queue.virtual-machine.ibm.com
            allocationMode: ExactCount
            count: 2
            selectors:
              - cel:
                  expression: device.attributes["cex.ibm.com"].type == "cca"
      constraints:
        - requests: ["cca-ap-queues"]
          matchAttribute: cex.ibm.com/apqi
```

## Multi-type claims

One claim can carry several requests with different selectors.
A constraint spanning all of them still forces a single shared domain:

```yaml
requests:
  - name: cca-ap-queues
    exactly:
      deviceClassName: ap-queue.virtual-machine.ibm.com
      allocationMode: ExactCount
      count: 2
      selectors:
        - cel:
            expression: device.attributes["cex.ibm.com"].type == "cca"
  - name: ep11-ap-queues
    exactly:
      deviceClassName: ap-queue.virtual-machine.ibm.com
      allocationMode: ExactCount
      count: 2
      selectors:
        - cel:
            expression: device.attributes["cex.ibm.com"].type == "ep11"
constraints:
  - requests: ["cca-ap-queues", "ep11-ap-queues"]
    matchAttribute: cex.ibm.com/apqi
```

For virtual machines, note how this meets KubeVirt: the driver prepares **one vfio-ap mediated device per claim**, whose matrix covers every allocated queue across all requests.
The VMI therefore lists **one `hostDevices` entry per claim**, with `requestName` naming any one of the claim's requests: that single mediated device already carries every allocated queue, so the guest sees all of them whichever request the entry names.
One entry per request fails, because libvirt accepts only one vfio-ap host device per guest (see [KubeVirt integration](../installation/kubevirt.md)).

## Claim configuration

The driver accepts an opaque configuration block on a claim, the `CryptoConfig`:

```yaml
spec:
  spec:
    devices:
      requests:
        - name: cca-queues
          exactly:
            deviceClassName: ap-queue.virtual-machine.ibm.com
            allocationMode: ExactCount
            count: 2
      config:
        - opaque:
            driver: cex-driver.ibm.com
            parameters:
              kind: CryptoConfig
              apiVersion: cex.ibm.com/v1alpha1
              controlDomainMode: "usage-and-control"
```

`controlDomainMode` decides whether the guest gets control access - the key-management side of a crypto domain, as opposed to the encrypt/decrypt usage side - to the domains of its allocated queues:

- `usage-and-control` (default): control access to exactly the domains the claim was allocated.
  What a CCA key-management VM needs.
- `usage-only`: usage access only, no control access.

Both values are bounded by the claim's own allocation.
Neither can reach a domain the claim was not given.
Two further modes (`all-control-domains` and a custom 256-bit mask) are defined but not enabled.
See the [claim configuration reference](../reference/claim-configuration.md) for details.

The configuration is strict: unknown fields, a wrong `kind`, or a wrong `apiVersion` are rejected.
Scope is the whole claim - one effective value, merged from the built-in default, then DeviceClass configuration, then claim configuration.
For container claims the field is ignored rather than rejected, so the same claim text works for either binding type.

The kernel has the last word: assigned control domains are intersected with the host's `ap_control_domain_mask`, so a domain the LPAR does not control never becomes effective in the guest.

## Verification

Watch a claim through its lifecycle:

```bash
kubectl get resourceclaims -n <namespace> -o wide
```

An allocated claim names its devices.
Each device name encodes machine, adapter, and domain (`<machineid>-<apid>-<apqi>`).
Inside a VM guest, `lszcrypt` shows the passed-through queues at those adapter-domain positions, and control-domain access is visible in `/sys/bus/ap/ap_control_domain_mask` read from the guest.

## Native container claims

Container passthrough uses DeviceClass `ap-queue.container.ibm.com` and requires the alpha `ContainerWorkload` feature gate (installed by the `feature-container-workload` Kustomize component).
The claim text is the same shape as a VM claim; only the DeviceClass name changes:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: cca-ap-queue-container
spec:
  spec:
    devices:
      requests:
        - name: cca-ap-queue
          exactly:
            deviceClassName: ap-queue.container.ibm.com
            allocationMode: ExactCount
            count: 1
            selectors:
              - cel:
                  expression: >-
                    device.attributes["cex.ibm.com"].type == "cca" &&
                    device.attributes["cex.ibm.com"].queue_status == "online"
```

A Pod consumes the claim through `resourceClaims` and `resources.claims`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cex-container-demo
spec:
  containers:
    - name: crypto
      image: registry.access.redhat.com/ubi9/ubi-minimal:latest
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: cex
            request: cca-ap-queue
  resourceClaims:
    - name: cex
      resourceClaimTemplateName: cca-ap-queue-container
```

A ready Pod sees `/dev/zcrypt` and `/dev/z90crypt` (both map to the filtered host node) and a shadow `/sys/bus/ap` / `/sys/devices/ap` that lists only the allocated APQNs.
`controlDomainMode` on `CryptoConfig` is ignored for container claims.
A full copy of this example lives under [`deploy/examples/pod-cex-container.yaml`](../../deploy/examples/pod-cex-container.yaml).
