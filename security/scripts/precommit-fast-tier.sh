#!/usr/bin/env bash
# security/scripts/precommit-fast-tier.sh — opt-in pre-commit fast tier for ox CLI.
#
# OFF by default. Activated via SEC_PRECOMMIT=1 to avoid compounding agentic-
# iteration friction (per the same rule as monorepo: pre-commit checks belong
# in path-scoped, sub-second territory or as opt-in for devs who want them).
#
# Installed by `make sec-install-hook` (creates a .git/hooks/pre-commit shim
# that source-includes this script). Uninstall by removing the shim.
#
# Path-scoped: only fires when .go OR sensitive paths are touched. Fail-open
# if a tool is missing (warn, never block). Target <10s wall time.

set -e

# Bail unless opted in.
[[ "${SEC_PRECOMMIT:-0}" != "1" ]] && exit 0

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
[[ -z "$ROOT" ]] && exit 0
cd "$ROOT" || exit 0

STAGED_GO=$(git diff --cached --name-only --diff-filter=ACM | grep -E '\.go$' || true)
STAGED_SENSITIVE=$(git diff --cached --name-only --diff-filter=ACM | grep -E '(internal/session/raw_writer\.go|internal/session/redactor|internal/session/gitleaks_|cmd/ox/prepush_scan\.go|internal/auth/|internal/mcp/|internal/exec/|internal/git/|cmd/ox/upgrade\.go|internal/upgrade/|^go\.sum$|^go\.mod$)' || true)

if [[ -n "$STAGED_GO" ]] && command -v govulncheck >/dev/null 2>&1; then
  echo "[SEC_PRECOMMIT] govulncheck on touched Go packages..."
  # array-build so package paths with spaces/special chars survive intact
  PKGS=()
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    PKGS+=("./$(dirname "$f")")
  done < <(printf '%s\n' "$STAGED_GO" | sort -u)
  # de-dupe (xargs uniq via sort -u was on file paths, not dirs)
  if [[ ${#PKGS[@]} -gt 0 ]]; then
    mapfile -t PKGS < <(printf '%s\n' "${PKGS[@]}" | sort -u)
    govulncheck "${PKGS[@]}" 2>&1 | head -50 || echo "[SEC_PRECOMMIT] govulncheck warnings (advisory, not blocking)"
  fi
fi

if [[ -n "$STAGED_SENSITIVE" ]] && [[ -x "./bin/opengrep" ]] && [[ -f "./security/rules/sageox-entry-points.yml" ]]; then
  echo "[SEC_PRECOMMIT] opengrep --diff-aware on sensitive paths..."
  # array-build so paths with spaces are passed as separate args
  mapfile -t SENSITIVE_PATHS < <(printf '%s\n' "$STAGED_SENSITIVE")
  ./bin/opengrep --config security/rules/sageox-entry-points.yml "${SENSITIVE_PATHS[@]}" 2>&1 | head -30 || echo "[SEC_PRECOMMIT] opengrep warnings (advisory, not blocking)"
fi

exit 0
