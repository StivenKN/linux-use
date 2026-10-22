package wl

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// shm formats we know how to read. Values come from wl_shm.format.
const (
	FormatARGB8888 = 0
	FormatXRGB8888 = 1
	FormatABGR8888 = 0x34324241
	FormatXBGR8888 = 0x34324258
)

// ShmBuffer is a wl_buffer backed by an anonymous memfd mapped into our address space.
type ShmBuffer struct {
	c      *Conn
	ID     uint32
	pool   uint32
	fd     int
	Data   []byte
	Width  int
	Height int
	Stride int
	Format uint32
}

// NewShmBuffer allocates a buffer through wl_shm object shm.
func NewShmBuffer(c *Conn, shm uint32, width, height, stride int, format uint32) (*ShmBuffer, error) {
	size := stride * height
	fd, err := Memfd("linux-use-shm", nil, size)
	if err != nil {
		return nil, err
	}
	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mmap shm: %w", err)
	}
	b := &ShmBuffer{c: c, fd: fd, Data: data, Width: width, Height: height, Stride: stride, Format: format}
	b.pool = c.NewObject(nil)
	if err := c.Request(shm, 0).Uint(b.pool).FD(fd).Int(int32(size)).Send(); err != nil {
		b.Destroy()
		return nil, err
	}
	b.ID = c.NewObject(nil)
	err = c.Request(b.pool, 0).Uint(b.ID).Int(0).Int(int32(width)).Int(int32(height)).Int(int32(stride)).Uint(format).Send()
	if err != nil {
		b.Destroy()
		return nil, err
	}
	return b, nil
}

// Destroy releases the protocol objects and the mapping.
func (b *ShmBuffer) Destroy() {
	if b.ID != 0 {
		b.c.Request(b.ID, 0).Send() // wl_buffer.destroy
	}
	if b.pool != 0 {
		b.c.Request(b.pool, 1).Send() // wl_shm_pool.destroy
	}
	if b.Data != nil {
		unix.Munmap(b.Data)
	}
	unix.Close(b.fd)
}

// Memfd creates an anonymous file of size bytes, optionally pre-filled with content.
func Memfd(name string, content []byte, size int) (int, error) {
	fd, err := unix.MemfdCreate(name, unix.MFD_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("memfd_create: %w", err)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("ftruncate: %w", err)
	}
	if len(content) > 0 {
		if _, err := unix.Pwrite(fd, content, 0); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("write memfd: %w", err)
		}
	}
	return fd, nil
}
