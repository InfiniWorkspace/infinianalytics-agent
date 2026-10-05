#!/usr/bin/env bash
# Installs the agent as a real systemd service with scripts/install.sh, the
# way a customer does, against a fake ingestion API (scripts/fakeingest), and
# checks it survives what used to break it:
#   1. a fresh install enrolls and pushes;
#   2. an upgrade with --reset --disks off, run as root, keeps it running, and
#      agent.env stays the service user's;
#   3. the stop_reason root records on a stop is the service user's too;
#   4. an agent.env left root-owned (what earlier versions did) makes `run`
#      say so, and the next install repairs it;
#   5. install fails, with the log, when the service does not stay up;
#   6. windows buffered while the backend is down survive the dashboard's
#      reinstall (its uninstall line, then a new code) and arrive afterwards.
# Needs systemd and passwordless sudo, and changes the machine: CI only.
set -euo pipefail

port=${AGENT_SMOKE_PORT:-8199}
name=infinianalytics-agent
# DynamicUser= keeps the StateDirectory here; /var/lib/$name links to it.
state=/var/lib/private/$name
tmp=$(mktemp -d)
mkdir -p "$tmp/dist" "$tmp/ingest"

asset=$name-linux-$(go env GOARCH)
go build -o "$tmp/dist/$asset" .
(cd "$tmp/dist" && sha256sum "$asset" > SHA256SUMS)
go build -o "$tmp/fakeingest" ./scripts/fakeingest

"$tmp/fakeingest" -addr "127.0.0.1:$port" -dir "$tmp/ingest" > "$tmp/fake.log" 2>&1 &
fake_pid=$!
cleanup() {
  code=$?
  if [ "$code" -ne 0 ]; then
    echo "--- failed; service, journal and state directory:"
    systemctl status "$name" --no-pager || true
    sudo journalctl -u "$name" --no-pager -n 50 || true
    sudo ls -ln "$state" || true
  fi
  kill "$fake_pid" 2>/dev/null || true
}
trap cleanup EXIT
sleep 1

# The dashboard's line: curl ... install.sh | sudo sh -s -- ...
installer() { sudo sh -s -- --download-url "file://$tmp/dist" "$@" < scripts/install.sh; }
batches() { [ -f "$tmp/ingest/batches.log" ] && wc -l < "$tmp/ingest/batches.log" || echo 0; }
prop() { systemctl show -p "$1" --value "$name"; }
owner() { sudo stat -c %u:%g "$1"; }
# Files in the state directory have its owner: the service user.
owned_by_service() {
  local want got f
  want=$(owner "$state")
  for f in "$@"; do
    got=$(owner "$state/$f")
    [ "$got" = "$want" ] || { echo "$f is owned by $got, want $want (the state directory's)"; return 1; }
  done
}
# Pushing, with one process and no restart, for $1 seconds. After a start the
# first window closes on the next 10 s boundary and the push loop runs on its
# own 10 s timer, so the first push can take ~30 s.
stays_up() {
  local pid before
  pid=$(prop MainPID) before=$(batches)
  sleep "$1"
  [ "$(prop ActiveState)" = active ] || { echo "not active: $(prop ActiveState)/$(prop SubState)"; return 1; }
  [ "$(prop MainPID)" = "$pid" ] || { echo "restarted: PID $pid became $(prop MainPID)"; return 1; }
  [ "$(batches)" -gt "$before" ] || { echo "no push in $1 s"; return 1; }
}

echo "--- 1. fresh install"
installer --code SMOKE-CODE-0001 --url "http://127.0.0.1:$port"
owned_by_service agent.env
stays_up 35

echo "--- 2. upgrade as root with --reset --disks off"
installer --reset --disks off | tee "$tmp/install.out"
if grep -q "back to the service user" "$tmp/install.out"; then
  echo "a healthy install had nothing to repair"
  exit 1
fi
sudo grep -qx 'IA_AGENT_DISKS=false' "$state/agent.env"
owned_by_service agent.env state.json
stays_up 35

echo "--- 3. stop_reason belongs to the service"
# The unit's ExecStop, as root. Run by hand: on a real stop the agent picks
# the file up and deletes it before anyone can look.
sudo "/usr/local/bin/$name" note-stop
owned_by_service stop_reason
sudo systemctl restart "$name"

echo "--- 4. a root-owned agent.env is reported, then repaired"
sudo chown root:root "$state/agent.env"
since=$(date '+%Y-%m-%d %H:%M:%S')
sudo systemctl restart "$name"
sleep 3
sudo journalctl -u "$name" --since "$since" --no-pager | tee "$tmp/journal"
grep -q "cannot read /var/lib/$name/agent.env: permission denied (owner root, running as uid [0-9]*)" "$tmp/journal"
installer | tee "$tmp/install.out"
grep -q "back to the service user" "$tmp/install.out"
owned_by_service agent.env
stays_up 35

echo "--- 5. install fails when the service does not stay up"
dropin=/etc/systemd/system/$name.service.d
sudo mkdir -p "$dropin"
printf '[Service]\nEnvironment=IA_AGENT_CONFIG=/nonexistent/agent.env\n' | sudo tee "$dropin/broken.conf" > /dev/null
if sudo "/usr/local/bin/$name" install > "$tmp/install.out" 2>&1; then
  cat "$tmp/install.out"
  echo "install succeeded with a crash-looping service"
  exit 1
fi
cat "$tmp/install.out"
grep -q "does not stay up" "$tmp/install.out"
grep -q "not enrolled" "$tmp/install.out"
sudo rm -r "$dropin"
sudo "/usr/local/bin/$name" install
stays_up 35

echo "--- 6. the backend goes down, then the dashboard's reinstall: nothing buffered is lost"
touch "$tmp/ingest/down"
down_from=$(date -u +%s)
sleep 30
# The dashboard's uninstall line removes the service only: the state
# directory, and the spool in it, stay for the reinstall.
stopped_at=$(date -u +%s)
sudo "/usr/local/bin/$name" uninstall
sudo test -d "$state/spool" || { echo "uninstall removed the spool"; exit 1; }
installer --code SMOKE-CODE-0002 --url "http://127.0.0.1:$port"
rm "$tmp/ingest/down"
owned_by_service agent.env
# Every window that closed while the backend was down arrives, under the new key.
first=$(( down_from / 10 * 10 + 10 )) last=$(( (stopped_at - 10) / 10 * 10 ))
missing() {
  local t
  for ((t = first; t <= last; t += 10)); do
    grep -qx "$(date -u -d "@$t" +%Y-%m-%dT%H:%M:%SZ)" "$tmp/ingest/samples.log" || { echo "$t"; return; }
  done
}
for _ in $(seq 1 60); do
  [ -z "$(missing)" ] && break
  sleep 1
done
gap=$(missing)
[ -z "$gap" ] || { echo "the window at $(date -u -d "@$gap") never arrived"; exit 1; }
stays_up 35

echo "systemd test passed"
