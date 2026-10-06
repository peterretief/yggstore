# Contributing

Thanks for looking. yggstore is an early proof of concept, so issues that
say "I tried X and it didn't work" are as useful as code.

## Building and testing

You need Go 1.24 or later.

```sh
go build -o bin/yggstore ./cmd/yggstore
go vet ./...
go test ./...
```

Cross-builds for the usual boxes:

```sh
GOOS=linux GOARCH=amd64        go build -o bin/yggstore-linux-amd64 ./cmd/yggstore
GOOS=linux GOARCH=arm GOARM=7  go build -o bin/yggstore-linux-armv7 ./cmd/yggstore
```

`scripts/testnet.sh up` starts five Docker test nodes, each with its own
Yggdrasil, so you can try uploads and node failures on one machine (see the
README).

## Releases

Pushing a version tag builds and publishes a release (see
`.github/workflows/release.yml`): binaries for Linux (amd64, arm64, armv7),
Windows and macOS, with a `SHA256SUMS` file and notes made from the commits
since the last tag. Versions before 1.0, and tags with a suffix such as
`-rc1`, are marked as pre-releases.

```sh
git tag v0.1.0
git push origin v0.1.0
```

`yggstore version` prints the version a binary was built as (or, for
your own builds, the commit).

## Pull requests

- Keep changes small and focused; open an issue first for anything large.
- Add a test with each fix or feature, and run `gofmt`.
- Never commit a real `peers.json`, `sharing.key`, `invites.json`, stubs
  (`.ystub`) or challenge files. `.gitignore` covers the usual names.
- Write messages and dashboard text for people who aren't technical.

## Security issues

Please don't open public issues for these; see [SECURITY.md](SECURITY.md).
