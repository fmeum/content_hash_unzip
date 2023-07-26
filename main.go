package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 && len(args) != 3 && len(args) != 4 {
		return fmt.Errorf("usage: <zip> [<hash> <dir> [<strip_prefix>]]")
	}

	zipFile := args[0]
	hash, err := hashZip(zipFile)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		f, err := os.Open(zipFile)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = checkZip(f)
		if err != nil {
			return err
		}
		fmt.Println(hash)
		return nil
	}

	expectedHash := args[1]
	if hash != expectedHash {
		return fmt.Errorf("got hash %s, expected %s", hash, expectedHash)
	}

	dir := args[2]
	var prefix string
	if len(args) == 4 {
		prefix = args[3]
	}
	return unzip(dir, zipFile, prefix)
}

func hashZip(zip string) (string, error) {
	return dirhash.HashZip(zip, dirhash.Hash1)
}

const (
	// MaxZipFile is the maximum size in bytes of a module zip file. The
	// go command will report an error if either the zip file or its extracted
	// content is larger than this.
	MaxZipFile = 500 << 20
)

// CheckedFiles reports whether a set of files satisfy the name and size
// constraints required by module zip files. The constraints are listed in the
// package documentation.
//
// Functions that produce this report may include slightly different sets of
// files. See documentation for CheckFiles, CheckDir, and CheckZip for details.
type CheckedFiles struct {
	// Valid is a list of file paths that should be included in a zip file.
	Valid []string

	// Omitted is a list of files that are ignored when creating a module zip
	// file, along with the reason each file is ignored.
	Omitted []FileError

	// Invalid is a list of files that should not be included in a module zip
	// file, along with the reason each file is invalid.
	Invalid []FileError

	// SizeError is non-nil if the total uncompressed size of the valid files
	// exceeds the module zip size limit or if the zip file itself exceeds the
	// limit.
	SizeError error
}

// Err returns an error if [CheckedFiles] does not describe a valid module zip
// file. [CheckedFiles.SizeError] is returned if that field is set.
// A [FileErrorList] is returned
// if there are one or more invalid files. Other errors may be returned in the
// future.
func (cf CheckedFiles) Err() error {
	if cf.SizeError != nil {
		return cf.SizeError
	}
	if len(cf.Invalid) > 0 {
		return FileErrorList(cf.Invalid)
	}
	return nil
}

type FileErrorList []FileError

func (el FileErrorList) Error() string {
	buf := &strings.Builder{}
	sep := ""
	for _, e := range el {
		buf.WriteString(sep)
		buf.WriteString(e.Error())
		sep = "\n"
	}
	return buf.String()
}

type FileError struct {
	Path string
	Err  error
}

func (e FileError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Err)
}

func (e FileError) Unwrap() error {
	return e.Err
}

// checkZip implements checkZip and also returns the *zip.Reader. This is
// used in unzip to avoid redundant I/O.
func checkZip(f *os.File) (*zip.Reader, error) {
	// Check the total file size.
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zipSize := info.Size()
	if zipSize > MaxZipFile {
		cf := CheckedFiles{SizeError: fmt.Errorf("zip file is too large (%d bytes; limit is %d bytes)", zipSize, MaxZipFile)}
		return nil, cf.Err()
	}

	// Check for valid file names, collisions.
	var cf CheckedFiles
	addError := func(zf *zip.File, err error) {
		cf.Invalid = append(cf.Invalid, FileError{Path: zf.Name, Err: err})
	}
	z, err := zip.NewReader(f, zipSize)
	if err != nil {
		return nil, err
	}
	collisions := make(collisionChecker)
	var size int64
	for _, zf := range z.File {
		name := zf.Name
		isDir := strings.HasSuffix(name, "/")
		if isDir {
			name = name[:len(name)-1]
		}
		if path.Clean(name) != name {
			addError(zf, fmt.Errorf("file path is not clean: %s", name))
			continue
		}
		if err := module.CheckFilePath(name); err != nil {
			addError(zf, err)
			continue
		}
		if err := collisions.check(name, isDir); err != nil {
			addError(zf, err)
			continue
		}
		if isDir {
			continue
		}
		sz := int64(zf.UncompressedSize64)
		if sz >= 0 && MaxZipFile-size >= sz {
			size += sz
		} else if cf.SizeError == nil {
			cf.SizeError = fmt.Errorf("total uncompressed size of module contents too large (max size is %d bytes)", MaxZipFile)
		}
		cf.Valid = append(cf.Valid, zf.Name)
	}

	return z, cf.Err()
}

// unzip extracts the contents of a module zip file to a directory.
//
// unzip checks all restrictions listed in the package documentation and returns
// an error if the zip archive is not valid. In some cases, files may be written
// to dir before an error is returned (for example, if a file's uncompressed
// size does not match its declared size).
//
// dir may or may not exist: unzip will create it and any missing parent
// directories if it doesn't exist. If dir exists, it must be empty.
func unzip(dir string, zipFile string, prefix string) (err error) {
	defer func() {
		if err != nil {
			err = &zipError{verb: "unzip", path: zipFile, err: err}
		}
	}()

	// Check that the directory is empty. Don't create it yet in case there's
	// an error reading the zip.
	if files, _ := os.ReadDir(dir); len(files) > 0 {
		return fmt.Errorf("target directory %v exists and is not empty", dir)
	}

	// Open the zip and check that it satisfies all restrictions.
	f, err := os.Open(zipFile)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := checkZip(f)
	if err != nil {
		return err
	}

	// unzip, enforcing sizes declared in the zip file.
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	prefixMatched := false
	for _, zf := range z.File {
		name := zf.Name
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(name, prefix+"/") {
				continue
			}
			prefixMatched = true
			name = strings.TrimPrefix(name, prefix+"/")
		}
		dst := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
			return err
		}
		// Mark all files as executable.
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return err
		}
		r, err := zf.Open()
		if err != nil {
			w.Close()
			return err
		}
		lr := &io.LimitedReader{R: r, N: int64(zf.UncompressedSize64) + 1}
		_, err = io.Copy(w, lr)
		r.Close()
		if err != nil {
			w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		if lr.N <= 0 {
			return fmt.Errorf("uncompressed size of file %s is larger than declared size (%d bytes)", zf.Name, zf.UncompressedSize64)
		}
	}

	if prefix != "" && !prefixMatched {
		return fmt.Errorf("no file matched prefix %q", prefix)
	}

	return nil
}

// collisionChecker finds case-insensitive name collisions and paths that
// are listed as both files and directories.
//
// The keys of this map are processed with strToFold. pathInfo has the original
// path for each folded path.
type collisionChecker map[string]pathInfo

type pathInfo struct {
	path  string
	isDir bool
}

func (cc collisionChecker) check(p string, isDir bool) error {
	fold := strToFold(p)
	if other, ok := cc[fold]; ok {
		if p != other.path {
			return fmt.Errorf("case-insensitive file name collision: %q and %q", other.path, p)
		}
		if isDir != other.isDir {
			return fmt.Errorf("entry %q is both a file and a directory", p)
		}
		if !isDir {
			return fmt.Errorf("multiple entries for file %q", p)
		}
		// It's not an error if check is called with the same directory multiple
		// times. check is called recursively on parent directories, so check
		// may be called on the same directory many times.
	} else {
		cc[fold] = pathInfo{path: p, isDir: isDir}
	}

	if parent := path.Dir(p); parent != "." {
		return cc.check(parent, true)
	}
	return nil
}

type zipError struct {
	verb, path string
	err        error
}

func (e *zipError) Error() string {
	if e.path == "" {
		return fmt.Sprintf("%s: %v", e.verb, e.err)
	} else {
		return fmt.Sprintf("%s %s: %v", e.verb, e.path, e.err)
	}
}

func (e *zipError) Unwrap() error {
	return e.err
}

// strToFold returns a string with the property that
//
//	strings.EqualFold(s, t) iff strToFold(s) == strToFold(t)
//
// This lets us test a large set of strings for fold-equivalent
// duplicates without making a quadratic number of calls
// to EqualFold. Note that strings.ToUpper and strings.ToLower
// do not have the desired property in some corner cases.
func strToFold(s string) string {
	// Fast path: all ASCII, no upper case.
	// Most paths look like this already.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf || 'A' <= c && c <= 'Z' {
			goto Slow
		}
	}
	return s

Slow:
	var buf bytes.Buffer
	for _, r := range s {
		// SimpleFold(x) cycles to the next equivalent rune > x
		// or wraps around to smaller values. Iterate until it wraps,
		// and we've found the minimum value.
		for {
			r0 := r
			r = unicode.SimpleFold(r0)
			if r <= r0 {
				break
			}
		}
		// Exception to allow fast path above: A-Z => a-z
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		buf.WriteRune(r)
	}
	return buf.String()
}
