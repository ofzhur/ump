#!/usr/bin/env bash
# Test the packages of ump in containers (make package-test): two builds
# of this tree, v0.0.1 and v0.0.2, for the architecture of the container
# host, each distribution checking that
#  - v0.0.1 installs ump, its completions and the units, with the iPXE
#    binaries it recommends, enables nothing, and the preset keeps a site
#    disabled; ump pxe unit prints the installed units; systemd-analyze
#    verify passes them; the rpm names no builder's host;
#  - v0.0.2 upgrades it, keeping a site's settings and its enablement;
#  - removing it disables the site and keeps its settings, and a deb's
#    purge removes what the PXE servers kept;
# and, in a Debian container that runs systemd, with a rescue image and
# an HTTP backend of its own, which refuses a request without the site's
# token: ump-pxe-check@test fails without the token and passes as its
# dynamic user with it as a credential, binding TFTP on port 69;
# ump-pxe@test serves boot.ipxe and writes its status; an upgrade
# restarts it, a removal stops it.
#
# Usage: packaging/test.sh [IMAGE...]
#   (default: debian:12 debian:13 ubuntu:24.04 ubuntu:26.04 fedora:43
#    rockylinux/rockylinux:9 rockylinux/rockylinux:10)
# Environment: CONTAINER (docker or podman), SYSTEMD=0 (no systemd test),
# NFPM_VERSION (the Makefile's).
set -euo pipefail
cd "$(dirname "$0")/.."
CONTAINER="${CONTAINER:-$(command -v docker || command -v podman)}"
SYSTEMD="${SYSTEMD:-1}"
images=("$@")
[ ${#images[@]} -gt 0 ] || images=(debian:12 debian:13 ubuntu:24.04 ubuntu:26.04 fedora:43 rockylinux/rockylinux:9 rockylinux/rockylinux:10)
case "$("$CONTAINER" info --format '{{.Architecture}}' 2>/dev/null || uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) echo "test.sh: no packages for the container host's architecture" >&2; exit 2 ;;
esac
prefix="ump-pkgtest-$$"
work="$(mktemp -d)"
cleanup() {
	"$CONTAINER" ps -aq --filter "name=^$prefix-" | xargs "$CONTAINER" rm -f >/dev/null 2>&1 || true
	"$CONTAINER" rmi -f "$prefix-systemd" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

for v in 0.0.1 0.0.2; do
	echo "==> the packages v$v for $arch"
	OUT="$work/v$v" VERSION="v$v" ARCHS="$arch" NFPM_VERSION="${NFPM_VERSION:-$(sed -n 's/^NFPM_VERSION ?= //p' Makefile)}" packaging/build.sh >/dev/null
done
chmod -R a+rX "$work"

# in a container without systemd running: install, upgrade, remove, with
# systemctl and systemd-analyze offline
read -r -d '' offline <<'EOF' || true
set -eu
fail() { echo "  FAILED: $*"; exit 1; }
ok() { what="$1"; shift; out="$("$@" 2>&1)" || { echo "$out" | tail -n 20; fail "$what"; }; echo "  ok: $what"; }
if command -v apt-get >/dev/null; then
	fmt=deb recommended=ipxe zsh=/usr/share/zsh/vendor-completions/_ump
	export DEBIAN_FRONTEND=noninteractive
	pm() { out="$(apt-get -y -qq -o=Dpkg::Use-Pty=0 "$@" 2>&1)" || { echo "$out" | tail -n 20; return 1; }; }
	installed() { dpkg-query -W -f '${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'; }
	pm update
else
	fmt=rpm recommended="ipxe-bootimgs-x86 ipxe-bootimgs-aarch64" zsh=/usr/share/zsh/site-functions/_ump
	pm() { out="$(dnf -y -q --setopt=install_weak_deps=True "$@" 2>&1)" || { echo "$out" | tail -n 20; return 1; }; }
	installed() { rpm -q "$1" >/dev/null 2>&1; }
fi
pm install systemd
old="$(ls /pkg/v0.0.1/*."$fmt")" new="$(ls /pkg/v0.0.2/*."$fmt")"
version() { [ "$(ump version | cut -d' ' -f2)" = "$1" ]; }
files() {
	for f in /usr/bin/ump /usr/lib/systemd/system/ump-pxe@.service /usr/lib/systemd/system/ump-pxe-check@.service \
		/usr/lib/systemd/system-preset/90-ump.preset /usr/share/ump/pxe.env.example \
		/usr/share/bash-completion/completions/ump /usr/share/fish/vendor_completions.d/ump.fish "$zsh"; do
		[ -f "$f" ] || { echo "no $f"; return 1; }
	done
	[ "$(stat -c %a /etc/ump/pxe)" = 755 ]
}
recommends() { for p in $recommended; do installed "$p" || { echo "$p is not installed"; return 1; }; done; }
units() { [ "$(ump pxe unit)" = "$(cat /usr/lib/systemd/system/ump-pxe@.service)" ] && [ "$(ump pxe unit --check)" = "$(cat /usr/lib/systemd/system/ump-pxe-check@.service)" ]; }
unenabled() { ! ls /etc/systemd/system/*.wants/ump-* 2>/dev/null; }
preset() { unenabled && systemctl preset ump-pxe@test.service && unenabled; }
enabled() { [ -L /etc/systemd/system/multi-user.target.wants/ump-pxe@test.service ]; }
settings() { grep -qx UMP_PXE_HTTP=127.0.0.1:8067 /etc/ump/pxe/test.env; }
gone() { [ ! -e /usr/bin/ump ] && [ ! -e /usr/lib/systemd/system/ump-pxe@.service ]; }
installs() { pm install "$old" && files && version v0.0.1; }
upgrades() { pm install "$new" && version v0.0.2 && settings && enabled; }
removes() { pm remove ump && gone && settings && unenabled; }
purges() { pm purge ump && settings && [ ! -e /var/lib/ump-pxe ] && [ ! -e /var/lib/private/ump-pxe ] && [ ! -e /var/cache/private/ump-pxe ]; }
buildhost() { [ "$(rpm -qp --qf '%{BUILDHOST}' "$old")" = ump.zhurko.de ]; }

ok "install $(basename "$old"): ump v0.0.1, the units, the preset, the example, the completions, /etc/ump/pxe" installs
[ "$fmt" = deb ] || ok "the rpm's build host: ump.zhurko.de" buildhost
ok "the recommended iPXE binaries: $recommended" recommends
ok "ump pxe unit prints the installed units" units
ok "systemd-analyze verify ump-pxe@test.service ump-pxe-check@test.service" systemd-analyze verify ump-pxe@test.service ump-pxe-check@test.service
ok "nothing enabled; the preset keeps a site disabled" preset
install -m 0600 /dev/null /etc/ump/pxe/test.env
printf 'UMP_STATE=http://127.0.0.1:8070\nUMP_PXE_HTTP=127.0.0.1:8067\n' > /etc/ump/pxe/test.env
ok "a site enabled" systemctl enable ump-pxe@test.service
ok "upgrade to $(basename "$new"): ump v0.0.2, the site's settings and enablement kept" upgrades
ok "remove: ump gone, the site disabled, its settings kept" removes
if [ "$fmt" = deb ]; then
	mkdir -p /var/lib/private/ump-pxe/test /var/cache/private/ump-pxe/test /var/lib/ump-pxe
	ln -s ../private/ump-pxe/test /var/lib/ump-pxe/test
	ok "purge: the servers' journals and caches gone, the site's settings kept" purges
fi
EOF

for image in "${images[@]}"; do
	echo "==> $image"
	"$CONTAINER" run --rm --name "$prefix-${image//[^a-z0-9]/-}" -e UMP_NO_UPDATE_CHECK=1 -v "$work:/pkg:ro" "$image" sh -c "$offline"
done

[ "$SYSTEMD" = 1 ] || { echo "==> the packages passed"; exit 0; }
echo "==> debian:13 with systemd"
"$CONTAINER" build -q -t "$prefix-systemd" - >/dev/null <<'EOF'
FROM debian:13
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd systemd-sysv python3 curl >/dev/null
CMD ["/sbin/init"]
EOF
"$CONTAINER" run -d --name "$prefix-systemd" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
	-e container=docker -v "$work:/pkg:ro" "$prefix-systemd" >/dev/null
"$CONTAINER" exec -i "$prefix-systemd" bash -s <<'EOF'
set -euo pipefail
fail() { echo "  FAILED: $*"; journalctl -b -n 30 --no-pager -u 'ump-pxe*' 2>/dev/null | sed 's/^/    /'; exit 1; }
ok() { local what="$1" out; shift; out="$("$@" 2>&1)" || { echo "$out" | tail -n 20; fail "$what"; }; echo "  ok: $what"; }
wait_for() { local what="$1" i; shift; for i in $(seq 30); do "$@" >/dev/null 2>&1 && { echo "  ok: $what"; return; }; sleep 1; done; fail "$what"; }
export DEBIAN_FRONTEND=noninteractive UMP_NO_UPDATE_CHECK=1
systemctl is-system-running --wait >/dev/null 2>&1 || true
# systemd puts a service's credentials under /run, which the container's
# tmpfs keeps private: shared, as on a host
mount --make-shared /run
apt-get install -y -qq -o=Dpkg::Use-Pty=0 /pkg/v0.0.1/*.deb >/dev/null

# a rescue image of its own, served on 127.0.0.1:8000
mkdir -p /srv/rescue/amd64 && cd /srv/rescue/amd64
printf 'bzImage of the test' > vmlinuz && head -c 65536 /dev/urandom > initrd.zst
sum() { sha256sum "$1" | cut -d' ' -f1; }
cat > manifest.json <<JSON
{"version": "2026.11.01", "arch": "amd64", "features": ["boot-config", "pxe-install"], "memory_mb": 412,
 "files": {"vmlinuz": {"sha256": "$(sum vmlinuz)", "size": $(stat -c %s vmlinuz)}, "initrd.zst": {"sha256": "$(sum initrd.zst)", "size": $(stat -c %s initrd.zst)}}}
JSON
systemd-run -q --unit rescue-http python3 -m http.server --bind 127.0.0.1 --directory /srv/rescue 8000
# the backend, ump's HTTP backend over a directory on 127.0.0.1:8070,
# with the operator's configuration: a request without the token is 401
mkdir -p /srv/backend
printf 'staging:\n  image: http://127.0.0.1:8000\n' > /srv/backend/config.yaml
printf 'sites:\n  test:\n    description: the package test\nmachines:\n  web-07:\n    site: test\n    mac: 0c:c4:7a:aa:bb:01\n    arch: amd64\n' > /srv/backend/pxe.yaml
cat > /srv/backend.py <<'PY'
# internal/backend/http.go's protocol over the directory argv[1] on port
# argv[2], for the bearer tokens of the file argv[3] alone
import hashlib, http.server, json, os, sys, threading
root, port, tokens = sys.argv[1], int(sys.argv[2]), open(sys.argv[3]).read().split()
locks, mutex = {}, threading.Lock()
version = lambda data: hashlib.sha256(data).hexdigest()[:16]

class Backend(http.server.BaseHTTPRequestHandler):
    def answer(self, code, body=b'', etag=None):
        self.send_response(code)
        if etag:
            self.send_header('ETag', '"%s"' % etag)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def serve(self):
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        if self.headers.get('Authorization') not in ['Bearer ' + t for t in tokens]:
            return self.answer(401)
        name, _, query = self.path.partition('?')
        name = name.strip('/')
        key = query.partition('key=')[2]
        with mutex:
            if self.command == 'LOCK':
                if key in locks:
                    return self.answer(423, locks[key])
                locks[key] = body
                return self.answer(200)
            if self.command == 'UNLOCK':
                locks.pop(key, None)
                return self.answer(204)
            if query == 'locks':
                return self.answer(200, b'[' + b','.join(locks.values()) + b']')
            if query == 'list':
                files = []
                for d, _, names in os.walk(root):
                    for n in names:
                        with open(os.path.join(d, n), 'rb') as f:
                            files.append({'path': os.path.relpath(os.path.join(d, n), root), 'version': version(f.read())})
                return self.answer(200, json.dumps(files).encode())
            if not name or '..' in name.split('/'):
                return self.answer(400)
            path = os.path.join(root, name)
            current = open(path, 'rb').read() if os.path.isfile(path) else None
            if self.command == 'GET':
                return self.answer(200, current, version(current)) if current is not None else self.answer(404)
            if self.command == 'PUT':
                match = self.headers.get('If-Match')
                if (self.headers.get('If-None-Match') == '*' and current is not None
                        or match and (current is None or match != '"%s"' % version(current))):
                    return self.answer(412)
                os.makedirs(os.path.dirname(path), exist_ok=True)
                with open(path + '.tmp', 'wb') as f:
                    f.write(body)
                os.replace(path + '.tmp', path)
                return self.answer(200, etag=version(body))
            if self.command == 'DELETE':
                if current is not None:
                    os.remove(path)
                return self.answer(204)
        self.answer(405)

    do_GET = do_PUT = do_DELETE = do_LOCK = do_UNLOCK = serve

    def log_message(self, *args):
        pass

http.server.ThreadingHTTPServer(('127.0.0.1', port), Backend).serve_forever()
PY
install -m 0600 /dev/null /etc/ump/pxe/test.token && echo token-of-the-test > /etc/ump/pxe/test.token
systemd-run -q --unit backend-http python3 /srv/backend.py /srv/backend 8070 /etc/ump/pxe/test.token
wait_for "the rescue image on 127.0.0.1:8000" curl -fsS http://127.0.0.1:8000/amd64/manifest.json
wait_for "the backend on 127.0.0.1:8070: 401 without the token" sh -c '[ "$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:8070/?list")" = 401 ]'
ok "ump config pxe apply renders site test into the backend" env UMP_STATE=http://127.0.0.1:8070 UMP_STATE_TOKEN=token-of-the-test ump config pxe apply

# the site's settings, without the backend's token
install -m 0600 /dev/null /etc/ump/pxe/test.env
printf 'UMP_STATE=http://127.0.0.1:8070\nUMP_PXE_HTTP=127.0.0.1:8067\nUMP_PXE_TFTP=127.0.0.1:69\n' > /etc/ump/pxe/test.env

# a run of the check: what it logged, and how it ended
checkrun() {
	local cursor rc=0
	cursor="$(journalctl -b -n 1 --show-cursor --no-pager -o cat | sed -n 's/^-- cursor: //p')"
	systemctl start ump-pxe-check@test.service 2>/dev/null || rc=$?
	journalctl -b --no-pager -o cat --after-cursor="$cursor" -u ump-pxe-check@test.service
	return $rc
}
checked() {
	local log want
	log="$(checkrun)" || { echo "$log"; return 1; }
	for want in "Site test: site.json " "Rescue image 2026.11.01 for amd64: verified" "HTTP 127.0.0.1:8067: bound." "TFTP 127.0.0.1:69: bound, "; do
		grep -qF "$want" <<<"$log" || { echo "$log"; return 1; }
	done
}
dynamic() {
	local pid
	pid="$(systemctl show -p MainPID --value ump-pxe@test.service)"
	[ "$pid" != 0 ] && [ "$(awk '/^Uid:/ {print $2}' "/proc/$pid/status")" -ge 61184 ]
}
refused() { local log; ! log="$(checkrun)" && grep 'HTTP 401' <<<"$log"; }
# the token as a credential of the units
credential() {
	local unit
	for unit in ump-pxe@test ump-pxe-check@test; do
		mkdir -p "/etc/systemd/system/$unit.service.d"
		printf '[Service]\nLoadCredential=UMP_STATE_TOKEN:/etc/ump/pxe/test.token\n' > "/etc/systemd/system/$unit.service.d/test.conf"
	done
	systemctl daemon-reload
}
status() { grep -q "\"version\": \"$1\"" /srv/backend/.pxe/test/status/*.json; }
removed() { apt-get remove -y -qq ump >/dev/null && ! systemctl is-active -q ump-pxe@test.service && ! systemctl is-enabled -q ump-pxe@test.service; }
purged() { apt-get purge -y -qq ump >/dev/null && [ ! -e /var/lib/private/ump-pxe ] && [ ! -e /var/cache/private/ump-pxe ] && [ ! -e /var/lib/ump-pxe ] && [ -f /etc/ump/pxe/test.env ]; }

ok "ump-pxe-check@test without the token: refused by the backend (HTTP 401)" refused
ok "the token as a credential, LoadCredential= in drop-ins of both units" credential
ok "systemctl start ump-pxe-check@test: the backend read with the credential, verified, HTTP and TFTP on port 69 bound" checked
ok "systemctl enable --now ump-pxe@test" systemctl enable --now ump-pxe@test.service
ok "ump-pxe@test runs as a dynamic user" dynamic
wait_for "ump-pxe@test serves boot.ipxe" sh -c 'curl -fsS http://127.0.0.1:8067/boot.ipxe | grep -q "^#!ipxe"'
wait_for "its status in the backend: v0.0.1" status v0.0.1
echo "  systemd-analyze security: $(systemd-analyze security ump-pxe@test.service 2>/dev/null | sed -n 's/.*Overall exposure level for .*: //p')"
ok "upgrade to v0.0.2" apt-get install -y -qq -o=Dpkg::Use-Pty=0 /pkg/v0.0.2/*.deb
wait_for "the upgrade restarted ump-pxe@test: v0.0.2 in its status" status v0.0.2
ok "remove: ump-pxe@test stopped and disabled" removed
ok "purge: its journal and cache gone, the site's settings kept" purged
EOF
echo "==> the packages passed"
