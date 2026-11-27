//go:build !unix

package engine

import "os"

func verifySafeDirUnix(_ string, _ os.FileInfo) error { return nil }
func acquireWorktreeLock(_ *os.File) error            { return nil }
func releaseWorktreeLock(_ *os.File)                  {}
