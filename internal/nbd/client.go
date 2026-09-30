package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
)

// Client is a minimal synchronous NBD client. It is used by the tests and
// by the Linux attach helper (which negotiates in user space and then hands
// the socket to the kernel).
type Client struct {
	conn   net.Conn
	Size   uint64
	Flags  uint16
	mu     sync.Mutex
	handle uint64
}

// DialAddr connects to addr ("host:port" or "unix:/path").
func DialAddr(addr string) (net.Conn, error) {
	if strings.HasPrefix(addr, "unix:") {
		return net.Dial("unix", strings.TrimPrefix(addr, "unix:"))
	}
	return net.Dial("tcp", addr)
}

// Dial connects and negotiates the export with NBD_OPT_GO.
func Dial(addr, export string) (*Client, error) {
	conn, err := DialAddr(addr)
	if err != nil {
		return nil, err
	}
	c, err := Negotiate(conn, export)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// List returns the export names offered by the server.
func List(addr string) ([]string, error) {
	conn, err := DialAddr(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := clientHandshake(conn); err != nil {
		return nil, err
	}
	if err := sendOption(conn, optList, nil); err != nil {
		return nil, err
	}
	var names []string
	for {
		typ, data, err := readOptReply(conn, optList)
		if err != nil {
			return nil, err
		}
		switch typ {
		case repServer:
			if len(data) < 4 {
				return nil, errors.New("short NBD_REP_SERVER")
			}
			n := binary.BigEndian.Uint32(data)
			names = append(names, string(data[4:4+n]))
		case repAck:
			_ = sendOption(conn, optAbort, nil)
			return names, nil
		default:
			return nil, fmt.Errorf("server rejected NBD_OPT_LIST (reply %#x)", typ)
		}
	}
}

func clientHandshake(conn net.Conn) error {
	var hdr [18]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint64(hdr[0:]) != nbdMagic || binary.BigEndian.Uint64(hdr[8:]) != iHaveOpt {
		return errors.New("not a newstyle NBD server")
	}
	if binary.BigEndian.Uint16(hdr[16:])&flagFixedNewstyle == 0 {
		return errors.New("server does not support fixed newstyle negotiation")
	}
	var cf [4]byte
	binary.BigEndian.PutUint32(cf[:], clientFlagFixedNewstyle|clientFlagNoZeroes)
	_, err := conn.Write(cf[:])
	return err
}

func sendOption(conn net.Conn, opt uint32, data []byte) error {
	b := make([]byte, 16+len(data))
	binary.BigEndian.PutUint64(b[0:], iHaveOpt)
	binary.BigEndian.PutUint32(b[8:], opt)
	binary.BigEndian.PutUint32(b[12:], uint32(len(data)))
	copy(b[16:], data)
	_, err := conn.Write(b)
	return err
}

func readOptReply(conn net.Conn, opt uint32) (uint32, []byte, error) {
	var h [20]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return 0, nil, err
	}
	if binary.BigEndian.Uint64(h[0:]) != optReplyMagic {
		return 0, nil, errors.New("bad option reply magic")
	}
	if got := binary.BigEndian.Uint32(h[8:]); got != opt {
		return 0, nil, fmt.Errorf("reply for option %d, expected %d", got, opt)
	}
	typ := binary.BigEndian.Uint32(h[12:])
	n := binary.BigEndian.Uint32(h[16:])
	if n > 1<<16 {
		return 0, nil, errors.New("option reply too long")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, nil, err
	}
	return typ, data, nil
}

// Negotiate performs the handshake and NBD_OPT_GO on an open connection.
func Negotiate(conn net.Conn, export string) (*Client, error) {
	if err := clientHandshake(conn); err != nil {
		return nil, err
	}
	data := make([]byte, 4+len(export)+2)
	binary.BigEndian.PutUint32(data, uint32(len(export)))
	copy(data[4:], export)
	if err := sendOption(conn, optGo, data); err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	gotExport := false
	for {
		typ, data, err := readOptReply(conn, optGo)
		if err != nil {
			return nil, err
		}
		switch typ {
		case repInfo:
			if len(data) >= 12 && binary.BigEndian.Uint16(data) == infoExport {
				c.Size = binary.BigEndian.Uint64(data[2:])
				c.Flags = binary.BigEndian.Uint16(data[10:])
				gotExport = true
			}
		case repAck:
			if !gotExport {
				return nil, errors.New("server sent no NBD_INFO_EXPORT")
			}
			return c, nil
		case repErrUnknown:
			return nil, fmt.Errorf("unknown export %q", export)
		default:
			return nil, fmt.Errorf("server rejected NBD_OPT_GO (reply %#x): %s", typ, data)
		}
	}
}

// Conn returns the underlying connection (in transmission phase).
func (c *Client) Conn() net.Conn { return c.conn }

func (c *Client) do(typ, flags uint16, off uint64, length uint32, payload []byte, want int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handle++
	h := c.handle
	var hdr [28]byte
	binary.BigEndian.PutUint32(hdr[0:], requestMagic)
	binary.BigEndian.PutUint16(hdr[4:], flags)
	binary.BigEndian.PutUint16(hdr[6:], typ)
	binary.BigEndian.PutUint64(hdr[8:], h)
	binary.BigEndian.PutUint64(hdr[16:], off)
	binary.BigEndian.PutUint32(hdr[24:], length)
	if _, err := c.conn.Write(hdr[:]); err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return nil, err
		}
	}
	if typ == cmdDisc {
		return nil, nil
	}
	var rh [16]byte
	if _, err := io.ReadFull(c.conn, rh[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(rh[0:]) != simpleReplyMagic {
		return nil, errors.New("bad reply magic")
	}
	if got := binary.BigEndian.Uint64(rh[8:]); got != h {
		return nil, fmt.Errorf("reply handle %d, expected %d", got, h)
	}
	if errno := binary.BigEndian.Uint32(rh[4:]); errno != 0 {
		return nil, syscall.Errno(errno)
	}
	if want > 0 {
		buf := make([]byte, want)
		if _, err := io.ReadFull(c.conn, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return nil, nil
}

// Read returns length bytes at off.
func (c *Client) Read(off uint64, length uint32) ([]byte, error) {
	return c.do(cmdRead, 0, off, length, nil, int(length))
}

// Write writes data at off; fua requests durability before the reply.
func (c *Client) Write(off uint64, data []byte, fua bool) error {
	var flags uint16
	if fua {
		flags = cmdFlagFUA
	}
	_, err := c.do(cmdWrite, flags, off, uint32(len(data)), data, 0)
	return err
}

// WriteZeroes zeroes length bytes at off.
func (c *Client) WriteZeroes(off uint64, length uint32) error {
	_, err := c.do(cmdWriteZeroes, 0, off, length, nil, 0)
	return err
}

// Flush issues NBD_CMD_FLUSH.
func (c *Client) Flush() error {
	_, err := c.do(cmdFlush, 0, 0, 0, nil, 0)
	return err
}

// Close sends NBD_CMD_DISC and closes the connection.
func (c *Client) Close() error {
	_, _ = c.do(cmdDisc, 0, 0, 0, nil, 0)
	return c.conn.Close()
}
