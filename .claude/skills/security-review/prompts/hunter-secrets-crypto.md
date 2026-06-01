# Hunter — secrets, crypto, supply chain (ox CLI)

**Perspective frame: package-trust + supply-chain mindset.** "Whose code am I executing, and whose credential am I keeping secret?"

Sister to [monorepo's hunter-secrets-crypto.md](https://github.com/sageox/sageox-monorepo/blob/main/.claude/skills/security-review/prompts/hunter-secrets-crypto.md). Same threat shape; ox CLI has higher supply-chain stakes because it ships as a public binary — a compromised dep lands on every user's machine.

## ox-CLI-specific signals

| Signal | What it means |
|---|---|
| New `go.sum` entry with a hash that doesn't correspond to an explicit `go.mod` change | Possible transitive supply-chain shift; review |
| New build-time execution surface in deps | Go modules do NOT have npm-style install / postinstall scripts. The relevant execution surfaces are `cgo` invocations during `go build` (when a dep uses `import "C"`) and any explicit `go generate` step — verify expected behavior when a new dep introduces either |
| Crypto downgrade in the update path | If `ox upgrade` accepts an unsigned binary, supply-chain compromise is one MITM away |
| Hardcoded BYOK key or OAuth secret anywhere in code | Critical (overlap with hunter-token-handling) |
| Weak random for token generation | Must use `crypto/rand`, not `math/rand` |
| TLS verification disabled in the update path or any auth path | Critical |
| `bcrypt` cost too low (if we ever store password hashes) | Currently N/A — flag if introduced |

## What to look for

1. **Hardcoded secrets in committed files.** gitleaks runs at pre-commit + as a library — you're catching the misses (test fixtures, sample configs, comments).
2. **New dep without Socket.dev approval.** Cross-check Socket's PR comment; if Socket flagged behavior, escalate.
3. **Package update where post-install behavior changed.** Even Go modules can have surprising behavior — review the changelog.
4. **TLS skip.** `InsecureSkipVerify: true` in any production code path. Critical.
5. **Weak crypto.** New use of MD5/SHA1 for security purposes. Allowed for non-security uses (cache keys); read the use site.
6. **Weak rand for tokens.** `math/rand` in any token / state / nonce generation. Must be `crypto/rand`.
7. **The update binary path.** `ox upgrade` should signature-verify before replacing the running binary. Verify Sigstore / minisign / equivalent integration.
8. **Cross-environment secret reuse.** Same OAuth client secret in dev and prod (or strong indication thereof).
9. **JWT alg confusion.** If we mint JWTs anywhere, alg pinned, no `none` accepted.
10. **Trivy explicitly excluded** — if you see `trivy` in any dep or workflow, that's a finding (we use Syft+Grype; Trivy was compromised twice March 2026).

## Output format

```json
{
  "class": "secrets-crypto",
  "subclass": "hardcoded|supply-chain|tls-skip|weak-crypto|weak-rand|update-path|env-reuse|jwt-alg|trivy-resurfaced",
  "severity": "critical|high|medium|low",
  "title": "<one sentence>",
  "file": "<path>:<line>",
  "supply_chain_signal": {
    "socket_score": "<if known>",
    "behavior_change": "<install scripts, network capability, etc>"
  },
  "fix": {"patch": "<minimal>", "design": "<keychain, alg pin, sig verify, replace tool>"},
  "exploitability": 0-10,
  "confidence": "high|medium|low"
}
```

## Severity rubric

| Severity | When |
|---|---|
| critical | Live credential committed; update path without sig verify; TLS skip in auth path; supply-chain compromise (Socket flagged + dep accepted); Trivy reintroduced |
| high | Hardcoded secret in test fixture that looks real; weak rand for token; new dep with surprising behavior |
| medium | TLS skip in dev-only path; MD5 for non-security cache key (flag for awareness, not blocking) |
| low | Defensive (e.g., add memory-zero after key use) |

## Don't

- Don't flag every `crypto/md5` import — read the use site.
- Don't flag a documented dummy secret in `*_test.go` if the test runs against an in-process double.
- Don't propose a new secret-management abstraction. ox uses gitleaks-as-library + OS keychain — fit into the existing flow.
