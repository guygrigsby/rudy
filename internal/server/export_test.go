// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// SetBeforeAttachHook installs f to run inside resume between loading a session and
// attaching to it, and returns what puts the previous hook back. It exists so the close that
// races a resume can be made to land in that window every time instead of once in twenty
// runs on a loaded machine (rudy-anw).
func SetBeforeAttachHook(f func()) func() {
	old := beforeAttachHook.Swap(&f)
	return func() {
		if old == nil {
			beforeAttachHook.Store(nil)
			return
		}
		beforeAttachHook.Store(old)
	}
}
