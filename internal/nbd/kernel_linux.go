//go:build linux

package nbd

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ioctls from <linux/nbd.h>: _IO(0xab, n).
const (
	ioctlSetSock       = 0xab00
	ioctlSetBlkSize    = 0xab01
	ioctlDoIt          = 0xab03
	ioctlClearSock     = 0xab04
	ioctlClearQue      = 0xab05
	ioctlSetSizeBlocks = 0xab07
	ioctlDisconnect    = 0xab08
	ioctlSetTimeout    = 0xab09
	ioctlSetFlags      = 0xab0a
)

func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// Attach connects the export to a /dev/nbdX device using the kernel NBD
// driver, the same way nbd-client does, and blocks until the device is
// disconnected (see Detach). It needs root and the nbd kernel module.
func Attach(device, addr, export string, timeout time.Duration) error {
	cl, err := Dial(addr, export)
	if err != nil {
		return err
	}
	defer cl.conn.Close()
	var sockFile *os.File
	switch c := cl.conn.(type) {
	case *net.TCPConn:
		sockFile, err = c.File()
	case *net.UnixConn:
		sockFile, err = c.File()
	default:
		err = fmt.Errorf("unsupported connection type %T", cl.conn)
	}
	if err != nil {
		return fmt.Errorf("get socket fd: %w", err)
	}
	defer sockFile.Close()
	if err := syscall.SetNonblock(int(sockFile.Fd()), false); err != nil {
		return err
	}

	dev, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer dev.Close()
	fd := dev.Fd()

	const blkSize = 4096
	if err := ioctl(fd, ioctlSetBlkSize, blkSize); err != nil {
		return fmt.Errorf("NBD_SET_BLKSIZE: %w", err)
	}
	if err := ioctl(fd, ioctlSetSizeBlocks, uintptr(cl.Size/blkSize)); err != nil {
		return fmt.Errorf("NBD_SET_SIZE_BLOCKS: %w", err)
	}
	_ = ioctl(fd, ioctlClearSock, 0)
	if err := ioctl(fd, ioctlSetFlags, uintptr(cl.Flags)); err != nil {
		return fmt.Errorf("NBD_SET_FLAGS: %w", err)
	}
	if timeout > 0 {
		if err := ioctl(fd, ioctlSetTimeout, uintptr(timeout/time.Second)); err != nil {
			return fmt.Errorf("NBD_SET_TIMEOUT: %w", err)
		}
	}
	if err := ioctl(fd, ioctlSetSock, sockFile.Fd()); err != nil {
		return fmt.Errorf("NBD_SET_SOCK: %w", err)
	}
	// NBD_DO_IT blocks in the kernel until the device is disconnected.
	err = ioctl(fd, ioctlDoIt, 0)
	_ = ioctl(fd, ioctlClearQue, 0)
	_ = ioctl(fd, ioctlClearSock, 0)
	if err != nil {
		return fmt.Errorf("NBD_DO_IT: %w", err)
	}
	return nil
}

// Detach disconnects a /dev/nbdX device that was attached with Attach or
// nbd-client.
func Detach(device string) error {
	dev, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer dev.Close()
	if err := ioctl(dev.Fd(), ioctlDisconnect, 0); err != nil {
		return fmt.Errorf("NBD_DISCONNECT: %w", err)
	}
	_ = ioctl(dev.Fd(), ioctlClearSock, 0)
	return nil
}

// Attached reports whether a /dev/nbdX device currently has a backend, by
// reading the sysfs "pid" attribute the driver exposes while connected.
func Attached(device string) bool {
	name := device
	if i := len("/dev/"); len(name) > i && name[:i] == "/dev/" {
		name = name[i:]
	}
	_, err := os.Stat("/sys/block/" + name + "/pid")
	return err == nil
}

var _ = unsafe.Pointer(nil)
