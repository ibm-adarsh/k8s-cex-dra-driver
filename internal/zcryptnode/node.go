// Package zcryptnode creates and destroys filtered zcrypt character-device
// nodes through /sys/class/zcrypt. Each node is restricted to the APQNs a
// container claim was allocated, so the workload sees /dev/zcrypt without
// reaching every queue on the host.
//
// Per-node apmask/aqmask are independent of the bus-level masks the driver
// requires to stay all-1s for vfio-ap driver_override. Creating a filtered
// node therefore coexists with the virtual-machine path on the same host.
package zcryptnode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/mdev"
)

// ClassPath roots the multi-device-node control interface. A var so tests can
// point the package at a fake sysfs tree. Nothing in production reassigns it.
var ClassPath = "/sys/class/zcrypt"

// DevPath roots where udev (or the kernel) places the character device for a
// created node. CDI HostPath entries point here. A var for the same reason as
// ClassPath.
var DevPath = "/dev"

// sysfsWriteMode is the mode passed to os.WriteFile for sysfs attributes. The
// kernel ignores it. 0o600 satisfies gosec G306.
const sysfsWriteMode os.FileMode = 0o600

// ZCDNMaxName is the kernel's printable-name budget for a zcrypt node
// (ZCDN_MAX_NAME minus the trailing NUL). Keep generated names at or under it.
const ZCDNMaxName = 31

// testCreateHook, when set, runs after a successful write to ClassPath/create.
// Unit tests use it to mkdir the node directory the kernel would create.
// Production leaves it nil.
var testCreateHook func(name string)

// testDestroyHook, when set, runs after a successful write to ClassPath/destroy.
// Unit tests use it to remove the node directory the kernel would remove.
var testDestroyHook func(name string)


// NodeName derives a stable, kernel-legal zcrypt device name from a claim UID.
// Claim UIDs are 36-character UUIDs; the kernel only accepts 31 characters, so
// the hyphens are stripped and the hex is truncated. Collision across distinct
// UIDs that share a 29-hex-char prefix is negligible for DRA claim UIDs.
func NodeName(claimUID string) string {
	hex := strings.ReplaceAll(claimUID, "-", "")
	// "zc-" (3) + 28 hex chars = 31, the kernel printable-name budget.
	if len(hex) > 28 {
		hex = hex[:28]
	}
	return "zc-" + hex
}

// HostDevicePath returns the host path of the character device for a claim.
func HostDevicePath(claimUID string) string {
	return filepath.Join(DevPath, NodeName(claimUID))
}

// Create builds a filtered zcrypt device node covering exactly the given
// APQNs. The Cartesian product of the node apmask and aqmask equals the
// allocated set only when the caller has already enforced the matrix
// constraint (adapters × domains == allocated queues); Create still writes
// the relative masks for the distinct adapters and domains present.
//
// Idempotent for a retry: an existing node of the same name is destroyed and
// rebuilt so a partial earlier attempt cannot leave the wrong filter in place.
func Create(claimUID string, apqns []mdev.APQN) error {
	if len(apqns) == 0 {
		return fmt.Errorf("create zcrypt node for claim %s: no APQNs", claimUID)
	}
	name := NodeName(claimUID)
	if len(name) > ZCDNMaxName {
		return fmt.Errorf("zcrypt node name %q exceeds kernel limit of %d", name, ZCDNMaxName)
	}

	if nodeExists(name) {
		logphase.Logf(logphase.Preparation, "zcrypt node %s already exists, recreating", name)
		if err := Destroy(claimUID); err != nil {
			return fmt.Errorf("destroy existing zcrypt node %s: %w", name, err)
		}
	}

	if err := os.WriteFile(filepath.Join(ClassPath, "create"), []byte(name), sysfsWriteMode); err != nil {
		return fmt.Errorf("create zcrypt node %s: %w", name, err)
	}
	// testCreateHook materialises the node directory in unit tests; the real
	// kernel creates it as a side effect of the write above.
	if testCreateHook != nil {
		testCreateHook(name)
	}

	if err := configureMasks(name, apqns); err != nil {
		_ = Destroy(claimUID)
		return err
	}

	logphase.Logf(logphase.Preparation, "created zcrypt node %s for claim %s (%d APQN(s))", name, claimUID, len(apqns))
	return nil
}

// Destroy removes the filtered zcrypt device node for a claim. Missing nodes
// are success: Unprepare retries after partial failures.
func Destroy(claimUID string) error {
	name := NodeName(claimUID)
	if !nodeExists(name) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(ClassPath, "destroy"), []byte(name), sysfsWriteMode); err != nil {
		return fmt.Errorf("destroy zcrypt node %s: %w", name, err)
	}
	if testDestroyHook != nil {
		testDestroyHook(name)
	}
	logphase.Logf(logphase.Preparation, "destroyed zcrypt node %s", name)
	return nil
}

// Exists reports whether the filtered node for claimUID is present under
// ClassPath. Used as the container-claim liveness witness for CDI GC.
func Exists(claimUID string) bool {
	return nodeExists(NodeName(claimUID))
}

func nodeExists(name string) bool {
	_, err := os.Stat(filepath.Join(ClassPath, name))
	return err == nil
}

// configureMasks restricts the node to the adapters and domains in apqns and
// opens every ioctl. Relative sysfs form is used so multi-bit sets stay
// readable in logs: clear the full range, then add each needed index.
func configureMasks(name string, apqns []mdev.APQN) error {
	nodeDir := filepath.Join(ClassPath, name)

	adapters := uniqueSorted(apqns, func(q mdev.APQN) string { return q.APID })
	domains := uniqueSorted(apqns, func(q mdev.APQN) string { return q.APQI })

	apRel, err := relativeMask(adapters)
	if err != nil {
		return fmt.Errorf("build apmask for %s: %w", name, err)
	}
	aqRel, err := relativeMask(domains)
	if err != nil {
		return fmt.Errorf("build aqmask for %s: %w", name, err)
	}

	// Clear first so a kernel default of "all bits set" cannot leak APQNs
	// the claim was not allocated.
	if err := os.WriteFile(filepath.Join(nodeDir, "apmask"), []byte("-0-255"), sysfsWriteMode); err != nil {
		return fmt.Errorf("clear apmask on %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "aqmask"), []byte("-0-255"), sysfsWriteMode); err != nil {
		return fmt.Errorf("clear aqmask on %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "apmask"), []byte(apRel), sysfsWriteMode); err != nil {
		return fmt.Errorf("set apmask on %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "aqmask"), []byte(aqRel), sysfsWriteMode); err != nil {
		return fmt.Errorf("set aqmask on %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "ioctlmask"), []byte("+0-255"), sysfsWriteMode); err != nil {
		return fmt.Errorf("set ioctlmask on %s: %w", name, err)
	}
	return nil
}

func uniqueSorted(apqns []mdev.APQN, key func(mdev.APQN) string) []string {
	seen := make(map[string]struct{}, len(apqns))
	out := make([]string, 0, len(apqns))
	for _, q := range apqns {
		k := key(q)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// relativeMask builds a "+0xNN,+0xMM,..." string from hex APID/APQI values.
func relativeMask(hexIDs []string) (string, error) {
	parts := make([]string, 0, len(hexIDs))
	for _, h := range hexIDs {
		v, err := strconv.ParseUint(h, 16, 8)
		if err != nil {
			return "", fmt.Errorf("parse %q as hex byte: %w", h, err)
		}
		parts = append(parts, fmt.Sprintf("+0x%02x", v))
	}
	return strings.Join(parts, ","), nil
}
