//go:build windows

package backend

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// IOCTL_DISK_GET_LENGTH_INFO returns the size of a disk or volume opened
// as \\.\PhysicalDriveN or \\.\X:.
const ioctlDiskGetLengthInfo = 0x0007405C

func blockDeviceSize(f *os.File) (int64, error) {
	var length int64
	var returned uint32
	err := syscall.DeviceIoControl(syscall.Handle(f.Fd()), ioctlDiskGetLengthInfo,
		nil, 0, (*byte)(unsafe.Pointer(&length)), uint32(unsafe.Sizeof(length)), &returned, nil)
	if err != nil {
		return 0, fmt.Errorf("IOCTL_DISK_GET_LENGTH_INFO: %w", err)
	}
	if returned < uint32(unsafe.Sizeof(length)) {
		return 0, fmt.Errorf("IOCTL_DISK_GET_LENGTH_INFO returned %d bytes", returned)
	}
	return length, nil
}
