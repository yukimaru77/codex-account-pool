#!/bin/sh
# Install the local codex-account-pool: binary, pool.json, codex wrapper and
# a user service (launchd on macOS, systemd --user on Linux). Safe to re-run.
#
# usage: scripts/install-local.sh [--codex-bin PATH] [--accounts-dir DIR]
#                                 [--prefix DIR] [--pool-home DIR]
#
# Environment:
#   CODEX_POOL_BIN=PATH       use this prebuilt codex-pool instead of go build
#   CODEX_POOL_NO_SERVICE=1   do not install or restart the service
set -eu

usage() {
	sed -n '5,6p' "$0" | sed 's/^# //; s/^#//' >&2
	exit 2
}

die() {
	echo "install-local: $*" >&2
	exit 1
}

CODEX_BIN=
ACCOUNTS_DIR=$HOME/.codex-accounts
PREFIX=$HOME/.local/bin
POOL_HOME=$HOME/.codex-pool

while [ $# -gt 0 ]; do
	case $1 in
	--codex-bin | --accounts-dir | --prefix | --pool-home)
		[ $# -ge 2 ] || die "$1 needs a value"
		case $1 in
		--codex-bin) CODEX_BIN=$2 ;;
		--accounts-dir) ACCOUNTS_DIR=$2 ;;
		--prefix) PREFIX=$2 ;;
		--pool-home) POOL_HOME=$2 ;;
		esac
		shift 2
		;;
	-h | --help) usage ;;
	*) die "unknown argument: $1" ;;
	esac
done

REPO=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)

# absdir DIR: create DIR and print its absolute path.
absdir() {
	mkdir -p "$1"
	(CDPATH='' cd "$1" && pwd)
}

# check_path PATH: reject characters that the wrapper, plist or unit would
# need to escape.
check_path() {
	case $1 in
	*[\"\$\`\\%]* | *'
'*) die "unsupported character in path: $1" ;;
	esac
}

# canon_dir DIR: physical path of DIR, empty when it does not exist.
canon_dir() {
	(CDPATH='' cd "$1" 2>/dev/null && pwd -P) || true
}

# link_target FILE: FILE's symlink target resolved against FILE's directory.
link_target() {
	t=$(readlink "$1") || return 1
	case $t in
	/*) echo "$t" ;;
	*) echo "$(dirname "$1")/$t" ;;
	esac
}

# reaches_wrapper FILE: FILE is the wrapper location ($PREFIX/codex, compared
# by physical directory) or a symlink chain passing through it. Each hop is
# checked, so a $PREFIX/codex that links to the real codex does not hide it.
reaches_wrapper() {
	f=$1
	n=0
	while :; do
		d=$(canon_dir "$(dirname "$f")")
		[ -n "$d" ] && [ "$d/$(basename "$f")" = "$WRAPPER_REAL" ] && return 0
		[ -L "$f" ] || return 1
		f=$(link_target "$f") || return 1
		n=$((n + 1))
		[ $n -lt 40 ] || return 1
	done
}

# final_target FILE: end of FILE's symlink chain.
final_target() {
	f=$1
	n=0
	while [ -L "$f" ] && [ $n -lt 40 ]; do
		f=$(link_target "$f") || break
		n=$((n + 1))
	done
	echo "$f"
}

# is_pool_wrapper FILE: FILE is a script that runs codex-pool launch.
is_pool_wrapper() {
	[ -f "$1" ] && grep -q 'codex-pool launch' "$1" 2>/dev/null
}

PREFIX=$(absdir "$PREFIX")
POOL_HOME=$(absdir "$POOL_HOME")
case $ACCOUNTS_DIR in
/*) ;;
*) ACCOUNTS_DIR=$(pwd)/$ACCOUNTS_DIR ;;
esac
CONFIG=$POOL_HOME/pool.json
STATE=$POOL_HOME/state
LOG=$STATE/serve.log
POOL_BIN=$PREFIX/codex-pool
WRAPPER=$PREFIX/codex
WRAPPER_REAL=$(canon_dir "$PREFIX")/codex
for p in "$PREFIX" "$POOL_HOME" "$ACCOUNTS_DIR" "$HOME"; do
	check_path "$p"
done

# 1. codex-pool binary (replaced by rename so a running serve keeps its file).
if [ -n "${CODEX_POOL_BIN:-}" ]; then
	[ -x "$CODEX_POOL_BIN" ] || die "CODEX_POOL_BIN is not executable: $CODEX_POOL_BIN"
	cp "$CODEX_POOL_BIN" "$POOL_BIN.tmp.$$"
else
	command -v go >/dev/null 2>&1 || die "go is required (or set CODEX_POOL_BIN)"
	(cd "$REPO" && go build -o "$POOL_BIN.tmp.$$" ./cmd/codex-pool)
fi
chmod 0755 "$POOL_BIN.tmp.$$"
mv -f "$POOL_BIN.tmp.$$" "$POOL_BIN"
echo "installed $POOL_BIN"

# 2-3. pool.json. An existing config is kept as is, including its codex_bin.
if [ -e "$CONFIG" ]; then
	echo "keeping existing $CONFIG"
	stored=$(sed -n 's/^ *"codex_bin": *"\(.*\)",\{0,1\} *$/\1/p' "$CONFIG")
	if [ -n "$CODEX_BIN" ] && [ "$CODEX_BIN" != "$stored" ]; then
		echo "install-local: WARNING: --codex-bin $CODEX_BIN ignored; $CONFIG keeps codex_bin ${stored:-(unset)}. Edit codex_bin there to change it." >&2
	fi
	if [ -n "$stored" ] && reaches_wrapper "$stored"; then
		echo "install-local: WARNING: codex_bin $stored in $CONFIG is the wrapper location; codex would exec itself. Set it to the real codex." >&2
	fi
else
	if [ -z "$CODEX_BIN" ]; then
		# $PREFIX/codex is skipped: it is the wrapper or is moved aside below.
		prefix_real=$(canon_dir "$PREFIX")
		old_ifs=$IFS
		IFS=:
		set -f
		for dir in $PATH; do
			IFS=$old_ifs
			candidate=${dir:-.}/codex
			[ -f "$candidate" ] && [ -x "$candidate" ] || continue
			[ "$(canon_dir "${dir:-.}")" = "$prefix_real" ] && continue
			reaches_wrapper "$candidate" && continue
			is_pool_wrapper "$candidate" && continue
			CODEX_BIN=$candidate
			break
		done
		set +f
		IFS=$old_ifs
		[ -n "$CODEX_BIN" ] || die "no codex found on PATH (other than the pool wrapper); pass --codex-bin PATH"
	fi
	case $CODEX_BIN in
	/*) ;;
	*) die "--codex-bin must be an absolute path: $CODEX_BIN" ;;
	esac
	if reaches_wrapper "$CODEX_BIN"; then
		die "codex bin $CODEX_BIN is (or links to) the wrapper location $WRAPPER; codex would exec itself. Pass the real codex, e.g. --codex-bin $(final_target "$CODEX_BIN")"
	fi
	[ -x "$CODEX_BIN" ] || die "codex bin is not executable: $CODEX_BIN"
	is_pool_wrapper "$CODEX_BIN" && die "codex bin is a codex-pool wrapper: $CODEX_BIN"
	"$POOL_BIN" init --config "$CONFIG" --accounts-dir "$ACCOUNTS_DIR" \
		--codex-home "$HOME/.codex" --codex-bin "$CODEX_BIN" --listen 127.0.0.1:18473
	echo "codex_bin: $CODEX_BIN"
fi
mkdir -p "$STATE"

# json_string S: S as a JSON string literal (paths are already checked).
json_string() {
	printf '"%s"' "$(printf '%s' "$1" | sed 's/[\\"]/\\&/g')"
}

# 3b. kb-pool.json for kb-repomap --pool-config and pool-rr. Never overwritten.
listen=$(sed -n 's/^ *"listen": *"\(.*\)",\{0,1\} *$/\1/p' "$CONFIG")
listen=${listen:-127.0.0.1:18473}
case $listen in
:* | 0.0.0.0:*) listen=127.0.0.1:${listen##*:} ;;
esac
ORIGIN=http://$listen
KB_POOL=$POOL_HOME/kb-pool.json
if [ -e "$KB_POOL" ]; then
	echo "keeping existing $KB_POOL"
else
	printf '{"origin": %s, "key_file": %s}\n' "$(json_string "$ORIGIN")" \
		"$(json_string "$STATE/client.key")" >"$KB_POOL.tmp.$$"
	chmod 0600 "$KB_POOL.tmp.$$"
	mv -f "$KB_POOL.tmp.$$" "$KB_POOL"
	echo "wrote $KB_POOL"
fi

# 4. codex wrapper. Anything else at $WRAPPER is kept as codex.pre-pool-<date>.
tmp_wrapper=$WRAPPER.tmp.$$
cat >"$tmp_wrapper" <<WRAPPER
#!/bin/sh
# codex-pool launch: picks CODEX_HOME by remaining quota.
exec "$POOL_BIN" launch --config "$CONFIG" -- "\$@"
WRAPPER
chmod 0755 "$tmp_wrapper"
if { [ -e "$WRAPPER" ] || [ -L "$WRAPPER" ]; } && ! is_pool_wrapper "$WRAPPER"; then
	backup=$WRAPPER.pre-pool-$(date +%Y%m%d)
	if [ -e "$backup" ] || [ -L "$backup" ]; then
		backup=$backup-$(date +%H%M%S)
	fi
	mv "$WRAPPER" "$backup"
	echo "moved previous $WRAPPER to $backup"
fi
mv -f "$tmp_wrapper" "$WRAPPER"
echo "installed wrapper $WRAPPER"

# render TEMPLATE OUT: fill @...@ placeholders (paths already checked).
render() {
	sed_escape() { printf '%s' "$1" | sed 's/[&|\\]/\\&/g'; }
	sed -e "s|@CODEX_POOL_BIN@|$(sed_escape "$POOL_BIN")|g" \
		-e "s|@POOL_CONFIG@|$(sed_escape "$CONFIG")|g" \
		-e "s|@POOL_HOME@|$(sed_escape "$POOL_HOME")|g" \
		-e "s|@SERVE_LOG@|$(sed_escape "$LOG")|g" \
		"$1" >"$2.tmp.$$"
	mv -f "$2.tmp.$$" "$2"
}

# 5. service.
if [ "${CODEX_POOL_NO_SERVICE:-}" = 1 ]; then
	echo "skipping service (CODEX_POOL_NO_SERVICE=1)"
else
	case $(uname -s) in
	Darwin)
		for p in "$POOL_BIN" "$CONFIG" "$POOL_HOME" "$LOG"; do
			case $p in *[\&\<\>]*) die "unsupported character for plist: $p" ;; esac
		done
		label=com.local.codex-pool
		plist=$HOME/Library/LaunchAgents/$label.plist
		domain=gui/$(id -u)
		mkdir -p "$(dirname "$plist")"
		render "$REPO/deploy/$label.plist.tmpl" "$plist"
		if launchctl print "$domain/$label" >/dev/null 2>&1; then
			launchctl bootout "$domain/$label" || true
		fi
		# bootout finishes asynchronously; retry bootstrap briefly.
		n=0
		until launchctl bootstrap "$domain" "$plist" 2>/dev/null; do
			n=$((n + 1))
			[ $n -lt 10 ] || die "launchctl bootstrap $domain $plist failed"
			sleep 1
		done
		echo "started launchd service $label ($plist)"
		;;
	Linux)
		unit=$HOME/.config/systemd/user/codex-pool.service
		mkdir -p "$(dirname "$unit")"
		render "$REPO/deploy/codex-pool.service.tmpl" "$unit"
		systemctl --user daemon-reload
		systemctl --user enable --now codex-pool
		# pick up a rebuilt binary when the service was already running
		systemctl --user restart codex-pool
		echo "started systemd user service codex-pool ($unit)"
		;;
	*)
		echo "unsupported OS for the service; run: $POOL_BIN serve --config $CONFIG" >&2
		;;
	esac
fi

# 6. next steps.
cat <<EOF

Next steps:
  "$POOL_BIN" account add main --from ~/.codex/auth.json --config "$CONFIG"
  "$POOL_BIN" account add NAME --config "$CONFIG"      # log in another account
  "$POOL_BIN" account list --config "$CONFIG"

For kb-repomap (add to your shell profile):
  export KB_POOL_ORIGIN=$ORIGIN
  export KB_POOL_KEY_FILE="$STATE/client.key"
For kb create, add to build_args in ~/.config/kb/config.json:
  "--pool-config", "$KB_POOL"

Service log: $LOG
EOF
first=$(command -v codex 2>/dev/null || true)
if [ "$first" != "$WRAPPER" ]; then
	cat <<EOF

WARNING: $PREFIX must come before the old codex location in PATH.
'codex' currently resolves to: ${first:-(nothing)}
EOF
fi
echo
echo "which -a codex:"
found=$(which -a codex 2>/dev/null || true)
if [ -n "$found" ]; then
	printf '%s\n' "$found" | sed 's/^/  /'
else
	echo "  (none on PATH)"
fi
