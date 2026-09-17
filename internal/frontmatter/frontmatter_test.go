// SPDX-License-Identifier: AGPL-3.0-or-later

package frontmatter

import "testing"

func TestSplit(t *testing.T) {
	if _, _, err := Split("no fence here"); err != ErrNoOpeningFence {
		t.Errorf("got %v, want ErrNoOpeningFence", err)
	}
	if _, _, err := Split("---\nname: x\nno closing fence"); err != ErrNoClosingFence {
		t.Errorf("got %v, want ErrNoClosingFence", err)
	}
	if yamlPart, body, err := Split("---\nname: x\n---\nbody\n"); err != nil || yamlPart != "name: x" || body != "body\n" {
		t.Errorf("yamlPart %q body %q err %v", yamlPart, body, err)
	}
	if yamlPart, body, err := Split("---\nname: x\n---"); err != nil || yamlPart != "name: x" || body != "" {
		t.Errorf("trailing fence with no body: yamlPart %q body %q err %v", yamlPart, body, err)
	}
}
