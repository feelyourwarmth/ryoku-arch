package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The startup restore (restoreOutputs) is a single best-effort pass, but two
// things it depends on can lag the daemon at login: the file the stored choice
// names (a home or media mount that is not ready yet) and the compositor
// outputs a live wallpaper spans. A one-shot restore that met either left the
// shell on the empty frame -- the grey default -- until the next manual set.
// retryRestore closes the first gap by re-running until the choice applies;
// watchOutputs closes the second by re-applying whenever Hyprland brings an
// output up after the first pass ran.

const (
	restoreRetryWindow   = 30 * time.Second
	restoreRetryInterval = 1 * time.Second
)

// retryRestore re-runs restoreOutputs until something is on screen or the window
// elapses. It runs only when the first pass left the desktop bare (applied ==
// 0) and stops the moment a frame lands, so a wallpaper already showing is never
// re-revealed; a pass that still cannot apply publishes nothing, so the retries
// are silent.
func (d *daemon) retryRestore() {
	deadline := time.Now().Add(restoreRetryWindow)
	for time.Now().Before(deadline) {
		time.Sleep(restoreRetryInterval)
		if want, applied := d.restoreOutputs(); want == 0 || applied > 0 {
			return
		}
	}
}

// externalLiveStored reports whether the persisted choice is a live wall the
// external ryogami-live engine plays. Only that wall needs re-placing when an
// output appears: its player is spawned per output, where a static frame and
// the in-shell video engine both ride the retained topic the shell repaints on
// any new screen by itself.
func (d *daemon) externalLiveStored() bool {
	if wallPrefs().Engine == "in_shell" {
		return false
	}
	state := map[string]map[string]interface{}{}
	loadJSON(filepath.Join(d.config().cacheDir(), "outputs.json"), &state)
	for _, e := range state {
		if e["type"] == "video" {
			return true
		}
	}
	return false
}

// watchOutputs re-spans the external live-wall player whenever Hyprland adds an
// output. A static frame and the in-shell video engine ride the retained topic
// the shell repaints on any new screen by itself, but the ryogami-live player is
// spawned per output at restore time, so a monitor that appears after the first
// pass (a login-time race, a hotplug, a DP-MST panel that enumerates late) keeps
// no live wall until a re-apply. Mirrors ryoku-shell's hyprwatch.go event loop.
func (d *daemon) watchOutputs() {
	for {
		sock := hyprEventSocket()
		if sock == "" {
			time.Sleep(restoreRetryInterval)
			continue
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			time.Sleep(restoreRetryInterval)
			continue
		}
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "monitoradded") && d.config().restoreEnabled() && d.externalLiveStored() {
				d.restoreOutputs()
			}
		}
		_ = conn.Close()
		time.Sleep(restoreRetryInterval)
	}
}

// hyprEventSocket resolves Hyprland's event socket (.socket2.sock). It picks the
// newest instance directory rather than trusting HYPRLAND_INSTANCE_SIGNATURE,
// which a daemon (re)started from a lagging user-manager environment can inherit
// stale. "" when no compositor has landed its socket yet, so the watcher backs
// off and retries. The modern path is under XDG_RUNTIME_DIR; older Hyprland kept
// it in /tmp.
func hyprEventSocket() string {
	best, bestMod := "", time.Time{}
	for _, base := range hyprRunDirs() {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sock := filepath.Join(base, e.Name(), ".socket2.sock")
			fi, err := os.Stat(sock)
			if err != nil {
				continue
			}
			if best == "" || fi.ModTime().After(bestMod) {
				best, bestMod = sock, fi.ModTime()
			}
		}
	}
	return best
}

// hyprRunDirs lists the per-user directories Hyprland drops each instance's
// sockets in, newest layout first.
func hyprRunDirs() []string {
	var dirs []string
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		dirs = append(dirs, filepath.Join(rt, "hypr"))
	}
	dirs = append(dirs, filepath.Join("/tmp", "hypr"))
	return dirs
}
