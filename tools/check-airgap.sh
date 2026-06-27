#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage:
  tools/check-airgap.sh --staged
  tools/check-airgap.sh --range <git-rev-range>
  tools/check-airgap.sh --files <file> [file...]

Checks only the selected files, not the full repository history. This keeps the gate usable on a
public tree that may already contain intentional public placeholder or maintainer data, while still
blocking new private identifiers in a commit or pull request.
USAGE
}

mac1='B0:A6:04:4D:'"D8:D8"
mac2='B0:A6:04:4E:'"52:18"
ip1='10[.]13[.]37[.]'"16"
ip2='10[.]37[.]13[.]'"42"
ip3='157[.]90[.]88[.]'"224"
ssid='HTH-'"Mitarbeiter"
domain1='gottz[.]'"de"
domain2='janetzky[.]'"cloud"
host1='gro'"gru"
host2='home'"core"
ctx_id='019[de][0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'

pattern="(${mac1}|${mac2}|${ip1}|${ip2}|${ip3}|${ssid}|${domain1}|${domain2}|${host1}|${host2}|${ctx_id})"

# Allowlist: the maintainer's own public contact (the author/license attribution) is intentional
# public data, not leaked infra -- this is the "intentional public maintainer data" the gate is meant
# to tolerate. We strip only this exact token before scanning, so editing a file that carries the
# attribution (e.g. README) does not trip the gate, while ANY OTHER use of the domain still fails
# (only the contact email is removed, not the domain broadly).
allow='(mailto:)?git@gottz[.]de'

mode="${1:-}"
shift || true

files=()
case "$mode" in
    --staged)
        while IFS= read -r -d '' file; do files+=("$file"); done < <(git diff --cached --name-only -z --diff-filter=ACMRT)
        ;;
    --range)
        if [ "$#" -ne 1 ]; then usage; exit 2; fi
        while IFS= read -r -d '' file; do files+=("$file"); done < <(git diff --name-only -z --diff-filter=ACMRT "$1")
        ;;
    --files)
        if [ "$#" -lt 1 ]; then usage; exit 2; fi
        files=("$@")
        ;;
    -h|--help|"")
        usage
        exit 0
        ;;
    *)
        usage
        exit 2
        ;;
esac

if [ "${#files[@]}" -eq 0 ]; then
    printf 'airgap: no files to check\n'
    exit 0
fi

status=0
for file in "${files[@]}"; do
    [ -f "$file" ] || continue
    # Strip the allowlisted maintainer contact first; scan the remainder. sed keeps the line count,
    # so grep -n line numbers stay accurate. A line that mixes the contact with leaked infra still
    # fails, because only the exact contact token is removed.
    if sed -E "s#${allow}##g" "$file" | grep -I -nE "$pattern"; then
        status=1
    fi
done

if [ "$status" -ne 0 ]; then
    printf '\nairgap: blocked private identifier pattern(s). Use public placeholders instead.\n' >&2
    exit 1
fi

printf 'airgap: clean (%d file(s))\n' "${#files[@]}"
