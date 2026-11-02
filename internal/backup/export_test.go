package backup

import "time"

// SetCreateRetryDelays replaces the pauses of CreateJobRetrying, so that tests do not wait
// for them, and returns the function that puts the old ones back.
func SetCreateRetryDelays(delays ...time.Duration) (restore func()) {
	old := createRetryDelays
	createRetryDelays = delays
	return func() { createRetryDelays = old }
}
