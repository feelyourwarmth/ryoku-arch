package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ryoku-cli/internal/sys"
)

// ---- reconciler: limine per-kernel boot images -------------------------------

// A limine box boots each installed kernel from a self-contained UKI on the ESP
// (/boot/EFI/Linux/ryoku_<kernel>.efi), or, without the UKI design, from
// /boot/initramfs-<kernel>.img beside vmlinuz-<kernel>. Either bundle has to
// stay in step with /usr/lib/modules/<version>: when a kernel changes and its
// image is NOT rebuilt (an interrupted update, a hook that never fired, a kernel
// package removed out from under its entry), the entry still boots a kernel
// whose module tree is gone from the root filesystem, and systemd drops to an
// emergency shell right after switch-root. That is the linux-cachyos "emergency
// mode" bug (#140): the cachyos entry alone went stale while the stock linux
// entry stayed current, so the box booted linux (reporting Arch, which is just
// /etc/os-release) and the hand-picked cachyos entry died.
//
// The generator does not catch this -- a missing module is only an mkinitcpio
// warning, so the UKI build still "succeeds" -- and nothing else on `ryoku
// update` re-checks per-kernel freshness. This reconciler does: for every
// installed kernel it rebuilds an image that is missing, older than the kernel,
// or built for a version no longer installed; and it reports (and prunes) a
// boot entry for a kernel the box no longer has, instead of leaving a dead
// second entry that only ever boots into emergency mode.

type installedKernel struct {
	version string
	vmlinuz time.Time
}

// limineBootKernel is one tool-generated "//<name>" kernel sub-entry: the kernel
// it names, the version its image was built for (the "Kernel version" comment),
// and the ESP path of the image it boots, enriched with that image's on-disk
// state.
type limineBootKernel struct {
	name        string
	version     string
	image       string
	imageExists bool
	imageOlder  bool
}

// installedKernelVersions maps each installed kernel's pkgbase name to its
// module-tree version and vmlinuz mtime. A module tree with no vmlinuz is not
// bootable and is skipped, so it never masquerades as an installed kernel.
func installedKernelVersions() map[string]installedKernel {
	out := map[string]installedKernel{}
	pkgbases, _ := filepath.Glob("/usr/lib/modules/*/pkgbase")
	for _, pb := range pkgbases {
		name := strings.TrimSpace(readFileSafe(pb))
		if name == "" || strings.HasPrefix(name, "(") {
			continue
		}
		dir := strings.TrimSuffix(pb, "/pkgbase")
		fi, err := os.Stat(dir + "/vmlinuz")
		if err != nil {
			continue
		}
		out[name] = installedKernel{version: filepath.Base(dir), vmlinuz: fi.ModTime()}
	}
	return out
}

// gatherLimineBootKernels reads the "//<name>" kernel sub-entries directly under
// a top-level OS directory (skipping the "//Snapshots" submenu and the snapshot
// kernels nested deeper), pulling each entry's declared kernel version and the
// image it boots, then stats that image. esp is where boot():/ resolves (the ESP
// mount, /boot).
func gatherLimineBootKernels(conf, esp string, installed map[string]installedKernel) []limineBootKernel {
	var out []limineBootKernel
	var cur *limineBootKernel
	flush := func() {
		if cur == nil {
			return
		}
		e := *cur
		if e.image != "" {
			if fi, err := os.Stat(e.image); err == nil {
				e.imageExists = true
				if k, ok := installed[e.name]; ok && !k.vmlinuz.IsZero() && fi.ModTime().Before(k.vmlinuz) {
					e.imageOlder = true
				}
			}
		}
		out = append(out, e)
		cur = nil
	}
	inDir := false
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(t, "/") {
			switch limineDepth(t) {
			case 1:
				flush()
				inDir = true
			case 2:
				flush()
				if name := limineNodeName(t); inDir && name != "" && !strings.EqualFold(name, "Snapshots") {
					cur = &limineBootKernel{name: name}
				}
			default:
				flush()
			}
			continue
		}
		if cur == nil {
			continue
		}
		if v := strings.TrimPrefix(t, "comment: Kernel version: "); v != t {
			cur.version = strings.TrimSpace(v)
		} else if p := limineEntryImagePath(t, esp); p != "" {
			cur.image = p
		}
	}
	flush()
	return out
}

// limineEntryImagePath resolves the boot image a kernel entry body line names:
// the UKI "path:" or the regular "module_path:" (the initramfs). "" for any
// other line.
func limineEntryImagePath(bodyLine, esp string) string {
	for _, key := range []string{"path: ", "module_path: "} {
		if v := strings.TrimPrefix(bodyLine, key); v != bodyLine {
			return resolveLimineBootPath(strings.TrimSpace(v), esp)
		}
	}
	return ""
}

// resolveLimineBootPath turns a "boot():/EFI/Linux/ryoku_linux.efi#<hash>" value
// into the absolute path on the running system (boot() is the ESP, mounted at
// esp). "" when the value is not a boot()-relative path.
func resolveLimineBootPath(raw, esp string) string {
	rest := strings.TrimPrefix(raw, "boot():")
	if rest == raw || !strings.HasPrefix(rest, "/") {
		return ""
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimRight(esp, "/") + rest
}

// planLimineKernelImages compares the tool-generated kernel entries against the
// installed kernels. stale: an installed kernel whose entry is missing, names a
// different version than the one installed, or whose image is gone or older than
// the kernel -- each boots into an emergency shell and needs a rebuild. stray:
// an entry for a kernel that is not installed -- a dead menu item. pure and
// order-stable so the decision is unit-testable.
func planLimineKernelImages(installed map[string]installedKernel, entries []limineBootKernel) (stale, stray []string) {
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.name] = true
		inst, ok := installed[e.name]
		if !ok {
			stray = append(stray, e.name)
			continue
		}
		if (e.version != "" && e.version != inst.version) || (e.image != "" && !e.imageExists) || e.imageOlder {
			stale = append(stale, e.name)
		}
	}
	for name := range installed {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	sort.Strings(stray)
	return stale, stray
}

// pruneLimineStrayImages removes the orphaned boot images for kernels the box no
// longer has, then regenerates the menu so their dead entries drop out.
func pruneLimineStrayImages(names []string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var victims []string
	efis, _ := filepath.Glob("/boot/EFI/Linux/*.efi")
	for _, path := range efis {
		base := strings.TrimSuffix(filepath.Base(path), ".efi")
		if i := strings.IndexByte(base, '_'); i >= 0 && want[base[i+1:]] {
			victims = append(victims, path)
		}
	}
	imgs, _ := filepath.Glob("/boot/initramfs-*.img")
	for _, path := range imgs {
		name := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), ".img"), "initramfs-")
		if want[name] {
			victims = append(victims, path)
			if k := "/boot/vmlinuz-" + name; sys.Exists(k) {
				victims = append(victims, k)
			}
		}
	}
	if len(victims) > 0 {
		if err := removeRootFiles(victims...); err != nil {
			return err
		}
	}
	if sys.Has("limine-update") {
		return sys.Sudo("limine-update")
	}
	return nil
}

// reconcileLimineKernelImages keeps every installed kernel's boot image current
// and reports a boot entry for a kernel the box no longer has. Runs on every
// `ryoku update`, so a box whose cachyos entry went stale heals without a
// reinstall; idempotent -- a box whose images all match its kernels is a no-op
// (no costly rebuild).
func reconcileLimineKernelImages(checkOnly bool) recResult {
	if !sys.PkgInstalled("limine") {
		return okRes("not a limine-managed boot on this box")
	}
	installed := installedKernelVersions()
	if len(installed) == 0 {
		return okRes("no installed kernels to check")
	}
	entries := gatherLimineBootKernels(readFileSafe(limineESPConf), "/boot", installed)
	if len(entries) == 0 {
		return okRes("no tool-generated kernel entries yet; the boot tree reconciler owns that")
	}
	stale, stray := planLimineKernelImages(installed, entries)
	if len(stale) == 0 && len(stray) == 0 {
		return okRes("every installed kernel has a current boot image")
	}
	if checkOnly {
		var parts []string
		if len(stale) > 0 {
			parts = append(parts, fmt.Sprintf("boot image missing, older than its kernel, or built for a version no longer installed: %s (boots into an emergency shell)", strings.Join(stale, ", ")))
		}
		if len(stray) > 0 {
			parts = append(parts, fmt.Sprintf("boot entry for a kernel that is not installed: %s (a dead menu item)", strings.Join(stray, ", ")))
		}
		return wouldRes("%s", strings.Join(parts, "; ")).
			withFix("ryoku doctor rebuilds the stale image(s) and prunes the stray entry")
	}
	var done, problems []string
	if len(stale) > 0 {
		if err := rebuildInitramfs(); err != nil {
			return failRes("kernel boot image stale for %s but the rebuild failed: %v", strings.Join(stale, ", "), err).
				withFix("sudo limine-mkinitcpio  (or: sudo mkinitcpio -P)")
		}
		done = append(done, fmt.Sprintf("rebuilt the boot image for %s to match the installed kernel", strings.Join(stale, ", ")))
	}
	if len(stray) > 0 {
		if err := pruneLimineStrayImages(stray); err != nil {
			problems = append(problems, fmt.Sprintf("could not prune the stray entry for %s: %v", strings.Join(stray, ", "), err))
		} else {
			done = append(done, fmt.Sprintf("pruned the stray boot entry for %s (kernel not installed)", strings.Join(stray, ", ")))
		}
	}
	if len(problems) > 0 {
		return warnRes("%s", strings.Join(append(done, problems...), "; ")).
			withFix("check the ESP and rerun sudo ryoku doctor")
	}
	return fixedRes("%s", strings.Join(done, "; "))
}
