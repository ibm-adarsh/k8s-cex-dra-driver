package shadowsysfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/sysfs"
)

func TestBuildAndRemove(t *testing.T) {
	host := t.TempDir()
	apRoot := filepath.Join(host, "devices", "ap")
	busRoot := filepath.Join(host, "bus", "ap")
	t.Cleanup(restore(&sysfs.APDevicesPath, apRoot))
	t.Cleanup(restore(&APBusPath, busRoot))

	card := filepath.Join(apRoot, "card01")
	queue := filepath.Join(card, "01.0002")
	if err := os.MkdirAll(queue, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(card, "type"), "CCA")
	write(t, filepath.Join(queue, "type"), "CCA")
	write(t, filepath.Join(queue, "online"), "1")
	if err := os.MkdirAll(busRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	pluginData := t.TempDir()
	claimUID := "claim-shadow"
	apqns := []mdev.APQN{{APID: "01", APQI: "0002"}}
	if err := Build(pluginData, claimUID, apqns); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !Exists(pluginData, claimUID) {
		t.Fatal("Exists = false after Build")
	}

	gotType, err := os.ReadFile(filepath.Join(DevicesMount(pluginData, claimUID), "card01", "01.0002", "type"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotType) != "CCA" {
		t.Errorf("queue type = %q, want CCA", gotType)
	}

	apmask, err := os.ReadFile(filepath.Join(BusMount(pluginData, claimUID), "apmask"))
	if err != nil {
		t.Fatal(err)
	}
	// Adapter 0x01 → bit 1 set → 0x40 in the first byte of a 256-bit BE mask.
	if !strings.HasPrefix(strings.TrimSpace(string(apmask)), "0x40") {
		t.Errorf("apmask = %q, want 0x40...", apmask)
	}

	if err := Remove(pluginData, claimUID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Exists(pluginData, claimUID) {
		t.Fatal("Exists = true after Remove")
	}
}

func TestGCStale(t *testing.T) {
	pluginData := t.TempDir()
	liveUID := "live"
	deadUID := "dead"
	for _, uid := range []string{liveUID, deadUID} {
		if err := os.MkdirAll(ClaimRoot(pluginData, uid), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := GCStale(pluginData, func(uid string) bool { return uid == liveUID }); err != nil {
		t.Fatalf("GCStale: %v", err)
	}
	if !Exists(pluginData, liveUID) {
		t.Error("live shadow removed")
	}
	if Exists(pluginData, deadUID) {
		t.Error("dead shadow kept")
	}
}

func TestBitMaskHex(t *testing.T) {
	got, err := bitMaskHex(map[string]struct{}{"00": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "0x80") {
		t.Errorf("bit 0 mask = %q, want 0x80...", got)
	}
}

func restore(p *string, v string) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
