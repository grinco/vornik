# Contributing to Vornik

Contributions are welcome. Vornik Community is AGPL-3.0; please read the CLA
section below before opening a pull request.

## Contributor License Agreement (CLA)

Vornik requires a **Contributor License Agreement**. By contributing you grant a
broad copyright + patent license for your contribution to **Vadim Grinco** (the
project's IP owner), who licenses the project — including contributions — to
EaseIT for the commercial offerings. This is what lets the project sustain a
proprietary Enterprise overlay and a hosted SaaS while keeping the Community
Edition open under AGPL.

- The agreement is the [Contributor License Agreement](https://github.com/grinco/vornik/blob/main/CLA.md)
  (`CLA.md` at the repo root).
- It is checked automatically on every pull request — your first PR will prompt
  you to sign by posting a one-line comment. One signature covers your future
  contributions.
- If you contribute on behalf of an employer, you may need your employer's
  consent (corporate CLA) — contact `cla@vornik.io`.

## Development setup

```sh
git clone https://github.com/grinco/vornik
cd vornik
go build ./cmd/vornik      # build the Community daemon
go test ./...              # run the test suite
make lint                  # golangci-lint + the project's drift/contract linters
```

An automated **import-law test** enforces the Community/Enterprise boundary:
Community code can never import Enterprise code, and CI fails if it does. Keep
contributions within the Community surface.

## Pull request expectations

- **Tests for changed code**, and a **regression test for each bug fix** (a test
  that fails before the fix and passes after).
- **`make lint` clean** before you open the PR.
- **Focused commits** with clear messages — one logical change per PR where
  practical.
- Update the relevant docs under `docs/public/` when you change user-facing
  behaviour.

## Code of conduct

> **Before public release:** a `CODE_OF_CONDUCT.md` is added at the repo root and
> linked here.
