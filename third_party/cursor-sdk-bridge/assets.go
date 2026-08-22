// Package cursorbridge exposes the vendored release metadata needed to
// install the pinned standalone bridge at runtime.
package cursorbridge

import _ "embed"

var (
	//go:embed bridge.lock
	lockFile string
	//go:embed v1.0.28/LICENSE
	licenseFile string
)

// LockFile returns the immutable release lock embedded in ccLoad.
func LockFile() string { return lockFile }

// LicenseFile returns the license shipped with the pinned bridge release.
func LicenseFile() string { return licenseFile }
