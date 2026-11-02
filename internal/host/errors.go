package host

import "errors"

// ErrFileGone means the file a link pointed at no longer exists on its
// host (or the code never matched one). It is permanent: retrying cannot
// bring the file back, so every resolver returns it immediately instead of
// burning its retries. Use errors.Is to test for it; implementations may
// wrap it with more context.
var ErrFileGone = errors.New("file no longer exists on this host")
