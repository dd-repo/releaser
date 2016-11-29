releaser
========

The `release-caddy` command is used by the Caddy maintainers to publish new releases of Caddy.

It requires:

- `go` and `git` in PATH
- GOPATH with Caddy repo in a clean state
- GitHub Access Token from an account with permission to push to the Caddy repository
- ID and key to update the Caddy build server

Credentials must be set in environment variables. Recommended use (assuming your default GOPATH is used; can specify a different one if you need to):

```bash
$ GITHUB_TOKEN="your_token" BUILD_SERVER_ID="your_id" BUILD_SERVER_KEY="your_key" release-caddy
```

This program will perform some checks, ask some simple questions, then confirm with you before proceeding. Since it will tag the release for you, you need only be checked out at the commit you wish to release.

Note: Before running tests, this program runs `go get -u` on the Caddy package in your GOPATH, which updates Caddy and its dependencies to the latest commits. If the tests fail, the deploy will abort, but the updates will not be reverted.
