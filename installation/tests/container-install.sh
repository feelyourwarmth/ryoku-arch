#!/usr/bin/env bash
# container install smoke test: prove a PACKAGED Ryoku install delivers the whole
# ~/.config a user needs. builds the [ryoku] packages from THIS checkout into a
# local repo, installs ryoku-desktop (which pulls every depend), materializes the
# config as a throwaway user, then asserts the materialized tree. this catches
# the "config file lands in the repo but no package ships it" class the delivery
# contract exists to prevent (docs/updates.md).
#
# runs as root inside an Arch or CachyOS container (archlinux:latest in CI).
# heavy (full build + desktop install), so it is driven by the install-test
# workflow (dispatch/schedule), never on every push.
#
#   installation/tests/container-install.sh [arch|cachyos]   (also RYOKU_TEST_BASE, default arch)
set -euo pipefail

BASE=${1:-${RYOKU_TEST_BASE:-arch}}
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ARCH=x86_64
OUT="$REPO/release/repo/out/$ARCH"
TESTUSER=ryokutest

log() { printf '\033[1;35m::\033[0m %s\n' "$*"; }
die() { printf 'container-install: error: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root (installs packages); got uid $EUID"

# CachyOS differs only in its extra signing keyring; everything downstream (build,
# materialize, assertions) is identical.
case "$BASE" in
  arch)    keyring=(archlinux-keyring) ;;
  cachyos) keyring=(cachyos-keyring archlinux-keyring) ;;
  *)       die "unknown base '$BASE' (want arch|cachyos)" ;;
esac
log "base: $BASE"

# 1. host toolchain. refresh the keyring first so signature checks on the freshly
#    synced core/extra pass on a stale base image, then the build set from
#    release/repo/build-toolchain.packages -- the same file publish-repo.yml
#    reads. makepkg's deps live on the host because build-repo.sh builds
#    --nodeps.
#
#    That file exists because this list used to be a second hand-maintained copy
#    with a comment claiming it matched publish-repo.yml. It drifted: `meson`
#    landed in the other copy for the controller packages, this gate went red on
#    the first main sync, and the publish job was skipped as a result.
pacman -Sy --noconfirm --needed "${keyring[@]}"
mapfile -t toolchain < <(grep -vE '^[[:space:]]*(#|$)' "$REPO/release/repo/build-toolchain.packages")
pacman -Syu --noconfirm --needed "${toolchain[@]}"

# pacman 7 runs install scriptlets in a sandbox that cannot open a network
# namespace inside a container, so post-install hooks (fc-cache, icon cache, ...)
# error out. they are irrelevant here; disable the sandbox for this run.
grep -q '^DisableSandboxNetwork' /etc/pacman.conf \
  || sed -i '/^\[options\]/a DisableSandboxNetwork' /etc/pacman.conf

# 2. build the [ryoku] packages into a local repo with a throwaway key (consumed
#    with SigLevel=Never below). shared with the VM install test.
log "building [ryoku] packages from the checkout -> $OUT"
RYOKU_REPO_NAME=ryoku-local "$REPO/installation/tests/build-ryoku-repo.sh"

# Regression guard for the CachyOS oh-my-zsh-git swap: cachyos-zsh-config depends
# on oh-my-zsh-git, so ryoku-oh-my-zsh must claim it in provides + conflict +
# replaces or `pacman -Syu ryoku-desktop` deadlocks on that box (see the package
# PKGBUILD). Assert it on the freshly built package before we ever publish.
omz=$(find "$OUT" -name 'ryoku-oh-my-zsh-*.pkg.tar.zst' | head -1)
[[ -n $omz ]] || die "ryoku-oh-my-zsh package was not built"
pkginfo=$(bsdtar -xOf "$omz" .PKGINFO)
for field in provides conflict replaces; do
  grep -qE "^$field = oh-my-zsh-git" <<<"$pkginfo" \
    || die "ryoku-oh-my-zsh is missing '$field = oh-my-zsh-git'; a CachyOS box (cachyos-zsh-config) would deadlock the install"
done
log "oh-my-zsh-git swap metadata present on ryoku-oh-my-zsh"

# 3. register the local repo and install the desktop. SigLevel=Never relaxes
#    verification for THIS repo only (it is signed with the throwaway key above,
#    which pacman does not trust); official repos keep their SigLevel. an
#    unresolved depend must fail loudly here -- that is the whole point.
cat >>/etc/pacman.conf <<EOF

[ryoku-local]
SigLevel = Never
Server = file://$OUT
EOF

pacman -Sy --noconfirm
log "installing ryoku-desktop from [ryoku-local]"
pacman -S --noconfirm ryoku-desktop

[[ -d /usr/share/ryoku/config ]] || die "ryoku-desktop did not lay /usr/share/ryoku/config"
[[ -x /usr/bin/ryoku ]] || die "the ryoku CLI was not installed"

# 4. materialize as a throwaway user, forcing HOME/USER like deploy.sh's
#    ryoku_deploy_materialize (runuser keeps root's env otherwise).
id "$TESTUSER" &>/dev/null || useradd --create-home "$TESTUSER"
log "materializing config as $TESTUSER"
runuser -u "$TESTUSER" -- env \
  "HOME=/home/$TESTUSER" "USER=$TESTUSER" "LOGNAME=$TESTUSER" \
  ryoku materialize

# 5. assert the materialized ~/.config is complete: a representative, high-signal
#    slice spanning shell, compositor, palette, and every per-app config.
cfg="/home/$TESTUSER/.config"
files=(
  quickshell/shell/shell.qml
  hypr/hyprland.lua
  fish/config.fish
  ryoku-terminal/env.sh
  bash/ryoku.bash
  bash/rashin.bash
  zsh/ryoku.zsh
  zsh/rashin.zsh
  starship.toml
  kitty/kitty.conf
  yazi/yazi.toml
  nvim/init.lua
  pip/pip.conf
  wireplumber/wireplumber.conf.d/51-ryoku-bluetooth.conf
  chromium-flags.conf
)
dirs=(quickshell/hub)

missing=()
for f in "${files[@]}"; do
  [[ -s "$cfg/$f" ]] || missing+=("$cfg/$f")
done
for d in "${dirs[@]}"; do
  [[ -n $(find "$cfg/$d" -mindepth 1 -type f -print -quit 2>/dev/null) ]] \
    || missing+=("$cfg/$d/ (empty or absent)")
done
# the nvim handler and the default-app map ship system-wide (not materialized:
# ~/.config/mimeapps.list belongs to the user's own "Set as default" picks), so
# check the system tree.
[[ -s /usr/share/applications/ryoku-nvim.desktop ]] \
  || missing+=("/usr/share/applications/ryoku-nvim.desktop")
[[ -s /usr/share/applications/mimeapps.list ]] \
  || missing+=("/usr/share/applications/mimeapps.list")
if [[ -e "$cfg/mimeapps.list" ]]; then
  missing+=("$cfg/mimeapps.list (materialize must not create the user's default-app file)")
fi

# Every Ryoku.* QML module the shell (and hub/apps) imports must be installed on
# the system Qt import path. deploy.sh puts them there for a dev box, so a package
# that forgets one ships a desktop only a dev box can run: qs -c shell fails the
# import at login and paints a bare grey Hyprland screen with no shell. Derive the
# set from the source imports so a new module cannot be added without packaging it.
mapfile -t ryoku_mods < <(
  grep -rhoE "import Ryoku\.[A-Za-z0-9]+" \
    "$REPO/ryoku/shell" "$REPO/ryoku/hub" "$REPO/ryoku/apps" "$REPO/ryoku/ui" 2>/dev/null \
    | awk '{print $2}' | sort -u
)
for mod in "${ryoku_mods[@]}"; do
  [[ -s "/usr/lib/qt6/qml/Ryoku/${mod#Ryoku.}/qmldir" ]] \
    || missing+=("/usr/lib/qt6/qml/Ryoku/${mod#Ryoku.}/qmldir (imported as $mod, but no package installs it)")
done

if (( ${#missing[@]} )); then
  echo "container-install: FAIL -- packaged install is missing config a user needs:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  exit 1
fi

# 6. every shipped QML root must LOAD against the installed Qt modules, not just
#    exist. One file that cannot instantiate (a handler on a signal the type does
#    not have, a property that is not there) blanks its whole surface: a Hub page
#    renders empty, a shell root paints bare Hyprland. This is the same import
#    path a user's session resolves, so a finding here is a finding on their box.
log "linting the materialized QML for load failures"
"$REPO/bin/ryoku-dev-lint-qml" "$cfg/quickshell/shell" "$cfg/quickshell/hub" \
  || die "shipped QML cannot load (see above); a user's desktop would come up broken"

# 7. release naming and channels. the package must carry the /etc/ryoku-release
#    marker (a hand build names itself local-<pkgver>), the CLI must read it
#    and report the channel from the [ryoku] Server line, and `ryoku track`
#    must refuse to move a box whose [ryoku] points at a mirror Ryoku does not
#    publish (this local repo), instead of rewriting it into a channel it
#    cannot serve. the live switch itself (stable -> testing -> a pinned
#    release) needs the published channels and runs in the VM install test.
log "checking release naming and channel handling"
[[ -f /etc/ryoku-release ]] || die "ryoku-desktop did not ship /etc/ryoku-release"
grep -qE '^RELEASE=local-[0-9]' /etc/ryoku-release || die "unexpected /etc/ryoku-release: $(cat /etc/ryoku-release)"
grep -qE '^CHANNEL=local$' /etc/ryoku-release || die "hand build must be marked CHANNEL=local"
name=$(tr -d '[:space:]' < "$REPO/CODENAME")
grep -qx "NAME=$name" /etc/ryoku-release || die "ryoku-desktop did not carry the line's name ($name) into /etc/ryoku-release"
ver=$(runuser -u "$TESTUSER" -- env "HOME=/home/$TESTUSER" ryoku version)
[[ $ver == local-* ]] || die "ryoku version should print the release marker, got: $ver"
pretty=$(runuser -u "$TESTUSER" -- env "HOME=/home/$TESTUSER" ryoku version --pretty)
[[ $pretty == "$name local-"* ]] || die "ryoku version --pretty should lead with the name, got: $pretty"
cat >>/etc/pacman.conf <<EOF

[ryoku]
SigLevel = Never
Server = file://$OUT
EOF
if runuser -u "$TESTUSER" -- env "HOME=/home/$TESTUSER" ryoku track testing 2>/tmp/track.err; then
  die "ryoku track must refuse a [ryoku] repo Ryoku does not publish"
fi
grep -q "does not publish" /tmp/track.err || die "unexpected track refusal: $(cat /tmp/track.err)"
grep -q "^Server = file://$OUT" /etc/pacman.conf || die "track rewrote a foreign [ryoku] Server line"

# 8. the boot guard. the ryoku package ships the unit and the tmpfiles entry
#    for the sessions' boot records; `ryoku boot-guard` with nothing pending is
#    a no-op, and a marker whose desktop was proven up in another boot is
#    disarmed rather than counted.
log "checking the boot guard"
[[ -f /usr/lib/systemd/system/ryoku-boot-guard.service ]] || die "ryoku did not ship ryoku-boot-guard.service"
[[ -f /usr/lib/tmpfiles.d/ryoku.conf ]] || die "ryoku did not ship its tmpfiles entry"
systemd-tmpfiles --create /usr/lib/tmpfiles.d/ryoku.conf
[[ -d /var/lib/ryoku/boot ]] || die "tmpfiles did not create /var/lib/ryoku/boot"
[[ $(stat -c %a /var/lib/ryoku/boot) == 1777 ]] || die "/var/lib/ryoku/boot must be 1777, got $(stat -c %a /var/lib/ryoku/boot)"
ryoku boot-guard || die "boot-guard with nothing pending must succeed"
printf '{"from":"v0.0.1","to":"v0.0.2","armedBoot":"armed-boot","boots":0,"at":"x"}\n' >/var/lib/ryoku/update-pending.json
runuser -u "$TESTUSER" -- sh -c "echo later-boot >/var/lib/ryoku/boot/ok-$(id -u "$TESTUSER")" || die "a session cannot record its boot"
ryoku boot-guard | grep -q "disarmed" || die "a proven boot must disarm the guard"
[[ ! -e /var/lib/ryoku/update-pending.json ]] || die "disarm left the marker behind"

log "container-install: OK -- ryoku-desktop delivered the full config to $cfg"
