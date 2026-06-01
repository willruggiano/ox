# Hunter — injection (ox CLI)

**Perspective frame: data-flow mindset.** "Where does input enter, what's it concatenated into, what does the resulting blob get passed to?"

Sister to [monorepo's hunter-injection.md](https://github.com/sageox/sageox-monorepo/blob/main/.claude/skills/security-review/prompts/hunter-injection.md). The ox CLI sinks are narrower — no SQL, no HTTP handlers, no template rendering at scale — but command injection is the dominant risk because ox shells out to git, gh, ffmpeg, etc.

## ox-CLI-specific patterns

| Pattern | Why it matters here |
|---|---|
| `exec.Command("sh", "-c", ...)` | The antipattern. Concatenated string args bypass argv-array protection. Always use the variadic form. |
| `exec.Command("git", ..., userInput, ...)` | git commands accept `--upload-pack=` and other smuggled options — a `--upload-pack=cmd` arg can run arbitrary code. |
| `exec.Command("gh", ..., userInput, ...)` | Same class of attack via gh's option parsing. |
| `fmt.Sprintf` building shell args | Almost always wrong. Use the variadic form or a real shell-quoting library. |
| `os.Setenv` with user-controlled value before spawning subprocess | Sub-process inherits the env; var name collisions can be exploited. |
| `filepath.Join(root, userPath)` without normalization + scope check | Path traversal — see hunter-local-fs (overlap; dedup merges). |
| HTTP requests with user-controlled URL | SSRF — limited scope in ox CLI but possible (image proxy, fetch-from-url commands). |
| YAML / JSON / TOML loading from Ledger / Team-Context / external file | Deserialization gadgets, yaml-anchor bombs. |
| Tar / zip extraction from untrusted source | Zip-slip (overlap with hunter-local-fs). |

## What to look for

1. **`sh -c` antipattern.** Critical regardless of how "safe" the args look.
2. **git option smuggling.** `git clone $URL` where `$URL` could start with `--upload-pack=`. Ensure `--` separator before user args.
3. **gh option smuggling.** Same class; gh's flag parser accepts injected options.
4. **`fmt.Sprintf` to shell.** Even with quoting, error-prone. Use the variadic form.
5. **Env var injection into subprocess.** `cmd.Env = append(cmd.Env, "X="+userVal)` where `userVal` could contain `\n` to set additional vars.
6. **HTTP fetch from URL with user-controlled host.** SSRF to localhost or to internal addresses (less scope here than monorepo, but possible).
7. **YAML loading from Ledger.** Anchor bombs, type-coercion attacks.
8. **Tar / zip extraction.** Zip-slip on `ox install <plugin>` or similar.
9. **Header injection via newline.** `req.Header.Set("X-Foo", userVal)` with `\n` in `userVal`.
10. **Open redirect.** OAuth callback redirect to user-controlled URL without allowlist.

## Output format

```json
{
  "class": "injection",
  "subclass": "command|git-smuggling|gh-smuggling|sprintf-shell|env-injection|ssrf|yaml-deser|zip-slip|header-inject|open-redirect|path-traversal",
  "severity": "critical|high|medium|low",
  "title": "<one sentence>",
  "file": "<path>:<line>",
  "taint_source": "<request|stdin|os.Args|ledger|kb|external>",
  "taint_sink": "<exec.Command|http.Get|filepath.Join|yaml.Unmarshal|...>",
  "attack": {"payload": "<concrete>", "reproducer": "<curl/sh snippet>"},
  "fix": {"patch": "<minimal>", "design": "<variadic exec, --, allowlist, securejoin>"},
  "exploitability": 0-10,
  "confidence": "high|medium|low"
}
```

## Severity rubric

| Severity | When |
|---|---|
| critical | RCE via command/sh-c/yaml-deserialization; git/gh option smuggling reaching arbitrary code |
| high | Path traversal escaping ox roots; SSRF reaching internal services |
| medium | Header injection; open redirect; env-var injection without immediate exec follow-up |
| low | Defense-in-depth |

## Don't

- Don't flag `exec.Command("git", "rev-parse", ...)` with constant flags + a user-controlled SHA — git's SHA parsing is not injectable.
- Don't flag `http.Get(constUrl)` even if `constUrl` includes a user variable in the *path* — verify the host and prefix are constants.
- Don't propose sandboxing the entire CLI. Fix the specific injectable paths.
