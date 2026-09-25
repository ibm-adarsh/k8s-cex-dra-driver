---
title: Architecture
weight: 4
description: >
  How the driver, the scheduler, kubelet, and the AP bus fit together
---

The driver is a single kubelet plugin, deployed as a DaemonSet on every s390x node.
Each instance:

- scans the Adjunct Processor (AP) bus through sysfs on a fixed interval,
- publishes every visible AP queue as a device, one ResourceSlice per Crypto Express adapter on the node,
- prepares allocated devices when a workload starts.
  For a virtual-machine claim that means creating a vfio-ap mediated device covering the allocated queues.
  For a container claim (alpha `ContainerWorkload`) that means creating a filtered zcrypt device node and a shadow AP sysfs tree, returned to the runtime as a CDI device.
  Unprepare tears the prepared state down again.

Scheduling stays entirely in Kubernetes: the scheduler matches ResourceClaims against ResourceSlice devices using the DeviceClass and claim selectors.
The driver never picks devices itself.
It publishes honestly and prepares what the allocator chose.

The vocabulary used below - APQNs, DeviceClasses, and the attributes a device carries - is defined in [Concepts](../concepts.md).

## Overview

One picture of the whole system.
Each Crypto Express domain reaches its node as an AP queue, the driver publishes the queues to the control plane, and the scheduler allocates them to claims.
Color marks ownership: blue is the upstream Kubernetes DRA feature, green is this driver, gray is hardware and the Linux AP bus.

![Cluster overview: a Kubernetes cluster with three control-plane nodes and three nodes running the cex-dra-driver DaemonSet, above an IBM Z I/O drawer whose Crypto Express domains reach the nodes as APQNs, are published as ResourceSlices, and are allocated by the scheduler](diagrams/cluster-overview.svg)

The sections below walk the arrows of this picture one path at a time, each at full size.

## Publish path

The scan loop turns hardware into schedulable inventory.
It runs whether or not any workload wants a queue, which is why no user appears in this picture: the inventory exists before anyone asks.

![Publish path: the AP scanner reads the AP bus through sysfs and publishes one ResourceSlice device per AP queue, which the DeviceClass selects](diagrams/publish-path.svg)

Queues are grouped by the card they sit on: each Crypto Express adapter gets its own ResourceSlice, in a pool named `<node>-card<apid>`, holding one device per queue on that card.
A node with two adapters therefore publishes two slices.

Grouping by card is what keeps each slice inside the Kubernetes limit of 128 devices per ResourceSlice.
An adapter carries at most 85 domains, so one slice per card always fits, where one slice per node would overflow on a node with two well-populated cards.

## Allocate and prepare path

This is where the user enters: a workload operator creates a ResourceClaim, the scheduler matches it against the ResourceSlices the publish path put up, and the allocation comes back down to the node where the driver binds what the scheduler chose.
The node and the control plane sit in the same places as in the publish path, and the ResourceSlice is the same object the publish path ended on.
The crossing arrow points the other way.

![Allocate and prepare path: a workload operator creates a ResourceClaim, the scheduler picks devices from the published ResourceSlices, kubelet calls NodePrepareResources, and the driver rebinds the queue to vfio_ap and creates a vfio-ap mediated device for the VM](diagrams/prepare-path.svg)

A claim against `ap-queue.container.ibm.com` takes a different prepare path on the same published inventory.
The allocated queues stay bound to the host zcrypt stack.
The driver creates a filtered zcrypt character device under `/sys/class/zcrypt` (per-node `apmask`/`aqmask`, independent of the bus-level masks), builds a claim-scoped shadow of `/sys/bus/ap` and `/sys/devices/ap`, writes a CDI spec of kind `ibm.com/zcrypt`, and returns that CDI device ID to kubelet.
The container runtime injects `/dev/zcrypt` (and `/dev/z90crypt`) plus the shadow mounts into the Pod.
Unprepare destroys the node, removes the shadow tree, and deletes the CDI file.
Bus-level `apmask`/`aqmask` stay all-1s, so the same node can still serve vfio-ap VM claims.

## Queue binding states

A queue is bound to exactly one kernel driver at a time, and preparing a claim is what moves it.
This is the rebind edge from the prepare path in detail.

![Queue binding: a queue sits bound either to cex4queue in the zcrypt stack or to vfio_ap, and the driver moves it between them on prepare and unprepare](diagrams/queue-binding.svg)

Intent and current state are separate.
The DeviceClass on the claim is what the workload asked for.
The `cex.ibm.com/driver` attribute is what the queue is bound to at the moment you look.
A queue that no claim has prepared yet reports the binding it happens to have, not the one some claim would like it to have.
Container claims never flip that attribute: they consume queues that remain on `cex4queue`.

## Claim lifecycle

End to end, from the VM owner applying manifests to the mediated device reaching the guest.
The manifests themselves are in [KubeVirt integration](../installation/kubevirt.md).

![Claim lifecycle: a KubeVirt VM owner applies a VirtualMachine, the claim controller creates the ResourceClaim from its template, the scheduler allocates APQNs against the published ResourceSlice and binds the Pod to a node, kubelet reads the allocated claim and calls NodePrepareResources, the driver rebinds the queues and creates a vfio-ap mediated device, and KubeVirt attaches it to the guest](diagrams/claim-lifecycle.svg)

Reading it top to bottom tells you which actor is due to act next, which is what a VMI stuck in `Scheduled` usually comes down to.
