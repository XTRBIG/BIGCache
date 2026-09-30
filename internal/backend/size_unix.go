//go:build !windows

package backend

import (
	"io"
	"os"
)

// blockDeviceSize seeks to the end of the device, which works for block
// devices on Linux, macOS and the BSDs.
func blockDeviceSize(f *os.File) (int64, error) {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}
