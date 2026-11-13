//go:build unix

package fileman

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// sysOwner reads the owner, group and hard link count the kernel keeps
// for a local file.
func sysOwner(fi fs.FileInfo) (uid, gid uint32, nlink uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return st.Uid, st.Gid, uint64(st.Nlink), true
}

// localIdentity is who this process is to the kernel: what decides whether
// it may write a local file.
func localIdentity() (id identity, ok bool) {
	gids, err := os.Getgroups()
	if err != nil {
		return identity{}, false
	}
	id = identity{uid: uint32(os.Geteuid()), groupsKnown: true, rootAll: true}
	id.gids = append(id.gids, uint32(os.Getegid()))
	for _, g := range gids {
		id.gids = append(id.gids, uint32(g))
	}
	return id, true
}

// readOnlyFS says err is this machine's filesystem refusing to create a file
// because it's mounted read-only. A file in it may still be writable: one
// bind-mounted read-write from elsewhere (docker --read-only -v file:file, a
// Kubernetes subPath under readOnlyRootFilesystem).
func readOnlyFS(err error) bool { return errors.Is(err, syscall.EROFS) }
