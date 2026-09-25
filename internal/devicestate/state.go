// Package devicestate tracks which AP crypto queues (APQNs) the node offers
// for scheduling and builds the resourceslice.DriverResources that the kubelet
// plugin publishes as ResourceSlices to the API server.
package devicestate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"k8s-cex-dra-driver/internal/binding"
	"k8s-cex-dra-driver/internal/cdi"
	"k8s-cex-dra-driver/internal/cryptoconfig"
	"k8s-cex-dra-driver/internal/features"
	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/metadata"
	"k8s-cex-dra-driver/internal/shadowsysfs"
	"k8s-cex-dra-driver/internal/zcryptnode"
)

const (
	// DeviceClassVM is the DeviceClass for queues passed through to a VM via
	// vfio-ap mdev.
	DeviceClassVM = "ap-queue.virtual-machine.ibm.com"
	// DeviceClassContainer is the DeviceClass for queues exposed directly to a
	// container.
	DeviceClassContainer = "ap-queue.container.ibm.com"
)

// AllocatableDevices maps device name to Device.
type AllocatableDevices map[string]resourceapi.Device

// DeviceState tracks allocatable devices and driver resources. A single
// instance per kubelet plugin process is shared between the scan loop
// (CompareAndUpdate, DriverResources) and the kubelet DRA RPC handlers
// (Prepare, Unprepare). These may run concurrently, so each method holds
// the state lock while reading or writing the mutable fields.
type DeviceState struct {
	// mu is the state lock, a capacity-1 channel rather than sync.Mutex so
	// the RPC handlers can bound their wait with the kubelet's per-call
	// context (lockCtx). See lock, unlock, lockCtx.
	mu              chan struct{}
	driverResources resourceslice.DriverResources
	allocatable     AllocatableDevices
	// updateCount is an in-memory counter, incremented each time
	// CompareAndUpdate detects a change. It is unrelated to the
	// apiserver-tracked ResourceSlice.Spec.Pool.Generation, which is
	// managed by the kubeletplugin helper and persists across driver
	// restarts. Used only for log correlation.
	updateCount   int64
	nodeName      string
	previousHash  string
	driverName    string
	cdiRoot       string
	pluginDataDir string
}

// New creates a new DeviceState.
func New(nodeName, driverName, cdiRoot, pluginDataDir string) *DeviceState {
	return &DeviceState{
		mu: make(chan struct{}, 1),
		driverResources: resourceslice.DriverResources{
			Pools: make(map[string]resourceslice.Pool),
		},
		allocatable:   make(AllocatableDevices),
		nodeName:      nodeName,
		driverName:    driverName,
		cdiRoot:       cdiRoot,
		pluginDataDir: pluginDataDir,
	}
}

// lock acquires the state lock unconditionally. For the scan-loop paths,
// which have no RPC deadline to honor.
func (s *DeviceState) lock() { s.mu <- struct{}{} }

// unlock releases the state lock.
func (s *DeviceState) unlock() { <-s.mu }

// lockCtx acquires the state lock, giving up when ctx ends first. The DRA RPC
// handlers pass the kubelet's per-call context here. Why: the sysfs writes in
// Prepare and Unprepare can block in the kernel with no timeout, in an
// uninterruptible sleep no signal can stop. An RPC blocked that way holds the
// lock forever. Nothing can recover that holder, but the RPCs queued behind
// it can still be saved: with a deadline on the lock wait they fail fast
// instead of piling up after the kubelet has already given up on them.
func (s *DeviceState) lockCtx(ctx context.Context) error {
	// Checked first so an already-expired context never acquires: the
	// select below picks randomly when both channels are ready.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("device state lock: %w", err)
	}
	select {
	case s.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("device state lock: %w", ctx.Err())
	}
}

// CompareAndUpdate compares devices with previous state and updates if changed.
// Returns true if the state was updated.
func (s *DeviceState) CompareAndUpdate(devicesByCard map[string][]resourceapi.Device) bool {
	s.lock()
	defer s.unlock()

	newHash := computeDevicesHash(devicesByCard)
	if newHash == s.previousHash {
		return false
	}

	s.allocatable = make(AllocatableDevices)
	for _, cardDevices := range devicesByCard {
		for _, device := range cardDevices {
			s.allocatable[device.Name] = device
		}
	}

	pools := make(map[string]resourceslice.Pool)
	for cardID, cardDevices := range devicesByCard {
		poolName := fmt.Sprintf("%s-%s", s.nodeName, cardID)
		pools[poolName] = resourceslice.Pool{
			Slices: []resourceslice.Slice{{Devices: cardDevices}},
		}
	}
	s.driverResources = resourceslice.DriverResources{Pools: pools}

	s.updateCount++
	s.previousHash = newHash

	return true
}

// UpdateCount returns the in-memory update counter. It is NOT the
// apiserver Pool.Generation. See the field comment.
func (s *DeviceState) UpdateCount() int64 {
	s.lock()
	defer s.unlock()
	return s.updateCount
}

// DriverResources returns a read-only snapshot of the driver resources.
// The returned struct shares the inner Pools map with s.driverResources
// (guarded by s.Mutex), so callers must not mutate it. This is safe because
// CompareAndUpdate replaces the whole map while holding s.Mutex rather than
// mutating it in place, leaving the returned value aliasing a now-immutable
// map.
func (s *DeviceState) DriverResources() resourceslice.DriverResources {
	s.lock()
	defer s.unlock()
	return s.driverResources
}

// computeDevicesHash computes a hash of the devices for change detection.
func computeDevicesHash(devicesByCard map[string][]resourceapi.Device) string {
	cardKeys := make([]string, 0, len(devicesByCard))
	for k := range devicesByCard {
		cardKeys = append(cardKeys, k)
	}
	sort.Strings(cardKeys)

	h := sha256.New()
	for _, cardKey := range cardKeys {
		h.Write([]byte(cardKey))

		devices := devicesByCard[cardKey]
		sortedDevices := make([]resourceapi.Device, len(devices))
		copy(sortedDevices, devices)
		sort.Slice(sortedDevices, func(i, j int) bool {
			return sortedDevices[i].Name < sortedDevices[j].Name
		})

		for _, device := range sortedDevices {
			h.Write([]byte(device.Name))
			var attrKeys []string
			for k := range device.Attributes {
				attrKeys = append(attrKeys, string(k))
			}
			sort.Strings(attrKeys)
			for _, k := range attrKeys {
				attr := device.Attributes[resourceapi.QualifiedName(k)]
				h.Write([]byte(k))
				if attr.StringValue != nil {
					h.Write([]byte(*attr.StringValue))
				}
				if attr.IntValue != nil {
					h.Write([]byte(strconv.FormatInt(*attr.IntValue, 10)))
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// deviceClassForRequest looks up the DeviceClassName for a given request name
// in a claim. ok is false when the claim spec has no request of that name with
// an Exactly clause.
func deviceClassForRequest(claim *resourceapi.ResourceClaim, requestName string) (name string, ok bool) {
	for _, req := range claim.Spec.Devices.Requests {
		if req.Name == requestName && req.Exactly != nil {
			return req.Exactly.DeviceClassName, true
		}
	}
	return "", false
}

// Prepare prepares devices for a claim. ctx is the kubelet's per-RPC context.
// It bounds the wait for the state lock, and it is checked again before each
// per-queue driver switch, so an expired deadline stops new hardware work
// without interrupting a switch already in progress.
func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	if err := s.lockCtx(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()

	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	// Determine binding mode from DeviceClass names
	hasVM := false
	hasContainer := false
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != s.driverName {
			continue
		}
		dcName, ok := deviceClassForRequest(claim, result.Request)
		if !ok {
			return nil, fmt.Errorf("no request %q with an exactly clause in claim spec", result.Request)
		}
		switch dcName {
		case DeviceClassVM:
			hasVM = true
		case DeviceClassContainer:
			hasContainer = true
		default:
			return nil, fmt.Errorf("unknown DeviceClass %q for request %q", dcName, result.Request)
		}
	}

	if hasVM && hasContainer {
		return nil, fmt.Errorf("claim %s mixes VM and container DeviceClasses, which is not supported", claim.UID)
	}

	// The container path stays behind an off-by-default alpha gate. Checked
	// before anything is created: the claim is left untouched for a retry
	// after a gate flip and plugin restart.
	if hasContainer && !features.Gate.Enabled(features.ContainerWorkload) {
		return nil, fmt.Errorf("DeviceClass %q requires the ContainerWorkload feature gate, which is disabled", DeviceClassContainer)
	}

	// The VM path is guarded the same way: its gate off means the node was
	// declared container-only, so a VM claim landing here is a scheduling
	// mismatch, reported before anything is created or switched. Unprepare
	// stays ungated - cleanup of existing claims must survive a gate flip.
	if hasVM && !features.Gate.Enabled(features.VirtualMachineWorkload) {
		return nil, fmt.Errorf("DeviceClass %q requires the VirtualMachineWorkload feature gate, which is disabled on this node", DeviceClassVM)
	}

	// Resolve the claim's device configuration ahead of the binding split. A
	// malformed CryptoConfig - unknown field, wrong kind, an apiVersion this
	// driver does not know - is a defect in the claim itself, so it must fail
	// wherever the claim lands. A claim that parses on one node and is waved
	// through on another would make strict parsing worthless as a typo guard.
	cfg, err := cryptoconfig.Resolve(s.driverName, claim.Status.Allocation.Devices.Config)
	if err != nil {
		return nil, fmt.Errorf("resolve device configuration for claim %s: %w", claim.UID, err)
	}

	claimUID := string(claim.UID)

	if hasContainer {
		return s.prepareContainerClaim(claim, claimUID)
	}

	// VM DeviceClass: per-queue switching + mdev creation

	// Check the control-domain mode before anything touches the mdev, so a claim
	// asking for a mode the driver cannot serve fails without leaving a
	// half-built device behind. Field-level, unlike the parse above: a container
	// binding has no assign_control_domain to act on, so the mode is ignored
	// there rather than rejected.
	if err := cryptoconfig.ValidateControlDomainMode(cfg.ControlDomainMode); err != nil {
		return nil, fmt.Errorf("claim %s: %w", claimUID, err)
	}

	if err := s.ensureMdevForClaim(ctx, claim, claimUID, cfg.ControlDomainMode); err != nil {
		return nil, err
	}

	// Write KEP-5304 metadata for KubeVirt DRA integration
	requestDevices := s.groupResultsByRequest(claim)
	podClaimName := ""
	if ann, ok := claim.Annotations[metadata.PodClaimNameAnnotation]; ok {
		podClaimName = ann
	}
	metaMounts, err := metadata.GenerateClaimMetadata(&metadata.ClaimParams{
		PluginDataDir:  s.pluginDataDir,
		ClaimName:      claim.Name,
		ClaimNamespace: claim.Namespace,
		ClaimUID:       claimUID,
		DriverName:     s.driverName,
		MdevUUID:       claimUID,
		PodClaimName:   podClaimName,
		RequestDevices: requestDevices,
	})
	if err != nil {
		return nil, fmt.Errorf("generate KEP-5304 metadata for claim %s: %w", claimUID, err)
	}

	// Convert metadata mounts to CDI mount entries
	var cdiMounts []*cdispec.Mount
	for _, m := range metaMounts {
		cdiMounts = append(cdiMounts, &cdispec.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Options:       []string{"ro", "bind"},
		})
	}

	// Generate CDI spec for vfio-ap device with metadata mounts
	cdiDeviceID, err := cdi.GenerateClaimSpec(s.cdiRoot, claimUID, cdiMounts)
	if err != nil {
		return nil, fmt.Errorf("generate CDI spec for claim %s: %w", claimUID, err)
	}

	return s.devicesFromClaim(claim, []string{cdiDeviceID})
}

// prepareContainerClaim binds allocated APQNs into a native container via a
// filtered zcrypt device node and a claim-scoped shadow AP sysfs tree. Queues
// stay on the host zcrypt stack - unlike the VM path, nothing is rebound to
// vfio-ap. The matrix constraint still applies: the node apmask×aqmask is a
// Cartesian product, so a non-rectangular allocation would expose queues the
// claim was not given.
func (s *DeviceState) prepareContainerClaim(claim *resourceapi.ResourceClaim, claimUID string) ([]*drapbv1.Device, error) {
	_, _, allocated, err := s.collectAllocatedAPQNs(claim)
	if err != nil {
		return nil, err
	}
	apqns := make([]mdev.APQN, 0, len(allocated))
	for q := range allocated {
		apqns = append(apqns, q)
	}

	if err := zcryptnode.Create(claimUID, apqns); err != nil {
		return nil, fmt.Errorf("create zcrypt node for claim %s: %w", claimUID, err)
	}
	if err := shadowsysfs.Build(s.pluginDataDir, claimUID, apqns); err != nil {
		_ = zcryptnode.Destroy(claimUID)
		return nil, fmt.Errorf("build shadow sysfs for claim %s: %w", claimUID, err)
	}

	cdiMounts := []*cdispec.Mount{
		{
			HostPath:      shadowsysfs.BusMount(s.pluginDataDir, claimUID),
			ContainerPath: "/sys/bus/ap",
			Options:       []string{"ro", "bind"},
		},
		{
			HostPath:      shadowsysfs.DevicesMount(s.pluginDataDir, claimUID),
			ContainerPath: "/sys/devices/ap",
			Options:       []string{"ro", "bind"},
		},
	}

	cdiDeviceID, err := cdi.GenerateZcryptClaimSpec(s.cdiRoot, claimUID, cdiMounts)
	if err != nil {
		_ = shadowsysfs.Remove(s.pluginDataDir, claimUID)
		_ = zcryptnode.Destroy(claimUID)
		return nil, fmt.Errorf("generate zcrypt CDI spec for claim %s: %w", claimUID, err)
	}

	logphase.Logf(logphase.Preparation, "Container claim %s prepared: zcrypt node %s, CDI %s",
		claimUID, zcryptnode.NodeName(claimUID), cdiDeviceID)
	return s.devicesFromClaim(claim, []string{cdiDeviceID})
}

// ensureMdevForClaim brings the claim's mdev to its complete state: matrix
// constraint validated, each allocated queue switched from zcrypt to vfio-ap
// (with rollback on failure), the mdev created, and adapters, usage domains,
// and the control domains controlDomainMode implies assigned.
//
// An existing mdev whose matrix already equals the allocation is kept as is:
// that is a kubelet retry after a later Prepare step failed, or a re-prepare
// while the claim's VM may already use the device, so it is not safe to touch.
// Any other existing mdev is a partial build from an interrupted attempt and
// is destroyed and rebuilt. Skipping on bare existence would report success
// over a matrix missing queues the claim was allocated. The matrix cannot
// witness control domains (a separate sysfs attribute), so a crash between
// the matrix and control-domain assigns stays undetected - accepted as the
// far narrower window.
func (s *DeviceState) ensureMdevForClaim(ctx context.Context, claim *resourceapi.ResourceClaim, claimUID, controlDomainMode string) error {
	adapters, domains, allocated, err := s.collectAllocatedAPQNs(claim)
	if err != nil {
		return err
	}

	if mdev.Exists(claimUID) {
		complete, err := mdevMatrixEquals(claimUID, allocated)
		if err != nil {
			return fmt.Errorf("read matrix of existing mdev for claim %s: %w", claimUID, err)
		}
		if complete {
			return nil
		}
		logphase.Logf(logphase.Preparation, "Mdev %s exists with an incomplete matrix, rebuilding", claimUID)
		if err := mdev.Destroy(claimUID); err != nil {
			return fmt.Errorf("destroy partial mdev for claim %s: %w", claimUID, err)
		}
	}

	if err := switchQueuesToVFIOAP(ctx, allocated); err != nil {
		return err
	}
	if err := mdev.Create(claimUID); err != nil {
		return fmt.Errorf("create mdev for claim %s: %w", claimUID, err)
	}
	if err := assignMdevMatrix(claimUID, adapters, domains); err != nil {
		destroyPartialMdev(claimUID)
		return err
	}
	if err := assignMdevControlDomains(claimUID, controlDomainMode, domains); err != nil {
		destroyPartialMdev(claimUID)
		return err
	}
	return nil
}

// mdevMatrixEquals reports whether the mdev's matrix holds exactly the
// allocated APQNs.
func mdevMatrixEquals(uuid string, allocated map[mdev.APQN]bool) (bool, error) {
	queues, err := mdev.ReadMatrix(uuid)
	if err != nil {
		return false, err
	}
	if len(queues) != len(allocated) {
		return false, nil
	}
	for _, q := range queues {
		if !allocated[q] {
			return false, nil
		}
	}
	return true, nil
}

// destroyPartialMdev removes the mdev a failed build left behind, so nothing
// half-assigned survives the attempt. Best-effort: the build error is the one
// worth returning, and a retry destroys any leftover through the matrix check
// anyway.
func destroyPartialMdev(claimUID string) {
	if err := mdev.Destroy(claimUID); err != nil {
		logphase.Logf(logphase.Preparation, "Warning: destroy of partial mdev %s failed: %v", claimUID, err)
	}
}

// collectAllocatedAPQNs walks the claim's allocation results and returns the
// adapter set, domain set, and APQN set. It also enforces the matrix constraint
// (adapters × domains == allocated queues) so the mdev cross-product never
// includes unallocated queues.
func (s *DeviceState) collectAllocatedAPQNs(claim *resourceapi.ResourceClaim) (
	adapters, domains map[string]bool, allocated map[mdev.APQN]bool, err error,
) {
	adapters = make(map[string]bool)
	domains = make(map[string]bool)
	allocated = make(map[mdev.APQN]bool)

	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != s.driverName {
			continue
		}
		device, exists := s.allocatable[result.Device]
		if !exists {
			return nil, nil, nil, fmt.Errorf("requested device is not allocatable: %q", result.Device)
		}
		apid, apidOK := StringAttribute(device, "cex.ibm.com/apid")
		apqi, apqiOK := StringAttribute(device, "cex.ibm.com/apqi")
		if !apidOK || apid == "" || !apqiOK || apqi == "" {
			return nil, nil, nil, fmt.Errorf("device %q missing apid or apqi attributes", result.Device)
		}
		adapters[apid] = true
		domains[apqi] = true
		allocated[mdev.APQN{APID: apid, APQI: apqi}] = true
	}

	if len(adapters)*len(domains) != len(allocated) {
		return nil, nil, nil, fmt.Errorf(
			"matrix constraint violation: %d adapters × %d domains = %d queues, "+
				"but only %d were allocated, so the mdev cross-product would include "+
				"unallocated queues - constrain all queues to the same domain (apqi) "+
				"using matchAttribute in the ResourceClaimTemplate",
			len(adapters), len(domains), len(adapters)*len(domains), len(allocated))
	}
	return adapters, domains, allocated, nil
}

// switchQueuesToVFIOAP switches each allocated queue from zcrypt to vfio-ap.
// ctx is checked before each switch: once it ends, the kubelet has already
// discarded this RPC's answer, so continuing would only detach more queues
// for nothing. Both abort paths - a failed switch and an ended ctx - roll
// back the queues switched so far (best-effort) and return the wrapped
// error. The rollback includes the queue whose switch failed, not only the
// completed ones: a switch that fails partway can leave its queue unbound
// with the override set. SwitchToZcrypt handles every partial state, and a
// queue it cannot return to zcrypt keeps the vfio_ap override so the startup
// drain retries it.
func switchQueuesToVFIOAP(ctx context.Context, allocated map[mdev.APQN]bool) error {
	rollback := func(queues []mdev.APQN) {
		for _, sq := range queues {
			if rbErr := binding.SwitchToZcrypt(sq.APID, sq.APQI); rbErr != nil {
				logphase.Logf(logphase.Preparation, "Warning: rollback failed for %s.%s: %v", sq.APID, sq.APQI, rbErr)
			}
		}
	}
	var switchedAPQNs []mdev.APQN
	for q := range allocated {
		if err := ctx.Err(); err != nil {
			rollback(switchedAPQNs)
			return fmt.Errorf("switching queues to vfio-ap: %w", err)
		}
		if err := binding.SwitchToVFIOAP(q.APID, q.APQI); err != nil {
			rollback(append(switchedAPQNs, q))
			return fmt.Errorf("switch queue %s.%s to vfio-ap: %w", q.APID, q.APQI, err)
		}
		switchedAPQNs = append(switchedAPQNs, q)
	}
	return nil
}

// assignMdevMatrix assigns the adapter and domain sets to an existing mdev.
func assignMdevMatrix(claimUID string, adapters, domains map[string]bool) error {
	for apid := range adapters {
		if err := mdev.AssignAdapter(claimUID, apid); err != nil {
			return err
		}
	}
	for apqi := range domains {
		if err := mdev.AssignDomain(claimUID, apqi); err != nil {
			return err
		}
	}
	return nil
}

// assignMdevControlDomains assigns the control-domain set that mode implies to
// an existing mdev. Only modes bounded by the claim's own allocation are served
// here, and the caller has already rejected the rest: usage-only assigns none,
// usage-and-control assigns exactly the allocated usage domains.
//
// The driver writes the requested set and stops there. The kernel intersects it
// with the host's ap_control_domain_mask, so a domain the host does not control
// never becomes effective in the guest, and one the host gains later becomes
// effective without a reassignment.
func assignMdevControlDomains(claimUID, mode string, domains map[string]bool) error {
	if mode == cryptoconfig.ControlDomainModeUsageOnly {
		logphase.Logf(logphase.Preparation, "controlDomainMode=%s for claim %s: no control domains assigned", mode, claimUID)
		return nil
	}

	assigned := make([]string, 0, len(domains))
	for apqi := range domains {
		if err := mdev.AssignControlDomain(claimUID, apqi); err != nil {
			return err
		}
		assigned = append(assigned, apqi)
	}
	sort.Strings(assigned)
	logphase.Logf(logphase.Preparation, "controlDomainMode=%s for claim %s: control domains %v assigned",
		mode, claimUID, assigned)
	return nil
}

// Unprepare unprepares devices for a claim. ctx bounds only the wait for the
// state lock. Once teardown starts it runs to completion. Stopping partway
// would be worse than finishing late: wasVM below is derived from files this
// teardown deletes, so a retry after a partial run would classify the claim
// as a container claim, skip the queue reconcile, and leave the claim's
// queues on vfio-ap until the next driver restart.
func (s *DeviceState) Unprepare(ctx context.Context, claimUID string) error {
	if err := s.lockCtx(ctx); err != nil {
		return err
	}
	defer s.unlock()

	// Captured before the deletes below remove the witnesses. Only the VM path
	// builds an mdev and writes claim metadata, so finding either means this
	// claim may have switched queues to vfio-ap. A container claim leaves a
	// filtered zcrypt node and/or shadow sysfs instead, and must not fall
	// through to the bus-wide vfio-ap reconcile.
	wasVM := mdev.Exists(claimUID) || metadata.ClaimMetadataExists(s.pluginDataDir, claimUID)
	wasContainer := zcryptnode.Exists(claimUID) || shadowsysfs.Exists(s.pluginDataDir, claimUID)

	if err := cdi.DeleteClaimSpec(s.cdiRoot, claimUID); err != nil {
		return fmt.Errorf("delete CDI spec for claim %s: %w", claimUID, err)
	}

	// Container-path teardown: drop the filtered node and its shadow tree.
	// Best-effort ordering - CDI is already gone so the runtime will not newly
	// resolve the device; destroying the node next releases the minor.
	if wasContainer {
		if err := zcryptnode.Destroy(claimUID); err != nil {
			logphase.Warnf(logphase.Preparation, "destroy zcrypt node for claim %s: %v", claimUID, err)
		}
		if err := shadowsysfs.Remove(s.pluginDataDir, claimUID); err != nil {
			logphase.Warnf(logphase.Preparation, "remove shadow sysfs for claim %s: %v", claimUID, err)
		}
	}

	// Delete KEP-5304 metadata (best-effort for cleanup)
	if err := metadata.DeleteClaimMetadata(s.pluginDataDir, claimUID); err != nil {
		logphase.Warnf(logphase.Preparation, "failed to delete metadata for claim %s: %v", claimUID, err)
	}

	// Read the mdev's matrix BEFORE destroying it. Matrix sysfs is the
	// authoritative source for the claimed APQNs and survives driver
	// restart. In-memory state does not.
	var apqns []mdev.APQN
	if mdev.Exists(claimUID) {
		var err error
		apqns, err = mdev.ReadMatrix(claimUID)
		if err != nil {
			logphase.Warnf(logphase.Preparation, "read mdev matrix for claim %s: %v", claimUID, err)
		}
	}

	if err := mdev.Destroy(claimUID); err != nil {
		return fmt.Errorf("destroy mdev for claim %s: %w", claimUID, err)
	}

	if len(apqns) == 0 {
		if !wasVM {
			logphase.Logf(logphase.Preparation, "Claim %s bound no vfio-ap queues (container or empty claim), skipping vfio-ap reconcile", claimUID)
			return nil
		}
		// The matrix is authoritative, but it only lives as long as the mdev
		// does - and the mdev is not ours alone to keep. Anything that removes
		// it behind our back leaves nothing to roll back, and without this the
		// queue would stay bound to vfio_ap while Unprepare returned success,
		// shrinking the pool zcrypt can hand out until the driver restarts.
		// Reconcile against sysfs instead: the same pass the driver runs at
		// startup, which returns every vfio-ap queue no live mdev claims. Safe
		// to run here because Prepare takes the same lock, so no other claim
		// can be between its driver_override write and its matrix assignment.
		logphase.Logf(logphase.Preparation, "Claim %s left no matrix APQNs, reconciling orphaned vfio-ap queues", claimUID)
		if err := binding.DrainOrphanedVFIOAP(); err != nil {
			logphase.Warnf(logphase.Preparation, "failed to drain orphaned vfio-ap queues after claim %s: %v", claimUID, err)
		}
		return nil
	}

	for _, q := range apqns {
		if err := binding.SwitchToZcrypt(q.APID, q.APQI); err != nil {
			logphase.Warnf(logphase.Preparation, "failed to switch %s.%s back to zcrypt: %v", q.APID, q.APQI, err)
		}
	}

	return nil
}

func (s *DeviceState) devicesFromClaim(claim *resourceapi.ResourceClaim, cdiDeviceIDs []string) ([]*drapbv1.Device, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	var devices []*drapbv1.Device
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != s.driverName {
			continue
		}

		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %q", result.Device)
		}

		device := &drapbv1.Device{
			RequestNames: []string{result.Request},
			PoolName:     result.Pool,
			DeviceName:   result.Device,
			CdiDeviceIds: cdiDeviceIDs,
		}
		devices = append(devices, device)
	}

	return devices, nil
}

// groupResultsByRequest groups allocation results by request name for metadata generation.
func (s *DeviceState) groupResultsByRequest(claim *resourceapi.ResourceClaim) map[string][]metadata.DeviceInfo {
	result := make(map[string][]metadata.DeviceInfo)
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != s.driverName {
			continue
		}
		result[r.Request] = append(result[r.Request], metadata.DeviceInfo{
			Driver: r.Driver,
			Pool:   r.Pool,
			Name:   r.Device,
		})
	}
	return result
}

// StringAttribute returns the string value of a device attribute and whether
// it was present. ok is false when the attribute is absent, distinct from a
// present but empty value.
func StringAttribute(device resourceapi.Device, name string) (value string, ok bool) {
	if device.Attributes == nil {
		return "", false
	}
	attr, exists := device.Attributes[resourceapi.QualifiedName(name)]
	if !exists || attr.StringValue == nil {
		return "", false
	}
	return *attr.StringValue, true
}
