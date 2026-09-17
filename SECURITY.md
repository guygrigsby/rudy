# Security

## Reporting

Report a vulnerability privately through GitHub's
[report a vulnerability](https://github.com/guygrigsby/rudy/security/advisories/new)
form. It reaches the maintainer and nobody else, and it is the only channel
this project asks you to use. Please do not open a public issue for one.

No GitHub account, or the form is refusing you? Open an issue that says you
have a security report and nothing else about it, and you will get a private
channel back.

Say what an attacker controls, what they get, and how you reproduced it. A
patch is welcome and not expected. Expect an acknowledgement within a week and
a fix or a reason before any disclosure.

## What rudy already assumes

Rudy runs a model that runs commands on your machine. The README's trust model
section is the whole boundary and it is deliberately drawn at your user
account, so these are design, not bugs:

- Any process running as you can drive any live session on the socket:
  `rudy serve` checks the peer uid and nothing beyond it.
- Tools run as you, with your filesystem, your network and your credentials.
  Permission modes govern what the model may do, not what a local caller may
  do. This is not a sandbox.
- Installing a plugin runs its code, and a workspace's own plugins run once
  you trust that workspace.
- A session log holds what the model saw, `0600` and unencrypted.
- `web_fetch` puts a fetched page into the model's context, and a page can
  carry instructions the model reads like any other text.

A report that crosses the boundary is a vulnerability. Anything that lets a
model, a fetched page, a tool result or a remote plugin reach past what the
operator allowed is in scope, and so is a permission decision that is recorded
as allowed without an asker answering it, a session log or socket with wider
permissions than it claims, a provider key in a place the README says it never
goes, and a peer uid check that can be skipped.

## Versions

The newest release gets the fix, and `main` gets it first. There are no
maintained branches behind it.
