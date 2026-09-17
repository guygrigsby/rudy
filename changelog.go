// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rudy is the module root, and exists for one reason: to embed the files a built
// binary has to carry with it. Nothing else lives here; the harness is under internal/.
package rudy

import _ "embed"

// Changelog is CHANGELOG.md as the binary was built with it. The startup header reads the
// newest release out of it, so a built rudy reports what it actually is rather than what
// the checkout beside it happens to say. ADR 0016.
//
//go:embed CHANGELOG.md
var Changelog string
