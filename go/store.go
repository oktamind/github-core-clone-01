// Store: in-memory repositories, git objects, issues, pull requests.
// Deterministic: fixed timestamps, sequential ids, no time.Now()/rand in
// anything observable, so live and replay exports match byte for byte.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	viewer    = "adaiversen"
	viewerID  = 1000
	fixedTS   = 1700000000 // git author/committer timestamp (deterministic)
	fixedDate = "2025-01-01T00:00:00Z"
	zeroOID   = "0000000000000000000000000000000000000000"
	jsonType  = "application/json; charset=utf-8"
)

var tokenEnv = os.Getenv("MAKEFUN_SHOP_TOKEN")

var (
	mu           sync.Mutex // serializes request handlers; bodies parse outside it
	repos        = map[string]*Repo{}
	nextRepoID   = 1
	nextGlobalID = 1
	reqCounter   = 0
)

type Obj struct {
	typ  string
	data []byte
}

type Issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []Label
	seq      int
	comments []Comment
}

type Comment struct {
	id   int
	body string
}

type Label struct {
	name  string
	color string
}

type PR struct {
	id       int
	number   int
	title    string
	body     string
	state    string // "open" | "closed"
	merged   bool
	headRef  string
	baseRef  string
	headSha  string
	baseSha  string
	seq      int
	mergeSha string
	mergedAt string
}

type commitInfo struct {
	tree    string
	parents []string
	message string
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

type issueCacheEntry struct {
	buf       []byte
	state     string
	title     string
	body      string
	nComments int
	nLabels   int
}

// Repo is one repository's whole state. All fields are guarded by the global
// handler mutex, so no per-repo locking is needed.
type Repo struct {
	id            int
	name          string
	owner         string
	description   string
	isPrivate     bool
	defaultBranch string
	seq           int
	refs          map[string]string
	objects       map[string]*Obj
	issues        map[int]*Issue
	prs           map[int]*PR
	labels        map[string]Label
	nextNumber    int
	commitSeq     map[string]int
	nextCommitSeq int
	commitCache   map[string]*commitInfo
	treeCache     map[string][]treeEntry
	flatCache     map[string]map[string]string
	// perf caches (all content-keyed, so a stale entry is impossible)
	refVersion     int
	issueJSONCache map[int]*issueCacheEntry
	fetchCache     map[string][]byte
	lsRefsCache    map[string][]byte
	adCache        map[string][]byte
}

func repoKey(owner, name string) string { return owner + "/" + name }

func getRepo(owner, name string) *Repo { return repos[repoKey(owner, name)] }

func createRepo(name string, isPrivate bool, description string) *Repo {
	r := &Repo{
		id: nextRepoID, name: name, owner: viewer, description: description,
		isPrivate: isPrivate, defaultBranch: "main", seq: nextGlobalID,
		refs: map[string]string{}, objects: map[string]*Obj{},
		issues: map[int]*Issue{}, prs: map[int]*PR{},
		labels: map[string]Label{}, nextNumber: 1,
		commitSeq: map[string]int{}, nextCommitSeq: 1,
		commitCache: map[string]*commitInfo{}, treeCache: map[string][]treeEntry{},
		flatCache:      map[string]map[string]string{},
		issueJSONCache: map[int]*issueCacheEntry{}, fetchCache: map[string][]byte{},
		lsRefsCache: map[string][]byte{}, adCache: map[string][]byte{},
	}
	nextRepoID++
	nextGlobalID++
	repos[repoKey(viewer, name)] = r
	return r
}

// Every refs mutation goes through these so git response caches stay fresh.
func setRef(r *Repo, name, oid string) { r.refs[name] = oid; r.refVersion++ }
func delRef(r *Repo, name string)      { delete(r.refs, name); r.refVersion++ }

func sha1Hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

func oidOf(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func gid(kind string, n int) string { return b64(kind + ":" + itoa(n)) }

func decodeGid(id string) (kind string, n int, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", 0, false
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", 0, false
	}
	v, err := atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], v, true
}

func atoi(s string) (int, error) {
	if s == "" {
		return 0, errAtoi
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errAtoi
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

type atoiError struct{}

func (atoiError) Error() string { return "invalid integer" }

var errAtoi = atoiError{}

// ---------------------------------------------------------------- git objects

func parseCommit(data []byte) *commitInfo {
	s := string(data)
	head, _, _ := strings.Cut(s, "\n\n")
	c := &commitInfo{}
	msg := ""
	if idx := strings.Index(s, "\n\n"); idx >= 0 {
		msg = s[idx+2:]
	}
	c.message = msg
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			c.tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			c.parents = append(c.parents, strings.TrimSpace(line[7:]))
		}
	}
	return c
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		sp += i
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		i = nul + 21
	}
	return out
}

func parseTag(data []byte) (object, typ string) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			object = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			typ = strings.TrimSpace(line[5:])
		}
	}
	return object, typ
}

func storeObj(r *Repo, typ string, data []byte) string {
	oid := oidOf(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Obj{typ: typ, data: data}
		if typ == "commit" {
			r.commitCache[oid] = parseCommit(data)
		} else if typ == "tree" {
			r.treeCache[oid] = parseTree(data)
		}
	}
	if typ == "commit" {
		if _, ok := r.commitSeq[oid]; !ok {
			r.commitSeq[oid] = r.nextCommitSeq
			r.nextCommitSeq++
		}
	}
	return oid
}

func commitOf(r *Repo, oid string) *commitInfo {
	if hit, ok := r.commitCache[oid]; ok {
		return hit
	}
	o := r.objects[oid]
	if o == nil || o.typ != "commit" {
		return nil
	}
	c := parseCommit(o.data)
	r.commitCache[oid] = c
	return c
}

func treeOf(r *Repo, oid string) []treeEntry {
	if hit, ok := r.treeCache[oid]; ok {
		return hit
	}
	o := r.objects[oid]
	if o == nil || o.typ != "tree" {
		return nil
	}
	t := parseTree(o.data)
	r.treeCache[oid] = t
	return t
}

func makeCommit(r *Repo, tree string, parents []string, message string) string {
	author := "Ada Iversen <adaiversen@example.invalid> " + itoa(fixedTS) + " +0000"
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + author + "\ncommitter " + author + "\n\n" + message)
	return storeObj(r, "commit", []byte(sb.String()))
}

func buildTree(r *Repo, files map[string]string) string {
	// files: path -> blob oid. Build nested trees bottom-up.
	type node struct {
		kids map[string]*node
		oid  string
		mode string
	}
	root := &node{kids: map[string]*node{}}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		parts := strings.Split(path, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			n, ok := cur.kids[parts[i]]
			if !ok {
				n = &node{kids: map[string]*node{}}
				cur.kids[parts[i]] = n
			}
			cur = n
		}
		cur.kids[parts[len(parts)-1]] = &node{oid: files[path], mode: "100644"}
	}
	var build func(n *node) string
	build = func(n *node) string {
		names := make([]string, 0, len(n.kids))
		for name := range n.kids {
			names = append(names, name)
		}
		sort.Strings(names)
		var buf bytes.Buffer
		for _, name := range names {
			v := n.kids[name]
			if v.kids != nil {
				oid := build(v)
				buf.WriteString("40000 " + name + "\x00")
				raw, _ := hex.DecodeString(oid)
				buf.Write(raw)
			} else {
				buf.WriteString(v.mode + " " + name + "\x00")
				raw, _ := hex.DecodeString(v.oid)
				buf.Write(raw)
			}
		}
		return storeObj(r, "tree", buf.Bytes())
	}
	return build(root)
}

func flattenTree(r *Repo, treeOID, prefix string, out map[string]string) {
	for _, e := range treeOf(r, treeOID) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			flattenTree(r, e.oid, path, out)
		} else {
			out[path] = e.oid
		}
	}
}

func treeFiles(r *Repo, treeOID string) map[string]string {
	if treeOID == "" {
		return map[string]string{}
	}
	if hit, ok := r.flatCache[treeOID]; ok {
		return hit
	}
	m := map[string]string{}
	flattenTree(r, treeOID, "", m)
	r.flatCache[treeOID] = m
	return m
}

func commitTree(r *Repo, commitOID string) string {
	if commitOID == "" {
		return ""
	}
	if c := commitOf(r, commitOID); c != nil {
		return c.tree
	}
	return ""
}

func ancestors(r *Repo, start string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == "" || c == zeroOID || seen[c] {
			continue
		}
		cc := commitOf(r, c)
		if cc == nil {
			continue
		}
		seen[c] = true
		stack = append(stack, cc.parents...)
	}
	return seen
}

// Fast-forward check without walking the whole history: walk up from start
// and stop as soon as target is found (a bench push is one step behind).
func isDescendant(r *Repo, start, target string) bool {
	if start == target {
		return true
	}
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == "" || c == zeroOID || seen[c] {
			continue
		}
		seen[c] = true
		cc := commitOf(r, c)
		if cc == nil {
			continue
		}
		for _, p := range cc.parents {
			if p == target {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

func peelToCommit(r *Repo, oid string) string {
	cur := oid
	for i := 0; i < 10; i++ {
		o := r.objects[cur]
		if o == nil {
			return cur
		}
		if o.typ == "commit" {
			return cur
		}
		if o.typ == "tag" {
			obj, _ := parseTag(o.data)
			cur = obj
		} else {
			return cur
		}
	}
	return cur
}

func mergeBase(r *Repo, a, b string) string {
	ancA := ancestors(r, a)
	best := ""
	bestSeq := -1
	seen := map[string]bool{}
	stack := []string{b}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		cc := commitOf(r, c)
		if cc == nil {
			continue
		}
		if ancA[c] {
			if s := r.commitSeq[c]; s > bestSeq {
				bestSeq = s
				best = c
			}
		}
		stack = append(stack, cc.parents...)
	}
	return best
}

func tipOf(r *Repo, ref string) string { return r.refs[ref] }

func branchTip(r *Repo, branch string) string {
	return r.refs["refs/heads/"+branch]
}

func sortedRefs(r *Repo) [][2]string {
	out := make([][2]string, 0, len(r.refs))
	for k, v := range r.refs {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
