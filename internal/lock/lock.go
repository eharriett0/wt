package lock

import "errors"

// ErrTimeout is SharedWithin or ExclusiveWithin giving up: another process
// held the file's lock for the whole wait (#234 review, #235).
var ErrTimeout = errors.New("the file stayed locked by another process")

// ErrSeenHeld is a lock failure that came after a try found the file HELD
// (#235 review): locks work on this machine and someone holds this one, so it
// is never Unsupported, and a caller must not go on as if the filesystem
// couldn't lock.
var ErrSeenHeld = errors.New("flock failed after another process was seen holding the lock")
