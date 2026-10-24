# Contributing

Issues and pull requests are welcome. For a larger change (a new
provider, an OS image, a disk layout, a new command) open an issue first,
so that the shape is agreed on before the code is written.

## Building and testing

ump is one Go binary; `go.mod` names the Go version (with
`GOTOOLCHAIN=auto` an older Go fetches it).

```sh
make build        # ./ump
make check        # gofmt, go vet, the tests and the shell syntax checks: what CI runs
make dist         # the binaries for every platform, into dist/
```

The tests need no provider accounts: the providers' APIs are faked.
Where Docker answers, one test runs MinIO in a container for the host
files in a storage (`-short` skips it).
Images are built in a privileged Debian container (`make rescue-image`,
`make os-image`; Docker or Podman) and the rescue image boots in QEMU
with `make rescue-test`. See [the rescue image](images/rescue/README.md)
and [the base images](images/base/README.md).

Anything that erases disks is tested on a machine you can afford to
lose: `ump server reinstall NAME --dry-run` first, then a throwaway
server.

## Pull requests

- One topic per pull request; it is squashed on merge, so the title
  becomes the commit message: say what changed and why, in a sentence.
- `make check` passes, and new behaviour comes with a test.
- User-visible changes update the docs in the same pull request (the
  README, `docs/`, the command's help).
- No real credentials, server names, addresses or IDs in code, tests or
  docs: use placeholders and the documentation ranges (192.0.2.0/24,
  2001:db8::/32).

## Adding a provider or an image

- Providers: [adding a provider type](docs/providers.md#adding-a-provider-type).
- Operating systems: the [base images](images/base/README.md) and the
  [installer](install/README.md).

## License

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE), like the rest of ump.
