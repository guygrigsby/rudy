// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type browserRunner func(context.Context, string, ...string) error

func browserCommand() string {
	switch runtime.GOOS {
	case "darwin":
		return "open"
	case "windows":
		return "rundll32"
	default:
		return "xdg-open"
	}
}

func runBrowser(ctx context.Context, name string, args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(bounded, name, args...).Run()
}

// openAuthURL treats the challenge as untrusted data. The opener receives one URL argv,
// never a shell command, and only approved HTTPS hosts can reach it.
func openAuthURL(ctx context.Context, raw string, run browserRunner) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return errors.New("login URL must be HTTPS on an approved host")
	}
	host := strings.ToLower(u.Hostname())
	if !approvedAuthHost(host) {
		return errors.New("login URL host is not approved")
	}
	if runtime.GOOS == "windows" {
		return run(ctx, browserCommand(), "url.dll,FileProtocolHandler", raw)
	}
	return run(ctx, browserCommand(), raw)
}

func approvedAuthHost(host string) bool {
	return host == "openai.com" || strings.HasSuffix(host, ".openai.com") ||
		host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}
