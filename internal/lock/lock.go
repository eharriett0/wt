package lock

import "errors"

// ErrTimeout is SharedWithin giving up: another process held the file's
// exclusive lock for the whole wait (#234 review).
var ErrTimeout = errors.New("the file stayed locked by another process")
