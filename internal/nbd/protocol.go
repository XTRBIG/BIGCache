// Package nbd implements the Network Block Device protocol (fixed newstyle
// negotiation, simple replies) so that cached volumes can be attached as
// /dev/nbdX block devices on Linux, and so that other NBD clients (qemu,
// nbd-client, nbdfuse) can use them.
package nbd

// Handshake.
const (
	nbdMagic         = 0x4e42444d41474943 // "NBDMAGIC"
	iHaveOpt         = 0x49484156454F5054 // "IHAVEOPT"
	optReplyMagic    = 0x3e889045565a9
	requestMagic     = 0x25609513
	simpleReplyMagic = 0x67446698

	flagFixedNewstyle = 1 << 0
	flagNoZeroes      = 1 << 1

	clientFlagFixedNewstyle = 1 << 0
	clientFlagNoZeroes      = 1 << 1
)

// Options.
const (
	optExportName      = 1
	optAbort           = 2
	optList            = 3
	optStartTLS        = 5
	optInfo            = 6
	optGo              = 7
	optStructuredReply = 8
)

// Option replies.
const (
	repAck        = 1
	repServer     = 2
	repInfo       = 3
	repErrUnsup   = 1<<31 + 1
	repErrPolicy  = 1<<31 + 2
	repErrInvalid = 1<<31 + 3
	repErrUnknown = 1<<31 + 6
)

// Info types.
const (
	infoExport    = 0
	infoName      = 1
	infoBlockSize = 3
)

// Transmission flags.
const (
	TxHasFlags        = 1 << 0
	TxReadOnly        = 1 << 1
	TxSendFlush       = 1 << 2
	TxSendFUA         = 1 << 3
	TxRotational      = 1 << 4
	TxSendTrim        = 1 << 5
	TxSendWriteZeroes = 1 << 6
	TxCanMultiConn    = 1 << 8
)

// Commands.
const (
	cmdRead        = 0
	cmdWrite       = 1
	cmdDisc        = 2
	cmdFlush       = 3
	cmdTrim        = 4
	cmdWriteZeroes = 6

	cmdFlagFUA = 1 << 0
)

// Errors (Linux errno values, as required by the protocol).
const (
	errPerm     = 1
	errIO       = 5
	errNoMem    = 12
	errInval    = 22
	errNoSpc    = 28
	errOverflow = 75
	errNotSup   = 95
	errShutdown = 108
)

// MaxRequest is the largest READ/WRITE payload the server accepts.
const MaxRequest = 32 << 20
