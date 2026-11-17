package main

import (
	"bytes"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
)

func (s *store) createPull(r *repository, input mapJSON) *pull {
	title := required(input["title"], "title")
	head, base := required(input["head"], "head"), required(input["base"], "base")
	if user, branch, ok := strings.Cut(head, ":"); ok && strings.EqualFold(user, r.user.Login) {
		head = branch
	}
	r.git.mu.Lock()
	defer r.git.mu.Unlock()
	h, b := r.git.refs[branchRef(head)], r.git.refs[branchRef(base)]
	if h == plumbing.ZeroHash || b == plumbing.ZeroHash || h == b {
		reject(422, "Invalid pull request branches or no commits between them")
	}
	for _, p := range r.pulls {
		if p.state == "open" && p.head == head && p.base == base {
			reject(422, "Pull request already exists")
		}
	}
	id, now := s.allocate(), s.now()
	p := &pull{id: id, nodeID: nodeID("PullRequest", id), number: r.nextNumber, title: title, body: stringVal(input["body"]), state: "open", head: head, base: base, headStart: h, baseStart: b, created: now, updated: now, repo: r}
	r.nextNumber++
	r.pulls[p.number] = p
	s.nodes[p.nodeID] = p
	return p
}
func (s *store) editPull(p *pull, input mapJSON) {
	state := p.state
	if x, ok := input["state"]; ok {
		state = stringVal(x)
		if state != "open" && state != "closed" {
			reject(422, "Invalid pull request state")
		}
	}
	if p.merged != nil && state == "open" {
		reject(422, "Pull request is merged")
	}
	title := p.title
	if x, ok := input["title"]; ok {
		title = required(x, "title")
	}
	now := s.now()
	if state != p.state {
		p.closed = nil
		if state == "closed" {
			p.closed = &now
		}
	}
	p.state, p.updated, p.title, p.body = state, now, title, optional(input["body"], p.body)
}
func (p *pull) tips() (base, head plumbing.Hash) {
	base, head = p.baseStart, p.headStart
	if p.merged == nil {
		if id := p.repo.git.refs[branchRef(p.base)]; id != plumbing.ZeroHash {
			base = id
		}
		if id := p.repo.git.refs[branchRef(p.head)]; id != plumbing.ZeroHash {
			head = id
		}
	}
	return
}
func (p *pull) view() mapJSON {
	p.repo.git.mu.Lock()
	defer p.repo.git.mu.Unlock()
	b, h := p.tips()
	var merge any
	if p.merged != nil {
		merge = p.mergeCommit.String()
	}
	return mapJSON{"id": p.id, "node_id": p.nodeID, "number": p.number, "title": p.title, "body": p.body, "state": p.state, "user": p.repo.user,
		"created_at": p.created, "updated_at": p.updated, "closed_at": p.closed, "merged_at": p.merged, "merged": p.merged != nil, "merge_commit_sha": merge,
		"head": mapJSON{"ref": p.head, "sha": h.String(), "label": p.repo.user.Login + ":" + p.head},
		"base": mapJSON{"ref": p.base, "sha": b.String(), "label": p.repo.user.Login + ":" + p.base}}
}
func (r *repository) pullList(state string, asc bool) []*pull {
	list := []*pull{}
	for _, p := range r.pulls {
		if state == "all" || p.state == state {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(a, b int) bool {
		if asc {
			return list[a].number < list[b].number
		}
		return list[a].number > list[b].number
	})
	return list
}

type difference struct {
	name          string
	before, after fileEntry
}

func differences(before, after map[string]fileEntry) []difference {
	out := []difference{}
	for name, e := range before {
		if e != after[name] {
			out = append(out, difference{name, e, after[name]})
		}
	}
	for name, e := range after {
		if before[name].oid == plumbing.ZeroHash {
			out = append(out, difference{name, fileEntry{}, e})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
func (p *pull) changedFiles(base, head plumbing.Hash) []difference {
	g := p.repo.git
	seen := make(map[plumbing.Hash]bool)
	for _, oid := range g.history(base) {
		seen[oid] = true
	}
	start := plumbing.ZeroHash
	for _, oid := range g.history(head) {
		if seen[oid] {
			start = oid
			break
		}
	}
	return differences(g.files(start), g.files(head))
}

type hunk struct {
	start, end int
	lines      []string
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return []string{}
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
func hunks(a, b []string) []hunk {
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	ae, be := len(a), len(b)
	for ae > start && be > start && a[ae-1] == b[be-1] {
		ae--
		be--
	}
	if ae == start && be == start {
		return nil
	}
	n, m := ae-start, be-start
	if int64(n+1)*int64(m+1) > 2_000_000 {
		return []hunk{{start, ae, b[start:be]}}
	}
	stride := m + 1
	grid := make([]uint32, (n+1)*stride)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[start+i] == b[start+j] {
				grid[i*stride+j] = grid[(i+1)*stride+j+1] + 1
			} else {
				grid[i*stride+j] = max(grid[(i+1)*stride+j], grid[i*stride+j+1])
			}
		}
	}
	out := []hunk{}
	x, y := 0, 0
	for x < n || y < m {
		if x < n && y < m && a[start+x] == b[start+y] {
			x++
			y++
			continue
		}
		ox, oy := x, y
		for x < n || y < m {
			if x < n && y < m && a[start+x] == b[start+y] {
				break
			}
			if y == m || (x < n && grid[(x+1)*stride+y] >= grid[x*stride+y+1]) {
				x++
			} else {
				y++
			}
		}
		out = append(out, hunk{start + ox, start + x, b[start+oy : start+y]})
	}
	return out
}
func combineText(base, ours, theirs []byte) ([]byte, bool) {
	for _, raw := range [][]byte{base, ours, theirs} {
		if len(raw) > 256<<10 || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
			return nil, false
		}
	}
	lines := splitLines(base)
	combined := append(hunks(lines, splitLines(ours)), hunks(lines, splitLines(theirs))...)
	sort.SliceStable(combined, func(a, b int) bool {
		if combined[a].start == combined[b].start {
			return combined[a].end < combined[b].end
		}
		return combined[a].start < combined[b].start
	})
	var out strings.Builder
	at := 0
	var prev *hunk
	for j := range combined {
		h := &combined[j]
		if prev != nil && h.start == prev.start && h.end == prev.end && slices.Equal(h.lines, prev.lines) {
			continue
		}
		if h.start < at || (prev != nil && h.start == prev.start && h.end == h.start) {
			return nil, false
		}
		for _, line := range lines[at:h.start] {
			out.WriteString(line)
		}
		for _, line := range h.lines {
			out.WriteString(line)
		}
		at = h.end
		prev = h
	}
	for _, line := range lines[at:] {
		out.WriteString(line)
	}
	return []byte(out.String()), true
}
func (g *gitRepository) mergeEntry(base, ours, theirs fileEntry) (fileEntry, []byte, bool) {
	if base.oid == plumbing.ZeroHash || ours.oid == plumbing.ZeroHash || theirs.oid == plumbing.ZeroHash {
		return fileEntry{}, nil, false
	}
	mode := ours.mode
	if ours.mode == base.mode {
		mode = theirs.mode
	} else if theirs.mode != ours.mode && theirs.mode != base.mode {
		return fileEntry{}, nil, false
	}
	if ours.oid == base.oid {
		return fileEntry{mode, theirs.oid}, nil, true
	}
	if theirs.oid == base.oid || theirs.oid == ours.oid {
		return fileEntry{mode, ours.oid}, nil, true
	}
	for _, e := range []fileEntry{base, ours, theirs} {
		if e.mode != "100644" && e.mode != "100755" {
			return fileEntry{}, nil, false
		}
	}
	data, ok := combineText(g.getBytes(base.oid), g.getBytes(ours.oid), g.getBytes(theirs.oid))
	return fileEntry{mode, ours.oid}, data, ok
}
func (p *pull) plan(base, head plumbing.Hash) (map[string]fileEntry, map[string][]byte, bool) {
	g := p.repo.git
	files := g.files(base)
	material := make(map[string][]byte)
	conflict := g.refs[branchRef(p.base)] == plumbing.ZeroHash || g.refs[branchRef(p.head)] == plumbing.ZeroHash
	for _, d := range p.changedFiles(base, head) {
		ours := files[d.name]
		if ours != d.before && ours != d.after {
			e, data, ok := g.mergeEntry(d.before, ours, d.after)
			if !ok {
				conflict = true
				continue
			}
			files[d.name] = e
			if data != nil {
				material[d.name] = data
			}
			continue
		}
		if d.after.oid == plumbing.ZeroHash {
			delete(files, d.name)
		} else {
			files[d.name] = d.after
		}
	}
	for name := range files {
		for i := strings.IndexByte(name, '/'); i >= 0; {
			if _, ok := files[name[:i]]; ok {
				conflict = true
				break
			}
			j := strings.IndexByte(name[i+1:], '/')
			if j < 0 {
				break
			}
			i += j + 1
		}
	}
	return files, material, conflict
}
func (p *pull) mergeable() string {
	p.repo.git.mu.Lock()
	defer p.repo.git.mu.Unlock()
	b, h := p.tips()
	_, _, conflict := p.plan(b, h)
	if conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}
func (s *store) mergePull(p *pull, input mapJSON) mapJSON {
	if p.state != "open" {
		reject(405, "Pull request is closed")
	}
	r := p.repo
	r.git.mu.Lock()
	defer r.git.mu.Unlock()
	base, head := p.tips()
	if sha, ok := input["sha"]; ok && sha != head.String() {
		reject(409, "Head branch was modified")
	}
	files, material, conflict := p.plan(base, head)
	if conflict {
		reject(405, "Pull request is not mergeable")
	}
	method := optional(input["merge_method"], "squash")
	if method != "squash" && method != "merge" {
		reject(422, "Supported merge methods are squash and merge")
	}
	for name, data := range material {
		e := files[name]
		e.oid = r.git.put(plumbing.BlobObject, data)
		files[name] = e
	}
	title := optional(input["commit_title"], p.title+" (#"+strconv.Itoa(p.number)+")")
	if body := optional(input["commit_message"], p.body); body != "" {
		title += "\n\n" + body
	}
	parents := []plumbing.Hash{base}
	if method == "merge" {
		parents = append(parents, head)
	}
	id := s.commit(r, p.base, files, parents, title)
	now := s.now()
	p.state, p.merged, p.closed, p.updated, p.mergeCommit, p.headStart, p.baseStart = "closed", &now, &now, now, id, head, base
	return mapJSON{"sha": id.String(), "merged": true, "message": "Pull Request successfully merged"}
}

type pullFile struct {
	Filename  string `json:"filename"`
	SHA       string `json:"sha"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func (p *pull) files() []pullFile {
	g := p.repo.git
	g.mu.Lock()
	defer g.mu.Unlock()
	base, head := p.tips()
	out := []pullFile{}
	for _, d := range p.changedFiles(base, head) {
		status, id := "modified", d.after.oid
		if d.before.oid == plumbing.ZeroHash {
			status = "added"
		}
		if d.after.oid == plumbing.ZeroHash {
			status, id = "removed", d.before.oid
		}
		before, after := g.getBytes(d.before.oid), g.getBytes(d.after.oid)
		adds, dels := 0, 0
		if bytes.IndexByte(before, 0) < 0 && bytes.IndexByte(after, 0) < 0 {
			for _, h := range hunks(splitLines(before), splitLines(after)) {
				adds += len(h.lines)
				dels += h.end - h.start
			}
		}
		out = append(out, pullFile{d.name, id.String(), status, adds, dels, adds + dels})
	}
	return out
}
