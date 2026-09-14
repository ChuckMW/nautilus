#!/usr/bin/env bash
#
# modbus-sim.sh — run the foreign Modbus TCP simulator the modbus-sim CI job
# runs, on a laptop, so `go test ./modbus/ -run TestForeign` has something to
# talk to.
#
#   scripts/modbus-sim.sh                      # 127.0.0.1:5020, the seeded map
#   scripts/modbus-sim.sh --port 5021 --latency-ms 1000   # the slow twin
#
# Then, in another terminal (the script prints the exact line):
#
#   NAUTILUS_MODBUS_SIM=127.0.0.1:5020 go test ./modbus/ -run TestForeign -v
#
# and add NAUTILUS_MODBUS_SIM_SLOW=127.0.0.1:5021 when the slow twin is up.
#
# pymodbus goes in a venv OUTSIDE the repo (under $TMPDIR, or /tmp) — nothing
# to gitignore, nothing for `go vet ./...` to trip over. The pin matches
# ci.yml; the sim targets pymodbus 3.15's SimDevice API.
#
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sim="$repo/modbus/testdata/sim/pymodbus_sim.py"
pin='pymodbus==3.15.0'
venv="${NAUTILUS_MODBUS_SIM_VENV:-${TMPDIR:-/tmp}/nautilus-modbus-sim/.venv-modbus-sim}"

if [ ! -x "$venv/bin/python" ]; then
  echo "creating venv $venv" >&2
  python3 -m venv "$venv"
fi
if ! "$venv/bin/python" -c 'import pymodbus' 2>/dev/null; then
  echo "installing $pin" >&2
  "$venv/bin/pip" install --quiet "$pin"
fi

# Echo the env line the test wants, from the same flags the sim will parse.
host=127.0.0.1 port=5020 var=NAUTILUS_MODBUS_SIM
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    --host) host=${args[i + 1]:-$host} ;;
    --port) port=${args[i + 1]:-$port} ;;
    --latency-ms) var=NAUTILUS_MODBUS_SIM_SLOW ;;
  esac
done
echo "$var=$host:$port go test ./modbus/ -run TestForeign -v" >&2

exec "$venv/bin/python" "$sim" "$@"
