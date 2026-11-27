package engine

import "os"

// Win32 directory entries do not carry the file ID and change time.
func fileChangeTime(os.FileInfo) int64      { return 0 }
func captureVolume(*os.File) (uint64, bool) { return 0, false }
