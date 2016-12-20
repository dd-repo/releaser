releaser
========

The `release-caddy` command is used by the Caddy maintainers to publish new releases of Caddy.

It requires:

- `go` and `git` in PATH
- GOPATH with Caddy repo in a clean state, with HEAD at the commit to deploy
- GitHub Access Token from an account with permission to push to the Caddy repository
- Developer portal ID and key from an account authorized to update the Caddy build server

Credentials must be set in environment variables. Example use (assuming your default GOPATH is used; can specify a different one if you need to):

```bash
$ GITHUB_TOKEN="your_token" DEVPORTAL_ID="your_id" DEVPORTAL_KEY="your_key" release-caddy
```

This program will perform some checks, ask some simple questions, then confirm with you before proceeding. Since it will tag the release for you, you need only be checked out at the commit you wish to release.

Note: Before running tests, this program runs `go get -u` on the Caddy package in your GOPATH, which updates Caddy and its dependencies to the latest commits. If the tests fail, the deploy will abort, but the updates will not be reverted.

If a release failed after the tag was pushed, the release can be picked up at a later point, skipping all the other deploy steps, by using the `-resume` flag: `-resume="github"` will pick up a deploy at the current tag by publishing the release to GitHub. This is useful if there are network errors at the end of a deploy.
