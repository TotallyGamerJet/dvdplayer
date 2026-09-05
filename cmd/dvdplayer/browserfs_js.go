// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build js

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"syscall/js"
	"time"
)

// The browser hands a directory over as a tree of handles rather than as a
// filesystem: there are no paths, only a root the viewer picked and a
// getFileHandle call to walk down from it. browserFS is that tree behind
// [io/fs], which is all dvdread wants.
//
// Everything the API offers is asynchronous, so every call here settles a
// promise before it returns. That is safe because the player reads the
// disc on its own goroutine, never in Ebitengine's Update or Draw: a
// goroutine waiting on a promise parks, the Go runtime hands the thread
// back to the browser's event loop, and the frame that is being drawn is
// not held up. Reading the disc from Update would deadlock.
type browserFS struct {
	root js.Value // a FileSystemDirectoryHandle
}

var (
	_ fs.FS        = browserFS{}
	_ fs.StatFS    = browserFS{}
	_ fs.ReadDirFS = browserFS{}
)

// await waits for a JS promise to settle. It must not be called from the
// goroutine that runs the frame loop; see the note on browserFS.
func await(promise js.Value) (js.Value, error) {
	type settled struct {
		value js.Value
		err   error
	}
	ch := make(chan settled, 1)

	onResolve := js.FuncOf(func(_ js.Value, args []js.Value) any {
		var v js.Value
		if len(args) > 0 {
			v = args[0]
		}
		ch <- settled{value: v}
		return nil
	})
	defer onResolve.Release()

	onReject := js.FuncOf(func(_ js.Value, args []js.Value) any {
		var v js.Value
		if len(args) > 0 {
			v = args[0]
		}
		ch <- settled{err: jsError(v)}
		return nil
	})
	defer onReject.Release()

	promise.Call("then", onResolve, onReject)
	s := <-ch
	return s.value, s.err
}

// jsError turns a rejected promise's reason into an error, keeping the
// DOMException name so that a lookup that missed can be told from one that
// was refused.
func jsError(v js.Value) error {
	if v.IsUndefined() || v.IsNull() {
		return errors.New("the browser refused the request")
	}
	name, message := v.Get("name"), v.Get("message")
	switch {
	case name.Type() == js.TypeString && message.Type() == js.TypeString:
		return fmt.Errorf("%s: %s", name.String(), message.String())
	case message.Type() == js.TypeString:
		return errors.New(message.String())
	}
	return errors.New(js.Global().Get("String").Invoke(v).String())
}

// notFound reports whether the browser said the name is not there, which is
// how a lookup that missed arrives. dvdread probes for files that a given
// disc need not have — the lowercase spelling of a directory, a title set
// that does not exist — so this has to be the ordinary [fs.ErrNotExist]
// rather than a failure.
func notFound(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "NotFoundError")
}

func pathError(op, name string, err error) error {
	if notFound(err) {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// dir walks down from the root to the directory handle for name, which is
// an [fs.FS] path or "." for the root itself.
func (b browserFS) dir(name string) (js.Value, error) {
	handle := b.root
	if name == "." {
		return handle, nil
	}
	for part := range strings.SplitSeq(name, "/") {
		next, err := await(handle.Call("getDirectoryHandle", part))
		if err != nil {
			return js.Undefined(), err
		}
		handle = next
	}
	return handle, nil
}

// split takes an [fs.FS] path apart into the directory to look in and the
// name to look for.
func split(name string) (dir, base string) {
	dir, base = path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	return dir, base
}

func (b browserFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &browserDir{name: name}, nil
	}
	dir, base := split(name)
	handle, err := b.dir(dir)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	fileHandle, err := await(handle.Call("getFileHandle", base))
	if err != nil {
		// Not a file: it may still be a directory, which dvdread opens
		// to list it.
		if _, derr := await(handle.Call("getDirectoryHandle", base)); derr == nil {
			return &browserDir{name: name}, nil
		}
		return nil, pathError("open", name, err)
	}
	file, err := await(fileHandle.Call("getFile"))
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return &browserFile{
		name: base,
		file: file,
		size: int64(file.Get("size").Float()),
	}, nil
}

func (b browserFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return browserInfo{name: ".", dir: true}, nil
	}
	dir, base := split(name)
	handle, err := b.dir(dir)
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	if _, err := await(handle.Call("getDirectoryHandle", base)); err == nil {
		return browserInfo{name: base, dir: true}, nil
	}
	fileHandle, err := await(handle.Call("getFileHandle", base))
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	file, err := await(fileHandle.Call("getFile"))
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return browserInfo{name: base, size: int64(file.Get("size").Float())}, nil
}

func (b browserFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	handle, err := b.dir(name)
	if err != nil {
		return nil, pathError("readdir", name, err)
	}
	// values() is an async iterator, so each turn is its own promise.
	it := handle.Call("values")
	var entries []fs.DirEntry
	for {
		step, err := await(it.Call("next"))
		if err != nil {
			return nil, pathError("readdir", name, err)
		}
		if step.Get("done").Bool() {
			break
		}
		v := step.Get("value")
		entries = append(entries, browserInfo{
			name: v.Get("name").String(),
			dir:  v.Get("kind").String() == "directory",
		})
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// errRefused is a file the browser has, and reports a size for, but will not
// hand the bytes of. An operating system does that to a scrambled disc it has
// decided not to let go of, and the browser reads through the same filesystem
// it does.
//
// It arrives either as a read that returns nothing, or as the browser saying
// NotReadableError, which is what Chromium makes of the refusal underneath.
//
// It is usually not the last word. The system withholds a scrambled disc
// until the drive has been through the CSS handshake, and lifts that once
// something has done it — after which the VOBs read as scrambled data, which
// is all this needs. Hence the remedy rather than an apology.
var errRefused = errors.New("the system is withholding this disc's data until the drive " +
	"has been authenticated: run dvdplay -unlock on the machine with the disc in it, " +
	"or open the disc once in a DVD player, then reload")

// withheld reports whether an error from the browser is the file being kept
// back rather than anything to do with dvdplay.
func withheld(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.HasPrefix(s, "NotReadableError") || strings.HasPrefix(s, "NotAllowedError")
}

// readError names what went wrong with a read, saying so plainly where the
// browser is simply refusing to part with the file.
func (f *browserFile) readError(err error) error {
	if withheld(err) {
		return &fs.PathError{Op: "read", Path: f.name, Err: fmt.Errorf("%w (%v)", errRefused, err)}
	}
	return &fs.PathError{Op: "read", Path: f.name, Err: err}
}

// browserChunk is how much of a file is fetched at a time to read
// sequentially through it.
//
// Every read costs a promise, and dvdread reads an IFO a field at a time —
// thousands of reads of a few bytes each — so serving those from a chunk
// already in hand is the difference between a disc that opens and one that
// grinds. It is not a cache: only the last chunk is kept, which is all a
// reader going forwards needs.
const browserChunk = 1 << 18 // 256 KiB

// browserFile is one file of the picked directory. Reads are served by
// slicing the underlying Blob, so a 4GB VOB costs nothing until the part
// being read is asked for.
type browserFile struct {
	name string
	file js.Value // a File
	size int64
	pos  int64

	// buf holds the bytes of the file from bufAt, the chunk the last read
	// came from.
	buf   []byte
	bufAt int64
}

var (
	_ fs.File     = (*browserFile)(nil)
	_ io.Seeker   = (*browserFile)(nil)
	_ io.ReaderAt = (*browserFile)(nil)
)

// Read serves the reader going forwards through the file, a chunk at a
// time. A read too big to be worth buffering goes straight to the Blob.
func (f *browserFile) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if len(p) >= browserChunk {
		n, err := f.ReadAt(p, f.pos)
		f.pos += int64(n)
		if err == io.EOF && n > 0 {
			// Hand back what there is and leave the end to the next
			// read: a reader that reports both at once is within its
			// rights, but it is the awkward shape for callers to get
			// right, and not every caller does.
			return n, nil
		}
		return n, err
	}
	if f.pos < f.bufAt || f.pos >= f.bufAt+int64(len(f.buf)) {
		if err := f.fill(f.pos); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.buf[f.pos-f.bufAt:])
	f.pos += int64(n)
	return n, nil
}

// fill fetches the chunk holding off.
func (f *browserFile) fill(off int64) error {
	end := min(off+browserChunk, f.size)
	buf, err := await(f.file.Call("slice", off, end).Call("arrayBuffer"))
	if err != nil {
		f.buf, f.bufAt = nil, 0
		return f.readError(err)
	}
	if cap(f.buf) < int(end-off) {
		f.buf = make([]byte, end-off)
	}
	n := js.CopyBytesToGo(f.buf[:end-off], js.Global().Get("Uint8Array").New(buf))
	// Keep only what arrived. A slice that comes back short of what was
	// asked for is a file that would not read, and the buffer behind it is
	// zeroes: handing those on would pass them off as the disc's own.
	f.buf, f.bufAt = f.buf[:n], off
	if n == 0 {
		return &fs.PathError{Op: "read", Path: f.name, Err: errRefused}
	}
	return nil
}

// ReadAt reads straight off the Blob, leaving the chunk Read holds alone.
func (f *browserFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.size {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), f.size)
	buf, err := await(f.file.Call("slice", off, end).Call("arrayBuffer"))
	if err != nil {
		return 0, f.readError(err)
	}
	n := js.CopyBytesToGo(p, js.Global().Get("Uint8Array").New(buf))
	if n == 0 && off < f.size {
		// Asked for bytes that are there and given none: the file is
		// being withheld rather than ended.
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: errRefused}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *browserFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.pos = offset
	case io.SeekCurrent:
		f.pos += offset
	case io.SeekEnd:
		f.pos = f.size + offset
	default:
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	if f.pos < 0 {
		f.pos = 0
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	return f.pos, nil
}

func (f *browserFile) Stat() (fs.FileInfo, error) {
	return browserInfo{name: f.name, size: f.size}, nil
}

func (f *browserFile) Close() error { return nil }

// browserDir stands in for a directory opened as a file, which dvdread does
// only to stat it.
type browserDir struct{ name string }

func (d *browserDir) Stat() (fs.FileInfo, error) {
	return browserInfo{name: path.Base(d.name), dir: true}, nil
}

func (d *browserDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *browserDir) Close() error { return nil }

// browserInfo is both the [fs.FileInfo] and the [fs.DirEntry] of one
// handle. The API reports no modification time, so it reads as the zero
// time, which nothing in dvdread looks at.
type browserInfo struct {
	name string
	size int64
	dir  bool
}

var (
	_ fs.FileInfo = browserInfo{}
	_ fs.DirEntry = browserInfo{}
)

func (i browserInfo) Name() string { return i.name }
func (i browserInfo) Size() int64  { return i.size }
func (i browserInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i browserInfo) ModTime() time.Time         { return time.Time{} }
func (i browserInfo) IsDir() bool                { return i.dir }
func (i browserInfo) Sys() any                   { return nil }
func (i browserInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i browserInfo) Info() (fs.FileInfo, error) { return i, nil }
