// Package snapshot reads a broadcast directory into the tree uploaded when a
// session starts, and diffs it against the server's copy on rejoin.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/biga-app/cli/internal/e2ee"
	"github.com/biga-app/cli/internal/linediff"
	"github.com/biga-app/cli/internal/secrets"
)

// MaxFileBytes is the largest file whose content is uploaded. Bigger files
// appear in the tree as placeholders, so a stray multi-gigabyte binary cannot
// exhaust memory or produce an unacceptably large upload.
const MaxFileBytes = 2 << 20

// File is a file's upload-ready content and identity.
type File struct {
	Size int64
	// Hash is the hex SHA-256 the server stores the content under.
	Hash string
	// Content is empty for binary and oversize files.
	Content string
	// Binary marks files whose bytes are never uploaded.
	Binary bool
	// Oversize marks files larger than [MaxFileBytes].
	Oversize bool
	// Withheld lists why the content was not uploaded: it looks like it holds
	// a secret. Withheld files are placeholders, like oversize ones.
	Withheld []secrets.Finding
	// Lines is how many lines a text file has.
	Lines int
	// Plain is the file's text, kept when the content is sealed so that line
	// counts can be worked out locally.
	Plain string
}

// ReadOptions adjusts how [ReadFile] treats a file.
type ReadOptions struct {
	// NoSecretScan shares files even when their content looks like a secret.
	NoSecretScan bool
	// Cipher seals text content and names every file by a keyed hash, for an
	// encrypted session. Nil leaves content readable and names it by SHA-256.
	Cipher *e2ee.Cipher
}

// idOf names content: its SHA-256, or a keyed hash for an encrypted session.
func (o ReadOptions) idOf(data []byte) string {
	if o.Cipher != nil {
		return o.Cipher.ID(data)
	}
	return hashBytes(data)
}

func ReadFile(root *os.Root, rel string, opts ReadOptions) (File, error) {
	name, err := filepath.Localize(rel)
	if err != nil {
		return File{}, err
	}
	var parent string
	for part := range strings.SplitSeq(filepath.Dir(name), string(filepath.Separator)) {
		parent = filepath.Join(parent, part)
		info, err := root.Lstat(parent)
		if err != nil {
			return File{}, err
		}
		if !info.IsDir() {
			return File{}, fs.ErrNotExist
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fs.ErrNotExist
	}
	f, err := root.Open(name)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fs.ErrNotExist
	}

	// Reading one byte past the limit detects oversize files without trusting
	// a size from stat that may be stale by the time the read happens.
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return File{}, err
	}
	if len(data) > MaxFileBytes {
		info, err := f.Stat()
		if err != nil {
			return File{}, err
		}
		return File{
			Size: info.Size(),
			// The content is unknown, so the hash only needs to change when
			// the size does; keyed by path so it never collides with a blob.
			Hash:     opts.idOf(fmt.Appendf(nil, "oversize:%d:%s", info.Size(), rel)),
			Binary:   true,
			Oversize: true,
		}, nil
	}

	if bytes.IndexByte(data, 0) >= 0 {
		return File{Size: int64(len(data)), Hash: opts.idOf(data), Binary: true}, nil
	}

	if !opts.NoSecretScan {
		if findings := secrets.Scan(data); len(findings) > 0 {
			return File{
				Size:     int64(len(data)),
				Hash:     opts.idOf(fmt.Appendf(nil, "withheld:%d:%s", len(data), rel)),
				Binary:   true,
				Withheld: findings,
			}, nil
		}
	}

	plain := string(data)
	file := File{Size: int64(len(data)), Hash: opts.idOf(data), Content: plain, Lines: linediff.Lines(plain)}
	if opts.Cipher != nil {
		file.Content, file.Plain = string(opts.Cipher.Seal(file.Hash, data)), plain
	}
	return file, nil
}

// EmptyHash is the hash of an empty file, which needs no uploaded content.
var EmptyHash = hashBytes(nil)

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
