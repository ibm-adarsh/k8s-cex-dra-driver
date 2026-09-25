// Package cdi generates Container Device Interface specs for prepared claims.
// Virtual-machine claims get a vfio-ap passthrough device; container claims get
// a filtered zcrypt character device plus shadow AP sysfs mounts.
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/zcryptnode"
)

const (
	// cdiVersion is the CDI spec version written into generated files.
	// Use 0.5.0 (not specs-go.CurrentVersion) for broad container runtime
	// compatibility - 0.5.0 is the minimum version that supports hostPath
	// on DeviceNode and is accepted by all modern containerd/CRI-O releases.
	cdiVersion = "0.5.0"
	cdiVendor  = "ibm.com"

	cdiClassVFIO   = "vfio-ap-passthrough"
	cdiClassZcrypt = "zcrypt"

	cdiKindVFIO   = cdiVendor + "/" + cdiClassVFIO
	cdiKindZcrypt = cdiVendor + "/" + cdiClassZcrypt

	// cdiSpecFileMode is the mode for the generated CDI spec file. The CRI
	// runtime reads it back. Rootless runtimes run as a non-root user, so the
	// world-read bit is kept. The file holds only a device spec, no secrets.
	cdiSpecFileMode os.FileMode = 0o644

	// cdiDirMode is the mode for the CDI spec directory. Group needs the
	// execute bit to traverse into it. World has no access.
	cdiDirMode os.FileMode = 0o750
)

// GenerateClaimSpec creates a CDI spec file for a prepared vfio-ap claim.
// It discovers the VFIO group from sysfs and writes the CDI JSON.
// The mounts parameter embeds additional bind mounts (e.g. KEP-5304 metadata
// files) into the CDI device's container edits.
// Returns the fully-qualified CDI device ID.
func GenerateClaimSpec(cdiRoot, claimUID string, mounts []*cdispec.Mount) (string, error) {
	group, err := VFIOGroupFromMdev(claimUID)
	if err != nil {
		return "", fmt.Errorf("discover VFIO group for mdev %s: %w", claimUID, err)
	}

	devPath := fmt.Sprintf("/dev/vfio/%d", group)

	spec := &cdispec.Spec{
		Version: cdiVersion,
		Kind:    cdiKindVFIO,
		Devices: []cdispec.Device{
			{
				Name: claimUID,
				ContainerEdits: cdispec.ContainerEdits{
					DeviceNodes: []*cdispec.DeviceNode{
						{
							Path:     devPath,
							HostPath: devPath,
						},
						{
							Path:     "/dev/vfio/vfio",
							HostPath: "/dev/vfio/vfio",
						},
					},
					Mounts: mounts,
				},
			},
		},
	}

	return writeSpec(cdiRoot, cdiClassVFIO, cdiKindVFIO, claimUID, spec)
}

// GenerateZcryptClaimSpec creates a CDI spec for a prepared container claim.
// The host character device is the filtered zcrypt node created for the claim;
// inside the container it appears as both /dev/zcrypt and /dev/z90crypt so
// modern and legacy crypto stacks resolve it. mounts carries the shadow AP
// sysfs bind mounts (and any future extras).
func GenerateZcryptClaimSpec(cdiRoot, claimUID string, mounts []*cdispec.Mount) (string, error) {
	hostDev := zcryptnode.HostDevicePath(claimUID)

	spec := &cdispec.Spec{
		Version: cdiVersion,
		Kind:    cdiKindZcrypt,
		Devices: []cdispec.Device{
			{
				Name: claimUID,
				ContainerEdits: cdispec.ContainerEdits{
					DeviceNodes: []*cdispec.DeviceNode{
						{
							Path:        "/dev/zcrypt",
							HostPath:    hostDev,
							Permissions: "rw",
						},
						{
							Path:        "/dev/z90crypt",
							HostPath:    hostDev,
							Permissions: "rw",
						},
					},
					Mounts: mounts,
				},
			},
		},
	}

	return writeSpec(cdiRoot, cdiClassZcrypt, cdiKindZcrypt, claimUID, spec)
}

func writeSpec(cdiRoot, class, kind, claimUID string, spec *cdispec.Spec) (string, error) {
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDI spec: %w", err)
	}

	filePath := specPath(cdiRoot, class, claimUID)
	if err := os.MkdirAll(cdiRoot, cdiDirMode); err != nil {
		return "", fmt.Errorf("create CDI directory %s: %w", cdiRoot, err)
	}
	if err := os.WriteFile(filePath, data, cdiSpecFileMode); err != nil {
		return "", fmt.Errorf("write CDI spec %s: %w", filePath, err)
	}

	deviceID := fmt.Sprintf("%s=%s", kind, claimUID)
	logphase.Logf(logphase.Preparation, "CDI spec written: %s (device ID: %s)", filePath, deviceID)
	return deviceID, nil
}

// DeleteClaimSpec removes every CDI spec file this driver may have written for
// a claim (vfio-ap and zcrypt). Missing files are success: Unprepare retries
// after partial failures.
func DeleteClaimSpec(cdiRoot, claimUID string) error {
	for _, class := range []string{cdiClassVFIO, cdiClassZcrypt} {
		filePath := specPath(cdiRoot, class, claimUID)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove CDI spec %s: %w", filePath, err)
		}
		logphase.Logf(logphase.Preparation, "CDI spec removed: %s", filePath)
	}
	return nil
}

// GCStaleClaimSpecs removes CDI spec files this driver wrote for claims that
// are no longer live. Specs are written at Prepare and removed at Unprepare,
// so a claim that dies without an Unprepare - a crash between the two, or a
// Prepare that failed after the write - leaks its file forever.
//
// live reports whether a claimUID still has live backing state (an mdev for
// vfio-ap specs, a filtered zcrypt node for zcrypt specs). Only files matching
// this driver's vendor-class prefixes are considered, because cdiRoot is shared
// with other CDI producers. Best-effort per file. An error is returned only
// when a listing itself fails.
func GCStaleClaimSpecs(cdiRoot string, live func(claimUID string) bool) error {
	for _, class := range []string{cdiClassVFIO, cdiClassZcrypt} {
		prefix := cdiVendor + "-" + class + "-"
		matches, err := filepath.Glob(filepath.Join(cdiRoot, prefix+"*.json"))
		if err != nil {
			return fmt.Errorf("glob CDI specs under %s: %w", cdiRoot, err)
		}
		for _, p := range matches {
			claimUID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), prefix), ".json")
			if claimUID == "" || live(claimUID) {
				continue
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				logphase.Warnf(logphase.Preparation, "remove stale CDI spec %s: %v", p, err)
				continue
			}
			logphase.Logf(logphase.Preparation, "removed stale CDI spec %s (claim %s no longer live)", p, claimUID)
		}
	}
	return nil
}

func specPath(cdiRoot, class, claimUID string) string {
	return filepath.Join(cdiRoot, fmt.Sprintf("%s-%s-%s.json", cdiVendor, class, claimUID))
}

// VFIOGroupFromMdev reads the VFIO iommu_group number for a given mdev UUID.
func VFIOGroupFromMdev(uuid string) (int, error) {
	link := filepath.Join(mdev.VFIOAPMatrixPath, uuid, "iommu_group")
	target, err := os.Readlink(link)
	if err != nil {
		return 0, fmt.Errorf("readlink %s: %w", link, err)
	}
	// target is like "../../../../kernel/iommu_groups/2"
	group := filepath.Base(target)
	return strconv.Atoi(group)
}
