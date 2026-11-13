package prodask

import "os"

// fileOwnedByUs isn't checked on Windows: a token file there is only as
// private as the user's temp directory.
func fileOwnedByUs(os.FileInfo) bool { return true }
