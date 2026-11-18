package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

const mainRef = "refs/heads/main"
const maxObject = 32 << 20
const maxPackOutput = 128 << 20

var badPack = errors.New("invalid pack")
var badGraph = errors.New("invalid object graph")

type wireCache struct {
	request  []byte
	response []byte
}

type gitRepository struct {
	mu      sync.Mutex
	db      *memory.Storage
	refs    map[string]plumbing.Hash
	headRef string
	objects map[plumbing.Hash]objectLinks

	receiveAdvertisement []byte
	refsReply            atomic.Pointer[wireCache]
	fetchReply           atomic.Pointer[wireCache]
}

type objectLinks struct {
	kind     plumbing.ObjectType
	children []plumbing.Hash
	parents  []plumbing.Hash
}

func newGitRepository() *gitRepository {
	return &gitRepository{
		db: memory.NewStorage(), refs: make(map[string]plumbing.Hash), headRef: mainRef,
		objects: make(map[plumbing.Hash]objectLinks),
	}
}

func (r *gitRepository) invalidateAdvertisements() {
	r.receiveAdvertisement = nil
	r.refsReply.Store(nil)
	r.fetchReply.Store(nil)
}

func (r *gitRepository) refNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func parseHash(s string) (plumbing.Hash, bool) {
	var id plumbing.Hash
	if len(s) != hex.EncodedLen(len(id)) {
		return id, false
	}
	_, err := hex.Decode(id[:], []byte(s))
	return id, err == nil
}

func readLinks(obj plumbing.EncodedObject) (objectLinks, error) {
	links := objectLinks{kind: obj.Type()}
	if links.kind == plumbing.BlobObject {
		return links, nil
	}
	var data []byte
	if direct, ok := obj.(*fullObject); ok {
		data = direct.data
	} else {
		reader, err := obj.Reader()
		if err != nil {
			return links, err
		}
		defer reader.Close()
		data, err = io.ReadAll(io.LimitReader(reader, maxObject+1))
		if err != nil || len(data) > maxObject {
			return links, badGraph
		}
	}
	switch links.kind {
	case plumbing.CommitObject, plumbing.TagObject:
		for len(data) > 0 {
			line, tail, _ := bytes.Cut(data, []byte{'\n'})
			data = tail
			if len(line) == 0 {
				break
			}
			key, value, _ := bytes.Cut(line, []byte{' '})
			switch string(key) {
			case "tree", "object", "parent":
				id, ok := parseHash(string(value))
				if !ok || id == plumbing.ZeroHash {
					return links, badGraph
				}
				if string(key) == "parent" {
					links.parents = append(links.parents, id)
				} else {
					links.children = append(links.children, id)
				}
			}
		}
		if len(links.children) != 1 {
			return links, badGraph
		}
	case plumbing.TreeObject:
		for len(data) != 0 {
			mode, tail, ok := bytes.Cut(data, []byte{' '})
			if !ok || len(mode) == 0 {
				return links, badGraph
			}
			name, tail, ok := bytes.Cut(tail, []byte{0})
			if !ok || len(name) == 0 || len(tail) < len(plumbing.ZeroHash) {
				return links, badGraph
			}
			var id plumbing.Hash
			copy(id[:], tail[:len(id)])
			if string(mode) != "160000" {
				links.children = append(links.children, id)
			}
			data = tail[len(id):]
		}
	default:
		return links, badGraph
	}
	return links, nil
}

type stagedPack struct {
	objects  map[plumbing.Hash]plumbing.EncodedObject
	existing *memory.Storage
	total    int64
}

func (s *stagedPack) EncodedObject(kind plumbing.ObjectType, id plumbing.Hash) (plumbing.EncodedObject, error) {
	if obj := s.objects[id]; obj != nil && (kind == plumbing.AnyObject || obj.Type() == kind) {
		return obj, nil
	}
	return s.existing.EncodedObject(kind, id)
}

func (s *stagedPack) SetEncodedObject(object plumbing.EncodedObject) (plumbing.Hash, error) {
	size := object.Size()
	if size < 0 || size > maxObject || s.total+size > maxPackOutput {
		return plumbing.ZeroHash, badPack
	}
	s.total += size
	switch object.Type() {
	case plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject:
	default:
		return plumbing.ZeroHash, badPack
	}
	if s.objects == nil {
		s.objects = make(map[plumbing.Hash]plumbing.EncodedObject)
	}
	id := object.Hash()
	s.objects[id] = object
	return id, nil
}

// The delta parser uses the staged read and write methods. The embedded
// store supplies the rest of its object-storage interface without exposing
// the live repository to writes.
type deltaPackStorage struct {
	*stagedPack
	*memory.Storage
}

func (s deltaPackStorage) EncodedObject(kind plumbing.ObjectType, id plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.stagedPack.EncodedObject(kind, id)
}

func (s deltaPackStorage) SetEncodedObject(object plumbing.EncodedObject) (plumbing.Hash, error) {
	return s.stagedPack.SetEncodedObject(object)
}

func (r *gitRepository) decodePack(data []byte) (*stagedPack, error) {
	staged := &stagedPack{existing: r.db}
	if len(data) == 0 {
		return staged, nil
	}
	if len(data) < 32 || string(data[:4]) != "PACK" {
		return nil, badPack
	}
	end := len(data) - sha1.Size
	version, count := binary.BigEndian.Uint32(data[4:]), binary.BigEndian.Uint32(data[8:])
	digest := sha1.Sum(data[:end])
	if (version != 2 && version != 3) || count > 100_000 || int64(count) > int64(end-12)/3 || !bytes.Equal(digest[:], data[end:]) {
		return nil, badPack
	}
	if err := staged.decodeFullObjects(data[12:end], count); err != needsDeltaResolution {
		if err != nil {
			return nil, err
		}
		return staged, nil
	}
	staged = &stagedPack{existing: r.db}
	scan := packfile.NewScanner(bytes.NewReader(data))
	defer scan.Close()
	parser, err := packfile.NewParserWithStorage(scan, deltaPackStorage{staged, memory.NewStorage()})
	if err != nil {
		return nil, err
	}
	parsed, err := parser.Parse()
	if err != nil || !bytes.Equal(parsed[:], digest[:]) {
		return nil, badPack
	}
	return staged, nil
}

// Closure validates every newly reachable object before a ref is published.
func (r *gitRepository) closure(staged *stagedPack, tip plumbing.Hash) (map[plumbing.Hash]objectLinks, error) {
	found := make(map[plumbing.Hash]objectLinks)
	todo := []plumbing.Hash{tip}
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if _, ok := r.objects[id]; ok {
			continue
		}
		if _, ok := found[id]; ok {
			continue
		}
		obj := staged.objects[id]
		if obj == nil {
			return nil, badGraph
		}
		links, err := readLinks(obj)
		if err != nil {
			return nil, err
		}
		found[id] = links
		todo = append(todo, links.children...)
		todo = append(todo, links.parents...)
	}
	return found, nil
}

func (r *gitRepository) ancestor(old, tip plumbing.Hash, newObjects map[plumbing.Hash]objectLinks) bool {
	seen := make(map[plumbing.Hash]bool)
	todo := []plumbing.Hash{tip}
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if id == old {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		links, ok := newObjects[id]
		if !ok {
			links = r.objects[id]
		}
		todo = append(todo, links.parents...)
	}
	return false
}

func (r *gitRepository) refConflict(name string) bool {
	for existing := range r.refs {
		if strings.HasPrefix(existing, name+"/") || strings.HasPrefix(name, existing+"/") {
			return true
		}
	}
	return false
}

func (r *gitRepository) peel(id plumbing.Hash) plumbing.Hash {
	for i := 0; i < 64; i++ {
		obj := r.objects[id]
		if obj.kind != plumbing.TagObject || len(obj.children) != 1 {
			return id
		}
		id = obj.children[0]
	}
	return id
}

type shallowSpec struct {
	wants, haves   []plumbing.Hash
	shallow        map[plumbing.Hash]bool
	depth          int
	done, relative bool
	includeTag     bool
}

func (r *gitRepository) shallowBoundary(spec shallowSpec) (map[plumbing.Hash]bool, map[plumbing.Hash]bool) {
	boundary := make(map[plumbing.Hash]bool)
	unshallow := make(map[plumbing.Hash]bool)
	if spec.depth == 0 && len(spec.shallow) == 0 {
		return boundary, unshallow
	}
	type position struct {
		id       plumbing.Hash
		distance int
	}
	start := 1
	if spec.relative {
		start = -1
	}
	queue := make([]position, 0, len(spec.wants))
	for _, id := range spec.wants {
		queue = append(queue, position{r.peel(id), start})
	}
	best := make(map[plumbing.Hash]int)
	for i := 0; i < len(queue); i++ {
		id, distance := queue[i].id, queue[i].distance
		if distance == -1 && spec.shallow[id] {
			distance = 0
		}
		if previous, ok := best[id]; ok && previous <= distance {
			continue
		}
		best[id] = distance
		parents := r.objects[id].parents
		if len(parents) == 0 {
			continue
		}
		if spec.depth > 0 && distance >= spec.depth {
			boundary[id] = true
			continue
		}
		delete(boundary, id)
		if spec.shallow[id] {
			unshallow[id] = true
		}
		if distance >= 0 {
			distance++
		}
		for _, parent := range parents {
			queue = append(queue, position{parent, distance})
		}
	}
	return boundary, unshallow
}

// walkObjects follows tree and tag links even when a shallow boundary cuts commit parents.
func (r *gitRepository) walkObjects(tips []plumbing.Hash, stopParents map[plumbing.Hash]bool,
	seen map[plumbing.Hash]bool, visit func(plumbing.Hash)) {
	todo := append([]plumbing.Hash(nil), tips...)
	for len(todo) > 0 {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		obj, ok := r.objects[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		visit(id)
		todo = append(todo, obj.children...)
		if !stopParents[id] {
			todo = append(todo, obj.parents...)
		}
	}
}
