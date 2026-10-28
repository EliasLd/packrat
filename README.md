# packrat

🐀 Hauls every part of your big downloads — concurrent, resumable, single Go binary.

[![CI](https://github.com/EliasLd/packrat/actions/workflows/ci.yml/badge.svg)](https://github.com/EliasLd/packrat/actions/workflows/ci.yml)

## What it does

packrat reads a list of landing page links, resolves each one to its direct
download URL, and fetches every part concurrently — with retries and HTTP
range resume — into one tidy directory per archive.

- input: a plain text file, one landing page URL per line (markdown bullet lists too)
- output: `<output-dir>/<archive-name>/<filename>` for every part
- single static Go binary, no runtime dependencies

## Status

Work in progress — architecture first, engine pieces landing one branch at a time:

- [x] host abstraction + registry
- [x] links file parsing and archive grouping
- [ ] first host implementation
- [ ] download engine (worker pool, range resume, retries)
- [x] CI (vet, test, race, golangci-lint)
- [ ] polished CLI UX and per-part progress
- [ ] release builds via goreleaser

## Usage

In development. The current binary previews the download plan from a links
file without resolving or downloading anything:

```sh
packrat --dry-run links.txt
```

Planned full usage:

```sh
go install github.com/EliasLd/packrat/cmd/packrat@latest
packrat links.txt
```

With `links.txt` like:

```text
https://some-file-host.example/abcdef#BigGame.multi-part.part01.rar
- https://some-file-host.example/ghijk#BigGame.multi-part.part02.rar
```

## How it works

1. **Resolve** — every landing page is fetched with a client whose TLS
   fingerprint impersonates a real browser, then the page's download
   mechanism is replayed to obtain the direct link. No browser required.
2. **Download** — direct links are fetched with Go's stdlib HTTP client:
   streaming, resumable (`Range`), retried, and verified against the
   reported size before being moved into place.

## Legal

Independent open-source tool, not affiliated with any file hosting service.
Use it for content you are entitled to download, and respect the terms of
service of the hosts you point it at.

## License

[MIT](LICENSE)