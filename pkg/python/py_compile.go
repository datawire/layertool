package python

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/datawire/dlib/dexec"

	"github.com/datawire/ocibuild/pkg/fsutil"
)

// A Compiler is a function that takes an source .py file, and emits 1 or more compiled .pyc files.
type Compiler func(context.Context, time.Time, fsutil.FileReference) (map[string]fsutil.FileReference, error)

// ExternalCompiler returns a `Compiler` that uses an external command to compile .py files to .pyc
// files.  It is designed for use with Python's "compileall" module.  It makes use of the "-p" flag,
// so the "py_compile" module is not appropriate.
//
// For example:
//
//     plat.Compile = ExternalCompiler("python3", "-m", "compileall")
func ExternalCompiler(cmdline ...string) (Compiler, error) {
	exe, err := dexec.LookPath(cmdline[0])
	if err != nil {
		return nil, err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, clampTime time.Time, in fsutil.FileReference) (compiled map[string]fsutil.FileReference, err error) {
		maybeSetErr := func(_err error) {
			if _err != nil && err == nil {
				err = _err
			}
		}

		// Set up the tmpdir
		tmpdir, err := os.MkdirTemp("", "ocibuild-pycompile.")
		if err != nil {
			return nil, err
		}
		defer func() {
			maybeSetErr(os.RemoveAll(tmpdir))
		}()

		// Get the input file
		inReader, err := in.Open()
		if err != nil {
			return nil, err
		}
		inBytes, err := io.ReadAll(inReader)
		if err != nil {
			_ = inReader.Close()
			return nil, err
		}
		if err := inReader.Close(); err != nil {
			return nil, err
		}

		// Write the input file to the tempdir
		filename := filepath.Join(tmpdir, path.Base(in.FullName()))
		if err := os.WriteFile(filename, inBytes, 0666); err != nil {
			return nil, err
		}
		if err := os.Chtimes(filename, in.ModTime(), in.ModTime()); err != nil {
			return nil, err
		}

		// Run the compiler
		cmd := dexec.CommandContext(ctx, exe, append(cmdline[1:],
			"-p", path.Join("/", path.Dir(in.FullName())), // prepend-dir for the in-.pyc filename
			in.Name(), // file to compile
		)...)
		cmd.Dir = tmpdir
		if !clampTime.IsZero() {
			cmd.Env = append(os.Environ(),
				"PYTHONHASHSEED=0",
				fmt.Sprintf("SOURCE_DATE_EPOCH=%d", clampTime.Unix()))
		}
		if err := cmd.Run(); err != nil {
			return nil, err
		}

		// Read in the output
		vfs := make(map[string]fsutil.FileReference)
		// vfs["slash-path"] and zipEntry.Name are slash-paths, so use fs.WalkDir instead of
		// filepath.Walk so that we don't need to worry about converting between forward and
		// backward slashes.
		dirFS := os.DirFS(tmpdir)
		err = fs.WalkDir(dirFS, ".", func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if p == "." {
				return nil
			}
			if !strings.HasSuffix(p, ".pyc") && !d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			var content []byte
			if !d.IsDir() {
				fh, err := dirFS.Open(p)
				if err != nil {
					return err
				}
				defer func() {
					_ = fh.Close()
				}()
				content, err = io.ReadAll(fh)
				if err != nil {
					return err
				}
			}
			fullname := path.Join(path.Dir(in.FullName()), p)
			vfs[fullname] = &fsutil.InMemFileReference{
				FileInfo:  info,
				MFullName: fullname,
				MContent:  content,
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return vfs, nil
	}, nil
}
