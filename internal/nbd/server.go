package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
)

// Export is a block device served over NBD.
type Export interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	// Sync makes completed writes durable (NBD_CMD_FLUSH / FUA).
	Sync() error
	ReadOnly() bool
}

// Server serves one or more exports.
type Server struct {
	log *log.Logger
	// PreferredBlockSize is advertised to clients (0 = 4096).
	PreferredBlockSize uint32
	// Workers bounds the number of in-flight requests per connection.
	Workers int

	mu      sync.RWMutex
	exports map[string]Export
	lns     map[net.Listener]struct{}
	conns   map[net.Conn]struct{}
	closed  bool
	wg      sync.WaitGroup
}

// NewServer returns an empty server.
func NewServer(logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		log:     logger,
		exports: map[string]Export{},
		lns:     map[net.Listener]struct{}{},
		conns:   map[net.Conn]struct{}{},
		Workers: 16,
	}
}

// AddExport registers an export under name.
func (s *Server) AddExport(name string, e Export) {
	s.mu.Lock()
	s.exports[name] = e
	s.mu.Unlock()
}

// ExportNames lists the registered exports.
func (s *Server) ExportNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.exports))
	for n := range s.exports {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Listen opens a listener for addr. "unix:/path" selects a unix socket,
// anything else is a TCP host:port.
func Listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix:") {
		return net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
	}
	return net.Listen("tcp", addr)
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return errors.New("nbd: server closed")
	}
	s.lns[ln] = struct{}{}
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// Close stops all listeners, closes all connections and waits for the
// handlers to finish.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for ln := range s.lns {
		ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) export(name string) Export {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exports[name]
}

func (s *Server) txFlags(e Export) uint16 {
	f := uint16(TxHasFlags | TxSendFlush | TxSendFUA | TxSendWriteZeroes | TxCanMultiConn)
	if e.ReadOnly() {
		f |= TxReadOnly
	}
	return f
}

// ---- negotiation ----

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	name, e, noZeroes, err := s.negotiate(conn)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, errAborted) {
			s.log.Printf("nbd: %s: negotiation failed: %v", conn.RemoteAddr(), err)
		}
		return
	}
	_ = noZeroes
	s.log.Printf("nbd: %s: attached to export %q", conn.RemoteAddr(), name)
	err = s.transmit(conn, name, e)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.log.Printf("nbd: %s: export %q: %v", conn.RemoteAddr(), name, err)
	}
	s.log.Printf("nbd: %s: detached from export %q", conn.RemoteAddr(), name)
}

var errAborted = errors.New("client aborted negotiation")

func (s *Server) negotiate(conn net.Conn) (string, Export, bool, error) {
	var hdr [18]byte
	binary.BigEndian.PutUint64(hdr[0:], nbdMagic)
	binary.BigEndian.PutUint64(hdr[8:], iHaveOpt)
	binary.BigEndian.PutUint16(hdr[16:], flagFixedNewstyle|flagNoZeroes)
	if _, err := conn.Write(hdr[:]); err != nil {
		return "", nil, false, err
	}
	var cf [4]byte
	if _, err := io.ReadFull(conn, cf[:]); err != nil {
		return "", nil, false, err
	}
	clientFlags := binary.BigEndian.Uint32(cf[:])
	if clientFlags&clientFlagFixedNewstyle == 0 {
		return "", nil, false, errors.New("client does not support fixed newstyle negotiation")
	}
	noZeroes := clientFlags&clientFlagNoZeroes != 0

	for {
		var oh [16]byte
		if _, err := io.ReadFull(conn, oh[:]); err != nil {
			return "", nil, false, err
		}
		if binary.BigEndian.Uint64(oh[0:]) != iHaveOpt {
			return "", nil, false, errors.New("bad option magic")
		}
		opt := binary.BigEndian.Uint32(oh[8:])
		length := binary.BigEndian.Uint32(oh[12:])
		if length > 1<<16 {
			return "", nil, false, fmt.Errorf("option %d too long (%d bytes)", opt, length)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(conn, data); err != nil {
			return "", nil, false, err
		}
		switch opt {
		case optExportName:
			name := string(data)
			e := s.export(name)
			if e == nil {
				// The protocol offers no error reply for EXPORT_NAME.
				return "", nil, false, fmt.Errorf("unknown export %q", name)
			}
			reply := make([]byte, 10, 134)
			binary.BigEndian.PutUint64(reply[0:], uint64(e.Size()))
			binary.BigEndian.PutUint16(reply[8:], s.txFlags(e))
			if !noZeroes {
				reply = reply[:134]
			}
			if _, err := conn.Write(reply); err != nil {
				return "", nil, false, err
			}
			return name, e, noZeroes, nil

		case optAbort:
			_ = writeOptReply(conn, opt, repAck, nil)
			return "", nil, false, errAborted

		case optList:
			for _, n := range s.ExportNames() {
				b := make([]byte, 4+len(n))
				binary.BigEndian.PutUint32(b, uint32(len(n)))
				copy(b[4:], n)
				if err := writeOptReply(conn, opt, repServer, b); err != nil {
					return "", nil, false, err
				}
			}
			if err := writeOptReply(conn, opt, repAck, nil); err != nil {
				return "", nil, false, err
			}

		case optInfo, optGo:
			if len(data) < 6 {
				if err := writeOptReply(conn, opt, repErrInvalid, nil); err != nil {
					return "", nil, false, err
				}
				continue
			}
			nlen := binary.BigEndian.Uint32(data)
			if int(nlen) > len(data)-6 {
				if err := writeOptReply(conn, opt, repErrInvalid, nil); err != nil {
					return "", nil, false, err
				}
				continue
			}
			name := string(data[4 : 4+nlen])
			e := s.export(name)
			if e == nil {
				msg := []byte(fmt.Sprintf("unknown export %q", name))
				if err := writeOptReply(conn, opt, repErrUnknown, msg); err != nil {
					return "", nil, false, err
				}
				continue
			}
			// NBD_INFO_EXPORT (mandatory).
			b := make([]byte, 12)
			binary.BigEndian.PutUint16(b[0:], infoExport)
			binary.BigEndian.PutUint64(b[2:], uint64(e.Size()))
			binary.BigEndian.PutUint16(b[10:], s.txFlags(e))
			if err := writeOptReply(conn, opt, repInfo, b); err != nil {
				return "", nil, false, err
			}
			// NBD_INFO_BLOCK_SIZE.
			pref := s.PreferredBlockSize
			if pref == 0 {
				pref = 4096
			}
			b = make([]byte, 14)
			binary.BigEndian.PutUint16(b[0:], infoBlockSize)
			binary.BigEndian.PutUint32(b[2:], 1)
			binary.BigEndian.PutUint32(b[6:], pref)
			binary.BigEndian.PutUint32(b[10:], MaxRequest)
			if err := writeOptReply(conn, opt, repInfo, b); err != nil {
				return "", nil, false, err
			}
			if err := writeOptReply(conn, opt, repAck, nil); err != nil {
				return "", nil, false, err
			}
			if opt == optGo {
				return name, e, noZeroes, nil
			}

		default:
			if err := writeOptReply(conn, opt, repErrUnsup, nil); err != nil {
				return "", nil, false, err
			}
		}
	}
}

func writeOptReply(w io.Writer, opt, typ uint32, data []byte) error {
	b := make([]byte, 20+len(data))
	binary.BigEndian.PutUint64(b[0:], optReplyMagic)
	binary.BigEndian.PutUint32(b[8:], opt)
	binary.BigEndian.PutUint32(b[12:], typ)
	binary.BigEndian.PutUint32(b[16:], uint32(len(data)))
	copy(b[20:], data)
	_, err := w.Write(b)
	return err
}

// ---- transmission ----

type request struct {
	flags  uint16
	typ    uint16
	handle uint64
	offset uint64
	length uint32
	data   []byte
}

func (s *Server) transmit(conn net.Conn, name string, e Export) error {
	var wmu sync.Mutex
	reply := func(handle uint64, errno uint32, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		var h [16]byte
		binary.BigEndian.PutUint32(h[0:], simpleReplyMagic)
		binary.BigEndian.PutUint32(h[4:], errno)
		binary.BigEndian.PutUint64(h[8:], handle)
		if _, err := conn.Write(h[:]); err != nil {
			return err
		}
		if len(data) > 0 {
			if _, err := conn.Write(data); err != nil {
				return err
			}
		}
		return nil
	}

	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			conn.Close()
		}
		errMu.Unlock()
	}
	defer wg.Wait()

	size := uint64(e.Size())
	var hdr [28]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			errMu.Lock()
			fe := firstErr
			errMu.Unlock()
			if fe != nil {
				return fe
			}
			return err
		}
		if binary.BigEndian.Uint32(hdr[0:]) != requestMagic {
			return errors.New("bad request magic")
		}
		req := request{
			flags:  binary.BigEndian.Uint16(hdr[4:]),
			typ:    binary.BigEndian.Uint16(hdr[6:]),
			handle: binary.BigEndian.Uint64(hdr[8:]),
			offset: binary.BigEndian.Uint64(hdr[16:]),
			length: binary.BigEndian.Uint32(hdr[24:]),
		}
		if req.typ == cmdWrite {
			if req.length > MaxRequest {
				return fmt.Errorf("write request of %d bytes exceeds the limit", req.length)
			}
			req.data = make([]byte, req.length)
			if _, err := io.ReadFull(conn, req.data); err != nil {
				return err
			}
		}
		if req.typ == cmdDisc {
			return nil
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.serveRequest(e, size, req, reply); err != nil {
				fail(err)
			}
		}(req)
	}
}

func (s *Server) serveRequest(e Export, size uint64, req request, reply func(uint64, uint32, []byte) error) error {
	inRange := req.offset <= size && uint64(req.length) <= size-req.offset
	switch req.typ {
	case cmdRead:
		if !inRange {
			return reply(req.handle, errInval, nil)
		}
		if req.length > MaxRequest {
			return reply(req.handle, errOverflow, nil)
		}
		buf := make([]byte, req.length)
		if _, err := e.ReadAt(buf, int64(req.offset)); err != nil {
			s.log.Printf("nbd: read %d@%d: %v", req.length, req.offset, err)
			return reply(req.handle, errIO, nil)
		}
		return reply(req.handle, 0, buf)

	case cmdWrite, cmdWriteZeroes:
		if e.ReadOnly() {
			return reply(req.handle, errPerm, nil)
		}
		if !inRange {
			return reply(req.handle, errNoSpc, nil)
		}
		if req.typ == cmdWriteZeroes {
			if err := writeZeroes(e, int64(req.offset), int64(req.length)); err != nil {
				s.log.Printf("nbd: write zeroes %d@%d: %v", req.length, req.offset, err)
				return reply(req.handle, errIO, nil)
			}
		} else if _, err := e.WriteAt(req.data, int64(req.offset)); err != nil {
			s.log.Printf("nbd: write %d@%d: %v", req.length, req.offset, err)
			return reply(req.handle, errIO, nil)
		}
		if req.flags&cmdFlagFUA != 0 {
			if err := e.Sync(); err != nil {
				s.log.Printf("nbd: fua sync: %v", err)
				return reply(req.handle, errIO, nil)
			}
		}
		return reply(req.handle, 0, nil)

	case cmdFlush:
		if err := e.Sync(); err != nil {
			s.log.Printf("nbd: flush: %v", err)
			return reply(req.handle, errIO, nil)
		}
		return reply(req.handle, 0, nil)

	case cmdTrim:
		// Not advertised; treat as a no-op, which the protocol allows.
		return reply(req.handle, 0, nil)

	default:
		return reply(req.handle, errInval, nil)
	}
}

var zeroes = make([]byte, 1<<20)

func writeZeroes(e Export, off, length int64) error {
	for length > 0 {
		n := int64(len(zeroes))
		if n > length {
			n = length
		}
		if _, err := e.WriteAt(zeroes[:n], off); err != nil {
			return err
		}
		off += n
		length -= n
	}
	return nil
}
