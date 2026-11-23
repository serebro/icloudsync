# Contributing

```sh
go vet ./...
go test -race ./...
gofmt -l .      # must print nothing
```

- Tests use a fake iCloud server. Do not add tests that talk to Apple.
- Keep default behavior stable: manifest keys and on-disk paths of existing options must not change.
- Never include real Apple IDs, tokens or photos in issues, tests or logs.
