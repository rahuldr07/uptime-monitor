# Contributing

Thanks for helping. This is a small project; keep changes small and focused.

## Build and run

You need the Go version in `go.mod` (1.26). There is no frontend build step.

```bash
cp config.example.json config.json
go run . -config config.json
```

Open http://localhost:8080. On first run the server logs a setup link
(`setup required: open …/setup?token=…`); open it to create the operator
account. The README covers configuration, accounts and the command-line flags.
The layout section of the README lists what each package does.

## Checks

CI (`.github/workflows/ci.yml`) runs these. Run them before opening a pull
request:

```bash
test -z "$(gofmt -l .)" || gofmt -l .
go vet ./...
go test -race -count=1 ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
docker build -t uptime-monitor:ci .
```

## Conventions

- Small files, grouped by area (see how `internal/web` and `internal/store` are split).
- Tests sit next to the code. Store and web tests use a real SQLite database in `t.TempDir()` and `httptest`, not mocks.
- Do not add dependencies without discussing it in an issue first. SQLite uses a pure-Go driver so the binary builds without cgo; keep it that way.
- The Content-Security-Policy forbids inline scripts and event handlers. Put JavaScript in the static assets and render pages with `html/template`.
- Match the surrounding code's style and naming.

## Decisions and design docs

Product and technical decisions are recorded in [docs/decisions.md](docs/decisions.md).
If your change alters behaviour, add a new `D-nnn` entry there; to change an
earlier decision, add an entry that supersedes it instead of editing the old
one. Update `docs/product.md` if the product description changes, and the
README if configuration, flags or operations change. Detailed designs live in
[docs/design/](docs/design/).

## Commits

Use conventional commit messages: `feat`, `fix`, `docs`, `test`, `style`,
`chore`, `refactor` or `build`, with an optional scope, for example
`fix(web): ShareURL uses base_url` or `feat(store): user suspension`. Keep the
subject short and in the imperative.

## Security issues

Do not report vulnerabilities in a public issue. Follow
[SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contribution is licensed under the
[MIT License](LICENSE).
