package main

import (
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
)

func jsonReply(status int, v any) httpReply {
	r := httpReply{status: status, typ: contentType}
	if status != 204 && status != 202 {
		r.body = encoded(v)
	}
	return r
}
func queryValue(q url.Values, name, defaultValue string) string {
	if q.Has(name) {
		return q.Get(name)
	}
	return defaultValue
}
func positive(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err == nil && n > 0 {
		return n
	}
	return fallback
}
func pageBounds(count, size, page int) (int, int) {
	if page-1 > count/size {
		return count, count
	}
	start := min(count, (page-1)*size)
	return start, min(count, start+size)
}
func pageJSON[T any](rows []T, query url.Values, origin, requestPath string) httpReply {
	size, page := min(100, positive(query.Get("per_page"), 30)), positive(query.Get("page"), 1)
	start, end := pageBounds(len(rows), size, page)
	r := jsonReply(200, rows[start:end])
	last := (len(rows) + size - 1) / size
	var links []string
	link := func(page int, rel string) {
		p := make(url.Values, len(query)+1)
		for k, value := range query {
			p[k] = value
		}
		p.Set("page", strconv.Itoa(page))
		links = append(links, "<"+origin+requestPath+"?"+p.Encode()+">; rel=\""+rel+"\"")
	}
	if page < last {
		link(page+1, "next")
		link(last, "last")
	}
	if page > 1 && last > 0 {
		link(1, "first")
		link(min(page-1, last), "prev")
	}
	r.link = strings.Join(links, ", ")
	return r
}
func parseBody(body []byte) mapJSON {
	if len(body) == 0 {
		return mapJSON{}
	}
	var data mapJSON
	if json.Unmarshal(body, &data) != nil || data == nil {
		reject(400, "Malformed JSON")
	}
	return data
}
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}
func scanPlainJSONString(b []byte, i int) (string, int, bool) {
	if i >= len(b) || b[i] != '"' {
		return "", i, false
	}
	i++
	start := i
	for i < len(b) {
		c := b[i]
		if c == '"' {
			return string(b[start:i]), i + 1, true
		}
		if c == '\\' || c < 0x20 || c >= 0x80 {
			return "", i, false
		}
		i++
	}
	return "", i, false
}
func parseIssueCreateFast(body []byte) (title, issueBody string, ok bool) {
	if len(body) == 0 || !utf8.Valid(body) {
		return "", "", false
	}
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return "", "", false
	}
	i = skipJSONSpace(body, i+1)
	if i >= len(body) || body[i] == '}' {
		return "", "", false
	}
	hasTitle := false
	for {
		key, next, okKey := scanPlainJSONString(body, i)
		if !okKey {
			return "", "", false
		}
		i = skipJSONSpace(body, next)
		if i >= len(body) || body[i] != ':' {
			return "", "", false
		}
		i = skipJSONSpace(body, i+1)
		val, nextVal, okVal := scanPlainJSONString(body, i)
		if !okVal {
			return "", "", false
		}
		switch key {
		case "title":
			title = val
			hasTitle = true
		case "body":
			issueBody = val
		default:
			return "", "", false
		}
		i = skipJSONSpace(body, nextVal)
		if i >= len(body) {
			return "", "", false
		}
		if body[i] == '}' {
			i = skipJSONSpace(body, i+1)
			if i != len(body) || !hasTitle || strings.TrimSpace(title) == "" {
				return "", "", false
			}
			return title, issueBody, true
		}
		if body[i] != ',' {
			return "", "", false
		}
		i = skipJSONSpace(body, i+1)
	}
}
func appendJSONString(dst []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			b, _ := json.Marshal(s)
			return append(dst, b...)
		}
	}
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&15])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
func (s *store) encodeIssue(i *issue) []byte {
	if len(i.Labels) != 0 || i.Closed != nil {
		return encoded(i)
	}
	dst := make([]byte, 0, 320+len(i.Title)+len(i.Body))
	dst = append(dst, "{\"id\":"...)
	dst = strconv.AppendInt(dst, int64(i.ID), 10)
	dst = append(dst, ",\"node_id\":"...)
	dst = appendJSONString(dst, i.NodeID)
	dst = append(dst, ",\"number\":"...)
	dst = strconv.AppendInt(dst, int64(i.Number), 10)
	dst = append(dst, ",\"title\":"...)
	dst = appendJSONString(dst, i.Title)
	dst = append(dst, ",\"body\":"...)
	dst = appendJSONString(dst, i.Body)
	dst = append(dst, ",\"state\":"...)
	dst = appendJSONString(dst, i.State)
	dst = append(dst, ",\"labels\":[],\"user\":"...)
	dst = append(dst, s.userJSON...)
	dst = append(dst, ",\"created_at\":"...)
	dst = appendJSONString(dst, i.Created)
	dst = append(dst, ",\"updated_at\":"...)
	dst = appendJSONString(dst, i.Updated)
	dst = append(dst, ",\"closed_at\":null,\"comments\":"...)
	dst = strconv.AppendInt(dst, int64(i.Comments), 10)
	return append(dst, '}')
}
func (s *store) rest(method, requestPath, origin, rawQuery string, body []byte) httpReply {
	path := strings.Trim(strings.TrimPrefix(requestPath, "/api/v3"), "/")
	var q url.Values
	if rawQuery != "" {
		q, _ = url.ParseQuery(rawQuery)
	}
	if method == "POST" && strings.HasPrefix(path, "repos/") && strings.HasSuffix(path, "/issues") {
		if title, issueBody, ok := parseIssueCreateFast(body); ok {
			seg0, restPath, ok := strings.Cut(path, "/")
			if ok && seg0 == "repos" {
				owner, restPath, ok := strings.Cut(restPath, "/")
				if ok {
					repoName, restPath, hasMore := strings.Cut(restPath, "/")
					if hasMore && restPath == "issues" {
						r := s.find(owner, repoName)
						if r == nil {
							reject(404, "Not Found")
						}
						i := s.createIssueValues(r, title, issueBody, []*label{})
						encodedBody := s.encodeIssue(i)
						r.issueJSON[i.Number] = encodedBody
						return httpReply{status: 201, typ: contentType, body: encodedBody}
					}
				}
			}
		}
	}
	var input mapJSON
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		input = parseBody(body)
	}
	if path == "" && method == "GET" {
		out := jsonReply(200, mapJSON{"current_user_url": origin + "/api/v3/user", "current_user_repositories_url": origin + "/api/v3/user/repos", "repository_url": origin + "/api/v3/repos/{owner}/{repo}"})
		out.scopes = "repo, read:org"
		return out
	}
	if path == "meta" && method == "GET" {
		return jsonReply(200, mapJSON{"installed_version": "1.0.0"})
	}
	if path == "user" && method == "GET" {
		return jsonReply(200, s.user)
	}
	if path == "user/repos" {
		if method == "POST" {
			return jsonReply(201, s.createRepo(input).view(origin))
		}
		if method == "GET" {
			repos := make([]*repository, 0, len(s.repos))
			for _, r := range s.repos {
				repos = append(repos, r)
			}
			sort.Slice(repos, func(i, j int) bool { return repos[i].id < repos[j].id })
			rows := make([]mapJSON, 0, len(repos))
			for _, r := range repos {
				rows = append(rows, r.view(origin))
			}
			return pageJSON(rows, q, origin, requestPath)
		}
	}
	seg0, restPath, ok := strings.Cut(path, "/")
	if !ok || seg0 != "repos" {
		reject(404, "Not Found")
	}
	owner, restPath, ok := strings.Cut(restPath, "/")
	if !ok {
		reject(404, "Not Found")
	}
	repoName, restPath, hasMore := strings.Cut(restPath, "/")
	r := s.find(owner, repoName)
	if r == nil {
		reject(404, "Not Found")
	}
	if !hasMore {
		switch method {
		case "GET":
			return jsonReply(200, r.view(origin))
		case "DELETE":
			s.deleteRepo(r)
			return jsonReply(204, nil)
		case "PATCH":
			if newBranch, ok := input["default_branch"].(string); ok {
				r.git.mu.Lock()
				defer r.git.mu.Unlock()
				if r.git.refs[branchRef(newBranch)] == plumbing.ZeroHash {
					reject(422, "Invalid default branch")
				}
				r.branch = newBranch
				r.git.headRef = branchRef(newBranch)
				r.git.invalidateAdvertisements()
			}
			r.description = optional(input["description"], r.description)
			if v, ok := input["private"].(bool); ok {
				r.private = v
			}
			if v, ok := input["has_issues"].(bool); ok {
				r.hasIssues = v
			}
			return jsonReply(200, r.view(origin))
		}
		reject(404, "Not Found")
	}
	section, tail, _ := strings.Cut(restPath, "/")
	switch section {
	case "issues":
		return s.restIssues(r, method, tail, requestPath, origin, q, input)
	case "pulls":
		return s.restPulls(r, method, tail, requestPath, origin, q, input)
	case "labels":
		if tail == "" && method == "POST" {
			return jsonReply(201, s.createLabel(r, input))
		}
		if tail == "" && method == "GET" {
			rows := make([]*label, 0, len(r.labels))
			for _, l := range r.labels {
				rows = append(rows, l)
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
			return pageJSON(rows, q, origin, requestPath)
		}
	case "git", "branches", "commits", "contents":
		r.git.mu.Lock()
		defer r.git.mu.Unlock()
		return s.restGit(r, method, section, tail, requestPath, origin, q, input)
	}
	reject(404, "Not Found")
	return httpReply{}
}
func (s *store) restGit(r *repository, method, section, tail, requestPath, origin string, q url.Values, input mapJSON) httpReply {
	g := r.git
	switch section {
	case "branches":
		if method != "GET" {
			break
		}
		view := func(ref string, id plumbing.Hash) mapJSON {
			return mapJSON{"name": strings.TrimPrefix(ref, "refs/heads/"), "commit": mapJSON{"sha": id.String()}, "protected": false}
		}
		if tail != "" {
			ref := branchRef(tail)
			id := g.refs[ref]
			if id == plumbing.ZeroHash {
				reject(404, "Not Found")
			}
			return jsonReply(200, view(ref, id))
		}
		rows := []mapJSON{}
		for _, ref := range g.refNames() {
			if strings.HasPrefix(ref, "refs/heads/") {
				rows = append(rows, view(ref, g.refs[ref]))
			}
		}
		return pageJSON(rows, q, origin, requestPath)
	case "commits":
		if method != "GET" {
			break
		}
		ref := queryValue(q, "sha", r.branch)
		if tail != "" {
			ref = tail
		}
		id := g.resolve(ref)
		view := func(oid plumbing.Hash) mapJSON {
			parents := []mapJSON{}
			for _, p := range g.objects[oid].parents {
				parents = append(parents, mapJSON{"sha": p.String()})
			}
			node := g.objects[oid]
			return mapJSON{"sha": oid.String(), "commit": mapJSON{"message": g.commitMessage(oid), "tree": mapJSON{"sha": node.children[0].String()}}, "parents": parents}
		}
		if tail != "" {
			if id == plumbing.ZeroHash {
				reject(404, "Not Found")
			}
			return jsonReply(200, view(id))
		}
		if id == plumbing.ZeroHash && q.Has("sha") {
			reject(404, "Not Found")
		}
		rows := []mapJSON{}
		for _, oid := range g.history(id) {
			rows = append(rows, view(oid))
		}
		return pageJSON(rows, q, origin, requestPath)
	case "git":
		if tail == "refs" && method == "POST" {
			ref := stringVal(input["ref"])
			id, ok := parseHash(stringVal(input["sha"]))
			meta, exists := g.objects[id]
			if !ok || !exists || !strings.HasPrefix(ref, "refs/") || plumbing.ReferenceName(ref).Validate() != nil || (strings.HasPrefix(ref, "refs/heads/") && meta.kind != plumbing.CommitObject) {
				reject(422, "Invalid ref or SHA")
			}
			if g.refs[ref] != plumbing.ZeroHash || g.refConflict(ref) {
				reject(422, "Reference already exists")
			}
			g.setRef(ref, id)
			return jsonReply(201, mapJSON{"ref": ref, "object": mapJSON{"sha": id.String(), "type": meta.kind.String()}})
		}
		if rest, ok := strings.CutPrefix(tail, "ref/"); ok && method == "GET" {
			ref := "refs/" + rest
			id := g.refs[ref]
			if id == plumbing.ZeroHash {
				reject(404, "Not Found")
			}
			return jsonReply(200, mapJSON{"ref": ref, "object": mapJSON{"sha": id.String(), "type": g.objects[id].kind.String()}})
		}
	case "contents":
		if method == "GET" {
			return jsonReply(200, r.contents(tail, queryValue(q, "ref", r.branch)))
		}
		if method == "PUT" || method == "DELETE" {
			status, v := s.updateFile(r, tail, input, method == "DELETE")
			return jsonReply(status, v)
		}
	}
	reject(404, "Not Found")
	return httpReply{}
}
func (s *store) readCachedIssue(path string) []byte {
	repoPath, ok := strings.CutPrefix(path, "/api/v3/repos/")
	if !ok {
		return nil
	}
	owner, tail, _ := strings.Cut(repoPath, "/")
	name, tail, _ := strings.Cut(tail, "/")
	item, ok := strings.CutPrefix(tail, "issues/")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(item)
	if err != nil || n <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r := s.find(owner, name); r != nil {
		return r.issueJSON[n]
	}
	return nil
}

func (s *store) restIssues(r *repository, method, tail, requestPath, origin string, q url.Values, input mapJSON) httpReply {
	if tail == "" {
		if method == "POST" {
			i := s.createIssue(r, input)
			encodedBody := s.encodeIssue(i)
			r.issueJSON[i.Number] = encodedBody
			return httpReply{status: 201, typ: contentType, body: encodedBody}
		}
		if method == "GET" {
			asc := q.Get("direction") == "asc"
			rows := r.issueList(queryValue(q, "state", "open"), asc)
			if q.Has("labels") {
				want := strings.Split(strings.ToLower(q.Get("labels")), ",")
				rows = slices.DeleteFunc(rows, func(i *issue) bool {
					for _, l := range want {
						if !slices.ContainsFunc(i.Labels, func(x *label) bool { return strings.EqualFold(x.Name, l) }) {
							return true
						}
					}
					return false
				})
			}
			if q.Get("sort") == "updated" {
				sort.SliceStable(rows, func(a, b int) bool {
					if asc {
						return rows[a].Updated < rows[b].Updated
					}
					return rows[a].Updated > rows[b].Updated
				})
			}
			return pageJSON(rows, q, origin, requestPath)
		}
	}
	item, part, _ := strings.Cut(tail, "/")
	n := positive(item, 0)
	i := r.issues[n]
	p := r.pulls[n]
	if part == "comments" && (i != nil || p != nil) {
		if method == "POST" {
			return jsonReply(201, s.addComment(r, n, input))
		}
		if method == "GET" {
			rows := r.comments[n]
			if rows == nil {
				rows = []*comment{}
			}
			return pageJSON(rows, q, origin, requestPath)
		}
	}
	if part == "" {
		if method == "GET" {
			if i != nil {
				data := r.issueJSON[n]
				if data == nil {
					data = s.encodeIssue(i)
					r.issueJSON[n] = data
				}
				return httpReply{status: 200, typ: contentType, body: data}
			}
			if p != nil {
				row := p.view()
				row["pull_request"] = mapJSON{"url": r.apiPath() + "/pulls/" + item}
				return jsonReply(200, row)
			}
		}
		if method == "PATCH" && i != nil {
			return jsonReply(200, s.editIssue(i, input))
		}
	}
	reject(404, "Not Found")
	return httpReply{}
}
func (s *store) restPulls(r *repository, method, tail, requestPath, origin string, q url.Values, input mapJSON) httpReply {
	if tail == "" {
		if method == "POST" {
			return jsonReply(201, s.createPull(r, input).view())
		}
		if method == "GET" {
			rows := []mapJSON{}
			for _, p := range r.pullList(queryValue(q, "state", "open"), q.Get("direction") == "asc") {
				if h := q.Get("head"); h != "" && h != p.head && h != r.user.Login+":"+p.head {
					continue
				}
				if b := q.Get("base"); b != "" && b != p.base {
					continue
				}
				rows = append(rows, p.view())
			}
			return pageJSON(rows, q, origin, requestPath)
		}
	}
	num, part, _ := strings.Cut(tail, "/")
	p := r.pulls[positive(num, 0)]
	if p != nil {
		switch part {
		case "":
			if method == "GET" {
				return jsonReply(200, p.view())
			}
			if method == "PATCH" {
				s.editPull(p, input)
				return jsonReply(200, p.view())
			}
		case "files":
			if method == "GET" {
				return pageJSON(p.files(), q, origin, requestPath)
			}
		case "merge":
			if method == "PUT" {
				return jsonReply(200, s.mergePull(p, input))
			}
			if method == "GET" && p.merged != nil {
				return jsonReply(204, nil)
			}
		}
	}
	reject(404, "Not Found")
	return httpReply{}
}
