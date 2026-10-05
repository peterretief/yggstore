package files

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

const KindFolder = "folder"

// PutFolder streams dir as a tar archive into the network. Paths inside the
// archive are relative to dir; symlinks and special files are skipped.
func PutFolder(ctx context.Context, c client.Client, dir string, online []peers.Peer, opts PutOptions) (manifest.Manifest, Challenges, string, error) {
	pr, pw := io.Pipe()
	var fileCount int
	var contentBytes int64
	go func() {
		pw.CloseWithError(writeTar(dir, pw, &fileCount, &contentBytes, opts.Log))
	}()
	m, chal, sum, err := PutReader(ctx, c, pr, filepath.Base(dir), online, opts)
	pr.CloseWithError(errors.New("upload stopped")) // unblocks the tar writer if Put failed early
	if err != nil {
		return m, chal, sum, err
	}
	m.Kind, m.FileCount, m.ContentBytes = KindFolder, fileCount, contentBytes
	return m, chal, sum, nil
}

func writeTar(dir string, w io.Writer, fileCount *int, contentBytes *int64, logf func(string, ...any)) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: filepath.ToSlash(rel) + "/",
				Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(rel),
				Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Size: info.Size()}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("%s changed size while being stored", rel)
			}
			*fileCount++
			*contentBytes += n
			return nil
		default:
			if logf != nil {
				logf("skipping %s: not a regular file or folder", rel)
			}
			return nil
		}
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Restore rebuilds the item described by m inside destDir and returns the
// path it was written to. Files and folders appear under their original name,
// with " (2)", " (3)", ... added if that name is taken. Nothing is visible at
// the final path until the restore has fully succeeded.
func Restore(ctx context.Context, c client.Client, m manifest.Manifest, destDir string, logf func(string, ...any)) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	target := freeName(filepath.Join(destDir, m.FileName))
	tmp := filepath.Join(destDir, ".restoring-"+m.FileID)
	os.RemoveAll(tmp)

	if m.Kind == KindFolder {
		if err := os.Mkdir(tmp, 0o755); err != nil {
			return "", err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(Get(ctx, c, m, pw, logf)) }()
		err := extractTar(pr, tmp)
		pr.CloseWithError(errors.New("extract stopped"))
		if err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
	} else {
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		err = Get(ctx, c, m, f, logf)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return target, nil
}

// ReadBack downloads the item and checks it hashes to wantSHA256.
func ReadBack(ctx context.Context, c client.Client, m manifest.Manifest, wantSHA256 string) error {
	h := sha256.New()
	if err := Get(ctx, c, m, h, nil); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("read-back hash mismatch")
	}
	return nil
}

func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			_, err = io.Copy(io.Discard, r) // drain padding so the writer side finishes cleanly
			return err
		}
		if err != nil {
			return err
		}
		name, err := safeRel(hdr.Name)
		if err != nil {
			return err
		}
		path := filepath.Join(dest, name)
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			os.Chtimes(path, hdr.ModTime, hdr.ModTime)
		default:
			// Only regular files and folders are ever written by PutFolder.
			continue
		}
	}
}

// safeRel rejects archive paths that would land outside the destination.
func safeRel(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSuffix(name, "/")))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return clean, nil
}

func freeName(path string) string {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 2; ; i++ {
		p := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}
