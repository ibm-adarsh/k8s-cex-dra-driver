package zcryptnode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s-cex-dra-driver/internal/mdev"
)

func TestNodeNameLength(t *testing.T) {
	uid := "8163c8ae-0392-4bd1-b3a6-8d059c9be180"
	name := NodeName(uid)
	if len(name) > ZCDNMaxName {
		t.Errorf("NodeName length %d exceeds %d", len(name), ZCDNMaxName)
	}
	if !strings.HasPrefix(name, "zc-") {
		t.Errorf("NodeName = %q, want zc- prefix", name)
	}
	if got, want := NodeName(uid), NodeName(uid); got != want {
		t.Errorf("NodeName not stable: %q vs %q", got, want)
	}
}

func TestCreateConfigureDestroy(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(restore(&ClassPath, root))
	write(t, filepath.Join(root, "create"), "")
	write(t, filepath.Join(root, "destroy"), "")

	t.Cleanup(func() {
		testCreateHook = nil
		testDestroyHook = nil
	})
	testCreateHook = func(name string) {
		nodeDir := filepath.Join(root, name)
		if err := os.MkdirAll(nodeDir, 0o750); err != nil {
			t.Errorf("mkdir node: %v", err)
			return
		}
		for _, attr := range []string{"apmask", "aqmask", "ioctlmask"} {
			write(t, filepath.Join(nodeDir, attr), "")
		}
	}
	testDestroyHook = func(name string) {
		_ = os.RemoveAll(filepath.Join(root, name))
	}

	claimUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	apqns := []mdev.APQN{{APID: "01", APQI: "0002"}, {APID: "02", APQI: "0002"}}
	if err := Create(claimUID, apqns); err != nil {
		t.Fatalf("Create: %v", err)
	}

	name := NodeName(claimUID)
	ap, err := os.ReadFile(filepath.Join(root, name, "apmask"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(ap); got != "+0x01,+0x02" {
		t.Errorf("apmask = %q, want +0x01,+0x02", got)
	}
	aq, err := os.ReadFile(filepath.Join(root, name, "aqmask"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(aq); got != "+0x02" {
		t.Errorf("aqmask = %q, want +0x02", got)
	}

	if !Exists(claimUID) {
		t.Fatal("Exists = false after Create")
	}
	if err := Destroy(claimUID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	destroyed, err := os.ReadFile(filepath.Join(root, "destroy"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(destroyed); got != name {
		t.Errorf("destroy wrote %q, want %q", got, name)
	}
}

func TestCreateRejectsEmptyAPQNs(t *testing.T) {
	if err := Create("claim", nil); err == nil {
		t.Fatal("Create accepted empty APQN list")
	}
}

func TestRelativeMaskRejectsBadHex(t *testing.T) {
	if _, err := relativeMask([]string{"zz"}); err == nil {
		t.Fatal("relativeMask accepted non-hex")
	}
}

func TestDestroyMissingIsOK(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(restore(&ClassPath, root))
	write(t, filepath.Join(root, "destroy"), "")
	if err := Destroy("no-such-claim"); err != nil {
		t.Fatalf("Destroy missing: %v", err)
	}
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
