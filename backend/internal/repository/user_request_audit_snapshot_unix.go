//go:build !windows

package repository

import "os"

func openAuditSnapshot(path string) (*os.File, error) { return os.Open(path) }
