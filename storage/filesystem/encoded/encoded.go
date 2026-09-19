// Package encoded provides a billy filesystem that transforms regular file
// contents before they reach an underlying filesystem.
package encoded

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
)

// Codec transforms complete file contents. Decode should accept any legacy
// unencoded representation that callers intend to migrate in place.
type Codec interface {
	Encode(path string, plaintext []byte) ([]byte, error)
	Decode(path string, encoded []byte) ([]byte, error)
}

// Filesystem wraps a billy filesystem with whole-file content transforms.
// Plaintext for open files is buffered in memory and encoded when they close.
type Filesystem struct {
	base  billy.Filesystem
	codec Codec
	open  *openFiles
}

type openFiles struct {
	mu    sync.Mutex
	files map[string]*fileState
}

type fileState struct {
	mu    sync.Mutex
	data  []byte
	dirty bool
	refs  int
}

// New returns a filesystem that applies codec to every regular file.
func New(base billy.Filesystem, codec Codec) (*Filesystem, error) {
	if base == nil {
		return nil, errors.New("encoded filesystem requires an underlying filesystem")
	}
	if codec == nil {
		return nil, errors.New("encoded filesystem requires a codec")
	}
	return &Filesystem{base: base, codec: codec, open: &openFiles{files: make(map[string]*fileState)}}, nil
}

func (fs *Filesystem) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (fs *Filesystem) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

func (fs *Filesystem) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	underlyingFlag := os.O_RDONLY
	if writable {
		underlyingFlag = os.O_RDWR
	}
	underlyingFlag |= flag & (os.O_CREATE | os.O_EXCL)
	underlying, err := fs.base.OpenFile(filename, underlyingFlag, perm)
	if err != nil {
		return nil, err
	}

	key := fs.fileKey(underlying.Name())
	fs.open.mu.Lock()
	state := fs.open.files[key]
	if state != nil {
		state.refs++
	}
	fs.open.mu.Unlock()
	if state == nil {
		state = &fileState{refs: 1}
		if flag&os.O_TRUNC != 0 {
			state.dirty = true
		} else {
			raw, readErr := io.ReadAll(underlying)
			if readErr != nil {
				_ = underlying.Close()
				return nil, readErr
			}
			state.data, err = fs.codec.Decode(filename, raw)
			if err != nil {
				_ = underlying.Close()
				return nil, fmt.Errorf("decode %s: %w", filename, err)
			}
		}
		if flag&os.O_CREATE != 0 && len(state.data) == 0 {
			state.dirty = true
		}
		fs.open.mu.Lock()
		if existing := fs.open.files[key]; existing != nil {
			existing.refs++
			state = existing
		} else {
			fs.open.files[key] = state
		}
		fs.open.mu.Unlock()
	}
	file := &file{filesystem: fs, underlying: underlying, state: state, key: key, name: filename, writable: writable}
	if flag&os.O_APPEND != 0 {
		state.mu.Lock()
		file.offset = int64(len(state.data))
		state.mu.Unlock()
	}
	return file, nil
}

func (fs *Filesystem) fileKey(filename string) string {
	return fs.base.Join(fs.base.Root(), filename)
}

func (fs *Filesystem) Stat(filename string) (os.FileInfo, error) {
	info, err := fs.base.Stat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return info, err
	}
	size, err := fs.plaintextSize(filename)
	if err != nil {
		return nil, err
	}
	return fileInfo{FileInfo: info, size: size}, nil
}

func (fs *Filesystem) Lstat(filename string) (os.FileInfo, error) {
	info, err := fs.base.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return info, err
	}
	size, err := fs.plaintextSize(filename)
	if err != nil {
		return nil, err
	}
	return fileInfo{FileInfo: info, size: size}, nil
}

func (fs *Filesystem) plaintextSize(filename string) (int64, error) {
	file, err := fs.base.Open(filename)
	if err != nil {
		return 0, err
	}
	key := fs.fileKey(file.Name())
	fs.open.mu.Lock()
	state := fs.open.files[key]
	fs.open.mu.Unlock()
	if state != nil {
		state.mu.Lock()
		size := int64(len(state.data))
		state.mu.Unlock()
		return size, file.Close()
	}
	raw, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	plaintext, err := fs.codec.Decode(filename, raw)
	if err != nil {
		return 0, fmt.Errorf("decode %s: %w", filename, err)
	}
	return int64(len(plaintext)), nil
}

func (fs *Filesystem) Rename(oldpath, newpath string) error {
	return fs.base.Rename(oldpath, newpath)
}

func (fs *Filesystem) Remove(filename string) error {
	return fs.base.Remove(filename)
}

func (fs *Filesystem) Join(elem ...string) string {
	return fs.base.Join(elem...)
}

func (fs *Filesystem) TempFile(dir, prefix string) (billy.File, error) {
	underlying, err := fs.base.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	name := underlying.Name()
	key := fs.fileKey(name)
	state := &fileState{dirty: true, refs: 1}
	fs.open.mu.Lock()
	fs.open.files[key] = state
	fs.open.mu.Unlock()
	return &file{filesystem: fs, underlying: underlying, state: state, key: key, name: name, writable: true}, nil
}

func (fs *Filesystem) ReadDir(path string) ([]os.FileInfo, error) {
	entries, err := fs.base.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for index, entry := range entries {
		if !entry.Mode().IsRegular() {
			continue
		}
		size, sizeErr := fs.plaintextSize(fs.Join(path, entry.Name()))
		if sizeErr != nil {
			return nil, sizeErr
		}
		entries[index] = fileInfo{FileInfo: entry, size: size}
	}
	return entries, nil
}

func (fs *Filesystem) MkdirAll(filename string, perm os.FileMode) error {
	return fs.base.MkdirAll(filename, perm)
}

func (fs *Filesystem) Symlink(target, link string) error {
	return fs.base.Symlink(target, link)
}

func (fs *Filesystem) Readlink(link string) (string, error) {
	return fs.base.Readlink(link)
}

func (fs *Filesystem) Chroot(path string) (billy.Filesystem, error) {
	base, err := fs.base.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &Filesystem{base: base, codec: fs.codec, open: fs.open}, nil
}

func (fs *Filesystem) Root() string {
	return fs.base.Root()
}

type fileInfo struct {
	os.FileInfo
	size int64
}

func (info fileInfo) Size() int64 { return info.size }

type file struct {
	mu         sync.Mutex
	filesystem *Filesystem
	underlying billy.File
	state      *fileState
	key        string
	name       string
	offset     int64
	writable   bool
	closed     bool
}

func (file *file) Name() string { return file.underlying.Name() }

func (file *file) Read(buffer []byte) (int, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	if file.offset >= int64(len(file.state.data)) {
		return 0, io.EOF
	}
	read := copy(buffer, file.state.data[file.offset:])
	file.offset += int64(read)
	return read, nil
}

func (file *file) ReadAt(buffer []byte, offset int64) (int, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	if offset < 0 {
		return 0, errors.New("negative read offset")
	}
	if offset >= int64(len(file.state.data)) {
		return 0, io.EOF
	}
	read := copy(buffer, file.state.data[offset:])
	if read < len(buffer) {
		return read, io.EOF
	}
	return read, nil
}

func (file *file) Write(data []byte) (int, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	if !file.writable {
		return 0, errors.New("file is not open for writing")
	}
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	end := file.offset + int64(len(data))
	if end > int64(len(file.state.data)) {
		file.state.data = append(file.state.data, make([]byte, end-int64(len(file.state.data)))...)
	}
	copy(file.state.data[file.offset:end], data)
	file.offset = end
	file.state.dirty = true
	return len(data), nil
}

func (file *file) Seek(offset int64, whence int) (int64, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return 0, os.ErrClosed
	}
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = file.offset + offset
	case io.SeekEnd:
		next = int64(len(file.state.data)) + offset
	default:
		return 0, errors.New("invalid seek whence")
	}
	if next < 0 {
		return 0, errors.New("negative seek offset")
	}
	file.offset = next
	return next, nil
}

func (file *file) Truncate(size int64) error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return os.ErrClosed
	}
	if !file.writable {
		return errors.New("file is not open for writing")
	}
	file.state.mu.Lock()
	defer file.state.mu.Unlock()
	if size < 0 {
		return errors.New("negative truncate size")
	}
	if size <= int64(len(file.state.data)) {
		file.state.data = file.state.data[:size]
	} else {
		file.state.data = append(file.state.data, make([]byte, size-int64(len(file.state.data)))...)
	}
	file.state.dirty = true
	return nil
}

func (file *file) Lock() error {
	return file.underlying.Lock()
}

func (file *file) Unlock() error {
	return file.underlying.Unlock()
}

func (file *file) Close() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return os.ErrClosed
	}
	file.state.mu.Lock()
	var flushErr error
	if file.writable && file.state.dirty {
		encoded, err := file.filesystem.codec.Encode(file.name, append([]byte(nil), file.state.data...))
		if err != nil {
			flushErr = fmt.Errorf("encode %s: %w", file.name, err)
		} else if err := file.underlying.Truncate(0); err != nil {
			flushErr = err
		} else if _, err := file.underlying.Seek(0, io.SeekStart); err != nil {
			flushErr = err
		} else if _, err := file.underlying.Write(encoded); err != nil {
			flushErr = err
		} else {
			file.state.dirty = false
		}
	}
	file.state.mu.Unlock()
	file.closed = true
	closeErr := file.underlying.Close()
	file.filesystem.open.mu.Lock()
	file.state.refs--
	if file.state.refs == 0 {
		delete(file.filesystem.open.files, file.key)
	}
	file.filesystem.open.mu.Unlock()
	return errors.Join(flushErr, closeErr)
}
