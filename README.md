# dispatch

An optional Hive module that runs some of the Hive's agent turns on another machine the person owns (first: iiks1), so an idle machine takes work when the main one is strained.

The Hive hands a held turn's sandbox spec to the module; the module sends it over SSH to a small runner on the other machine, which runs it in the hive binary's own sandbox there and sends its output, mailbox requests and changed files back. GitHub and every other credential stay on the main machine; the runner gets only what a local sandboxed turn gets.

- How it works and how to set up a second machine: [docs/README.md](docs/README.md)
- The core seam it needs (not in the core yet): [docs/core-seam.md](docs/core-seam.md)
- Tests: `go test ./...`
