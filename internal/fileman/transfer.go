package fileman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/pkg/sftp"
)

// FileOp is one file to copy.
type FileOp struct {
	Src, Dst string
	Size     int64
	Mode     fs.FileMode
	Exists   bool // something is already at Dst
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
		_, statErr := dst.Lstat(d)
		p.Files = append(p.Files, FileOp{Src: s, Dst: d, Size: e.Size, Mode: e.Mode, Exists: statErr == nil})
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

// Progress is reported while a plan runs.
type Progress struct {
	File               string // current source path
	FileDone, FileSize int64
	Done, Total        int64 // bytes, whole plan
	Files, FilesTotal  int   // finished files
	Started            time.Time
	Dst                string // where the current file goes
	Overwriting        bool   // the current file replaces an existing one
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
	Bytes           int64
	Files           []string // where each copied file went, in order
	// Partial is a new file a failed or stopped copy had started; it is
	// removed, unless PartialErr says why it couldn't be.
	Partial    string
	PartialErr error
}

// PartlyWrittenError is a copy over an existing file that failed or was
// stopped part way: the file is still there, but may hold part of the new
// content.
type PartlyWrittenError struct {
	Path string
	Err  error
}

func (e *PartlyWrittenError) Error() string {
	return fmt.Sprintf("%s may be partly written: %v", e.Path, e.Err)
}
func (e *PartlyWrittenError) Unwrap() error { return e.Err }

// Run executes the plan. overwrite decides what happens to files that
// already exist (false: leave them). Cancelling ctx stops at once.
func Run(ctx context.Context, p Plan, overwrite bool, report func(Progress)) (Result, error) {
	return runPlan(ctx, ctx, p, overwrite, report)
}

// runPlan is Run with two ways to stop. Cancelling after stops a new file
// at once, removing what it wrote (it's ours), but lets a file being
// overwritten finish, then stops. Cancelling now stops at once whatever
// the file; one being overwritten is then left in place, partly written.
// An existing file is never removed.
func runPlan(now, after context.Context, p Plan, overwrite bool, report func(Progress)) (Result, error) {
	var res Result
	pr := Progress{Total: p.TotalBytes, FilesTotal: len(p.Files), Started: time.Now()}
	for _, d := range p.Dirs {
		if err := p.Dst.MkdirAll(d); err != nil {
			return res, fmt.Errorf("create %s: %w", d, err)
		}
	}
	last := time.Time{}
	tick := func(force bool) {
		if report != nil && (force || time.Since(last) > 100*time.Millisecond) {
			last = time.Now()
			report(pr)
		}
	}
	for _, f := range p.Files {
		if err := after.Err(); err != nil {
			return res, err
		}
		if f.Exists && !overwrite {
			res.Skipped++
			pr.Done += f.Size
			pr.Files++
			continue
		}
		_, statErr := p.Dst.Lstat(f.Dst)
		existed := !errors.Is(statErr, fs.ErrNotExist)
		ctx := after
		if existed {
			ctx = now
		}
		pr.File, pr.FileDone, pr.FileSize, pr.Dst, pr.Overwriting = f.Src, 0, f.Size, f.Dst, existed
		tick(true)
		n, partial, removeErr, err := copyFile(ctx, p.Src, p.Dst, f, existed, func(delta int64) {
			pr.FileDone += delta
			pr.Done += delta
			tick(false)
		})
		res.Bytes += n
		if partial {
			res.Partial, res.PartialErr = f.Dst, removeErr
		}
		if err != nil {
			return res, fmt.Errorf("%s: %w", f.Src, err)
		}
		pr.Files++
		res.Copied++
		res.Files = append(res.Files, f.Dst)
	}
	pr.File = ""
	tick(true)
	return res, nil
}

// copyFile writes f.Src over f.Dst in place, as it always has. If it fails
// or is stopped, a file this copy created is removed (partial says one was
// started; removeErr says why it couldn't be removed); a file that was
// already there is left, and the error says it may be partly written.
func copyFile(ctx context.Context, src, dst FS, f FileOp, existed bool, onBytes func(int64)) (n int64, partial bool, removeErr, err error) {
	in, err := src.Open(f.Src)
	if err != nil {
		return 0, false, nil, err
	}
	defer in.Close()
	out, err := dst.Create(f.Dst, f.Mode)
	if err != nil {
		return 0, false, nil, err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
		switch {
		case err == nil:
		case existed:
			err = &PartlyWrittenError{Path: f.Dst, Err: err}
		default:
			partial, removeErr = true, dst.Remove(f.Dst)
		}
	}()
	n, err = copyData(ctx, out, in, onBytes)
	return n, false, nil, err
}

// copyData keeps pkg/sftp's pipelined fast paths (many requests in flight)
// while counting bytes: the SFTP file must be the side doing the copying, so
// the counting wrapper goes on the other side.
func copyData(ctx context.Context, dst io.Writer, src io.Reader, onBytes func(int64)) (int64, error) {
	cr := &ctxReader{ctx: ctx, r: src, on: onBytes}
	cw := &ctxWriter{ctx: ctx, w: dst, on: onBytes}
	switch s := src.(type) {
	case *sftp.File:
		return s.WriteTo(cw)
	}
	if d, ok := dst.(*sftp.File); ok {
		return d.ReadFrom(cr)
	}
	return io.Copy(cw, struct{ io.Reader }{src})
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
	on  func(int64)
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.on(int64(n))
	}
	return n, err
}

type ctxWriter struct {
	ctx context.Context
	w   io.Writer
	on  func(int64)
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if n > 0 {
		c.on(int64(n))
	}
	return n, err
}

// RemoveAll deletes p on fsys, recursively for a directory. A symlink is
// removed itself, never followed.
func RemoveAll(ctx context.Context, fsys FS, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e, err := fsys.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if e.IsDir && !e.Link {
		children, err := fsys.ReadDir(p)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := RemoveAll(ctx, fsys, fsys.Join(p, c.Name)); err != nil {
				return err
			}
		}
	}
	return fsys.Remove(p)
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
