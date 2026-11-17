package main

import (
	"cmp"
	"encoding/base64"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

func graphqlUser(u user) mapJSON {
	return mapJSON{"__typename": "User", "id": u.NodeID, "databaseId": u.ID, "login": u.Login}
}
func graphqlComment(c *comment) mapJSON {
	return mapJSON{"__typename": "IssueComment", "id": c.NodeID, "databaseId": c.ID, "body": c.Body, "author": graphqlUser(c.User), "createdAt": c.Created, "updatedAt": c.Updated}
}
func valueDate(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
func cursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + key)) }
func connection[T any](rows []T, args mapJSON, key func(T) string, view func(T) mapJSON) mapJSON {
	count := 30
	_, first := args["first"]
	_, last := args["last"]
	if first {
		count = intVal(args["first"])
	}
	if last {
		count = intVal(args["last"])
	}
	if count < 0 || count > 100 || (first && last) {
		reject(422, "Invalid page size")
	}
	index := func(encoded string) int {
		for i, r := range rows {
			if cursor(key(r)) == encoded {
				return i
			}
		}
		reject(422, "Invalid cursor")
		return 0
	}
	start, end := 0, len(rows)
	if after := stringVal(args["after"]); after != "" {
		start = index(after) + 1
	}
	if before := stringVal(args["before"]); before != "" {
		end = index(before)
	}
	end = max(start, end)
	if last {
		start = max(start, end-count)
	} else {
		end = min(end, start+count)
	}
	items, edges := make([]mapJSON, 0, end-start), make([]mapJSON, 0, end-start)
	var firstCursor, lastCursor any
	for _, r := range rows[start:end] {
		c := cursor(key(r))
		v := view(r)
		if firstCursor == nil {
			firstCursor = c
		}
		lastCursor = c
		items = append(items, v)
		edges = append(edges, mapJSON{"node": v, "cursor": c})
	}
	return mapJSON{"totalCount": len(rows), "nodes": items, "edges": edges, "pageInfo": mapJSON{"hasNextPage": end < len(rows), "hasPreviousPage": start > 0,
		"startCursor": firstCursor, "endCursor": lastCursor}}
}
func commentConnection(r *repository, n int, args mapJSON) any {
	return connection(r.comments[n], args, func(c *comment) string { return strconv.Itoa(c.ID) }, graphqlComment)
}
func graphqlIssue(i *issue) mapJSON {
	if i == nil {
		return nil
	}
	return mapJSON{"__typename": "Issue", "id": i.NodeID, "databaseId": i.ID, "number": i.Number, "title": i.Title, "body": i.Body,
		"state": strings.ToUpper(i.State), "closed": i.State == "closed", "createdAt": i.Created, "updatedAt": i.Updated, "closedAt": valueDate(i.Closed), "author": graphqlUser(i.User),
		"repository": gqlField(func(args mapJSON) any { return graphqlRepo(i.repo) }),
		"comments":   gqlField(func(args mapJSON) any { return commentConnection(i.repo, i.Number, args) }),
		"labels": gqlField(func(args mapJSON) any {
			return connection(i.Labels, args, func(l *label) string { return strconv.Itoa(l.ID) }, func(l *label) mapJSON {
				return mapJSON{"name": l.Name, "color": l.Color, "description": valueDate(l.Description)}
			})
		})}
}
func (p *pull) graphState() string {
	if p.merged != nil {
		return "MERGED"
	}
	return strings.ToUpper(p.state)
}
func graphqlPull(p *pull) mapJSON {
	if p == nil {
		return nil
	}
	return mapJSON{"__typename": "PullRequest", "id": p.nodeID, "databaseId": p.id, "number": p.number, "title": p.title, "body": p.body, "state": p.graphState(),
		"headRefName": p.head, "baseRefName": p.base, "merged": p.merged != nil, "closed": p.state == "closed", "createdAt": p.created, "updatedAt": p.updated,
		"closedAt": valueDate(p.closed), "mergedAt": valueDate(p.merged), "author": graphqlUser(p.repo.user),
		"repository": gqlField(func(args mapJSON) any { return graphqlRepo(p.repo) }),
		"headRefOid": gqlField(func(args mapJSON) any {
			p.repo.git.mu.Lock()
			defer p.repo.git.mu.Unlock()
			_, h := p.tips()
			return h.String()
		}),
		"baseRefOid": gqlField(func(args mapJSON) any {
			p.repo.git.mu.Lock()
			defer p.repo.git.mu.Unlock()
			b, _ := p.tips()
			return b.String()
		}),
		"mergeCommit": gqlField(func(args mapJSON) any {
			if p.merged == nil {
				return nil
			}
			return graphqlCommit(p.repo, p.mergeCommit)
		}),
		"mergeable": gqlField(func(args mapJSON) any {
			if p.state != "open" {
				return "UNKNOWN"
			}
			return p.mergeable()
		}),
		"files": gqlField(func(args mapJSON) any {
			return connection(p.files(), args, func(f pullFile) string { return f.Filename }, func(f pullFile) mapJSON {
				state := strings.ToUpper(f.Status)
				if state == "REMOVED" {
					state = "DELETED"
				}
				return mapJSON{"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": state}
			})
		}),
		"comments": gqlField(func(args mapJSON) any { return commentConnection(p.repo, p.number, args) })}
}
func graphqlCommit(r *repository, id plumbing.Hash) mapJSON {
	view := mapJSON{"oid": id.String()}
	view["message"] = gqlField(func(args mapJSON) any {
		r.git.mu.Lock()
		defer r.git.mu.Unlock()
		return r.git.commitMessage(id)
	})
	view["messageHeadline"] = gqlField(func(args mapJSON) any {
		r.git.mu.Lock()
		defer r.git.mu.Unlock()
		line, _, _ := strings.Cut(r.git.commitMessage(id), "\n")
		return line
	})
	return view
}
func graphqlRef(r *repository, full string) any {
	if !strings.HasPrefix(full, "refs/") {
		full = branchRef(full)
	}
	r.git.mu.Lock()
	defer r.git.mu.Unlock()
	id := r.git.refs[full]
	if id == plumbing.ZeroHash {
		return nil
	}
	category, name, _ := strings.Cut(strings.TrimPrefix(full, "refs/"), "/")
	return mapJSON{"name": name, "prefix": "refs/" + category + "/", "target": graphqlCommit(r, id)}
}
func gqlStates(input any, state string) bool {
	if values, ok := input.([]any); ok {
		return slices.Contains(values, any(state))
	}
	return true
}
func gqlHasLabels(list []*label, input any) bool {
	values, ok := input.([]any)
	if !ok {
		return true
	}
	for _, name := range values {
		if !slices.ContainsFunc(list, func(l *label) bool { return strings.EqualFold(l.Name, stringVal(name)) }) {
			return false
		}
	}
	return true
}
func graphqlRepo(r *repository) mapJSON {
	if r == nil {
		return nil
	}
	return mapJSON{"__typename": "Repository", "id": r.nodeID, "databaseId": r.id, "name": r.name, "nameWithOwner": r.fullName(), "owner": graphqlUser(r.user),
		"description": r.description, "viewerPermission": "ADMIN", "hasIssuesEnabled": r.hasIssues, "isPrivate": r.private,
		"defaultBranchRef": gqlField(func(args mapJSON) any { return graphqlRef(r, r.branch) }),
		"ref":              gqlField(func(args mapJSON) any { return graphqlRef(r, stringVal(args["qualifiedName"])) }),
		"issue":            gqlField(func(args mapJSON) any { return graphqlIssue(r.issues[intVal(args["number"])]) }),
		"pullRequest":      gqlField(func(args mapJSON) any { return graphqlPull(r.pulls[intVal(args["number"])]) }),
		"issues": gqlField(func(args mapJSON) any {
			rows := r.issueList("all", true)
			rows = slices.DeleteFunc(rows, func(i *issue) bool {
				return !gqlStates(args["states"], strings.ToUpper(i.State)) || !gqlHasLabels(i.Labels, args["labels"])
			})
			order := objectVal(args["orderBy"])
			slices.SortStableFunc(rows, func(a, b *issue) int {
				value := 0
				switch order["field"] {
				case "UPDATED_AT":
					value = cmp.Compare(a.Updated, b.Updated)
				case "COMMENTS":
					value = cmp.Compare(a.Comments, b.Comments)
				}
				if value == 0 {
					value = cmp.Compare(a.Number, b.Number)
				}
				if order["direction"] != "ASC" {
					return -value
				}
				return value
			})
			return connection(rows, args, func(i *issue) string { return strconv.Itoa(i.Number) }, graphqlIssue)
		}),
		"pullRequests": gqlField(func(args mapJSON) any {
			rows := r.pullList("all", true)
			h, b := stringVal(args["headRefName"]), stringVal(args["baseRefName"])
			rows = slices.DeleteFunc(rows, func(p *pull) bool {
				return !gqlStates(args["states"], p.graphState()) || (h != "" && h != p.head) || (b != "" && b != p.base)
			})
			order := objectVal(args["orderBy"])
			slices.SortStableFunc(rows, func(a, b *pull) int {
				value := 0
				if order["field"] == "UPDATED_AT" {
					value = cmp.Compare(a.updated, b.updated)
				}
				if value == 0 {
					value = cmp.Compare(a.number, b.number)
				}
				if order["direction"] != "ASC" {
					return -value
				}
				return value
			})
			return connection(rows, args, func(p *pull) string { return strconv.Itoa(p.number) }, graphqlPull)
		})}
}
func (s *store) entity(id string) any {
	if id == s.user.NodeID {
		return graphqlUser(s.user)
	}
	switch v := s.nodes[id].(type) {
	case *repository:
		return graphqlRepo(v)
	case *issue:
		return graphqlIssue(v)
	case *pull:
		return graphqlPull(v)
	case *comment:
		return graphqlComment(v)
	}
	return nil
}
func (s *store) searchGraphQL(args mapJSON) any {
	repoName, state := "", ""
	onlyIssue, onlyPR := false, false
	var terms []string
	for _, term := range strings.Fields(strings.ToLower(stringVal(args["query"]))) {
		k, value, filter := strings.Cut(term, ":")
		if !filter {
			terms = append(terms, term)
			continue
		}
		if k == "repo" {
			repoName = value
		}
		if k == "state" || k == "is" {
			switch value {
			case "open", "closed", "merged":
				state = value
			case "issue":
				onlyIssue = true
			case "pr":
				onlyPR = true
			}
		}
	}
	matches := func(v string) bool {
		v = strings.ToLower(v)
		for _, t := range terms {
			if !strings.Contains(v, t) {
				return false
			}
		}
		return true
	}
	hits := []mapJSON{}
	keys := make([]string, 0, len(s.repos))
	for k := range s.repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kind := stringVal(args["type"])
	for _, key := range keys {
		r := s.repos[key]
		if repoName != "" && repoName != strings.ToLower(r.fullName()) {
			continue
		}
		if kind == "REPOSITORY" && matches(r.fullName()+" "+r.description) {
			hits = append(hits, graphqlRepo(r))
		}
		if kind != "ISSUE" {
			continue
		}
		if !onlyPR {
			for _, i := range r.issueList("all", true) {
				if (state == "" || state == i.State) && matches(i.Title+" "+i.Body) {
					hits = append(hits, graphqlIssue(i))
				}
			}
		}
		if !onlyIssue {
			for _, p := range r.pullList("all", true) {
				if (state == "" || strings.EqualFold(state, p.graphState())) && matches(p.title+" "+p.body) {
					hits = append(hits, graphqlPull(p))
				}
			}
		}
	}
	if kind == "USER" && matches(s.user.Login) {
		hits = append(hits, graphqlUser(s.user))
	}
	out := connection(hits, args, func(v mapJSON) string { return stringVal(v["id"]) }, func(v mapJSON) mapJSON { return v })
	out["issueCount"], out["repositoryCount"], out["userCount"] = 0, 0, 0
	if k := map[string]string{"ISSUE": "issueCount", "REPOSITORY": "repositoryCount", "USER": "userCount"}[kind]; k != "" {
		out[k] = len(hits)
	}
	return out
}
func (s *store) queryRoot() mapJSON {
	mutation := func(run func(mapJSON) mapJSON) gqlField {
		return func(args mapJSON) any {
			in := objectVal(args["input"])
			r := run(in)
			r["clientMutationId"] = in["clientMutationId"]
			return r
		}
	}
	state := func(next string) gqlField {
		return mutation(func(in mapJSON) mapJSON {
			i, _ := s.nodes[stringVal(in["issueId"])].(*issue)
			if i == nil {
				reject(404, "Not Found")
			}
			return mapJSON{"issue": graphqlIssue(s.editIssue(i, mapJSON{"state": next}))}
		})
	}
	return mapJSON{"viewer": gqlField(func(args mapJSON) any { return graphqlUser(s.user) }),
		"repository": gqlField(func(args mapJSON) any { return graphqlRepo(s.find(stringVal(args["owner"]), stringVal(args["name"]))) }),
		"node":       gqlField(func(args mapJSON) any { return s.entity(stringVal(args["id"])) }),
		"nodes": gqlField(func(args mapJSON) any {
			out := []any{}
			ids, _ := args["ids"].([]any)
			for _, id := range ids {
				out = append(out, s.entity(stringVal(id)))
			}
			return out
		}),
		"search": gqlField(s.searchGraphQL),
		"createIssue": mutation(func(in mapJSON) mapJSON {
			r, _ := s.nodes[stringVal(in["repositoryId"])].(*repository)
			if r == nil {
				reject(404, "Not Found")
			}
			return mapJSON{"issue": graphqlIssue(s.createIssue(r, in))}
		}),
		"closeIssue": state("closed"), "reopenIssue": state("open"),
		"createPullRequest": mutation(func(in mapJSON) mapJSON {
			r, _ := s.nodes[stringVal(in["repositoryId"])].(*repository)
			if r == nil {
				reject(404, "Not Found")
			}
			return mapJSON{"pullRequest": graphqlPull(s.createPull(r, mapJSON{"title": in["title"], "body": in["body"], "head": in["headRefName"], "base": in["baseRefName"]}))}
		}),
		"mergePullRequest": mutation(func(in mapJSON) mapJSON {
			p, _ := s.nodes[stringVal(in["pullRequestId"])].(*pull)
			if p == nil {
				reject(404, "Not Found")
			}
			data := mapJSON{"commit_title": in["commitHeadline"], "commit_message": in["commitBody"]}
			if method := stringVal(in["mergeMethod"]); method != "" {
				data["merge_method"] = strings.ToLower(method)
			}
			if sha := stringVal(in["expectedHeadOid"]); sha != "" {
				data["sha"] = sha
			}
			s.mergePull(p, data)
			return mapJSON{"pullRequest": graphqlPull(p)}
		}),
		"addComment": mutation(func(in mapJSON) mapJSON {
			var r *repository
			var n int
			switch x := s.nodes[stringVal(in["subjectId"])].(type) {
			case *issue:
				r, n = x.repo, x.Number
			case *pull:
				r, n = x.repo, x.number
			}
			if r == nil {
				reject(404, "Not Found")
			}
			c := s.addComment(r, n, in)
			return mapJSON{"commentEdge": mapJSON{"cursor": cursor(strconv.Itoa(c.ID)), "node": graphqlComment(c)}}
		})}
}
