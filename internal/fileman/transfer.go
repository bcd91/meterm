package fileman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// FileOp is one file to copy.
type FileOp struct {
	Src, Dst string
	Size     int64
	Mode     fs.FileMode
	Exists   bool // something is already at Dst
	// LinkTo is the file Dst leads to when Dst is a symlink: what an
	// overwrite writes.
	LinkTo string
}

// Plan is everything a copy will do, worked out before any byte moves so the
// person can be asked about overwrites once, up front.
type Plan struct {
	Src, Dst   FS
	Items      []string // what was asked for (top-level paths on Src)
	DstDir     string   // where they go on Dst
	Dirs       []string // to create on Dst, parents first
	Files      []FileOp
	TotalBytes int64
	Skipped    []string // symlinked directories and other things not copied, with why
}

// Conflicts counts files that would replace something.
func (p Plan) Conflicts() int {
	n := 0
	for _, f := range p.Files {
		if f.Exists {
			n++
		}
	}
	return n
}

// MakePlan walks items (paths on src) and maps them under dstDir on dst.
// Directories are copied recursively. A symlink to a file is copied as the
// file; a symlink to a directory is skipped, to avoid loops.
func MakePlan(ctx context.Context, src FS, items []string, dst FS, dstDir string) (Plan, error) {
	p := Plan{Src: src, Dst: dst, Items: items, DstDir: dstDir}
	var walk func(s, d string) error
	walk = func(s, d string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		le, err := src.Lstat(s)
		if err != nil {
			return err
		}
		e := le
		if le.Link {
			if e, err = src.Stat(s); err != nil {
				p.Skipped = append(p.Skipped, s+" (broken symlink)")
				return nil
			}
			if e.IsDir {
				p.Skipped = append(p.Skipped, s+" (symlink to a directory)")
				return nil
			}
		}
		if e.IsDir {
			if _, lerr := dst.Lstat(d); lerr == nil {
				if t, serr := dst.Stat(d); serr != nil || !t.IsDir {
					return fmt.Errorf("%s is a file; not replacing a file with a directory; nothing was copied", where(dst, d))
				}
			}
			p.Dirs = append(p.Dirs, d)
			children, err := src.ReadDir(s)
			if err != nil {
				return err
			}
			for _, c := range children {
				if err := walk(src.Join(s, c.Name), dst.Join(d, c.Name)); err != nil {
					return err
				}
			}
			return nil
		}
		if !e.Mode.IsRegular() {
			p.Skipped = append(p.Skipped, s+" (not a regular file)")
			return nil
		}
		op := FileOp{Src: s, Dst: d, Size: e.Size, Mode: e.Mode}
		if de, lerr := dst.Lstat(d); lerr == nil {
			op.Exists = true
			if err := checkReplaceable(dst, d, de, &op); err != nil {
				return err
			}
		}
		p.Files = append(p.Files, op)
		p.TotalBytes += e.Size
		return nil
	}
	for _, it := range items {
		if err := walk(it, dst.Join(dstDir, src.Base(it))); err != nil {
			return p, err
		}
	}
	return p, nil
}

// checkReplaceable refuses, before anything is asked, a file that would
// replace something other than a file: a directory, a link to one, a
// device. A link to a file is fine, and op.LinkTo says where it leads; a
// link that leads nowhere is left for the copy to refuse.
func checkReplaceable(dst FS, d string, de Entry, op *FileOp) error {
	refuse := func(what string) error {
		return fmt.Errorf("%s %s; nothing was copied", where(dst, d), what)
	}
	if de.Link {
		t, err := dst.Stat(d)
		switch {
		case err != nil:
			return nil
		case t.IsDir:
			return refuse("is a symlink to a directory; not replacing a directory with a file")
		case !t.Mode.IsRegular():
			return refuse("is a symlink to something that isn't a regular file; not replacing it")
		}
		if to, err := dst.Resolve(d); err == nil {
			op.LinkTo = to
		}
		return nil
	}
	switch {
	case de.IsDir:
		return refuse("is a directory; not replacing a directory with a file")
	case !de.Mode.IsRegular():
		return refuse("isn't a regular file; not replacing it")
	}
	return nil
}

// Progress is reported while a plan runs.
type Progress struct {
	File               string // current source path
	FileDone, FileSize int64
	Done, Total        int64 // bytes, whole plan
	Files, FilesTotal  int   // finished files
	Started            time.Time
	At                 time.Time // when this report was made
	Temp               string    // the temporary file being written, if any
	InPlace            string    // the file being written in place, if any
}

// Rate is bytes per second so far.
func (p Progress) Rate() float64 {
	secs := time.Since(p.Started).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(p.Done) / secs
}

// Result summarises a finished (or stopped) run.
type Result struct {
	Copied, Skipped int
	Replaced        int        // of Copied, how many replaced an existing file
	ReplacedPaths   []string   // those, as "dst" or "dst (-> the file the link led to)"
	InPlace         []string   // of those, the ones written in place, not atomically
	Bytes           int64      // in files that were completed
	Warnings        []string   // things that went wrong without stopping the copy
	Lost            []string   // replaced though they lost owner, group or links, and how
	Followed        []Followed // replaced through a symlink
	Files           []string   // every file copied, in order, as "dst"
	// Leftovers are temporary files that couldn't be removed, each with
	// its side and why.
	Leftovers []string
}

// Followed is a symlink an overwrite went through, and the file it wrote.
type Followed struct{ Link, Target string }

// Add puts what a later run of the same plan did onto r.
func (r Result) Add(o Result) Result {
	r.Copied += o.Copied
	r.Skipped += o.Skipped
	r.Replaced += o.Replaced
	r.ReplacedPaths = append(append([]string(nil), r.ReplacedPaths...), o.ReplacedPaths...)
	r.InPlace = append(append([]string(nil), r.InPlace...), o.InPlace...)
	r.Bytes += o.Bytes
	r.Warnings = append(append([]string(nil), r.Warnings...), o.Warnings...)
	r.Lost = append(append([]string(nil), r.Lost...), o.Lost...)
	r.Followed = append(append([]Followed(nil), r.Followed...), o.Followed...)
	r.Files = append(append([]string(nil), r.Files...), o.Files...)
	r.Leftovers = append(append([]string(nil), r.Leftovers...), o.Leftovers...)
	return r
}

// RunOptions says how a plan is run.
type RunOptions struct {
	// Overwrite decides what happens to files that already exist (false:
	// leave them).
	Overwrite bool
	// InPlace holds the files on Dst (InPlaceNeeded.Target) the person
	// agreed may be written in place, because no temporary file can be
	// made next to them.
	InPlace map[string]bool
	// From is the first of Plan.Files to copy: a run that stopped to ask
	// goes on from where it stopped.
	From int
	// OnLoss is what to do with a file whose replacement would lose its
	// owner, group or hard links (*ReplaceLoses): ask (0), or the answer
	// given, for every such file in the rest of the copy.
	OnLoss LossChoice
	// After, when it's done, stops the copy the gentle way: a file going to
	// a temporary name stops at once (the target is untouched, so nothing
	// is lost), and a file being written in place is let finish, since
	// stopping it would leave it partly written; then the copy goes no
	// further. Cancelling the context RunWith was given stops at once,
	// whatever the file. After must be that context or derived from it;
	// nil means that context.
	After context.Context
}

// LossChoice is the answer to *ReplaceLoses.
type LossChoice byte

const (
	LossAsk     LossChoice = 0
	LossInPlace LossChoice = 'i' // write in place: keeps owner, group and links; not atomic
	LossReplace LossChoice = 'r' // replace anyway
	LossSkip    LossChoice = 's' // leave the file as it is
)

// ReplaceLoses stops a run at a file that replacing would change for
// someone else: its owner or group can't be given to the new file, or it
// has other hard links, which would keep the old content.
type ReplaceLoses struct {
	Index     int    // in Plan.Files
	File      FileOp // the file being copied
	Target    string // the file that would be replaced (a symlink's target)
	Where     string // Target with its side
	Owner     string // "owner 0, group 100": what it has now, when that would change
	NewOwner  string // what a new file there gets instead
	Links     uint64 // its hard links, when more than one is known
	InPlaceOK bool   // it may be writable, so writing in place is worth offering
}

// what is what replacing would lose, for messages and the log.
func (e *ReplaceLoses) what() string {
	var parts []string
	if e.Owner != "" {
		parts = append(parts, fmt.Sprintf("%s would become %s", e.Owner, e.NewOwner))
	}
	if e.Links > 1 {
		parts = append(parts, fmt.Sprintf("its %d hard links would split, the others keeping the old content", e.Links))
	}
	return strings.Join(parts, "; ")
}

func (e *ReplaceLoses) Error() string {
	return fmt.Sprintf("replacing %s: %s", e.Where, e.what())
}

// InPlaceNeeded stops a run at a file that is to be replaced, is writable,
// but sits in a directory where no temporary file can be made (or on a host
// that won't say why none could be: see Unclear). Writing it in
// place is possible but not atomic, so it's the person's call: run the plan
// again From Index with Target in RunOptions.InPlace.
type InPlaceNeeded struct {
	Index  int    // in Plan.Files
	File   FileOp // the file being copied
	Target string // the file that would be written (a symlink's target)
	Dir    string // the directory that can't be written, on Dst
	Where  string // Dir with its side, for messages
	Err    error  // why the temporary file couldn't be made
	// Known says the file is writable, from its mode and owner; otherwise
	// it only may be (over SFTP, who we are isn't known).
	Known bool
	// Unclear says the host gave no reason ("failure"): a read-only
	// filesystem, a full disk or a quota. Written in place on a full
	// disk, the file may be left partly written. Side names the host.
	Unclear bool
	Side    string
}

func (e *InPlaceNeeded) Error() string {
	if e.Unclear {
		return fmt.Sprintf("%s didn't say why it couldn't create a copy beside %s (%v): read-only, full disk or quota", e.Side, e.Target, e.Err)
	}
	is := "is"
	if !e.Known {
		is = "may be"
	}
	return fmt.Sprintf("can't write a temporary file in %s (%v); the file itself %s writable", e.Where, e.Err, is)
}

func (e *InPlaceNeeded) Unwrap() error { return e.Err }

// PartialWrite is a write in place that failed: the file may now hold some
// of the new bytes and none of the old ones past them. It is never removed.
type PartialWrite struct {
	Path  string // the file, on Dst
	Where string // the file, with its side
	Err   error
}

func (e *PartialWrite) Error() string {
	return fmt.Sprintf("writing %s in place failed: %v; %s may be partly written", e.Where, e.Err, e.Where)
}

func (e *PartialWrite) Unwrap() error { return e.Err }

// Run executes the plan; see RunWith.
func Run(ctx context.Context, p Plan, overwrite bool, report func(Progress)) (Result, error) {
	return RunWith(ctx, p, RunOptions{Overwrite: overwrite}, report)
}

// RunWith executes the plan. Each file is written under a temporary name
// and renamed into place when complete; a cancelled or failed copy removes
// its temporary file and leaves the target as it was. The one exception is
// a file written in place, which only happens where o.InPlace allows it.
func RunWith(ctx context.Context, p Plan, o RunOptions, report func(Progress)) (Result, error) {
	var res Result
	after := o.After
	if after == nil {
		after = ctx
	}
	pr := Progress{Total: p.TotalBytes, FilesTotal: len(p.Files), Started: time.Now()}
	if o.From == 0 {
		for _, d := range p.Dirs {
			if err := p.Dst.MkdirAll(d); err != nil {
				return res, fmt.Errorf("create %s: %w", where(p.Dst, d), cause(err))
			}
		}
	}
	last := time.Time{}
	tick := func(force bool) {
		if report != nil && (force || time.Since(last) > 100*time.Millisecond) {
			last = time.Now()
			pr.At = last
			report(pr)
		}
	}
	for i, f := range p.Files {
		if i < o.From {
			pr.Done += f.Size
			pr.Files++
			continue
		}
		if err := after.Err(); err != nil {
			return res, err
		}
		if f.Exists && !o.Overwrite {
			res.Skipped++
			pr.Done += f.Size
			pr.Files++
			continue
		}
		pr.File, pr.FileDone, pr.FileSize = f.Src, 0, f.Size
		tick(true)
		out, err := copyFile(ctx, after, p.Src, p.Dst, f, f.Exists && o.Overwrite, o, func(tmp string, inPlace bool) {
			if inPlace {
				pr.InPlace = tmp
			} else {
				pr.Temp = tmp
			}
			tick(true)
		}, func(delta int64) {
			pr.FileDone += delta
			pr.Done += delta
			tick(false)
		})
		if out.leftover != "" {
			res.Leftovers = append(res.Leftovers, fmt.Sprintf("%s (%v)", out.leftover, out.leftErr))
		}
		if err != nil {
			var need *InPlaceNeeded
			if errors.As(err, &need) {
				need.Index = i
			}
			var loses *ReplaceLoses
			if errors.As(err, &loses) {
				loses.Index = i
			}
			return res, err
		}
		if out.warn != "" {
			res.Warnings = append(res.Warnings, f.Dst+": "+out.warn)
		}
		if out.notice != "" {
			res.Warnings = append(res.Warnings, out.notice)
		}
		if out.skipped {
			res.Skipped++
			pr.Done += f.Size - pr.FileDone
			pr.Files++
			continue
		}
		if out.lost != "" {
			res.Lost = append(res.Lost, out.target+" ("+out.lost+")")
		}
		res.Bytes += out.n
		pr.Files++
		res.Copied++
		res.Files = append(res.Files, f.Dst)
		if f.Exists && o.Overwrite {
			res.Replaced++
			name := f.Dst
			if out.target != f.Dst {
				name += " (-> " + out.target + ")"
				res.Followed = append(res.Followed, Followed{f.Dst, out.target})
			}
			res.ReplacedPaths = append(res.ReplacedPaths, name)
			if out.inPlace {
				res.InPlace = append(res.InPlace, out.target)
			}
		}
	}
	pr.File = ""
	tick(true)
	return res, nil
}

// copied is what copyFile did.
type copied struct {
	n       int64
	target  string // the file written: f.Dst, or the file a link there led to
	inPlace bool
	skipped bool   // left as it was, by choice or because warn says it had to be
	lost    string // what replacing it lost (owner, group, hard links), when the person said go ahead
	warn    string
	// notice is a whole sentence for the person, naming the file itself.
	notice string
	// leftover is a temporary file that couldn't be removed, with its
	// side, and leftErr why.
	leftover string
	leftPath string
	leftErr  error
}

// Unchanged is a copy that stopped, for err, before its temporary file was
// moved into place: Where (the file it was for, with its side) is as it was,
// or, when New, was never created. Leftover, when not "", is the temporary
// file, which couldn't be removed either, with its side.
type Unchanged struct {
	Path        string // the file, on Dst
	Where       string
	New         bool
	Leftover    string
	LeftoverErr error
	// LeftoverPath is Leftover without its side.
	LeftoverPath string
	Err          error
}

func (e *Unchanged) Error() string {
	var s string
	if e.New {
		s = fmt.Sprintf("%s not created: %v", e.Where, e.Err)
		if e.Leftover != "" {
			s += fmt.Sprintf("; couldn't remove the unfinished new file %s (%v)", e.Leftover, e.LeftoverErr)
		}
		return s
	}
	s = fmt.Sprintf("%s unchanged: %v", e.Where, e.Err)
	if e.Leftover != "" {
		s += fmt.Sprintf("; the unfinished copy is still at %s (%v)", e.Leftover, e.LeftoverErr)
	}
	return s
}

func (e *Unchanged) Unwrap() error { return e.Err }

// specialBits are the mode bits besides permissions that a replacement
// keeps: set only on the finished copy, just before it's moved into place.
const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// inPlaceWorthAsking says a temporary file couldn't be made for a reason
// that leaves the file itself writable: permission to add to the directory,
// or here, a read-only filesystem with the file mounted into it. Anything
// else (no space, no inodes, a quota) would fail or truncate in place too.
func inPlaceWorthAsking(dst FS, err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	return isLocal(dst) && readOnlyFS(err)
}

// isLocal says fsys is this machine, where the kernel answers directly.
// Not by its label: a host may be saved under the name "local".
func isLocal(fsys FS) bool {
	_, ok := fsys.(interface{ thisMachine() })
	return ok
}

// statOf reads what the open file w is, by its handle: never by a name,
// which something else could have swapped by now.
func statOf(w io.Writer) (fs.FileInfo, bool) {
	s, ok := w.(interface{ Stat() (fs.FileInfo, error) })
	if !ok {
		return nil, false
	}
	fi, err := s.Stat()
	return fi, err == nil
}

// copyFile writes f to a temporary file next to its target and moves it
// into place only once every byte is there, so a cancelled or failed copy
// never touches what was at the target. replace says the person agreed to
// replace a file there; otherwise a file that appeared since the plan was
// made is left alone and the copy fails.
//
// When the target is a symlink, the file it leads to is what gets replaced
// (the temporary file goes next to that), so sites-enabled/x ->
// ../sites-available/x stays a link. A link that leads nowhere is refused.
//
// A file we may not write, or can't tell, is never renamed over: that would
// get round its mode. It's written in place, as v0.23.1 did, which the
// side's own permission check allows or refuses before anything changes.
//
// A replacement gets the replaced file's mode, owner and group where they
// can be given, all through the temporary file's own handle. setuid and
// setgid go on last, on the finished copy. When owner or group can't be
// given, or the file has other hard links (which would go on holding the
// old content), the copy stops with *ReplaceLoses unless o.OnLoss has the
// answer. A mode that can't be set is a warning, unless the new file would
// be more open than the old one: then the old one is left as it was.
//
// When no temporary file can be made there for lack of permission (or a
// read-only filesystem here), but the file to replace may be writable, it's
// written in place if o.InPlace holds it, and otherwise the copy stops with
// *InPlaceNeeded. Whether it may be writable is worked out from its mode and
// owner: it isn't opened to find out. Any other reason stops the copy with
// the file as it was. When only the final rename fails (a bind-mounted
// file), the file is untouched, and it's written in place, saying so.
//
// onTemp is told the file being written as it starts (the temporary file,
// or with inPlace set the target itself) and "" when it's done.
//
// now stops it at once; after stops a temporary file at once and lets a
// write in place finish (see RunOptions.After).
func copyFile(now, after context.Context, src, dst FS, f FileOp, replace bool, o RunOptions, onTemp func(p string, inPlace bool), onBytes func(int64)) (res copied, err error) {
	in, err := src.Open(f.Src)
	if err != nil {
		return res, fmt.Errorf("read %s: %w", where(src, f.Src), cause(err))
	}
	defer in.Close()
	target := f.Dst
	res.target = target
	var old Entry // the file being replaced, when it's a regular file
	haveOld := false
	if replace {
		if e, lerr := dst.Lstat(target); lerr == nil {
			if e.Link {
				resolved, rerr := dst.Resolve(target)
				if rerr != nil {
					return res, fmt.Errorf("%s is a symbolic link that doesn't lead to a file (%v); not replacing the link", where(dst, target), cause(rerr))
				}
				target = resolved
				res.target = target
				e, lerr = dst.Lstat(target)
			}
			if lerr == nil && e.Mode.IsRegular() {
				old, haveOld = e, true
			}
		}
	}
	// A replacement is made with the permissions it'll keep, so a 0600
	// file's replacement is never more open than that, even for a moment.
	// Never setuid or setgid: those go on the finished copy alone.
	mode := f.Mode.Perm()
	if haveOld {
		mode = old.Mode.Perm()
	}
	out, tmp, err := dst.CreateTemp(target, mode)
	permWarn := ""
	var pe *PermError
	if errors.As(err, &pe) && out != nil {
		permWarn, err = pe.Error(), nil
	}
	if err != nil {
		dir := dst.Dir(target)
		if !replace || !haveOld {
			return res, fmt.Errorf("can't write in %s: %w", where(dst, dir), cause(err))
		}
		// A host that only says "failure" may be read-only (a file mounted
		// into a read-only filesystem, where writing in place works) or full
		// (where it would leave a partial file): only the person can say.
		var se *sftp.StatusError
		unclear := errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxFailure
		if !unclear && !inPlaceWorthAsking(dst, err) {
			return res, fmt.Errorf("can't make a temporary file in %s (%w); %s was left as it was", where(dst, dir), cause(err), where(dst, target))
		}
		id, idOK := identityOf(dst)
		ok, known := mayWrite(old, id, idOK)
		if known && !ok {
			return res, fmt.Errorf("can't write a temporary file in %s (%v), and %s isn't writable either (mode %04o, owner %d, group %d); it was left as it was",
				where(dst, dir), cause(err), where(dst, target), old.Mode.Perm(), old.Uid, old.Gid)
		}
		if !o.InPlace[target] {
			return res, &InPlaceNeeded{File: f, Target: target, Dir: dir, Where: where(dst, dir), Err: cause(err), Known: known,
				Unclear: unclear, Side: dst.Label()}
		}
		return writeInPlace(now, after, in, dst, target, res, onTemp, onBytes)
	}
	if onTemp != nil {
		onTemp(tmp, false)
	}
	closed, committed, dropped := false, false, false
	// discard removes the temporary file, saying where it is when it
	// can't be.
	discard := func() {
		if !closed {
			closed = true
			_ = out.Close()
		}
		if rerr := dst.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			res.leftover, res.leftPath, res.leftErr = where(dst, tmp), tmp, cause(rerr)
			if err != nil {
				err = &Unchanged{Path: target, Where: where(dst, target), New: !haveOld, Leftover: where(dst, tmp), LeftoverPath: tmp, LeftoverErr: cause(rerr), Err: err}
			}
		}
	}
	defer func() {
		var stuck *ReplaceStuck
		var loses *ReplaceLoses
		switch {
		case committed || dropped:
		case errors.As(err, &stuck):
			// Neither copy is removed; the error says where both are.
		default:
			discard()
			// The file it was for stays as it was: say so, of that file.
			var u *Unchanged
			if err != nil && !errors.As(err, &u) && !errors.As(err, &loses) {
				err = &Unchanged{Path: target, Where: where(dst, target), New: !haveOld, Err: err}
			}
		}
		if onTemp != nil {
			onTemp("", false)
		}
	}()
	// drop discards the temporary file now, for a target left as it is or
	// written in place.
	drop := func() {
		dropped = true
		discard()
		if onTemp != nil {
			onTemp("", false)
		}
	}
	if haveOld {
		// A file we may not write is never renamed over: that would get
		// round its mode. Locally the kernel says who we are; on a host the
		// temporary file's owner and group say it, as far as they go.
		id, idOK := identityOf(dst)
		if !idOK {
			if fi, ok := statOf(out); ok {
				if t := entryOf(tmp, fi); t.HasOwner {
					id, idOK = identity{uid: t.Uid, gids: []uint32{t.Gid}}, true
				}
			}
		}
		if ok, known := mayWrite(old, id, idOK); !ok || !known {
			drop()
			res, err = writeInPlace(now, after, in, dst, target, res, onTemp, onBytes)
			if err == nil {
				res.warn = fmt.Sprintf("written in place, not replaced: its mode %04o, owner %d and group %d don't say it may be written by us", old.Mode.Perm(), old.Uid, old.Gid)
			}
			return res, err
		}
		loses, err := keepOwner(dst, out, tmp, old)
		if err != nil {
			return res, err
		}
		if loses != nil {
			loses.File, loses.Target, loses.Where = f, target, where(dst, target)
			switch o.OnLoss {
			case LossSkip:
				drop()
				res.skipped = true
				return res, nil
			case LossInPlace:
				drop()
				return writeInPlace(now, after, in, dst, target, res, onTemp, onBytes)
			case LossReplace:
				res.lost = loses.what()
			default:
				return res, loses
			}
		}
		// The mode after the owner: a chown by anyone but root clears
		// setuid and setgid, and a CreateTemp mode may not have been set at
		// all. Permissions only, for now.
		want := old.Mode.Perm()
		if c, ok := out.(interface{ Chmod(fs.FileMode) error }); ok {
			if cerr := c.Chmod(want); cerr != nil {
				permWarn = (&PermError{Err: cerr}).Error()
			} else {
				permWarn = "" // the mode that matters was set after all
			}
		}
		if permWarn != "" {
			// Not set: only go on if what the new file has is no more open.
			if fi, ok := statOf(out); !ok || fi.Mode().Perm()&^want != 0 {
				drop()
				res.skipped = true
				res.warn = fmt.Sprintf("couldn't keep permissions %04o; left the original", want)
				return res, nil
			}
		}
	}
	res.warn = permWarn
	n, wrote, err := copyData(after, out, in, onBytes)
	res.n = n
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return res, err
		}
		if wrote {
			return res, fmt.Errorf("write %s: %w", where(dst, target), cause(err))
		}
		return res, fmt.Errorf("read %s: %w", where(src, f.Src), cause(err))
	}
	if haveOld && old.Mode&specialBits != 0 {
		// setuid and setgid now, on the finished copy, and checked: the
		// kernel quietly drops setgid for a group that isn't ours.
		want := old.Mode.Perm() | old.Mode&specialBits
		c, ok := out.(interface{ Chmod(fs.FileMode) error })
		if ok {
			_ = c.Chmod(want)
		}
		if fi, sok := statOf(out); !ok || !sok || fi.Mode()&(fs.ModePerm|specialBits) != want {
			drop()
			res.skipped = true
			res.warn = fmt.Sprintf("couldn't keep its mode %04o (setuid, setgid or sticky); left the original", unixMode(want))
			return res, nil
		}
	}
	// What the handle is, to check its name still leads to it at the end.
	written, _ := statOf(out)
	closed = true
	if err = out.Close(); err != nil {
		return res, fmt.Errorf("write %s: %w", where(dst, target), cause(err))
	}
	if err = after.Err(); err != nil {
		return res, err
	}
	if err = stillOurs(dst, tmp, written, n); err != nil {
		return res, err
	}
	if replace {
		err = dst.Replace(tmp, target)
	} else {
		err = dst.Rename(tmp, target)
	}
	var left *BackupLeft
	if errors.As(err, &left) {
		res.warn, err = strings.TrimPrefix(res.warn+"; "+left.Error(), "; "), nil
	}
	var stuck *ReplaceStuck
	switch {
	case err == nil:
		committed = true
		return res, nil
	case errors.Is(err, fs.ErrExist), errors.As(err, &stuck):
		return res, err
	case !haveOld:
		return res, fmt.Errorf("move the copy into place at %s: %w", where(dst, target), cause(err))
	}
	// Only the rename failed (a bind-mounted file can't be renamed over):
	// the file is untouched, so write it in place, as v0.23.1 did.
	renameErr := cause(err)
	err = nil
	drop()
	if res.leftover != "" {
		return res, &Unchanged{Path: target, Where: where(dst, target), Err: fmt.Errorf("couldn't replace it in one step (%v)", renameErr),
			Leftover: res.leftover, LeftoverPath: res.leftPath, LeftoverErr: res.leftErr}
	}
	onBytes(-n) // written again below
	again, err := src.Open(f.Src)
	if err != nil {
		return res, fmt.Errorf("couldn't replace %s in one step (%v), then couldn't read %s again (%w); it was left as it was",
			where(dst, target), renameErr, where(src, f.Src), cause(err))
	}
	defer again.Close()
	res, err = writeInPlace(now, after, again, dst, target, res, onTemp, onBytes)
	var pw *PartialWrite
	if errors.As(err, &pw) {
		pw.Err = fmt.Errorf("couldn't replace it in one step (%v), then %w", renameErr, pw.Err)
	} else if err != nil {
		err = fmt.Errorf("couldn't replace %s in one step (%v), then %w", where(dst, target), renameErr, err)
	}
	if err == nil {
		res.notice = fmt.Sprintf("couldn't replace %s in one step (%v); wrote it in place", where(dst, target), renameErr)
	}
	return res, err
}

// unixMode is m as the octal number chmod takes.
func unixMode(m fs.FileMode) uint32 {
	u := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		u |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		u |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		u |= 0o1000
	}
	return u
}

// stillOurs checks, before tmp is moved into place, that its name still
// leads to the file written through the handle: in a directory others may
// write, something else could have been put there meanwhile, and the
// rename would move that over the target. Here the kernel says whether it's
// the same file; on a host, it must at least be a regular file of the size
// written.
func stillOurs(dst FS, tmp string, written fs.FileInfo, n int64) error {
	changed := func(why string) error {
		return fmt.Errorf("the temporary file %s %s; not moving it into place", where(dst, tmp), why)
	}
	if isLocal(dst) && written != nil {
		fi, err := os.Lstat(tmp)
		if err != nil {
			return changed(fmt.Sprintf("can't be read back (%v)", cause(err)))
		}
		if !os.SameFile(written, fi) {
			return changed("is no longer the file that was written")
		}
		return nil
	}
	e, err := dst.Lstat(tmp)
	switch {
	case err != nil:
		return changed(fmt.Sprintf("can't be read back (%v)", cause(err)))
	case e.Link || !e.Mode.IsRegular():
		return changed("is no longer a regular file")
	case e.Size != n:
		return changed(fmt.Sprintf("holds %d bytes, not the %d written", e.Size, n))
	}
	return nil
}

// keepOwner gives the temporary file out the owner and group of old, the
// file it will replace, as far as it can: all of it as root, the group alone
// where it's one of ours. It's all done through out's handle, and read back
// from it. It says what replacing would still lose, or nil when nothing.
func keepOwner(dst FS, out io.Writer, tmp string, old Entry) (*ReplaceLoses, error) {
	var l ReplaceLoses
	if old.Nlink > 1 {
		l.Links = old.Nlink
	}
	readBack := func() (Entry, error) {
		fi, ok := statOf(out)
		if !ok {
			return Entry{}, errors.New("its handle can't be read")
		}
		return entryOf(tmp, fi), nil
	}
	t, terr := readBack()
	if terr == nil && old.HasOwner && t.HasOwner && (t.Uid != old.Uid || t.Gid != old.Gid) {
		if c, ok := out.(interface{ Chown(uid, gid int) error }); ok {
			if c.Chown(int(old.Uid), int(old.Gid)) != nil && t.Gid != old.Gid {
				_ = c.Chown(int(t.Uid), int(old.Gid))
			}
			t, terr = readBack() // what it has now, not what was asked
		}
	}
	switch {
	case !old.HasOwner:
		// Nothing to keep that we know of.
	case terr != nil:
		return nil, fmt.Errorf("can't read back the copy's owner at %s: %w", where(dst, tmp), terr)
	case t.HasOwner && (t.Uid != old.Uid || t.Gid != old.Gid):
		l.Owner = fmt.Sprintf("owner %d, group %d", old.Uid, old.Gid)
		l.NewOwner = fmt.Sprintf("owner %d, group %d", t.Uid, t.Gid)
	}
	if l.Owner == "" && l.Links == 0 {
		return nil, nil
	}
	// Who we are on that side, as far as it's known: this machine knows;
	// on a host, the new file's owner and group say it.
	id, idOK := identityOf(dst)
	if !idOK && terr == nil && t.HasOwner {
		id, idOK = identity{uid: t.Uid, gids: []uint32{t.Gid}}, true
	}
	ok, _ := mayWrite(old, id, idOK)
	l.InPlaceOK = ok
	return &l, nil
}

// writeInPlace empties target and writes in into it. Nothing here removes
// target: a failure part way leaves what was written, and says so. An open
// that's refused changed nothing (permission is checked before the file is
// emptied), and says that instead.
//
// One that hasn't started yet doesn't once after is done; one under way only
// stops early for now.
func writeInPlace(now, after context.Context, in io.Reader, dst FS, target string, res copied, onTemp func(string, bool), onBytes func(int64)) (copied, error) {
	res.inPlace = true
	if err := after.Err(); err != nil {
		return res, &Unchanged{Path: target, Where: where(dst, target), Err: err}
	}
	if onTemp != nil {
		onTemp(target, true)
		defer onTemp("", true)
	}
	out, err := dst.OpenWrite(target)
	if err != nil {
		return res, fmt.Errorf("can't write %s in place (%w); it was left as it was", where(dst, target), cause(err))
	}
	n, _, err := copyData(now, out, in, onBytes)
	res.n = n
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return res, &PartialWrite{Path: target, Where: where(dst, target), Err: cause(err)}
	}
	return res, nil
}

// copyData keeps pkg/sftp's pipelined fast paths (many requests in flight)
// while counting bytes: the SFTP file must be the side doing the copying, so
// the counting wrapper goes on the other side. wrote says a failure was in
// writing dst rather than reading src.
func copyData(ctx context.Context, dst io.Writer, src io.Reader, onBytes func(int64)) (n int64, wrote bool, err error) {
	cr := &ctxReader{ctx: ctx, r: src, on: onBytes}
	cw := &ctxWriter{ctx: ctx, w: dst, on: onBytes}
	switch {
	case isSFTP(src):
		// The SFTP file reads; an error not from our writer is its own.
		n, err = src.(*sftp.File).WriteTo(cw)
		return n, err != nil && cw.err != nil, err
	case isSFTP(dst):
		n, err = dst.(*sftp.File).ReadFrom(cr)
		return n, err != nil && cr.err == nil, err
	}
	cr.on = func(int64) {} // the writer counts
	n, err = io.Copy(cw, struct{ io.Reader }{cr})
	return n, err != nil && cw.err != nil, err
}

func isSFTP(x any) bool { _, ok := x.(*sftp.File); return ok }

type ctxReader struct {
	ctx context.Context
	r   io.Reader
	on  func(int64)
	err error // the reader's own failure, if any
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.on(int64(n))
	}
	if err != nil && err != io.EOF {
		c.err = err
	}
	return n, err
}

type ctxWriter struct {
	ctx context.Context
	w   io.Writer
	on  func(int64)
	err error // the writer's own failure, if any
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if n > 0 {
		c.on(int64(n))
	}
	if err != nil {
		c.err = err
	}
	return n, err
}

// RemoveAll deletes p on fsys, recursively for a directory. A symlink is
// removed itself, never followed. removed, if not nil, is told each path as
// it goes, so a delete that stops part way can say what it did.
func RemoveAll(ctx context.Context, fsys FS, p string, removed func(string)) error {
	_, err := removeTree(ctx, fsys, p, removed)
	return err
}

// removeTree is RemoveAll, saying whether p was gone already.
func removeTree(ctx context.Context, fsys FS, p string, removed func(string)) (gone bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e, err := fsys.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return false, removeEntry(ctx, fsys, p, e, removed)
}

func removeEntry(ctx context.Context, fsys FS, p string, e Entry, removed func(string)) error {
	if e.IsDir && !e.Link {
		children, err := fsys.ReadDir(p)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := RemoveAll(ctx, fsys, fsys.Join(p, c.Name), removed); err != nil {
				return err
			}
		}
	}
	if err := fsys.Remove(p); err != nil {
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			err = fmt.Errorf("%s: %w", p, err) // say which one, however deep
		}
		return err
	}
	if removed != nil {
		removed(p)
	}
	return nil
}

// HumanBytes formats a size as 1.2 MB style.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// PermError is CreateTemp saying the file was created but its permissions
// couldn't be set (some SFTP servers refuse setstat). The file it comes
// with is usable: the copy goes on and the person is told.
type PermError struct{ Err error }

func (e *PermError) Error() string { return "permissions not set: " + e.Err.Error() }
func (e *PermError) Unwrap() error { return e.Err }
