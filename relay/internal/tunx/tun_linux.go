//go:build linux

package tunx

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ifreq is the classic Linux ioctl request block: an interface name followed by
// a union. TUNSETIFF reads the flags from the union; SIOCSIFMTU reads the MTU.
type ifreqFlags struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_     [24 - 2]byte
}

type ifreqMTU struct {
	Name [unix.IFNAMSIZ]byte
	MTU  int32
	_    [24 - 4]byte
}

// TUN is a Linux TUN device (/dev/net/tun, TUNSETIFF, IFF_TUN|IFF_NO_PI).
type TUN struct {
	f      *os.File
	name   string
	mtu    int
	mtuErr error
}

// Open creates (or attaches to) a TUN interface and configures its MTU.
//
// The device is left in non-blocking mode so the Go runtime's poller handles
// waiting; ReadPacket still blocks the calling goroutine, never a thread.
func Open(cfg Config) (Device, error) {
	name := cfg.Name
	if name == "" {
		name = DefaultName
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = MTU
	}
	if len(name) >= unix.IFNAMSIZ {
		return nil, fmt.Errorf("tunx: interface name %q too long", name)
	}

	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("tunx: open /dev/net/tun: %w", err)
	}

	var req ifreqFlags
	copy(req.Name[:], name)
	req.Flags = unix.IFF_TUN | unix.IFF_NO_PI
	if err := ioctl(fd, unix.TUNSETIFF, unsafe.Pointer(&req)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("tunx: TUNSETIFF %s: %w", name, err)
	}
	// The kernel may have picked a name (e.g. when name contained %d).
	actual := string(trimNul(req.Name[:]))

	var mreq ifreqMTU
	copy(mreq.Name[:], actual)
	mreq.MTU = int32(mtu)
	mtuErr := ioctl(fd, unix.SIOCSIFMTU, unsafe.Pointer(&mreq))

	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("tunx: set nonblock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "/dev/net/tun")
	t := &TUN{f: f, name: actual, mtu: mtu}
	if mtuErr != nil {
		// Not fatal: the caller may set it with `ip link set mtu`. Surface it so
		// the operator sees it in the log rather than silently running at 1500.
		t.mtuErr = mtuErr
	}
	return t, nil
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Name returns the interface name.
func (t *TUN) Name() string { return t.name }

// MTUError reports why the MTU could not be applied at open time, if it failed.
func (t *TUN) MTUError() error { return t.mtuErr }

// ReadPacket reads one IP packet.
func (t *TUN) ReadPacket(buf []byte) (int, error) {
	for {
		n, err := t.f.Read(buf)
		if err == nil {
			return n, nil
		}
		if err == os.ErrDeadlineExceeded {
			return 0, err
		}
		if pe, ok := err.(*os.PathError); ok && pe.Err == unix.EINTR {
			continue
		}
		return n, err
	}
}

// WritePacket writes one IP packet.
func (t *TUN) WritePacket(p []byte) (int, error) { return t.f.Write(p) }

// Close closes the device.
func (t *TUN) Close() error { return t.f.Close() }
