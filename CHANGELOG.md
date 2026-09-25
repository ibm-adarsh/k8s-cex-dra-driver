# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

Features graduate on their own, independently of the release that ships them and a gate flip is where behavior changes for an existing user.
Every release entry therefore also records the feature gates it changed: the gate name, its stage, and whether it defaults on.

## 1.0.0-alpha.0

First public pre-release, a Technical Preview with no compatibility promise.

### Added

- Kubelet plugin publishing each AP queue (APQN) as a DRA device, with attributes for CEL selection.
- KubeVirt virtual machine passthrough over vfio-ap, through the `ap-queue.virtual-machine.ibm.com` DeviceClass.
- Native container passthrough over filtered zcrypt device nodes and shadow AP sysfs, through the `ap-queue.container.ibm.com` DeviceClass (alpha, off by default).
- Claim configuration through the `CryptoConfig` envelope (`cex.ibm.com/v1alpha1`), carrying `controlDomainMode`.
- Feature gates on every driver deployable, through `--feature-gates` or `FEATURE_GATES`.
- Kustomize deployment, unprivileged by default.
- Documentation set under `docs/`.
- Example Pod and CDI manifests under `deploy/examples/`.

### Feature gates

| Gate                     | Stage | Default | Effect when enabled                                                                                          |
| ------------------------ | ----- | ------- | ------------------------------------------------------------------------------------------------------------ |
| `ContainerWorkload`      | alpha | off     | Native Pod passthrough via filtered zcrypt + CDI (`ibm.com/zcrypt`). Requires `/sys/class/zcrypt` on the node. |
| `VirtualMachineWorkload` | beta  | on      | The VM passthrough path. Disabling it frees a node from the vfio_ap/mdev checks.                             |
