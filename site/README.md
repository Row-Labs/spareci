# spareci.dev

The project's landing page. Plain HTML, CSS and JavaScript: no build step, no
dependencies, no analytics, no third-party requests. System fonts.

```sh
python3 -m http.server -d site 8000   # then open http://localhost:8000
```

- `index.html`: the page.
- `assets/style.css`: light and dark themes (follows the OS; the toggle is remembered in `localStorage`).
- `assets/app.js`: theme toggle, copy buttons, savings calculator.
- `assets/prices.js`: generated from `internal/estimate/prices.go`, so the calculator uses the same prices as `spareci estimate`. Don't edit it. After changing prices, run:

  ```sh
  SPARECI_WRITE_SITE_PRICES=1 go test ./internal/estimate -run TestSitePrices
  ```

  `go test ./...` fails while the two disagree.

Copy that states a fact (prices, timings, flags) should match the README and
`docs/`. The price date, source link and the hero's per-minute prices are filled in
from `prices.js`; the HTML holds the same values for readers without JavaScript.

## Deploying

`.github/workflows/pages.yml` deploys `site/` to GitHub Pages on pushes to
`main` that touch it. It does nothing until Pages is enabled with source
"GitHub Actions" and the repo variable `SITE_PAGES` is `true`. Pages on a
private repo needs a paid GitHub plan.
