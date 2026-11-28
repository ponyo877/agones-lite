# Security policy

## Supported versions

scrim is pre-1.0. Fixes go to `main` and into the next release; only
the latest release is supported.

## Verifying a release

Releases after v0.2.0 come with `checksums.txt`, a software bill of
materials for each binary (`*.spdx.json`), and GitHub artifact
attestations for the binaries, those files and the sidecar image. An
attestation is a signed record that the release workflow of this
repository built the file from the tagged commit:

```sh
gh attestation verify scrim-linux-amd64 --repo ponyo877/scrim \
  --signer-workflow ponyo877/scrim/.github/workflows/release.yml
gh attestation verify oci://ghcr.io/ponyo877/scrim-sidecar:VERSION --repo ponyo877/scrim
```

The binaries are built with `-trimpath`, so `make dist VERSION=...` on the
same commit with the same Go version gives the same bytes, which can be
compared with `checksums.txt`.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it in private through GitHub: on the
[Security tab](https://github.com/ponyo877/scrim/security) of this
repository, choose **Report a vulnerability**. Only the maintainer sees the
report.

It helps to say:

- the commit you ran, and how scrim was started,
- what you did, what happened, and what you expected,
- what someone could do with it.

The maintainer confirms that the report arrived, keeps you informed while it
is looked into, and names you in the advisory if you wish.

## What is in scope

Anything that lets someone do what their token, or their lack of one, should
not allow: on the management API, the Allocation API and the API for
agents, between a game server and its sidecar, and from one game server's
container to another's.

A problem in Agones or in Docker themselves is best reported to those
projects.
