package main

import "syscall"

// syncDir makes renames and new entries in a directory durable. FlushFileBuffers needs a handle with
// write access, which os.Open never asks for ("Access is denied"); FILE_FLAG_BACKUP_SEMANTICS opens directories.
func syncDir(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	return syscall.FlushFileBuffers(h)
}
