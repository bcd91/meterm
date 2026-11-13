//go:build !unix

package fileman

import "io/fs"

// sysOwner: no Unix owners here.
func sysOwner(fs.FileInfo) (uid, gid uint32, nlink uint64, ok bool) { return 0, 0, 0, false }

func localIdentity() (identity, bool) { return identity{}, false }
