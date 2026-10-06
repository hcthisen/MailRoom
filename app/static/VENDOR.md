# Vendored static files

The admin UI loads no scripts, styles or fonts from the network. Everything it needs is in this directory and embedded in the binary (`static.go`).

| File | Source | Version | License | SHA-256 |
|---|---|---|---|---|
| `htmx.min.js` | `dist/htmx.min.js` from the npm package `htmx.org` (<https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.11.tgz>, npm integrity `sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4UrVa3+sW0GmrM3p4gJgO89ecH4qtbnyzDDWFvBTqjnIMCgELTNt636dtamA==`, checked when downloaded) | 2.0.11 | 0BSD | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |

`app.css` and `app.js` are written for Mailroom (MIT, like the rest of the app).

## Upgrading htmx

1. Download the tarball of the new 2.x version from the npm registry (`npm pack htmx.org@2`, or `https://registry.npmjs.org/htmx.org/-/htmx.org-<version>.tgz`) and check it against the `integrity` value that `https://registry.npmjs.org/htmx.org/<version>` lists.
2. Extract `package/dist/htmx.min.js` over `static/htmx.min.js`.
3. Update the version and the SHA-256 (`sha256sum static/htmx.min.js`) in the table above. `internal/ui` has a test that compares the embedded file with this table.
4. Run the UI tests and click through the admin UI once: the CSP forbids `eval`, so the page sets `allowEval: false` in the `htmx-config` meta tag.
