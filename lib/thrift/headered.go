package thrift

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"math"
)

const (
	DefaulprotoID     = ProtocolIDCompact
	DefaultClientType = HeaderClientType
)

type HeaderTransport struct {
	transport io.ReadWriter

	// Used on read
	rbuf       *bufio.Reader
	framebuf   byteReader
	readHeader *tHeader
	// Remaining bytes in the current frame. If 0, read in a new frame.
	frameSize uint64

	// Used on write
	wbuf                       *bytes.Buffer
	identity                   string
	writeInfoHeaders           map[string]string
	persistentWriteInfoHeaders map[string]string

	// Negotiated
	protoID         ProtocolID
	seqID           uint32
	flags           uint16
	clientType      ClientType
	writeTransforms []TransformID

	// Options
	maxFramesize int
}

// NewHeaderTransport Create a new transport with defaults.
func NewHeaderTransport(transport io.ReadWriter) *HeaderTransport {
	return &HeaderTransport{
		transport: transport,
		rbuf:      bufio.NewReader(transport),
		framebuf:  newLimitedByteReader(bytes.NewReader(nil), 0),
		frameSize: 0,

		wbuf:                       bytes.NewBuffer(nil),
		writeInfoHeaders:           map[string]string{},
		persistentWriteInfoHeaders: map[string]string{},

		protoID:         DefaulprotoID,
		flags:           0,
		clientType:      DefaultClientType,
		writeTransforms: []TransformID{},

		maxFramesize: int(MaxFrameSize),
	}
}

func (t *HeaderTransport) ResetTransport(transport io.ReadWriter) {
	t.transport = transport
	t.rbuf.Reset(transport)
	t.wbuf.Reset()
	t.frameSize = 0
	t.writeInfoHeaders = map[string]string{}
	t.persistentWriteInfoHeaders = map[string]string{}
}

func (t *HeaderTransport) SetSeqID(seq uint32) {
	t.seqID = seq
}

func (t *HeaderTransport) SeqID() uint32 {
	return t.seqID
}

func (t *HeaderTransport) Identity() string {
	return t.identity
}

func (t *HeaderTransport) SetIdentity(identity string) {
	t.identity = identity
}

func (t *HeaderTransport) PeerIdentity() string {
	v, ok := t.ReadHeader(IdentityHeader)
	vers, versok := t.ReadHeader(IDVersionHeader)
	if ok && versok && vers == IDVersion {
		return v
	}
	return ""
}

func (t *HeaderTransport) SetPersistentHeader(key, value string) {
	t.persistentWriteInfoHeaders[key] = value
}

func (t *HeaderTransport) PersistentHeader(key string) (string, bool) {
	v, ok := t.persistentWriteInfoHeaders[key]
	return v, ok
}

func (t *HeaderTransport) PersistentHeaders() map[string]string {
	res := map[string]string{}
	for k, v := range t.persistentWriteInfoHeaders {
		res[k] = v
	}
	return res
}

func (t *HeaderTransport) ClearPersistentHeaders() {
	t.persistentWriteInfoHeaders = map[string]string{}
}

func (t *HeaderTransport) SetHeader(key, value string) {
	t.writeInfoHeaders[key] = value
}

func (t *HeaderTransport) Header(key string) (string, bool) {
	v, ok := t.writeInfoHeaders[key]
	return v, ok
}

func (t *HeaderTransport) Headers() map[string]string {
	res := map[string]string{}
	for k, v := range t.writeInfoHeaders {
		res[k] = v
	}
	return res
}

func (t *HeaderTransport) ClearHeaders() {
	t.writeInfoHeaders = map[string]string{}
}

func (t *HeaderTransport) ReadHeader(key string) (string, bool) {
	if t.readHeader == nil {
		return "", false
	}
	// per the C++ implementation, prefer persistent headers
	if v, ok := t.readHeader.pHeaders[key]; ok {
		return v, ok
	}
	v, ok := t.readHeader.headers[key]
	return v, ok
}

func (t *HeaderTransport) ReadHeaders() map[string]string {
	res := map[string]string{}
	if t.readHeader == nil {
		return res
	}
	for k, v := range t.readHeader.headers {
		res[k] = v
	}
	for k, v := range t.readHeader.pHeaders {
		res[k] = v
	}
	return res
}

func (t *HeaderTransport) ProtocolID() ProtocolID {
	return t.protoID
}

func (t *HeaderTransport) SetProtocolID(protoID ProtocolID) error {
	if !(protoID == ProtocolIDBinary || protoID == ProtocolIDCompact) {
		return NewApplicationException(
			//NOT_IMPLEMENTED,
			0,
			fmt.Sprintf("unimplemented proto ID: %s (%#x)", protoID.String(), int64(protoID)),
		)
	}
	t.protoID = protoID
	return nil
}

func (t *HeaderTransport) AddTransform(trans TransformID) error {
	if sup, ok := supportedTransforms[trans]; !ok || !sup {
		return NewApplicationException(
			//NOT_IMPLEMENTED,
			0,
			fmt.Sprintf("unimplemented transform ID: %s (%#x)", trans.String(), int64(trans)),
		)
	}
	for _, t := range t.writeTransforms {
		if t == trans {
			return nil
		}
	}
	t.writeTransforms = append(t.writeTransforms, trans)
	return nil
}

// applyUntransform Fully read the frame and untransform into a local buffer
// we need to know the full size of the untransformed data
func (t *HeaderTransport) applyUntransform() error {
	out, err := ioutil.ReadAll(t.framebuf)
	if err != nil {
		return err
	}
	t.frameSize = uint64(len(out))
	t.framebuf = newLimitedByteReader(bytes.NewBuffer(out), int64(len(out)))
	return nil
}

// ResetProtocol Needs to be called between every frame receive (BeginMessageRead)
// We do this to read out the header for each frame. This contains the length of the
// frame and protocol / metadata info.
func (t *HeaderTransport) ResetProtocol() error {
	t.readHeader = nil
	// TODO(carlverge): We should probably just read in the whole
	// frame here. A bit of extra memory, probably a lot less CPU.
	// Needs benchmark to test.

	hdr := &tHeader{}
	// Consume the header from the input stream
	err := hdr.Read(t.rbuf)
	if err != nil {
		//return FromErr(err)
		return err
	}

	// Set new header
	t.readHeader = hdr
	// Adopt the client's protocol
	t.protoID = hdr.protoID
	t.clientType = hdr.clientType
	t.seqID = hdr.seq
	t.flags = hdr.flags

	// If the client is using unframed, just pass up the data to the protocol
	if t.clientType == UnframedDeprecated || t.clientType == UnframedCompactDeprecated {
		t.framebuf = t.rbuf
		return nil
	}

	// Make sure we can'Message read past the current frame length
	t.frameSize = hdr.payloadLen
	t.framebuf = newLimitedByteReader(t.rbuf, int64(hdr.payloadLen))
	for _, trans := range hdr.transforms {
		xformer, terr := trans.Untransformer()
		if terr != nil {
			//return FromErr(terr)
			return terr
		}

		t.framebuf, terr = xformer(t.framebuf)
		if terr != nil {
			//return FromErr(terr)
			return terr
		}
	}

	// Fully read the frame and apply untransforms if we have them
	if len(hdr.transforms) > 0 {
		err = t.applyUntransform()
		if err != nil {
			//return FromErr(err)
			return err
		}
	}

	// respond in kind with the client's transforms
	t.writeTransforms = hdr.transforms

	return nil
}

// Read Read from the current framebuffer. EOF if the frame is done.
func (t *HeaderTransport) Read(buf []byte) (int, error) {
	// If we detected unframed, just pass the transport up
	if t.clientType == UnframedDeprecated || t.clientType == UnframedCompactDeprecated {
		return t.framebuf.Read(buf)
	}
	n, err := t.framebuf.Read(buf)
	// Shouldn'Message be possibe, but just in case the frame size was flubbed
	if uint64(n) > t.frameSize {
		n = int(t.frameSize)
	}
	t.frameSize -= uint64(n)
	return n, err
}

// ReadByte Read a single byte from the current framebuffer. EOF if the frame is done.
func (t *HeaderTransport) ReadByte() (byte, error) {
	// If we detected unframed, just pass the transport up
	if t.clientType == UnframedDeprecated || t.clientType == UnframedCompactDeprecated {
		return t.framebuf.ReadByte()
	}
	b, err := t.framebuf.ReadByte()
	t.frameSize--
	return b, err
}

// Write Write multiple bytes to the framebuffer, does not send to transport.
func (t *HeaderTransport) Write(buf []byte) (int, error) {
	n, err := t.wbuf.Write(buf)
	//if err != nil {
	//	err = FromErr(err)
	//}
	return n, err
}

// WriteByte Write a single byte to the framebuffer, does not send to transport.
func (t *HeaderTransport) WriteByte(c byte) error {
	err := t.wbuf.WriteByte(c)
	//if err != nil {
	//	err = FromErr(err)
	//}
	return err
}

// WriteString Write a string to the framebuffer, does not send to transport.
func (t *HeaderTransport) WriteString(s string) (int, error) {
	n, err := t.wbuf.WriteString(s)
	//if err != nil {
	//	err = FromErr(err)
	//}
	return n, err
}

// RemainingBytes Return how many bytes remain in the current recv framebuffer.
func (t *HeaderTransport) RemainingBytes() uint64 {
	if t.clientType == UnframedDeprecated || t.clientType == UnframedCompactDeprecated {
		// We cannot really tell the size without reading the whole struct in here
		return math.MaxUint64
	}
	return t.frameSize
}

func applyTransforms(buf *bytes.Buffer, transforms []TransformID) (*bytes.Buffer, error) {
	tmpbuf := bytes.NewBuffer(nil)
	for _, trans := range transforms {
		switch trans {
		case TransformZlib:
			zwr := zlib.NewWriter(tmpbuf)
			_, err := buf.WriteTo(zwr)
			if err != nil {
				return nil, err
			}
			err = zwr.Close()
			if err != nil {
				return nil, err
			}
			buf, tmpbuf = tmpbuf, buf
			tmpbuf.Reset()
		default:
			return nil, NewApplicationException(
				//NOT_IMPLEMENTED,
				0,
				fmt.Sprintf("unimplemented transform ID: %s (%#x)", trans.String(), int64(trans)),
			)
		}
	}
	return buf, nil
}

func (t *HeaderTransport) flushHeader() error {
	hdr := tHeader{}
	hdr.headers = t.writeInfoHeaders
	hdr.pHeaders = t.persistentWriteInfoHeaders
	hdr.protoID = t.protoID
	hdr.clientType = t.clientType
	hdr.seq = t.seqID
	hdr.flags = t.flags
	hdr.transforms = t.writeTransforms

	if t.identity != "" {
		hdr.headers[IdentityHeader] = t.identity
		hdr.headers[IDVersionHeader] = IDVersion
	}

	outbuf, err := applyTransforms(t.wbuf, t.writeTransforms)
	if err != nil {
		//return FromErr(err)
		return err
	}
	t.wbuf = outbuf

	hdr.payloadLen = uint64(t.wbuf.Len())
	err = hdr.calcLenFromPayload()
	if err != nil {
		//return FromErr(err)
		return err
	}

	hdrbuf := bytes.NewBuffer(make([]byte, 64))
	hdrbuf.Reset()
	err = hdr.Write(hdrbuf)
	if err != nil {
		//return FromErr(err)
		return err
	}

	if _, err = hdrbuf.WriteTo(t.transport); err != nil {
		//return FromErr(err)
		return err
	}
	return nil
}

func (t *HeaderTransport) flushFramed() error {
	buflen := t.wbuf.Len()
	framesize := uint32(buflen)
	if buflen > t.maxFramesize {
		return NewApplicationException(
			//INVALID_FRAME_SIZE,
			0,
			fmt.Sprintf("cannot send bigframe of size %d", buflen),
		)
	}

	err := binary.Write(t.transport, binary.BigEndian, framesize)
	if err != nil {
		//return FromErr(err)
		return err
	}
	return nil
}

func (t *HeaderTransport) Flush() error {
	// Closure incase wbuf pointer changes in xform
	defer func(tp *HeaderTransport) {
		tp.wbuf.Reset()
	}(t)
	var err error

	switch t.clientType {
	case HeaderClientType:
		err = t.flushHeader()
	case FramedDeprecated:
		err = t.flushFramed()
	case FramedCompact:
		err = t.flushFramed()
	case UnframedCompactDeprecated:
		err = nil
	case UnframedDeprecated:
		err = nil
	default:
		return NewApplicationException(
			//UNKNOWN_TRANSPORT_EXCEPTION,
			0,
			fmt.Sprintf("tHeader cannot flush for clientType %s", t.clientType.String()),
		)
	}
	if err != nil {
		return err
	}

	// Writeout the payload
	if t.wbuf.Len() > 0 {
		if _, err = t.wbuf.WriteTo(t.transport); err != nil {
			//return FromErr(err)
			return err
		}
	}

	// Remove the non-persistent headers on flush
	t.ClearHeaders()
	return nil
}
