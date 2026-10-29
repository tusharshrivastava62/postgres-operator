# Security policy

## Reporting a vulnerability

Report a vulnerability in the operator, the `pg-compliance` command, the Helm chart, or the
database images this project builds by email to **tusharshrivastava62@gmail.com**. Do not open a
public issue for it.

Include:

- what you found and which component it affects;
- the version or image tag;
- how to reproduce it, or a proof of concept;
- the impact as you see it.

## What to expect

- An acknowledgement within 5 business days.
- An assessment, and a fix or a mitigation plan if the report is confirmed.
- Credit in the release notes, if you want it.

Please give the project time to fix the issue before you disclose it publicly.

## Supported versions

Security fixes are made in the latest release.

## Hardening a deployment

The options that harden a cluster, and what each one defaults to, are in
[`docs/security.md`](docs/security.md).
