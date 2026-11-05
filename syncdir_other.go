//go:build !windows

package main

import "os"

// syncDir makes renames and new entries in a directory durable.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
