package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

type fileEntry struct {
	mode string
	oid  plumbing.Hash
}
type namedEntry struct {
	name string
	fileEntry
}

func directory(mode string) bool { return mode == "40000" || mode == "040000" }
func validFilePath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.ContainsAny(part, "\x00\\") {
			return false
		}
	}
	return true
}
func (g *gitRepository) getBytes(id plumbing.Hash) []byte {
	o := g.db.Objects[id]
	if o == nil {
		return nil
	}
	r, err := o.Reader()
	if err != nil {
		panic(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return b
}
func (g *gitRepository) put(kind plumbing.ObjectType, data []byte) plumbing.Hash {
	if len(data) > maxObject {
		reject(413, "Git object exceeds the size limit")
	}
	o := g.db.NewEncodedObject()
	o.SetType(kind)
	o.SetSize(int64(len(data)))
	w, err := o.Writer()
	if err != nil {
		panic(err)
	}
	_, err = w.Write(data)
	if err != nil {
		panic(err)
	}
	if err = w.Close(); err != nil {
		panic(err)
	}
	id, err := g.db.SetEncodedObject(o)
	if err != nil {
		panic(err)
	}
	l, err := readLinks(o)
	if err != nil {
		panic(err)
	}
	g.objects[id] = l
	return id
}
func (g *gitRepository) setRef(ref string, id plumbing.Hash) {
	if g.refs[ref] != id {
		g.refs[ref] = id
		g.invalidateAdvertisements()
	}
}
func (g *gitRepository) treeEntries(id plumbing.Hash) []namedEntry {
	b := g.getBytes(id)
	out := make([]namedEntry, 0)
	for len(b) > 0 {
		mode, tail, ok := bytes.Cut(b, []byte{' '})
		if !ok {
			reject(422, "Invalid Git tree")
		}
		name, rest, ok := bytes.Cut(tail, []byte{0})
		if !ok || len(rest) < 20 {
			reject(422, "Invalid Git tree")
		}
		var oid plumbing.Hash
		copy(oid[:], rest[:20])
		out = append(out, namedEntry{string(name), fileEntry{string(mode), oid}})
		b = rest[20:]
	}
	return out
}
func (g *gitRepository) resolve(ref string) plumbing.Hash {
	id := g.refs[ref]
	if id == plumbing.ZeroHash {
		id = g.refs[branchRef(ref)]
	}
	if id == plumbing.ZeroHash {
		id = g.refs["refs/tags/"+ref]
	}
	if id == plumbing.ZeroHash {
		id, _ = parseHash(ref)
	}
	id = g.peel(id)
	if g.objects[id].kind != plumbing.CommitObject {
		return plumbing.ZeroHash
	}
	return id
}
func (g *gitRepository) files(commit plumbing.Hash) map[string]fileEntry {
	files := make(map[string]fileEntry)
	if commit == plumbing.ZeroHash {
		return files
	}
	c := g.objects[commit]
	if c.kind != plumbing.CommitObject || len(c.children) != 1 {
		return files
	}
	var visit func(plumbing.Hash, string, int)
	visit = func(id plumbing.Hash, prefix string, depth int) {
		if depth > 256 {
			reject(422, "Git tree is too deep")
		}
		for _, entry := range g.treeEntries(id) {
			if directory(entry.mode) {
				visit(entry.oid, prefix+entry.name+"/", depth+1)
			} else {
				files[prefix+entry.name] = entry.fileEntry
			}
		}
	}
	visit(c.children[0], "", 0)
	return files
}

type fileDirectory struct {
	files map[string]fileEntry
	dirs  map[string]*fileDirectory
}

func newDirectory() *fileDirectory {
	return &fileDirectory{make(map[string]fileEntry), make(map[string]*fileDirectory)}
}
func (g *gitRepository) writeFiles(files map[string]fileEntry) plumbing.Hash {
	root := newDirectory()
	for name, entry := range files {
		parts := strings.Split(name, "/")
		d := root
		for _, part := range parts[:len(parts)-1] {
			if d.dirs[part] == nil {
				d.dirs[part] = newDirectory()
			}
			d = d.dirs[part]
		}
		d.files[parts[len(parts)-1]] = entry
	}
	var write func(*fileDirectory) plumbing.Hash
	write = func(d *fileDirectory) plumbing.Hash {
		entries := make([]namedEntry, 0, len(d.files)+len(d.dirs))
		for name, entry := range d.files {
			entries = append(entries, namedEntry{name, entry})
		}
		for name, child := range d.dirs {
			entries = append(entries, namedEntry{name, fileEntry{"40000", write(child)}})
		}
		sortKey := func(e namedEntry) string {
			if directory(e.mode) {
				return e.name + "/"
			}
			return e.name
		}
		sort.Slice(entries, func(i, j int) bool { return sortKey(entries[i]) < sortKey(entries[j]) })
		var data []byte
		for _, e := range entries {
			data = append(data, e.mode+" "+e.name+"\x00"...)
			data = append(data, e.oid[:]...)
		}
		return g.put(plumbing.TreeObject, data)
	}
	return write(root)
}
func (s *store) commit(r *repository, branch string, files map[string]fileEntry, parents []plumbing.Hash, message string) plumbing.Hash {
	var b strings.Builder
	b.WriteString("tree " + r.git.writeFiles(files).String() + "\n")
	for _, id := range parents {
		b.WriteString("parent " + id.String() + "\n")
	}
	identity := s.user.Login + " <" + s.user.Login + "@users.noreply.github.com> " + strconv.FormatInt(s.advance().Unix(), 10) + " +0000"
	b.WriteString("author " + identity + "\ncommitter " + identity + "\n\n" + message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteByte('\n')
	}
	id := r.git.put(plumbing.CommitObject, []byte(b.String()))
	r.git.setRef(branchRef(branch), id)
	return id
}
func (g *gitRepository) commitMessage(id plumbing.Hash) string {
	_, message, _ := bytes.Cut(g.getBytes(id), []byte("\n\n"))
	return strings.TrimRight(string(message), "\n")
}
func (g *gitRepository) history(tip plumbing.Hash) []plumbing.Hash {
	list := []plumbing.Hash{}
	q := []plumbing.Hash{tip}
	seen := make(map[plumbing.Hash]bool)
	for n := 0; n < len(q); n++ {
		id := q[n]
		if seen[id] || g.objects[id].kind != plumbing.CommitObject {
			continue
		}
		seen[id] = true
		list = append(list, id)
		q = append(q, g.objects[id].parents...)
	}
	return list
}
func contentInfo(r *repository, name string, e fileEntry) mapJSON {
	kind := "file"
	size := int64(0)
	if directory(e.mode) {
		kind = "dir"
	} else {
		if e.mode == "160000" {
			kind = "submodule"
		}
		if e.mode == "120000" {
			kind = "symlink"
		}
		if obj := r.git.db.Objects[e.oid]; obj != nil {
			size = obj.Size()
		}
	}
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return mapJSON{"name": path.Base(name), "path": name, "type": kind, "sha": e.oid.String(), "size": size, "url": r.apiPath() + "/contents/" + strings.Join(parts, "/")}
}
func (r *repository) contents(name, ref string) any {
	g := r.git
	commit := g.resolve(ref)
	if commit == plumbing.ZeroHash {
		reject(404, "Not Found")
	}
	entry := fileEntry{"40000", g.objects[commit].children[0]}
	if name != "" {
		if !validFilePath(name) {
			reject(404, "Not Found")
		}
		for _, part := range strings.Split(name, "/") {
			if !directory(entry.mode) {
				reject(404, "Not Found")
			}
			found := false
			for _, x := range g.treeEntries(entry.oid) {
				if x.name == part {
					entry = x.fileEntry
					found = true
					break
				}
			}
			if !found {
				reject(404, "Not Found")
			}
		}
	}
	if directory(entry.mode) {
		rows := make([]mapJSON, 0)
		for _, x := range g.treeEntries(entry.oid) {
			full := x.name
			if name != "" {
				full = name + "/" + full
			}
			rows = append(rows, contentInfo(r, full, x.fileEntry))
		}
		return rows
	}
	if g.objects[entry.oid].kind != plumbing.BlobObject {
		reject(404, "Not Found")
	}
	out := contentInfo(r, name, entry)
	out["encoding"] = "base64"
	out["content"] = base64.StdEncoding.EncodeToString(g.getBytes(entry.oid))
	return out
}
func (s *store) updateFile(r *repository, name string, input mapJSON, remove bool) (int, mapJSON) {
	if len(name) > 4096 || strings.Count(name, "/") >= 256 || !validFilePath(name) {
		reject(422, "Invalid path")
	}
	branch := optional(input["branch"], r.branch)
	if plumbing.ReferenceName(branchRef(branch)).Validate() != nil {
		reject(422, "Invalid branch")
	}
	parent := r.git.refs[branchRef(branch)]
	if parent == plumbing.ZeroHash && (branch != r.branch || len(r.git.refs) > 0) {
		reject(404, "Branch not found")
	}
	files := r.git.files(parent)
	old, exists := files[name]
	if remove && !exists {
		reject(404, "Not Found")
	}
	if exists {
		if input["sha"] != old.oid.String() {
			reject(409, "SHA does not match current file")
		}
	} else if _, hasSHA := input["sha"]; hasSHA {
		reject(409, "SHA does not match current file")
	}
	msg := required(input["message"], "message")
	if remove {
		delete(files, name)
	} else {
		for p := range files {
			if strings.HasPrefix(p, name+"/") || strings.HasPrefix(name, p+"/") {
				reject(409, "File and directory paths overlap")
			}
		}
		text, ok := input["content"].(string)
		if !ok {
			reject(422, "content is required")
		}
		token := strings.Join(strings.Fields(text), "")
		data, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(token)
		}
		if err != nil {
			reject(422, "content is not base64")
		}
		mode := "100644"
		if exists {
			mode = old.mode
		}
		files[name] = fileEntry{mode, r.git.put(plumbing.BlobObject, data)}
	}
	var parents []plumbing.Hash
	if parent != plumbing.ZeroHash {
		parents = []plumbing.Hash{parent}
	}
	id := s.commit(r, branch, files, parents, msg)
	output := mapJSON{"commit": mapJSON{"sha": id.String(), "message": msg}, "content": nil}
	if !remove {
		output["content"] = r.contents(name, branch)
	}
	if exists {
		return 200, output
	}
	return 201, output
}
