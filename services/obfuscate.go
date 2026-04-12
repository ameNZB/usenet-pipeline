package services

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ObfuscateFiles copies all files from src into dstDir with randomized
// filenames, preserving the original extension. src can be a file or directory.
func ObfuscateFiles(ctx context.Context, src, dstDir string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		ext := filepath.Ext(info.Name())
		return copyFile(src, filepath.Join(dstDir, GenerateRandomPassword(12)+ext))
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		ext := filepath.Ext(fi.Name())
		obfName := GenerateRandomPassword(12) + ext
		dst := filepath.Join(dstDir, obfName)

		return copyFile(path, dst)
	})
}

// CopyFiles stages files from src into dstDir preserving original filenames
// and directory structure. Prefers hardlinks (zero I/O) and falls back to a
// full copy when hardlinking fails (e.g. cross-device mounts in Docker).
func CopyFiles(ctx context.Context, src, dstDir string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return linkOrCopy(src, filepath.Join(dstDir, info.Name()))
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, _ := filepath.Rel(src, path)
		dst := filepath.Join(dstDir, rel)
		os.MkdirAll(filepath.Dir(dst), 0755)
		return linkOrCopy(path, dst)
	})
}

// linkOrCopy tries os.Link first (instant, zero I/O); falls back to copyFile
// when src and dst are on different devices or the filesystem doesn't support
// hardlinks.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// SanitizeBaseName turns a title string into a safe filename base for PAR2
// and other outputs. Removes characters that are illegal on common filesystems.
func SanitizeBaseName(title string) string {
	// Replace filesystem-unsafe characters with underscore.
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	name := replacer.Replace(strings.TrimSpace(title))
	if name == "" {
		return GenerateRandomPassword(12)
	}
	// Truncate to a reasonable length for filesystem safety.
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// copyBufSize is used for file copies. 256 KB cuts syscall overhead ~8x
// vs. the default 32 KB io.Copy buffer for large media files.
const copyBufSize = 256 * 1024

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	// Pre-allocate so the filesystem can lay out contiguous blocks.
	if info, err := in.Stat(); err == nil && info.Size() > 0 {
		out.Truncate(info.Size())
	}

	buf := make([]byte, copyBufSize)
	_, err = io.CopyBuffer(out, in, buf)
	return err
}
