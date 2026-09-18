# Security

Report a vulnerability through GitHub's
[private reporting form](https://github.com/guygrigsby/rudy/security/advisories/new).
It reaches the maintainer and nobody else. Please don't open a public issue for
one. No GitHub account, or the form refuses you: open an issue that says you
have a security report and nothing else about it, and you'll get a private
channel back.

Say what an attacker controls, what they get and how you reproduced it. A patch
is welcome and not expected. Expect an acknowledgement within a week and a fix
or a reason before any disclosure.

Rudy runs a model that runs commands on your machine. The boundary is your user
account, drawn there deliberately, and the README's trust model section is the
whole of it. So these are design, not bugs:

- Any process running as you can drive any live session on the socket. `rudy
  serve` checks the peer uid and nothing beyond it.
- Tools run as you, with your filesystem, your network and your credentials.
  Permission modes govern what the model may do, not what a local caller may
  do. This is not a sandbox.
- Installing a plugin runs its code, and a workspace's own plugins run once you
  trust that workspace.
- A session log holds what the model saw, `0600` and unencrypted.
- `web_fetch` puts a fetched page into the model's context, and a page can carry
  instructions the model reads like any other text.

Crossing that boundary is a vulnerability. So is anything that lets a model, a
fetched page, a tool result or a remote plugin reach past what the operator
allowed, a permission decision recorded as allowed without an asker answering
it, a session log or socket wider than it claims, a provider key somewhere the
README says it never goes, and a peer uid check that can be skipped.

The newest release gets the fix and `main` gets it first. There are no
maintained branches behind it.
