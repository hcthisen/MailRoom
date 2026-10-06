package system

import "errors"

// errDiskUnsupported is returned by diskFree on platforms without an
// implementation.
var errDiskUnsupported = errors.New("free disk space is not supported on this platform")
