package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func testPackedBlob(t *testing.T, declared int, payload []byte) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.Write([]byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 1})
	first := byte(plumbing.BlobObject)<<4 | byte(declared&15)
	for declared >>= 4; declared > 0; declared >>= 7 {
		pack.WriteByte(first | 128)
		first = byte(declared & 127)
	}
	pack.WriteByte(first)
	stream := zlib.NewWriter(&pack)
	if _, err := stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	return pack.Bytes()
}

func withPackChecksum(pack []byte) []byte {
	sum := sha1.Sum(pack)
	return append(bytes.Clone(pack), sum[:]...)
}

func TestFullPackObjectSizesAndChecksum(t *testing.T) {
	repo := newGitRepository()
	payload := bytes.Repeat([]byte("blob-"), 240)
	for _, valid := range [][]byte{nil, payload} {
		pack, err := repo.decodePack(withPackChecksum(testPackedBlob(t, len(valid), valid)))
		if err != nil {
			t.Fatal(err)
		}
		hash := plumbing.ComputeHash(plumbing.BlobObject, valid)
		if obj := pack.objects[hash]; obj == nil || obj.Type() != plumbing.BlobObject || obj.Size() != int64(len(valid)) {
			t.Fatal("decoded object or its metadata is missing")
		}
	}

	corruptStream := testPackedBlob(t, len(payload), payload)
	corruptStream[len(corruptStream)-1] ^= 1
	for _, sample := range []struct {
		name string
		pack []byte
	}{
		{"size_too_small", testPackedBlob(t, len(payload)-1, payload)},
		{"size_too_large", testPackedBlob(t, len(payload)+1, payload)},
		{"wrong_stream_checksum", corruptStream},
		{"trailing_pack_data", append(testPackedBlob(t, len(payload), payload), 0)},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if _, err := repo.decodePack(withPackChecksum(sample.pack)); err != badPack {
				t.Fatalf("got %v, want %v", err, badPack)
			}
			if len(repo.db.Objects) != 0 {
				t.Fatal("pack decoding changed the stored repository")
			}
		})
	}
}
