package lock

import "errors"

// ErrTimeout is SharedWithin or ExclusiveWithin giving up: another process
// held the file's lock for the whole wait (#234 review, #235).
var ErrTimeout = errors.New("the file stayed locked by another process")
