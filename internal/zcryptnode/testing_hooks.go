package zcryptnode

import (
	"os"
	"path/filepath"
	"testing"
)

// InstallTestHooks points ClassPath at root and installs create/destroy hooks
// that materialise and remove node directories the way the kernel would. For
// use by other packages' Prepare/Unprepare tests.
func InstallTestHooks(t testing.TB, root string) {
	t.Helper()
	old := ClassPath
	ClassPath = root
	t.Cleanup(func() { ClassPath = old })

	if err := os.WriteFile(filepath.Join(root, "create"), nil, 0o600); err != nil {
		t.Fatalf("write create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "destroy"), nil, 0o600); err != nil {
		t.Fatalf("write destroy: %v", err)
	}
	t.Cleanup(func() {
		testCreateHook = nil
		testDestroyHook = nil
	})
	testCreateHook = func(name string) {
		nodeDir := filepath.Join(root, name)
		if err := os.MkdirAll(nodeDir, 0o750); err != nil {
			t.Errorf("mkdir zcrypt node %s: %v", name, err)
			return
		}
		for _, attr := range []string{"apmask", "aqmask", "ioctlmask"} {
			if err := os.WriteFile(filepath.Join(nodeDir, attr), nil, 0o600); err != nil {
				t.Errorf("write %s: %v", attr, err)
			}
		}
	}
	testDestroyHook = func(name string) {
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}
