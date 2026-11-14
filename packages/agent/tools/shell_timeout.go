package tools

import (
	"fmt"
	"time"
)

const maxShellTimeoutSeconds = int64(1<<63-1) / int64(time.Second)

// ValidateShellTimeout rejects negative or overflowing configured timeouts.
func ValidateShellTimeout(seconds int) error {
	if seconds < 0 || int64(seconds) > maxShellTimeoutSeconds {
		return fmt.Errorf("shell_timeout_seconds must be between 0 and %d", maxShellTimeoutSeconds)
	}
	return nil
}

func shellTimeout(configured, requested int) (time.Duration, error) {
	if err := ValidateShellTimeout(configured); err != nil {
		return 0, err
	}
	seconds := requested
	if configured > 0 && (seconds <= 0 || seconds > configured) {
		seconds = configured
	}
	if seconds <= 0 {
		return 0, nil
	}
	if int64(seconds) > maxShellTimeoutSeconds {
		return 0, fmt.Errorf("timeout exceeds %d seconds", maxShellTimeoutSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
