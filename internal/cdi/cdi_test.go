package cdi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"k8s-cex-dra-driver/internal/mdev"
)

const testUUID = "8163c8ae-0392-4bd1-b3a6-8d059c9be180"

// newMatrixFixture points mdev.VFIOAPMatrixPath at a temp tree holding one
// mdev whose iommu_group symlink names the given group, mirroring the layout
// the vfio_ap matrix driver populates.
func newMatrixFixture(t *testing.T, uuid, groupTarget string) {
	t.Helper()
	matrix := t.TempDir()
	dir := filepath.Join(matrix, uuid)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if groupTarget != "" {
		if err := os.Symlink(groupTarget, filepath.Join(dir, "iommu_group")); err != nil {
			t.Fatalf("symlink iommu_group: %v", err)
		}
	}

	old := mdev.VFIOAPMatrixPath
	mdev.VFIOAPMatrixPath = matrix
	t.Cleanup(func() { mdev.VFIOAPMatrixPath = old })
}

func TestVFIOGroupFromMdev(t *testing.T) {
	newMatrixFixture(t, testUUID, "../../../../kernel/iommu_groups/7")

	group, err := VFIOGroupFromMdev(testUUID)
	if err != nil {
		t.Fatalf("VFIOGroupFromMdev: %v", err)
	}
	if group != 7 {
		t.Errorf("group = %d, want 7", group)
	}
}

func TestVFIOGroupFromMdevMissingLink(t *testing.T) {
	newMatrixFixture(t, testUUID, "")

	if _, err := VFIOGroupFromMdev(testUUID); err == nil {
		t.Fatal("VFIOGroupFromMdev succeeded without an iommu_group link")
	}
}

func TestVFIOGroupFromMdevNonNumericTarget(t *testing.T) {
	newMatrixFixture(t, testUUID, "../../../../kernel/iommu_groups/not-a-number")

	if _, err := VFIOGroupFromMdev(testUUID); err == nil {
		t.Fatal("VFIOGroupFromMdev accepted a non-numeric group")
	}
}

func TestGenerateClaimSpec(t *testing.T) {
	newMatrixFixture(t, testUUID, "../../../../kernel/iommu_groups/3")
	cdiRoot := filepath.Join(t.TempDir(), "cdi")

	mounts := []*cdispec.Mount{{
		HostPath:      "/host/metadata.json",
		ContainerPath: "/var/run/dra/metadata.json",
		Options:       []string{"ro", "bind"},
	}}
	deviceID, err := GenerateClaimSpec(cdiRoot, testUUID, mounts)
	if err != nil {
		t.Fatalf("GenerateClaimSpec: %v", err)
	}
	if want := "ibm.com/vfio-ap-passthrough=" + testUUID; deviceID != want {
		t.Errorf("deviceID = %q, want %q", deviceID, want)
	}

	data, err := os.ReadFile(filepath.Join(cdiRoot, "ibm.com-vfio-ap-passthrough-"+testUUID+".json"))
	if err != nil {
		t.Fatalf("read generated spec: %v", err)
	}
	var spec cdispec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal generated spec: %v", err)
	}

	if spec.Version != "0.5.0" {
		t.Errorf("version = %q, want %q", spec.Version, "0.5.0")
	}
	if spec.Kind != "ibm.com/vfio-ap-passthrough" {
		t.Errorf("kind = %q, want %q", spec.Kind, "ibm.com/vfio-ap-passthrough")
	}
	if len(spec.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(spec.Devices))
	}
	dev := spec.Devices[0]
	if dev.Name != testUUID {
		t.Errorf("device name = %q, want %q", dev.Name, testUUID)
	}

	var paths []string
	for _, n := range dev.ContainerEdits.DeviceNodes {
		paths = append(paths, n.Path)
		if n.HostPath != n.Path {
			t.Errorf("device node %s: hostPath = %q, want same as path", n.Path, n.HostPath)
		}
	}
	want := []string{"/dev/vfio/3", "/dev/vfio/vfio"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("device node paths = %v, want %v", paths, want)
	}

	if len(dev.ContainerEdits.Mounts) != 1 || dev.ContainerEdits.Mounts[0].ContainerPath != "/var/run/dra/metadata.json" {
		t.Errorf("mounts = %+v, want the metadata bind mount passed in", dev.ContainerEdits.Mounts)
	}
}

func TestGenerateZcryptClaimSpec(t *testing.T) {
	cdiRoot := filepath.Join(t.TempDir(), "cdi")
	claimUID := testUUID
	mounts := []*cdispec.Mount{{
		HostPath:      "/host/shadow/bus/ap",
		ContainerPath: "/sys/bus/ap",
		Options:       []string{"ro", "bind"},
	}}

	deviceID, err := GenerateZcryptClaimSpec(cdiRoot, claimUID, mounts)
	if err != nil {
		t.Fatalf("GenerateZcryptClaimSpec: %v", err)
	}
	if want := "ibm.com/zcrypt=" + claimUID; deviceID != want {
		t.Errorf("deviceID = %q, want %q", deviceID, want)
	}

	data, err := os.ReadFile(filepath.Join(cdiRoot, "ibm.com-zcrypt-"+claimUID+".json"))
	if err != nil {
		t.Fatalf("read generated spec: %v", err)
	}
	var spec cdispec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Kind != "ibm.com/zcrypt" {
		t.Errorf("kind = %q, want ibm.com/zcrypt", spec.Kind)
	}
	if len(spec.Devices) != 1 || len(spec.Devices[0].ContainerEdits.DeviceNodes) != 2 {
		t.Fatalf("spec devices = %+v, want zcrypt+z90crypt nodes", spec.Devices)
	}
	paths := []string{
		spec.Devices[0].ContainerEdits.DeviceNodes[0].Path,
		spec.Devices[0].ContainerEdits.DeviceNodes[1].Path,
	}
	if paths[0] != "/dev/zcrypt" || paths[1] != "/dev/z90crypt" {
		t.Errorf("paths = %v, want /dev/zcrypt and /dev/z90crypt", paths)
	}
}

func TestDeleteClaimSpecRemovesBothClasses(t *testing.T) {
	cdiRoot := t.TempDir()
	for _, class := range []string{"vfio-ap-passthrough", "zcrypt"} {
		p := filepath.Join(cdiRoot, "ibm.com-"+class+"-"+testUUID+".json")
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteClaimSpec(cdiRoot, testUUID); err != nil {
		t.Fatalf("DeleteClaimSpec: %v", err)
	}
	for _, class := range []string{"vfio-ap-passthrough", "zcrypt"} {
		p := filepath.Join(cdiRoot, "ibm.com-"+class+"-"+testUUID+".json")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after delete", p)
		}
	}
}


func TestDeleteClaimSpec(t *testing.T) {
	newMatrixFixture(t, testUUID, "../../../../kernel/iommu_groups/3")
	cdiRoot := filepath.Join(t.TempDir(), "cdi")

	if _, err := GenerateClaimSpec(cdiRoot, testUUID, nil); err != nil {
		t.Fatalf("GenerateClaimSpec: %v", err)
	}
	if err := DeleteClaimSpec(cdiRoot, testUUID); err != nil {
		t.Fatalf("DeleteClaimSpec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, "ibm.com-vfio-ap-passthrough-"+testUUID+".json")); !os.IsNotExist(err) {
		t.Errorf("spec file still present after delete (stat err: %v)", err)
	}

	// A second delete must stay silent: Unprepare retries after partial
	// failures, and the spec being already gone is success, not an error.
	if err := DeleteClaimSpec(cdiRoot, testUUID); err != nil {
		t.Errorf("DeleteClaimSpec on missing file: %v", err)
	}
}

func TestGCStaleClaimSpecs(t *testing.T) {
	cdiRoot := t.TempDir()
	stale := filepath.Join(cdiRoot, "ibm.com-vfio-ap-passthrough-dead-claim.json")
	kept := filepath.Join(cdiRoot, "ibm.com-vfio-ap-passthrough-"+testUUID+".json")
	foreign := filepath.Join(cdiRoot, "vendor.example-gpu-dead-claim.json")
	for _, p := range []string{stale, kept, foreign} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	live := func(claimUID string) bool { return claimUID == testUUID }
	if err := GCStaleClaimSpecs(cdiRoot, live); err != nil {
		t.Fatalf("GCStaleClaimSpecs: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spec still present (stat err: %v)", err)
	}
	// The live claim's spec and the foreign producer's file must survive:
	// cdiRoot is shared, and only this driver's stale files are fair game.
	for _, p := range []string{kept, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed or unreadable: %v", p, err)
		}
	}
}

func TestGCStaleClaimSpecsMissingDir(t *testing.T) {
	cdiRoot := filepath.Join(t.TempDir(), "does-not-exist")

	if err := GCStaleClaimSpecs(cdiRoot, func(string) bool { return false }); err != nil {
		t.Fatalf("GCStaleClaimSpecs on missing dir: %v", err)
	}
}
