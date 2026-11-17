package main

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
)

var needsDeltaResolution = errors.New("pack contains deltas")
var packInflaters sync.Pool

// fullObject owns the payload read from a Git pack. Its ID is computed using
// the same Git object hashing implementation as the delta decoder.
type fullObject struct {
	id   plumbing.Hash
	kind plumbing.ObjectType
	data []byte
}

func (o *fullObject) Hash() plumbing.Hash             { return o.id }
func (o *fullObject) Type() plumbing.ObjectType       { return o.kind }
func (o *fullObject) Size() int64                     { return int64(len(o.data)) }
func (o *fullObject) SetType(plumbing.ObjectType)     { panic("immutable Git object") }
func (o *fullObject) SetSize(int64)                   { panic("immutable Git object") }
func (o *fullObject) Writer() (io.WriteCloser, error) { return nil, errors.ErrUnsupported }
func (o *fullObject) Reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

type packInflater struct {
	source bytes.Reader
	stream io.ReadCloser
}

func (p *packInflater) inflate(size int) ([]byte, error) {
	if p.stream == nil {
		stream, err := zlib.NewReader(&p.source)
		if err != nil {
			return nil, badPack
		}
		p.stream = stream
	} else if p.stream.(zlib.Resetter).Reset(&p.source, nil) != nil {
		return nil, badPack
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(p.stream, data); err != nil {
		return nil, badPack
	}
	// Read through the checksum and reject a stream whose size differs from its pack header.
	var extra [1]byte
	if n, err := p.stream.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, badPack
	}
	return data, nil
}

func (s *stagedPack) decodeFullObjects(data []byte, count uint32) error {
	inflater, _ := packInflaters.Get().(*packInflater)
	if inflater == nil {
		inflater = new(packInflater)
	}
	inflater.source.Reset(data)
	defer func() {
		if inflater.stream != nil {
			inflater.stream.Close()
		}
		inflater.source.Reset(nil)
		packInflaters.Put(inflater)
	}()
	for range count {
		first, err := inflater.source.ReadByte()
		if err != nil {
			return badPack
		}
		kind := plumbing.ObjectType((first >> 4) & 7)
		if kind == plumbing.OFSDeltaObject || kind == plumbing.REFDeltaObject {
			return needsDeltaResolution
		}
		if kind < plumbing.CommitObject || kind > plumbing.TagObject {
			return badPack
		}
		size := int64(first & 15)
		for shift := 4; first&128 != 0; shift += 7 {
			first, err = inflater.source.ReadByte()
			if err != nil || shift >= 32 {
				return badPack
			}
			size |= int64(first&127) << shift
		}
		if size > maxObject || s.total+size > maxPackOutput {
			return badPack
		}
		payload, err := inflater.inflate(int(size))
		if err != nil {
			return err
		}
		obj := &fullObject{id: plumbing.ComputeHash(kind, payload), kind: kind, data: payload}
		if _, err := s.SetEncodedObject(obj); err != nil {
			return err
		}
	}
	if inflater.source.Len() != 0 {
		return badPack
	}
	return nil
}
