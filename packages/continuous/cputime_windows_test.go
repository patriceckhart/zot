//go:build windows

package continuous

import "time"

// cpuTime is not measured on Windows; the idle benchmark reports zero.
func cpuTime() time.Duration { return 0 }
