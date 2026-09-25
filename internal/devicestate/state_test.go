package devicestate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"

	"k8s-cex-dra-driver/internal/binding"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/metadata"
	"k8s-cex-dra-driver/internal/sysfs"
	"k8s-cex-dra-driver/internal/zcryptnode"
)

// unprepareFixture is the slice of sysfs Unprepare touches, under a temp dir.
type unprepareFixture struct {
	apRoot string // stands in for sysfs.APDevicesPath
	matrix string // stands in for mdev.VFIOAPMatrixPath
}

// newUnprepareFixture builds one AP queue directory per entry in overrides
// (mapping "<apid>.<apqi>" to its starting driver_override) and points the
// sysfs package roots at the temp tree for the duration of the test.
func newUnprepareFixture(t *testing.T, overrides map[string]string) *unprepareFixture {
	t.Helper()
	root := t.TempDir()
	f := &unprepareFixture{
		apRoot: filepath.Join(root, "ap"),
		matrix: filepath.Join(root, "matrix"),
	}

	for queue, override := range overrides {
		apid, _, ok := strings.Cut(queue, ".")
		if !ok {
			t.Fatalf("malformed queue id %q in test setup", queue)
		}
		dir := filepath.Join(f.apRoot, "card"+apid, queue)
		if err := os.MkdirAll(filepath.Join(dir, "driver"), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		write(t, filepath.Join(dir, "driver_override"), override)
		write(t, filepath.Join(dir, "driver", "unbind"), "")
	}
	if err := os.MkdirAll(f.matrix, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", f.matrix, err)
	}
	write(t, filepath.Join(root, "drivers_probe"), "")

	t.Cleanup(restore(&sysfs.APDevicesPath, f.apRoot))
	t.Cleanup(restore(&mdev.VFIOAPMatrixPath, f.matrix))
	t.Cleanup(restore(&binding.DriversProbePath, filepath.Join(root, "drivers_probe")))

	return f
}

// addMdev creates a live mdev whose matrix claims the given queues.
func (f *unprepareFixture) addMdev(t *testing.T, uuid string, queues ...string) {
	t.Helper()
	dir := filepath.Join(f.matrix, uuid)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	// A real mdev directory also carries a `remove` attribute, which
	// mdev.Destroy writes to. Without it Destroy fails and Unprepare returns
	// early, before reaching the rollback under test.
	write(t, filepath.Join(dir, "remove"), "")
	write(t, filepath.Join(dir, "matrix"), strings.Join(queues, "\n"))
}

// override returns a queue's driver_override, trimmed: SwitchToZcrypt clears
// the attribute by writing a newline, which the kernel reads as empty.
func (f *unprepareFixture) override(t *testing.T, queue string) string {
	t.Helper()
	apid, _, _ := strings.Cut(queue, ".")
	got, err := os.ReadFile(filepath.Join(f.apRoot, "card"+apid, queue, "driver_override"))
	if err != nil {
		t.Fatalf("read driver_override for %s: %v", queue, err)
	}
	return strings.TrimSpace(string(got))
}

func restore(p *string, v string) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedClaimMetadata writes the metadata a successful VM Prepare leaves on
// disk - the witness Unprepare consults to tell a VM claim from a container
// one after the mdev is gone.
func seedClaimMetadata(t *testing.T, pluginDataDir, claimUID string) {
	t.Helper()
	_, err := metadata.GenerateClaimMetadata(&metadata.ClaimParams{
		PluginDataDir:  pluginDataDir,
		ClaimName:      "claim",
		ClaimNamespace: "ns",
		ClaimUID:       claimUID,
		DriverName:     "cex-driver.ibm.com",
		MdevUUID:       claimUID,
		RequestDevices: map[string][]metadata.DeviceInfo{
			"req": {{Driver: "cex-driver.ibm.com", Pool: "pool", Name: "dev"}},
		},
	})
	if err != nil {
		t.Fatalf("seed claim metadata: %v", err)
	}
}

// TestUnprepareReturnsQueuesToZcrypt covers both routes back to zcrypt. The
// matrix is the authoritative APQN source, but it dies with the mdev, and the
// mdev is not the driver's alone to keep - KubeVirt's virt-handler removes
// mdevs whose type its CR does not keep, DRA-owned or not. When that happens
// first, Unprepare has no matrix to read and must fall back to reconciling
// sysfs, or the queue stays bound to vfio_ap while Unprepare reports success.
func TestUnprepareReturnsQueuesToZcrypt(t *testing.T) {
	const claimUID = "claim-under-test"

	tests := []struct {
		name string
		// overrides is each queue's starting driver_override.
		overrides map[string]string
		// mdevPresent creates the claim's own mdev with the given matrix.
		mdevPresent bool
		// metadataPresent seeds the claim metadata a VM Prepare writes.
		metadataPresent bool
		claimQueues     []string
		// otherMdevs are unrelated live mdevs whose queues must survive.
		otherMdevs map[string][]string
		// wantZcrypt lists queues whose driver_override must end up cleared.
		wantZcrypt []string
		// wantVFIOAP lists queues that must still be bound to vfio_ap.
		wantVFIOAP []string
	}{
		{
			name:            "mdev present: matrix queues go back to zcrypt",
			overrides:       map[string]string{"01.0002": "vfio_ap"},
			mdevPresent:     true,
			metadataPresent: true,
			claimQueues:     []string{"01.0002"},
			wantZcrypt:      []string{"01.0002"},
		},
		{
			name:      "mdev removed externally: queue still goes back to zcrypt",
			overrides: map[string]string{"01.0002": "vfio_ap"},
			// No mdev: something removed it between Prepare and here. The
			// metadata survives and witnesses the VM claim.
			mdevPresent:     false,
			metadataPresent: true,
			wantZcrypt:      []string{"01.0002"},
		},
		{
			name: "mdev removed externally: another claim's queue is spared",
			overrides: map[string]string{
				"01.0002": "vfio_ap", // this claim's, now orphaned
				"02.0002": "vfio_ap", // another claim's, still live
			},
			mdevPresent:     false,
			metadataPresent: true,
			otherMdevs:      map[string][]string{"other-claim": {"02.0002"}},
			wantZcrypt:      []string{"01.0002"},
			wantVFIOAP:      []string{"02.0002"},
		},
		{
			// Metadata externally gone: the live mdev alone must still count
			// as the VM witness, so its empty matrix falls back to the drain.
			name:            "metadata removed externally: live mdev still witnesses",
			overrides:       map[string]string{"01.0002": "vfio_ap"},
			mdevPresent:     true,
			metadataPresent: false,
			wantZcrypt:      []string{"01.0002"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnprepareFixture(t, tc.overrides)
			if tc.mdevPresent {
				f.addMdev(t, claimUID, tc.claimQueues...)
			}
			for uuid, queues := range tc.otherMdevs {
				f.addMdev(t, uuid, queues...)
			}

			pluginDataDir := t.TempDir()
			if tc.metadataPresent {
				seedClaimMetadata(t, pluginDataDir, claimUID)
			}

			s := New("node", "cex-driver.ibm.com", t.TempDir(), pluginDataDir)
			if err := s.Unprepare(t.Context(), claimUID); err != nil {
				t.Fatalf("Unprepare: %v", err)
			}

			for _, q := range tc.wantZcrypt {
				if got := f.override(t, q); got != "" {
					t.Errorf("queue %s: driver_override = %q, want cleared (back on zcrypt)", q, got)
				}
			}
			for _, q := range tc.wantVFIOAP {
				if got := f.override(t, q); got != "vfio_ap" {
					t.Errorf("queue %s: driver_override = %q, want %q (still claimed)", q, got, "vfio_ap")
				}
			}
		})
	}
}

// TestUnprepareContainerClaimSkipsDrain: a container claim builds no mdev and
// must not fall back to the bus-wide vfio-ap reconcile - that sweep walks the
// whole AP bus under the state lock the scan loop needs. Preparing then
// unpreparing also tears down the filtered zcrypt node.
func TestUnprepareContainerClaimSkipsDrain(t *testing.T) {
	f := newUnprepareFixture(t, map[string]string{"03.0002": "vfio_ap"})
	zcryptnode.InstallTestHooks(t, t.TempDir())
	enableContainerWorkload(t)

	pluginDataDir := t.TempDir()
	s := New("node", cdTestDriver, t.TempDir(), pluginDataDir)
	s.allocatable["cex-01-0002"] = resourceapi.Device{
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"cex.ibm.com/apid": {StringValue: ptr("01")},
			"cex.ibm.com/apqi": {StringValue: ptr("0002")},
		},
	}
	claim := claimWithMode(t, DeviceClassContainer, "")
	if _, err := s.Prepare(t.Context(), claim); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := s.Unprepare(t.Context(), string(claim.UID)); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}

	if got := f.override(t, "03.0002"); got != "vfio_ap" {
		t.Errorf("driver_override = %q, want %q (container teardown must not drain the bus)", got, "vfio_ap")
	}
	if zcryptnode.Exists(string(claim.UID)) {
		t.Error("zcrypt node still present after Unprepare")
	}
}

// removeAttr returns what was written to an mdev's remove attribute: "" while
// the mdev was never destroyed, "1" after mdev.Destroy wrote it.
func (f *unprepareFixture) removeAttr(t *testing.T, uuid string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(f.matrix, uuid, "remove")) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read remove attribute of %s: %v", uuid, err)
	}
	return string(got)
}

// TestPrepareRetryKeepsCompleteMdev pins the idempotent retry: when the mdev
// exists and its matrix equals the allocation - a retry after a later Prepare
// step failed, or a re-prepare while the VM may already use the device - the
// mdev must be kept untouched while the metadata and CDI steps still run to
// completion.
func TestPrepareRetryKeepsCompleteMdev(t *testing.T) {
	const claimUID = "claim-under-test"

	f := newUnprepareFixture(t, map[string]string{"01.0002": "vfio_ap"})
	f.addMdev(t, claimUID, "01.0002")
	// The CDI step resolves the VFIO group through this link. Dangling is fine,
	// only the target's basename is read.
	if err := os.Symlink("../../kernel/iommu_groups/7", filepath.Join(f.matrix, claimUID, "iommu_group")); err != nil {
		t.Fatalf("symlink iommu_group: %v", err)
	}

	s := cdTestState(t)
	devices, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassVM, ""))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("prepared %d devices, want 1", len(devices))
	}
	if ids := devices[0].GetCdiDeviceIds(); len(ids) != 1 {
		t.Errorf("prepared device carries CDI devices %v, want exactly one", ids)
	}
	if got := f.removeAttr(t, claimUID); got != "" {
		t.Errorf("remove attribute = %q, want untouched (complete mdev must not be destroyed)", got)
	}
}

// TestPrepareRebuildsPartialMdev pins the retry after an interrupted build: an
// existing mdev whose matrix does not hold the allocation (here empty, the
// create-then-crash shape) must be destroyed and rebuilt, never skipped - the
// skip would report success for a VM that gets no queues. The fixture's matrix
// root has no create attribute, so the rebuild fails right after the destroy.
// Reaching that failure is the proof the skip is gone.
func TestPrepareRebuildsPartialMdev(t *testing.T) {
	const claimUID = "claim-under-test"

	f := newUnprepareFixture(t, map[string]string{"01.0002": "vfio_ap"})
	f.addMdev(t, claimUID)

	s := cdTestState(t)
	_, err := s.Prepare(t.Context(), claimWithMode(t, DeviceClassVM, ""))
	if err == nil {
		t.Fatal("Prepare reported success over a partial mdev")
	}
	if !strings.Contains(err.Error(), "create mdev") {
		t.Errorf("err = %q, want the rebuild's create-mdev failure", err)
	}
	if got := f.removeAttr(t, claimUID); got != "1" {
		t.Errorf("remove attribute = %q, want %q (partial mdev destroyed)", got, "1")
	}
}

// TestPrepareUnblocksWhenLockHolderWedges: a Prepare wedged in an
// uninterruptible sysfs write keeps the state lock. A later RPC must give up
// when its kubelet deadline ends instead of blocking behind it forever.
func TestPrepareUnblocksWhenLockHolderWedges(t *testing.T) {
	s := cdTestState(t)
	s.lock() // stands in for a wedged Prepare holding the lock
	defer s.unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Prepare(ctx, claimWithMode(t, DeviceClassVM, ""))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// errAfterCtx reports Err as nil for the first n calls and Canceled after,
// simulating a deadline that expires between two per-queue switch sequences.
type errAfterCtx struct {
	context.Context
	calls, n int
}

func (c *errAfterCtx) Err() error {
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

// TestSwitchQueuesRollsBackOnContextEnd: when the deadline ends between two
// queue switches, the already-switched queue must be rolled back to zcrypt,
// not left claimed by an RPC whose answer the kubelet has discarded.
func TestSwitchQueuesRollsBackOnContextEnd(t *testing.T) {
	f := newUnprepareFixture(t, map[string]string{"01.0002": "", "02.0002": ""})

	ctx := &errAfterCtx{Context: t.Context(), n: 1}
	err := switchQueuesToVFIOAP(ctx, map[mdev.APQN]bool{
		{APID: "01", APQI: "0002"}: true,
		{APID: "02", APQI: "0002"}: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for _, q := range []string{"01.0002", "02.0002"} {
		if got := f.override(t, q); got != "" {
			t.Errorf("queue %s: driver_override = %q, want cleared (rolled back to zcrypt)", q, got)
		}
	}
}
