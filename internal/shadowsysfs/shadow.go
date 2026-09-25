// Package shadowsysfs builds a static, claim-scoped AP sysfs tree that a
// container claim mounts over /sys/bus/ap and /sys/devices/ap. The tree shows
// only the allocated APQNs so tools like lszcrypt and opencryptoki discover
// the same restricted world the filtered zcrypt device node can reach.
//
// The files are ordinary copies, not live sysfs: counters stay frozen and
// writes from inside the container do not reach the host. That matches the
// classic CEX device-plugin shadow and is enough for discovery.
package shadowsysfs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/mdev"
	"k8s-cex-dra-driver/internal/sysfs"
)

const (
	// SubDir is the plugin-data subdirectory holding per-claim shadow trees.
	SubDir = "shadow-sysfs"

	shadowDirMode  os.FileMode = 0o750
	shadowFileMode os.FileMode = 0o644
)

// APBusPath is the host AP bus sysfs root copied into the shadow. A var so
// tests can redirect it with the rest of the fake tree.
var APBusPath = "/sys/bus/ap"

// ClaimRoot returns the host directory that holds one claim's shadow tree.
func ClaimRoot(pluginDataDir, claimUID string) string {
	return filepath.Join(pluginDataDir, SubDir, claimUID)
}

// BusMount returns the host path mounted onto /sys/bus/ap inside the container.
func BusMount(pluginDataDir, claimUID string) string {
	return filepath.Join(ClaimRoot(pluginDataDir, claimUID), "bus", "ap")
}

// DevicesMount returns the host path mounted onto /sys/devices/ap inside the container.
func DevicesMount(pluginDataDir, claimUID string) string {
	return filepath.Join(ClaimRoot(pluginDataDir, claimUID), "devices", "ap")
}

// Build creates (or replaces) the shadow tree for the allocated APQNs.
func Build(pluginDataDir, claimUID string, apqns []mdev.APQN) error {
	if len(apqns) == 0 {
		return fmt.Errorf("build shadow sysfs for claim %s: no APQNs", claimUID)
	}
	root := ClaimRoot(pluginDataDir, claimUID)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clear shadow root %s: %w", root, err)
	}

	busRoot := BusMount(pluginDataDir, claimUID)
	devRoot := DevicesMount(pluginDataDir, claimUID)
	for _, dir := range []string{
		busRoot,
		filepath.Join(busRoot, "devices"),
		devRoot,
	} {
		if err := os.MkdirAll(dir, shadowDirMode); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	adapters := map[string]struct{}{}
	domains := map[string]struct{}{}
	for _, q := range apqns {
		adapters[q.APID] = struct{}{}
		domains[q.APQI] = struct{}{}
		if err := materializeQueue(devRoot, busRoot, q); err != nil {
			_ = os.RemoveAll(root)
			return err
		}
	}

	if err := writeFilteredMasks(busRoot, adapters, domains); err != nil {
		_ = os.RemoveAll(root)
		return err
	}

	// Optional bus-level attributes tools may read; copy when present, ignore
	// when absent so a lean fake tree still builds in tests.
	for _, name := range []string{"features", "poll_timeout", "poll_thread"} {
		_ = copyFileIfExists(filepath.Join(APBusPath, name), filepath.Join(busRoot, name))
	}

	logphase.Logf(logphase.Preparation, "shadow sysfs built for claim %s at %s (%d APQN(s))", claimUID, root, len(apqns))
	return nil
}

// Remove deletes the shadow tree for a claim. Missing trees are success.
func Remove(pluginDataDir, claimUID string) error {
	root := ClaimRoot(pluginDataDir, claimUID)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove shadow sysfs %s: %w", root, err)
	}
	logphase.Logf(logphase.Preparation, "shadow sysfs removed for claim %s", claimUID)
	return nil
}

// Exists reports whether a shadow tree for the claim is present.
func Exists(pluginDataDir, claimUID string) bool {
	_, err := os.Stat(ClaimRoot(pluginDataDir, claimUID))
	return err == nil
}

// GCStale removes shadow trees whose claimUID is not reported live. Best-effort
// per tree; only a listing failure is returned.
func GCStale(pluginDataDir string, live func(claimUID string) bool) error {
	root := filepath.Join(pluginDataDir, SubDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list shadow sysfs under %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uid := e.Name()
		if live(uid) {
			continue
		}
		path := filepath.Join(root, uid)
		if err := os.RemoveAll(path); err != nil {
			logphase.Warnf(logphase.Preparation, "remove stale shadow sysfs %s: %v", path, err)
			continue
		}
		logphase.Logf(logphase.Preparation, "removed stale shadow sysfs for claim %s", uid)
	}
	return nil
}

func materializeQueue(devRoot, busRoot string, q mdev.APQN) error {
	cardName := "card" + q.APID
	queueName := q.APID + "." + q.APQI

	hostCard := filepath.Join(sysfs.APDevicesPath, cardName)
	hostQueue := filepath.Join(hostCard, queueName)

	shadowCard := filepath.Join(devRoot, cardName)
	shadowQueue := filepath.Join(shadowCard, queueName)
	if err := os.MkdirAll(shadowQueue, shadowDirMode); err != nil {
		return fmt.Errorf("mkdir %s: %w", shadowQueue, err)
	}

	if err := copyAttrs(hostCard, shadowCard, cardAttrs); err != nil {
		return fmt.Errorf("copy card %s attrs: %w", cardName, err)
	}
	if err := copyAttrs(hostQueue, shadowQueue, queueAttrs); err != nil {
		return fmt.Errorf("copy queue %s attrs: %w", queueName, err)
	}

	// bus/ap/devices mirrors the live layout with symlinks into the devices tree.
	if err := os.Symlink(
		filepath.Join("../../../devices/ap", cardName),
		filepath.Join(busRoot, "devices", cardName),
	); err != nil && !os.IsExist(err) {
		return fmt.Errorf("symlink card %s into bus devices: %w", cardName, err)
	}
	if err := os.Symlink(
		filepath.Join("../../../devices/ap", cardName, queueName),
		filepath.Join(busRoot, "devices", queueName),
	); err != nil && !os.IsExist(err) {
		return fmt.Errorf("symlink queue %s into bus devices: %w", queueName, err)
	}
	return nil
}

// Attributes commonly read by lszcrypt / opencryptoki. Missing sources are
// skipped so a partial host tree still produces a usable shadow.
var cardAttrs = []string{
	"type", "raw_hwtype", "depth", "request_count", "requestq_count",
	"pendingq_count", "hwtype", "online", "config", "chkstop",
}

var queueAttrs = []string{
	"type", "depth", "request_count", "requestq_count", "pendingq_count",
	"online", "mkvps", "driver",
}

func copyAttrs(srcDir, dstDir string, names []string) error {
	for _, name := range names {
		if err := copyFileIfExists(filepath.Join(srcDir, name), filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is built from package roots + hex APQN segments
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, shadowFileMode) //nolint:gosec
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// writeFilteredMasks writes 256-bit big-endian hex masks that show only the
// allocated adapters and domains. Tools that read these masks then agree with
// the filtered zcrypt node.
func writeFilteredMasks(busRoot string, adapters, domains map[string]struct{}) error {
	apHex, err := bitMaskHex(adapters)
	if err != nil {
		return err
	}
	aqHex, err := bitMaskHex(domains)
	if err != nil {
		return err
	}
	for name, val := range map[string]string{
		"apmask":                 apHex,
		"aqmask":                 aqHex,
		"ap_adapter_mask":        apHex,
		"ap_domain_mask":         aqHex,
		"ap_control_domain_mask": aqHex,
	} {
		if err := os.WriteFile(filepath.Join(busRoot, name), []byte(val+"\n"), shadowFileMode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// bitMaskHex builds a 0x-prefixed 256-bit big-endian mask with the given hex
// indices set (bit 0 = leftmost / adapter 0).
func bitMaskHex(ids map[string]struct{}) (string, error) {
	var bits [32]byte
	for h := range ids {
		v, err := strconv.ParseUint(h, 16, 8)
		if err != nil {
			return "", fmt.Errorf("parse mask index %q: %w", h, err)
		}
		byteIdx := int(v) / 8
		bitIdx := 7 - (int(v) % 8) // big-endian bit order within the byte
		bits[byteIdx] |= 1 << bitIdx
	}
	var b strings.Builder
	b.WriteString("0x")
	for _, by := range bits {
		fmt.Fprintf(&b, "%02x", by)
	}
	return b.String(), nil
}
