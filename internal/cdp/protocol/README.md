# Vendored Chrome DevTools Protocol JSON

These two files are the machine-readable Chrome DevTools Protocol (CDP) schema
that the code generator (`../gen`) consumes to build the typed Go client:

- `browser_protocol.json` — browser/page/DOM/Network/... domains
- `js_protocol.json` — the `Runtime` domain (V8 / JavaScript)

They are **not authored here**. The CDP protocol definitions originate in the
Chromium project and are published, regenerated per Chrome release, by the
DevTools / Puppeteer toolchain. See `../../../../../docs/protocol-gen.md` for
how to refresh them.

## Provenance (fill this in on every refresh)

Record exactly where the current copy came from, so the license chain and the
target Chrome version are traceable.

| field | value |
|---|---|
| source | GitHub `ChromeDevTools/devtools-protocol`, `json/` dir (two-file split) |
| browse | https://github.com/ChromeDevTools/devtools-protocol/tree/master/json |
| raw (browser) | https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/json/browser_protocol.json |
| raw (js) | https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/json/js_protocol.json |
| vendored (date) | 2026-11-13 — re-fetched from `master` (supersedes an older, unknown-revision copy) |
| upstream roll | `Roll protocol to r1743766` (HEAD `c092aa9`) at fetch time |
| Chrome version targeted | tracks the roll above (not derivable from embedded `version` `1.3` — rely on roll + sha256) |
| protocol version | `1.3` |
| vendored sha256 | `browser_protocol.json` 1,423,565 B `b7d783749473516c3c4604697a6e98fb00518b624269de8fc10ddae280349229`; `js_protocol.json` 184,382 B `f582e48586f955fe8228d2037e25201674b21b0c8fbac9142824a3dbfa24eb91` |

> Vendored copy == upstream `master` at fetch time (sha256 pinned above). `master`
> rolls with each Chrome release; for a reproducible build pin a tagged commit and
> re-verify sha256. After ANY re-fetch run `go generate ./...` + build/test (see
> `docs/protocol-gen.md`).

> The embedded `version` field is only `1.3` and does **not** identify a Chrome
> release, so the table above is the real pin. Without it there is no way to
> tell which Chrome build these files came from.

## License

The protocol JSON and the Go code generated from it are derived from Chromium
and carry Chromium's **BSD 3-Clause "New"/"Revised" License** (text in
[`LICENSE`](./LICENSE) alongside these files).

Conditions (permissive):

- Keep this copyright notice, the condition list, and the disclaimer in any
  redistribution of these files or of source derived from them.
- Do not use "Google", "Chrome", "Chromium", or "DevTools" to endorse or
  promote a derivative product.

Practical rules for this repo:

1. Do **not** relicense or claim authorship of the JSON or the `*_gen.go`
   output — they stay BSD-3-Clause. Keep `LICENSE` next to them.
2. Your own original code (everything not under `internal/cdp/gen`/`protocol`
   and not `*_gen.go`) is licensed by the repo's top-level `LICENSE`; BSD-3 is
   compatible with MIT and Apache-2.0, so either is fine for the project.
3. When you add a protocol description that you hand-wrote into a tool, that
   string is yours; only the vendored JSON and its generated output are BSD.
