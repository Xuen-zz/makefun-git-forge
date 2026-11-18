package main

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

const receiveService = "git-receive-pack"
const uploadService = "git-upload-pack"
const receiveCaps = "report-status delete-refs side-band-64k ofs-delta object-format=sha1 agent=forge-probe/1.0"
const uploadCaps = "shallow deepen-relative side-band-64k ofs-delta include-tag object-format=sha1 symref=HEAD:"

var invalidPackets = errors.New("invalid Git packet stream")
var advertiseV2 = appendPackets("version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n", "agent=forge-probe/1.0\n")

func packetHeader(dst []byte, size int) []byte {
	const digits = "0123456789abcdef"
	return append(dst, digits[size>>12], digits[size>>8&15], digits[size>>4&15], digits[size&15])
}

func packet(dst []byte, line string) []byte {
	size := len(line) + 4
	if size > 65520 {
		panic("oversized Git packet")
	}
	return append(packetHeader(dst, size), line...)
}

func appendPackets(lines ...string) []byte {
	var dst []byte
	for _, line := range lines {
		dst = packet(dst, line)
	}
	return append(dst, "0000"...)
}

func band(dst, data []byte) []byte {
	for len(data) > 0 {
		chunk := min(len(data), 65515)
		dst = append(packetHeader(dst, chunk+5), 1)
		dst = append(dst, data[:chunk]...)
		data = data[chunk:]
	}
	return append(dst, "0000"...)
}

func digit(value byte) int {
	switch {
	case value >= '0' && value <= '9':
		return int(value - '0')
	case value >= 'a' && value <= 'f':
		return int(value-'a') + 10
	case value >= 'A' && value <= 'F':
		return int(value-'A') + 10
	}
	return -1
}

func packets(body []byte) (lines []string, tail []byte, flushed bool, err error) {
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, nil, false, invalidPackets
		}
		a, b, c, d := digit(body[0]), digit(body[1]), digit(body[2]), digit(body[3])
		if a|b|c|d < 0 {
			return nil, nil, false, invalidPackets
		}
		size := a<<12 | b<<8 | c<<4 | d
		switch size {
		case 0:
			return lines, body[4:], true, nil
		case 1:
			body = body[4:]
		default:
			if size < 4 || size > 65520 || size > len(body) {
				return nil, nil, false, invalidPackets
			}
			lines = append(lines, string(bytes.TrimSuffix(body[4:size], []byte{'\n'})))
			body = body[size:]
		}
	}
	return lines, nil, false, nil
}

func (r *gitRepository) advertise(service string, v2 bool) []byte {
	if service == uploadService && v2 {
		return advertiseV2
	}
	if service == receiveService && r.receiveAdvertisement != nil {
		return r.receiveAdvertisement
	}
	result := appendPackets("# service=" + service + "\n")
	caps := receiveCaps
	if service == uploadService {
		caps = uploadCaps + r.headRef
	}
	caps = "\x00" + caps
	if service == uploadService {
		if id, ok := r.refs[r.headRef]; ok {
			result = packet(result, id.String()+" HEAD"+caps+"\n")
			caps = ""
		}
	}
	for _, ref := range r.refNames() {
		id := r.refs[ref]
		result = packet(result, id.String()+" "+ref+caps+"\n")
		caps = ""
		if service == uploadService {
			if peeled := r.peel(id); id != peeled {
				result = packet(result, peeled.String()+" "+ref+"^{}\n")
			}
		}
	}
	if len(r.refs) == 0 {
		result = packet(result, plumbing.ZeroHash.String()+" capabilities^{}"+caps+"\n")
	}
	result = append(result, "0000"...)
	if service == receiveService && len(result) <= 1<<18 {
		r.receiveAdvertisement = result
	}
	return result
}

type refUpdate struct {
	old, next plumbing.Hash
	ref       string
}

func (r *gitRepository) update(op refUpdate, staged *stagedPack) string {
	if r.refs[op.ref] != op.old {
		return "stale ref"
	}
	if op.next == plumbing.ZeroHash {
		if op.old != plumbing.ZeroHash {
			delete(r.refs, op.ref)
			r.invalidateAdvertisements()
		}
		return ""
	}
	target, err := staged.EncodedObject(plumbing.AnyObject, op.next)
	if err != nil || (strings.HasPrefix(op.ref, "refs/heads/") && target.Type() != plumbing.CommitObject) {
		return "missing or invalid target"
	}
	if op.old == plumbing.ZeroHash && r.refConflict(op.ref) {
		return "ref namespace collision"
	}
	objects, err := r.closure(staged, op.next)
	if err != nil {
		return "invalid object graph"
	}
	if op.old != plumbing.ZeroHash && op.old != op.next && !r.ancestor(op.old, op.next, objects) {
		return "non-fast-forward"
	}
	for id, meta := range objects {
		if _, err := r.db.SetEncodedObject(staged.objects[id]); err != nil {
			return "invalid object"
		}
		r.objects[id] = meta
	}
	if op.old != op.next {
		r.refs[op.ref] = op.next
		r.invalidateAdvertisements()
	}
	return ""
}

func (r *gitRepository) receive(body []byte) ([]byte, error) {
	lines, data, flushed, err := packets(body)
	if err != nil || !flushed {
		return nil, invalidPackets
	}
	var commands []refUpdate
	var capabilities []string
	for _, line := range lines {
		if strings.HasPrefix(line, "shallow ") {
			continue
		}
		command, caps, _ := strings.Cut(line, "\x00")
		if len(commands) == 0 {
			capabilities = strings.Fields(caps)
		}
		oldHex, rest, _ := strings.Cut(command, " ")
		nextHex, ref, _ := strings.Cut(rest, " ")
		if !strings.HasPrefix(ref, "refs/") || len(ref) > 4096 || plumbing.ReferenceName(ref).Validate() != nil {
			return nil, invalidPackets
		}
		old, okOld := parseHash(oldHex)
		next, okNext := parseHash(nextHex)
		if !okOld || !okNext {
			return nil, invalidPackets
		}
		commands = append(commands, refUpdate{old, next, ref})
	}
	staged, unpackErr := r.decodePack(data)
	report := slices.Contains(capabilities, "report-status")
	var out []byte
	if report {
		if unpackErr == nil {
			out = packet(out, "unpack ok\n")
		} else {
			out = packet(out, "unpack invalid pack\n")
		}
	}
	for _, op := range commands {
		message := "unpacker error"
		if unpackErr == nil {
			message = r.update(op, staged)
		}
		if report {
			if message == "" {
				out = packet(out, "ok "+op.ref+"\n")
			} else {
				out = packet(out, "ng "+op.ref+" "+message+"\n")
			}
		}
	}
	if !report {
		return nil, nil
	}
	out = append(out, "0000"...)
	if slices.Contains(capabilities, "side-band-64k") {
		return band(nil, out), nil
	}
	return out, nil
}

func (r *gitRepository) lsRefs(lines []string) []byte {
	var prefixes []string
	for _, line := range lines {
		if prefix, ok := strings.CutPrefix(line, "ref-prefix "); ok {
			prefixes = append(prefixes, prefix)
		}
	}
	match := func(name string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return len(prefixes) == 0
	}
	var data []byte
	if match("HEAD") {
		if head, ok := r.refs[r.headRef]; ok {
			line := head.String() + " HEAD"
			if slices.Contains(lines, "symrefs") {
				line += " symref-target:" + r.headRef
			}
			data = packet(data, line+"\n")
		} else if slices.Contains(lines, "unborn") {
			data = packet(data, "unborn HEAD symref-target:"+r.headRef+"\n")
		}
	}
	peel := slices.Contains(lines, "peel")
	for _, name := range r.refNames() {
		if !match(name) {
			continue
		}
		id := r.refs[name]
		line := id.String() + " " + name
		if peel {
			if target := r.peel(id); target != id {
				line += " peeled:" + target.String()
			}
		}
		data = packet(data, line+"\n")
	}
	return append(data, "0000"...)
}

func fetchSpec(lines []string) (shallowSpec, error) {
	spec := shallowSpec{shallow: make(map[plumbing.Hash]bool)}
	for _, line := range lines {
		word, value, _ := strings.Cut(line, " ")
		switch word {
		case "want", "have", "shallow":
			id, ok := parseHash(value)
			if !ok {
				return spec, invalidPackets
			}
			switch word {
			case "want":
				spec.wants = append(spec.wants, id)
			case "have":
				spec.haves = append(spec.haves, id)
			case "shallow":
				spec.shallow[id] = true
			}
		case "deepen":
			depth, err := strconv.Atoi(value)
			if err != nil || depth < 1 {
				return spec, invalidPackets
			}
			spec.depth = depth
		case "deepen-relative":
			spec.relative = true
		case "include-tag":
			spec.includeTag = true
		case "done":
			spec.done = true
		}
	}
	return spec, nil
}

func sortedHashes(ids map[plumbing.Hash]bool) []plumbing.Hash {
	result := make([]plumbing.Hash, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i][:], result[j][:]) < 0 })
	return result
}

func (r *gitRepository) fetch(spec shallowSpec, v2, v0Have, sideband bool) ([]byte, error) {
	common := make([]plumbing.Hash, 0, len(spec.haves))
	for _, id := range spec.haves {
		if _, ok := r.objects[id]; ok {
			common = append(common, id)
		}
	}
	for _, id := range spec.wants {
		if _, ok := r.objects[id]; !ok {
			return appendPackets("ERR upload-pack: not our ref\n"), nil
		}
	}
	if len(spec.wants) == 0 {
		return []byte("0000"), nil
	}
	if v2 && !spec.done {
		reply := packet(nil, "acknowledgments\n")
		if len(common) == 0 {
			reply = packet(reply, "NAK\n")
		}
		for _, id := range common {
			reply = packet(reply, "ACK "+id.String()+"\n")
		}
		return append(reply, "0000"...), nil
	}
	boundary, unshallow := r.shallowBoundary(spec)
	var out []byte
	if spec.depth > 0 || len(unshallow) > 0 || len(boundary) > 0 {
		if v2 {
			out = packet(out, "shallow-info\n")
		}
		for _, id := range sortedHashes(unshallow) {
			out = packet(out, "unshallow "+id.String()+"\n")
		}
		for _, id := range sortedHashes(boundary) {
			if !spec.shallow[id] {
				out = packet(out, "shallow "+id.String()+"\n")
			}
		}
		if v2 {
			out = append(out, "0001"...)
		} else {
			out = append(out, "0000"...)
		}
	}
	if !v2 && !spec.done {
		if v0Have || spec.depth == 0 {
			out = packet(out, "NAK\n")
		}
		return out, nil
	}
	known := make(map[plumbing.Hash]bool)
	r.walkObjects(common, spec.shallow, known, func(plumbing.Hash) {})
	seen := make(map[plumbing.Hash]bool)
	var selected []plumbing.Hash
	add := func(id plumbing.Hash) {
		if !known[id] {
			selected = append(selected, id)
		}
	}
	r.walkObjects(spec.wants, boundary, seen, add)
	if spec.includeTag {
		for _, name := range r.refNames() {
			id := r.refs[name]
			if target := r.peel(id); target != id && seen[target] {
				r.walkObjects([]plumbing.Hash{id}, boundary, seen, add)
			}
		}
	}
	var packed bytes.Buffer
	if _, err := packfile.NewEncoder(&packed, r.db, false).Encode(selected, 0); err != nil {
		return nil, err
	}
	if v2 {
		out = packet(out, "packfile\n")
	} else if len(common) > 0 {
		out = packet(out, "ACK "+common[0].String()+"\n")
	} else {
		out = packet(out, "NAK\n")
	}
	if v2 || sideband {
		return band(out, packed.Bytes()), nil
	}
	return append(out, packed.Bytes()...), nil
}

func (r *gitRepository) cachedUpload(body []byte) []byte {
	if cached := r.refsReply.Load(); cached != nil && bytes.Equal(body, cached.request) {
		return cached.response
	}
	if cached := r.fetchReply.Load(); cached != nil && bytes.Equal(body, cached.request) {
		return cached.response
	}
	return nil
}

func (r *gitRepository) upload(body []byte, v2 bool) ([]byte, error) {
	if v2 {
		if cached := r.cachedUpload(body); cached != nil {
			return cached, nil
		}
	}
	lines, tail, flushed, err := packets(body)
	if err != nil || !flushed || len(lines) == 0 {
		return nil, invalidPackets
	}
	var caps []string
	var haveTail bool
	if v2 {
		if len(tail) != 0 {
			return nil, invalidPackets
		}
		if lines[0] == "command=ls-refs" {
			out := r.lsRefs(lines)
			if len(body) <= 16384 && len(out) <= 262144 {
				r.refsReply.Store(&wireCache{bytes.Clone(body), out})
			}
			return out, nil
		}
		if lines[0] != "command=fetch" {
			return nil, invalidPackets
		}
	} else {
		first := strings.Fields(lines[0])
		if len(first) >= 2 {
			caps = first[2:]
			lines[0] = first[0] + " " + first[1]
		}
		rest, _, _, err := packets(tail)
		if err != nil {
			return nil, err
		}
		haveTail = len(rest) != 0
		lines = append(lines, rest...)
	}
	spec, err := fetchSpec(lines)
	if err != nil {
		return nil, err
	}
	spec.includeTag = spec.includeTag || slices.Contains(caps, "include-tag")
	spec.relative = spec.relative || slices.Contains(caps, "deepen-relative")
	out, err := r.fetch(spec, v2, haveTail, slices.Contains(caps, "side-band-64k"))
	if err == nil && v2 && spec.done && spec.depth == 1 && len(spec.haves) == 0 && len(spec.shallow) == 0 && len(body) <= 16384 && len(out) <= 1<<20 {
		r.fetchReply.Store(&wireCache{bytes.Clone(body), out})
	}
	return out, err
}

type httpReply struct {
	status              int
	typ                 string
	body                []byte
	challenge           bool
	link, scopes, allow string
}

func (s *store) authorized(credential string, git bool) bool {
	if s.token == "" {
		return true
	}
	if secret, ok := strings.CutPrefix(credential, "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(secret), []byte(s.token)) == 1
	}
	if secret, ok := strings.CutPrefix(credential, "token "); ok {
		return subtle.ConstantTimeCompare([]byte(secret), []byte(s.token)) == 1
	}
	if !git {
		return false
	}
	if encoded, ok := strings.CutPrefix(credential, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false
		}
		_, password, ok := bytes.Cut(raw, []byte{':'})
		return ok && subtle.ConstantTimeCompare(password, []byte(s.token)) == 1
	}
	return false
}

func (s *store) gitHTTP(method, login, name, route, service, protocol, credential, encoding string, body []byte) httpReply {
	notFound := httpReply{status: 404, typ: contentType, body: missing}
	bad := httpReply{status: 400, typ: contentType, body: []byte("{\"message\":\"Invalid Git request\"}")}
	if !s.authorized(credential, true) {
		return httpReply{status: 401, typ: contentType, body: []byte("{\"message\":\"Bad credentials\"}"), challenge: true}
	}
	if !strings.EqualFold(login, s.user.Login) {
		return notFound
	}
	s.mu.RLock()
	repo := s.repos[strings.ToLower(name)]
	s.mu.RUnlock()
	if repo == nil {
		return notFound
	}
	v2 := slices.Contains(strings.Split(protocol, ":"), "version=2")
	advertise := route == "info/refs"
	if advertise {
		if method != "GET" {
			return notFound
		}
	} else {
		if method != "POST" {
			return notFound
		}
		service = route
	}
	if service != receiveService && service != uploadService {
		return notFound
	}
	if !advertise && encoding == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return bad
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxBody+1))
		reader.Close()
		if err != nil || len(data) > maxBody {
			return bad
		}
		body = data
	}
	r := repo.git
	typ := "application/x-" + service
	if service == uploadService && v2 {
		if advertise {
			return httpReply{status: 200, typ: typ + "-advertisement", body: advertiseV2}
		}
		// Published replies and their request keys are immutable. Ref updates drop both entries.
		if cached := r.cachedUpload(body); cached != nil {
			return httpReply{status: 200, typ: typ + "-result", body: cached}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if advertise {
		return httpReply{status: 200, typ: typ + "-advertisement", body: r.advertise(service, v2)}
	}
	var out []byte
	var err error
	if service == receiveService {
		out, err = r.receive(body)
	} else {
		out, err = r.upload(body, v2)
	}
	if err != nil {
		return bad
	}
	return httpReply{status: 200, typ: typ + "-result", body: out}
}
