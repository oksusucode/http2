package hpack

import (
	"bytes"
	"errors"
	"fmt"
)

const maxUint32 = ^uint32(0)

type indexType int

const (
	indexedTrue indexType = iota
	indexedFalse
	indexedNever
)

type huffman uint8

const (
	huffForceTrue huffman = iota
	huffTrue
	huffFalse
)

type Encoder struct {
	table       headerTable
	minSize     uint32
	sizeChanged bool
	i           bool
	h           huffman
}

func NeverSensitive(string, string) bool {
	return false
}

func NewEncoder(maxHeaderTableSize uint32) *Encoder {
	enc := new(Encoder)
	enc.minSize = maxUint32
	enc.table.maxSize = maxHeaderTableSize
	enc.i = true
	enc.h = huffTrue
	return enc
}

func (enc *Encoder) MaxHeaderTableSize() uint32 {
	return enc.table.maxSize
}

func (enc *Encoder) SetMaxHeaderTableSize(max uint32) {
	if max < enc.minSize {
		enc.minSize = max
	}
	enc.sizeChanged = true
	enc.table.setMaxSize(max)
}

func (enc *Encoder) EncodeHeaderField(dst []byte, name, value string, sensitive bool) (n uint32, _ []byte) {
	if enc.sizeChanged {
		enc.sizeChanged = false
		if enc.minSize < enc.table.maxSize {
			dst = encDynamicTableSizeUpdate(dst, uint64(enc.minSize))
		}
		enc.minSize = maxUint32
		dst = encDynamicTableSizeUpdate(dst, uint64(enc.table.maxSize))
	}

	hsize := HeaderFieldSize(name, value)

	switch {
	case sensitive:
		hsize, dst = encLiteral(dst, name, value, indexedNever, 0, enc.h)
		n += hsize
	case enc.table.maxSize == 0:
		idx, matched := index(name, value)
		if matched {
			dst = encIndexed(dst, idx)
		} else {
			hsize, dst = encLiteral(dst, name, value, indexedFalse, idx, enc.h)
			n += hsize
		}
	case hsize > enc.table.maxSize:
		hsize, dst = encLiteral(dst, name, value, indexedFalse, 0, enc.h)
		n += hsize
	default:
		if idx, matched := enc.table.find(name, value); matched {
			dst = encIndexed(dst, idx)
		} else {
			if enc.i {
				hsize, dst = encLiteral(dst, name, value, indexedTrue, idx, enc.h)
				enc.table.add(name, value)
			} else {
				hsize, dst = encLiteral(dst, name, value, indexedFalse, idx, enc.h)
			}
			n += hsize
		}
	}

	return n, dst
}

func encInteger(dst []byte, mask, n byte, i uint64) []byte {
	bound := uint64((1 << n) - 1)
	if i < bound {
		return append(dst, mask|byte(i))
	}

	dst = append(dst, mask|byte(bound))
	i -= bound

	for i >= 128 {
		dst = append(dst, byte((i&0x7f)|0x80))
		i >>= 7
	}

	return append(dst, byte(i))
}

func encStringLiteral(dst []byte, s string, h huffman) []byte {
	slen := uint64(len(s))

	switch h {
	case huffForceTrue:
		dst = encInteger(dst, 0x80, 7, HuffmanEncodedLen(s))
		dst = HuffmanEncode(dst, s)
	case huffTrue:
		if hlen := HuffmanEncodedLen(s); hlen < slen {
			dst = encInteger(dst, 0x80, 7, hlen)
			dst = HuffmanEncode(dst, s)
		} else {
			dst = encInteger(dst, 0x00, 7, slen)
			dst = append(dst, s...)
		}
	case huffFalse:
		dst = encInteger(dst, 0x00, 7, slen)
		dst = append(dst, s...)
	}

	return dst
}

func encIndexed(dst []byte, idx uint64) []byte {
	return encInteger(dst, 0x80, 7, idx)
}

func encLiteral(dst []byte, name, value string, idxType indexType, idx uint64, h huffman) (hsize uint32, _ []byte) {
	var mask, n byte

	switch idxType {
	case indexedTrue:
		mask, n = 0x40, 6
	case indexedFalse:
		mask, n = 0x00, 4
	case indexedNever:
		mask, n = 0x10, 4
	}

	dst = encInteger(dst, mask, n, idx)

	if idx == 0 {
		dst = encStringLiteral(dst, name, h)
		hsize += uint32(len(name))
	}

	hsize += uint32(len(value) + headerEntryOverhead)

	return hsize, encStringLiteral(dst, value, h)
}

func encDynamicTableSizeUpdate(dst []byte, size uint64) []byte {
	return encInteger(dst, 0x20, 5, size)
}

type DecodingError struct {
	s string
}

func (e DecodingError) Error() string {
	return fmt.Sprintf("decode error: %v", e.s)
}

var (
	ErrHeaderFieldsTooLarge = errors.New("header fields too large")
	errNeedsMore            = errors.New("short buffer")

	errHeaderTableSizeUpdateRequired = DecodingError{"header table size update required"}
	errHeaderTableSizeUpdate         = DecodingError{"header table size update must occur at the beginning of the first header block"}
	errTruncated                     = DecodingError{"header field has been truncated"}
	errBadHeaderBlock                = DecodingError{"bad header block"}
	errBadHeaderTableSize            = DecodingError{"bad max header table size"}
	errBadIndexValue                 = DecodingError{"bad index value"}
	errIntegerOverflow               = DecodingError{"integer overflow"}
)

type HeaderFieldHandler func(name, value string, sensitive bool) error

type Decoder struct {
	table        headerTable
	maxSize      uint32
	maxSizeLimit uint32
	sizeChanged  bool
	n            uint32
	err          error
	decoderState
}

func NewDecoder(maxHeaderTableSize uint32) *Decoder {
	dec := new(Decoder)
	dec.maxSize = maxHeaderTableSize
	dec.maxSizeLimit = maxHeaderTableSize
	dec.table.maxSize = maxHeaderTableSize
	dec.decoderState.dec = dec
	return dec
}

func (dec *Decoder) MaxHeaderTableSize() uint32 {
	return dec.table.maxSize
}

func (dec *Decoder) SetMaxHeaderTableSize(max uint32) {
	dec.maxSize = max
	if dec.maxSize < dec.maxSizeLimit {
		dec.sizeChanged = true
		dec.table.setMaxSize(max)
	}
}

func (dec *Decoder) Decode(headerBlock []byte, headerBlockSizeLimit uint32, handle HeaderFieldHandler) (uint32, error) {
	if dec.err != nil {
		return dec.n, dec.err
	}

	if len(headerBlock) == 0 {
		return dec.n, nil
	}

	dec.handle = handle

	if dec.b.Len() == 0 {
		dec.data = headerBlock
	} else {
		dec.b.Write(headerBlock)
		dec.data = dec.b.Bytes()
		dec.b.Reset()
	}

	if dec.sizeChanged && dec.data[0]&224 != 32 {
		dec.err = errHeaderTableSizeUpdateRequired
		return dec.n, dec.err
	}

	var (
		n   uint32
		err error
	)

	for dec.more() {
		n, err = dec.decode()
		if err != nil {
			if err == errNeedsMore {
				err = nil
				dec.b.Write(dec.data)
			}
			break
		}
		if headerBlockSizeLimit != 0 {
			if dec.n+n > headerBlockSizeLimit {
				err = ErrHeaderFieldsTooLarge
				break
			}
			dec.n += n
		}
	}

	if err != nil {
		dec.err = err
	}

	return dec.n, err
}

func (dec *Decoder) Len() uint32 {
	return dec.n
}

func (dec *Decoder) Reset() (err error) {
	dec.err = nil
	dec.st = false
	dec.n = 0
	if dec.b.Len() > 0 {
		dec.b.Reset()
		err = errTruncated
	}
	return
}

type decoderState struct {
	dec    *Decoder
	data   []byte
	hbuf   []byte
	b      bytes.Buffer
	handle HeaderFieldHandler
	st     bool
}

func (state *decoderState) more() bool {
	return len(state.data) > 0
}

func (state *decoderState) decode() (n uint32, err error) {
	data := state.data
	b := state.data[0]

	switch {
	case b&128 != 0:
		err = decIndexed(state)
	case b&192 == 64:
		n, err = decLiteral(state, indexedTrue)
	case b&240 == 0:
		n, err = decLiteral(state, indexedFalse)
	case b&240 == 16:
		n, err = decLiteral(state, indexedNever)
	case b&224 == 32:
		err = decDynamicTableSizeUpdate(state)
	default:
		err = errBadHeaderBlock
	}

	if err == errNeedsMore {
		state.data = data
	}

	return
}

func (state *decoderState) decInteger(n byte) (uint64, error) {
	if len(state.data) == 0 {
		return 0, errNeedsMore
	}

	bound := uint64((1 << n) - 1)
	i := uint64(state.data[0]) & bound
	state.data = state.data[1:]

	if i < bound {
		return i, nil
	}

	var m uint64

	for len(state.data) > 0 {
		b := state.data[0]
		state.data = state.data[1:]

		i += uint64(b&127) << m
		m += 7
		if b&128 == 0 {
			return i, nil
		}
		if m >= 63 {
			return 0, errIntegerOverflow
		}
	}

	return 0, errNeedsMore
}

func (state *decoderState) decStringLiteral() (s string, _ error) {
	if len(state.data) == 0 {
		return s, errNeedsMore
	}

	h := state.data[0]&128 != 0
	l, err := state.decInteger(7)
	if err != nil {
		return s, err
	}

	if uint64(len(state.data)) < l {
		return s, errNeedsMore
	}

	if h {
		state.hbuf = HuffmanDecode(state.hbuf[:0], state.data[:l])
		s, state.data = string(state.hbuf), state.data[l:]
	} else {
		s, state.data = string(state.data[:l]), state.data[l:]
	}

	return s, nil
}

func decIndexed(state *decoderState) error {
	state.st = true

	idx, err := state.decInteger(7)
	if err != nil {
		return err
	}

	hf, ok := state.dec.table.entry(idx)
	if !ok {
		return errBadIndexValue
	}

	if state.handle != nil {
		return state.handle(hf.name, hf.value, false)
	}

	return nil
}

func decLiteral(state *decoderState, idxType indexType) (hsize uint32, _ error) {
	state.st = true

	var n byte
	switch idxType {
	case indexedTrue:
		n = 6
	case indexedFalse:
		n = 4
	case indexedNever:
		n = 4
	}

	idx, err := state.decInteger(n)
	if err != nil {
		return 0, err
	}

	var name, value string

	if idx > 0 {
		hf, ok := state.dec.table.entry(idx)
		if !ok {
			return 0, errBadIndexValue
		}
		name = hf.name
	} else {
		if name, err = state.decStringLiteral(); err != nil {
			return 0, err
		}
		hsize += uint32(len(name))
	}

	if value, err = state.decStringLiteral(); err != nil {
		return 0, err
	}

	hsize += uint32(len(value) + headerEntryOverhead)

	if idxType == indexedTrue {
		state.dec.table.add(name, value)
	}

	if state.handle != nil {
		err = state.handle(name, value, idxType == indexedNever)
	}

	return hsize, err
}

func decDynamicTableSizeUpdate(state *decoderState) error {
	if state.st {
		return errHeaderTableSizeUpdate
	}

	i, err := state.decInteger(5)
	if err != nil {
		return err
	}

	size := uint32(i)
	if size > state.dec.maxSize {
		return errBadHeaderTableSize
	}

	state.dec.maxSizeLimit = size
	state.dec.sizeChanged = false
	state.dec.table.setMaxSize(size)

	return nil
}
