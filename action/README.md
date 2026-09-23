# niks3 GitHub Action

Configures a niks3 binary cache as a substituter, including a private read path
behind HTTP Basic auth, and pushes what the job builds to it.

Forked from [Mic92/niks3-action](https://github.com/Mic92/niks3-action) v1.1.0
(MIT, © Jörg Thalheim, see [LICENSE](LICENSE)). This copy is released with this
repository's tags: `firefly-engineering/niks3/action@<tag>` downloads the niks3
client from the `<tag>` release of this repository and verifies it against that
release's `checksums.txt`, so the client always matches the server release it
was tagged with.

## Usage

```yaml
permissions:
  contents: read
  id-token: write # only in jobs that should push
steps:
  - uses: actions/checkout@v5
  - uses: DeterminateSystems/determinate-nix-action@v3 # install Nix first
  - uses: firefly-engineering/niks3/action@v1.12.0-firefly.2
    with:
      server-url: https://cache.example.com
      netrc-login: ci
      netrc-password: ${{ secrets.CACHE_READ_PASSWORD }}
  - run: nix build .#foo
```

The action must run after Nix is installed and before anything that builds or
substitutes.

## Reads

The substituter URL and public keys come from the server's `/api/cache-config`,
which must be reachable without credentials. If the cache's reads are private,
pass a credential and the action makes the Nix daemon send it:

- **Determinate Nix**: `determinate-nixd` owns `netrc-file`, so the entries go
  to `/etc/determinate/niks3.netrc`, listed under
  `authentication.additionalNetrcSources` in `/etc/determinate/config.json`,
  and the daemon is restarted
  ([docs](https://docs.determinate.systems/determinate-nix/determinate-nixd/)).
  Determinate merges all netrc sources into a world-readable file.
- **Upstream Nix**: `netrc-file` is set in the action's `nix.conf`, with the
  entries of any existing netrc carried over. The runner user must be in
  `trusted-users`, as it must for the substituter itself.

The action then fetches `nix-cache-info` with the credential and warns if the
cache rejects it.

## Writes

When the job has `id-token: write` and `skip-push` is not set, the action
authenticates with GitHub OIDC, for the audience the server reports for
GitHub's issuer. It starts an upload daemon and registers a `post-build-hook`,
so every derivation the job builds is uploaded as soon as it finishes,
intermediate ones included (module downloads, vendored trees), even if the build
later fails. Paths the job substitutes are not uploaded. Each built path is
uploaded with its runtime closure, and the server skips what it already has. A
post-job step drains the queue.

Jobs without `id-token: write`, such as fork PRs, only get the substituter.

## Inputs

| Input | Required | Description |
|---|---|---|
| `server-url` | yes | niks3 server URL |
| `substituter` | no | override the substituter URL, e.g. a CDN mirror (defaults from server) |
| `netrc-login` | no | Basic auth login for the substituter's read path, with `netrc-password` |
| `netrc-password` | no | Basic auth password for `netrc-login` |
| `netrc` | no | netrc entries verbatim, instead of `netrc-login`/`netrc-password` |
| `skip-push` | no | configure the substituter only, don't upload |
| `cache-config-timeout` | no | seconds before each /api/cache-config request times out (default 15) |
| `cache-config-retries` | no | extra attempts to fetch /api/cache-config after a transient failure (default 3) |
| `drain-timeout` | no | seconds to wait for uploads to finish in the post step (default 600) |
| `niks3-bin` | no | path to a niks3 binary (with `niks3-hook` beside it), instead of downloading the release |
| `debug` | no | enable debug logging |

## Development

```sh
npm ci
npm run build      # regenerates dist/index.cjs
npm run typecheck
```

`dist/index.cjs` is committed; CI fails if it is stale.

The release this action downloads is [`NIKS3_VERSION`](NIKS3_VERSION), baked
into `dist/index.cjs` at bundle time. To release, set it to the new tag, run
`npm run build`, commit, then push the tag; the release workflow refuses a tag
that differs from it. `.github/workflows/action.yml` runs the action on a real
runner against a private niks3 started by [`test/fixture.sh`](test/fixture.sh),
on Determinate and upstream Nix.
