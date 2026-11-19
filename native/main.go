package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

type mapJSON = map[string]any

const maxBody = 64 << 20
const contentType = "application/json; charset=utf-8"

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string        { return e.message }
func reject(status int, message string) { panic(apiError{status, message}) }

type user struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	Login  string `json:"login"`
	Type   string `json:"type"`
}
type label struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Description *string `json:"description"`
}
type issue struct {
	ID       int      `json:"id"`
	NodeID   string   `json:"node_id"`
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	State    string   `json:"state"`
	Labels   []*label `json:"labels"`
	User     user     `json:"user"`
	Created  string   `json:"created_at"`
	Updated  string   `json:"updated_at"`
	Closed   *string  `json:"closed_at"`
	Comments int      `json:"comments"`
	repo     *repository
}
type pull struct {
	id                                int
	nodeID                            string
	number                            int
	title, body, state, head, base    string
	headStart, baseStart, mergeCommit plumbing.Hash
	created, updated                  string
	closed, merged                    *string
	repo                              *repository
}
type comment struct {
	ID      int    `json:"id"`
	NodeID  string `json:"node_id"`
	Body    string `json:"body"`
	User    user   `json:"user"`
	Created string `json:"created_at"`
	Updated string `json:"updated_at"`
}
type repository struct {
	id, nextNumber                             int
	name, description, branch, created, nodeID string
	private, hasIssues                         bool
	user                                       user
	issues                                     map[int]*issue
	issueJSON                                  map[int][]byte
	pulls                                      map[int]*pull
	labels                                     map[string]*label
	comments                                   map[int][]*comment
	git                                        *gitRepository
}
type store struct {
	mu       sync.RWMutex
	repos    map[string]*repository
	nodes    map[string]any
	sequence atomic.Uint64
	revision atomic.Uint64
	nextID   int
	clock    time.Time
	user     user
	userJSON []byte
	token    string
	gql      *querySchema
}

func newStore(login, token, epoch string) *store {
	clock, err := time.Parse(time.RFC3339Nano, epoch)
	if err != nil {
		clock = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	u := user{1, nodeID("User", 1), login, "User"}
	s := &store{repos: make(map[string]*repository), nodes: make(map[string]any), nextID: 1001, clock: clock.UTC(), user: u, userJSON: encoded(u), token: token}
	s.gql = newQuerySchema(s)
	return s
}
func (s *store) allocate() int      { id := s.nextID; s.nextID++; return id }
func (s *store) advance() time.Time { t := s.clock; s.clock = s.clock.Add(time.Second); return t }
func (s *store) now() string {
	t := s.advance()
	year, month, day := t.Date()
	hour, minute, sec := t.Clock()
	ms := t.Nanosecond() / 1_000_000
	var buf [24]byte
	buf[0] = byte('0' + year/1000%10)
	buf[1] = byte('0' + year/100%10)
	buf[2] = byte('0' + year/10%10)
	buf[3] = byte('0' + year%10)
	buf[4] = '-'
	buf[5] = byte('0' + int(month)/10)
	buf[6] = byte('0' + int(month)%10)
	buf[7] = '-'
	buf[8] = byte('0' + day/10)
	buf[9] = byte('0' + day%10)
	buf[10] = 'T'
	buf[11] = byte('0' + hour/10)
	buf[12] = byte('0' + hour%10)
	buf[13] = ':'
	buf[14] = byte('0' + minute/10)
	buf[15] = byte('0' + minute%10)
	buf[16] = ':'
	buf[17] = byte('0' + sec/10)
	buf[18] = byte('0' + sec%10)
	buf[19] = '.'
	buf[20] = byte('0' + ms/100)
	buf[21] = byte('0' + ms/10%10)
	buf[22] = byte('0' + ms%10)
	buf[23] = 'Z'
	return string(buf[:])
}
func nodeID(kind string, id int) string {
	var raw [48]byte
	if len(kind)+1+20 <= len(raw) {
		n := copy(raw[:], kind)
		raw[n] = ':'
		b := strconv.AppendInt(raw[:n+1], int64(id), 10)
		var enc [64]byte
		base64.RawURLEncoding.Encode(enc[:], b)
		return string(enc[:base64.RawURLEncoding.EncodedLen(len(b))])
	}
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(id)))
}
func stringVal(v any) string  { x, _ := v.(string); return x }
func objectVal(v any) mapJSON { x, _ := v.(mapJSON); return x }
func optional(v any, fallback string) string {
	if x, ok := v.(string); ok {
		return x
	}
	return fallback
}
func required(v any, field string) string {
	x := stringVal(v)
	if strings.TrimSpace(x) == "" {
		reject(422, field+" is required")
	}
	return x
}
func intVal(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		if float64(int(n)) == n {
			return int(n)
		}
	case json.Number:
		x, _ := strconv.Atoi(string(n))
		return x
	}
	return 0
}
func encoded(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || c >= 0x80 {
			return strings.ToLower(s)
		}
	}
	return s
}
func repoKey(name string) string     { return asciiLower(name) }
func branchRef(branch string) string { return "refs/heads/" + branch }

var legalName = regexp.MustCompile("^[A-Za-z0-9_.-]{1,100}$")
var legalColor = regexp.MustCompile("^[0-9a-fA-F]{6}$")

func (s *store) find(owner, name string) *repository {
	if !strings.EqualFold(s.user.Login, owner) {
		return nil
	}
	return s.repos[repoKey(name)]
}
func (s *store) createRepo(data mapJSON) *repository {
	name := required(data["name"], "name")
	key := repoKey(name)
	if !legalName.MatchString(name) || name == "." || name == ".." || strings.HasSuffix(key, ".git") {
		reject(422, "Invalid repository name")
	}
	if s.repos[key] != nil {
		reject(422, "Repository already exists")
	}
	id := s.allocate()
	r := &repository{id: id, nodeID: nodeID("Repository", id), nextNumber: 1, name: name, branch: "main", description: stringVal(data["description"]), created: s.now(), private: data["private"] == true, hasIssues: data["has_issues"] != false, user: s.user,
		issues: make(map[int]*issue), issueJSON: make(map[int][]byte), pulls: make(map[int]*pull), labels: make(map[string]*label), comments: make(map[int][]*comment), git: newGitRepository()}
	if data["auto_init"] == true {
		blob := r.git.put(plumbing.BlobObject, []byte("# "+r.name+"\n"))
		s.commit(r, r.branch, map[string]fileEntry{"README.md": {"100644", blob}}, nil, "Initial commit")
	}
	s.repos[key] = r
	s.nodes[r.nodeID] = r
	return r
}
func (s *store) deleteRepo(r *repository) {
	delete(s.repos, repoKey(r.name))
	delete(s.nodes, r.nodeID)
	for _, i := range r.issues {
		delete(s.nodes, i.NodeID)
	}
	for _, p := range r.pulls {
		delete(s.nodes, p.nodeID)
	}
	for _, cs := range r.comments {
		for _, c := range cs {
			delete(s.nodes, c.NodeID)
		}
	}
}
func (r *repository) fullName() string { return r.user.Login + "/" + r.name }
func (r *repository) apiPath() string  { return "/api/v3/repos/" + r.fullName() }
func (r *repository) view(origin string) mapJSON {
	visibility := "public"
	if r.private {
		visibility = "private"
	}
	return mapJSON{"id": r.id, "node_id": r.nodeID, "name": r.name, "full_name": r.fullName(), "owner": r.user,
		"private": r.private, "visibility": visibility, "description": r.description, "default_branch": r.branch, "has_issues": r.hasIssues,
		"fork": false, "archived": false, "created_at": r.created, "url": origin + r.apiPath(), "clone_url": origin + "/" + r.fullName() + ".git",
		"permissions": mapJSON{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true}}
}
func (s *store) createLabel(r *repository, input mapJSON) *label {
	name := required(input["name"], "name")
	color := optional(input["color"], "ededed")
	if !legalColor.MatchString(color) || r.labels[strings.ToLower(name)] != nil {
		reject(422, "Invalid or duplicate label")
	}
	l := &label{ID: s.allocate(), Name: name, Color: color}
	if x, ok := input["description"].(string); ok {
		l.Description = &x
	}
	r.labels[strings.ToLower(name)] = l
	return l
}
func issueLabels(r *repository, input any) []*label {
	result := make([]*label, 0)
	names, ok := input.([]any)
	if !ok {
		reject(422, "labels must be a list")
	}
	seen := make(map[string]bool)
	for _, v := range names {
		name := strings.ToLower(stringVal(v))
		found := r.labels[name]
		if found == nil {
			reject(422, "Label does not exist: "+stringVal(v))
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, found)
		}
	}
	return result
}
func (s *store) createIssueValues(r *repository, title, body string, labels []*label) *issue {
	if !r.hasIssues {
		reject(410, "Issues are disabled")
	}
	id, date := s.allocate(), s.now()
	i := &issue{ID: id, NodeID: nodeID("Issue", id), Number: r.nextNumber, Title: title, Body: body, State: "open", Labels: labels, User: s.user, Created: date, Updated: date, repo: r}
	r.nextNumber++
	r.issues[i.Number] = i
	s.nodes[i.NodeID] = i
	return i
}
func (s *store) createIssue(r *repository, input mapJSON) *issue {
	if !r.hasIssues {
		reject(410, "Issues are disabled")
	}
	title := required(input["title"], "title")
	labels := []*label{}
	if v, ok := input["labels"]; ok {
		labels = issueLabels(r, v)
	}
	return s.createIssueValues(r, title, stringVal(input["body"]), labels)
}
func (s *store) editIssue(i *issue, input mapJSON) *issue {
	title, labels := i.Title, i.Labels
	if v, ok := input["title"]; ok {
		title = required(v, "title")
	}
	if v, ok := input["labels"]; ok {
		labels = issueLabels(i.repo, v)
	}
	date := s.now()
	if v, ok := input["state"]; ok {
		state := stringVal(v)
		if state != "open" && state != "closed" {
			reject(422, "Unknown issue state")
		}
		if state != i.State {
			i.Closed = nil
			if state == "closed" {
				i.Closed = &date
			}
			i.State = state
		}
	}
	i.Title, i.Labels, i.Body, i.Updated = title, labels, optional(input["body"], i.Body), date
	delete(i.repo.issueJSON, i.Number)
	return i
}
func (s *store) addComment(r *repository, n int, input mapJSON) *comment {
	i := r.issues[n]
	if i == nil && r.pulls[n] == nil {
		reject(404, "Not Found")
	}
	body := required(input["body"], "body")
	id, when := s.allocate(), s.now()
	c := &comment{id, nodeID("IssueComment", id), body, s.user, when, when}
	r.comments[n] = append(r.comments[n], c)
	s.nodes[c.NodeID] = c
	if i != nil {
		i.Comments++
		i.Updated = when
		delete(r.issueJSON, n)
	}
	return c
}
func (r *repository) issueList(state string, asc bool) []*issue {
	result := make([]*issue, 0, len(r.issues))
	for _, i := range r.issues {
		if state == "all" || i.State == state {
			result = append(result, i)
		}
	}
	sort.Slice(result, func(a, b int) bool {
		if asc {
			return result[a].Number < result[b].Number
		}
		return result[a].Number > result[b].Number
	})
	return result
}
