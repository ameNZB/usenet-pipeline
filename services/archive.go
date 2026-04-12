package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
)

// GenerateRandomPassword creates a secure hex string of the given byte length.
func GenerateRandomPassword(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}

// EncryptWith7z creates a password-protected 7z archive with header encryption.
// This encrypts both file data AND filenames so the contents are completely opaque.
// SABnzbd and NZBGet both support automatic extraction when the password is
// provided via NZB <meta type="password"> header.
//
// The -mhe=on flag enables header encryption (filenames hidden).
// The -mx=0 flag stores without compression (files are already compressed video).
func EncryptWith7z(ctx context.Context, srcDir, destPath, password string) error {
	// 7z a -p<password> -mhe=on -mx=0 -mmt=on output.7z srcDir/*
	args := []string{
		"a",
		"-p" + password,
		"-mhe=on", // encrypt headers (hide filenames)
		"-mx=0",   // store only, no compression (faster, video is already compressed)
		"-mmt=on", // multi-threaded — uses all available cores for hashing/encryption
		destPath,
		filepath.Join(srcDir, "*"),
	}
	cmd := exec.CommandContext(ctx, "7z", args...)
	cmd.Dir = srcDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z: %v: %s", err, out)
	}
	return nil
}
