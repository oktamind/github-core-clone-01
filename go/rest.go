// REST v3 API under /api/v3, plus the top-level request dispatch.
// Ported from the Node forge with identical status codes, bodies and errors.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- body helpers

func parseJSONBody(body []byte) (map[string]any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	return map[string]any{}, true
}

func asStr(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func asBool(m map[string]any, k string) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func asInt(m map[string]any, k string) (int, bool) {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i), true
			}
		case float64:
			return int(n), true
		}
	}
	return 0, false
}

func authOk(req *request) bool {
	h := req.header("authorization")
	if !strings.HasPrefix(h, "Bearer ") || len(h) == 7 {
		return false
	}
	if tokenEnv != "" {
		return h[7:] == tokenEnv
	}
	return true
}

func hostOf(req *request) string {
	if h := req.header("host"); h != "" {
		return h
	}
	return "127.0.0.1"
}

// ---------------------------------------------------------------- serializers

func repoJSON(r *Repo) map[string]any {
	open := 0
	for _, i := range r.issues {
		if i.state == "open" {
			open++
		}
	}
	return map[string]any{
		"id": r.id, "node_id": gid("Repository", r.id), "name": r.name,
		"full_name": r.owner + "/" + r.name,
		"owner":     map[string]any{"login": r.owner, "id": ifThen(r.owner == viewer, viewerID, 1001), "type": "User"},
		"private":   r.isPrivate, "description": r.description, "default_branch": r.defaultBranch,
		"html_url":   "https://example.invalid/" + r.owner + "/" + r.name,
		"url":        "/api/v3/repos/" + r.owner + "/" + r.name,
		"created_at": fixedDate, "updated_at": fixedDate,
		"has_issues": true, "has_projects": false, "has_wiki": false,
		"forks_count": 0, "stargazers_count": 0, "watchers_count": 0, "open_issues_count": open,
	}
}

func ifThen(c bool, a, b int) int {
	if c {
		return a
	}
	return b
}

func labelJSON(l Label) map[string]any {
	return map[string]any{"name": l.name, "color": l.color}
}

func issueJSON(r *Repo, i *Issue) map[string]any {
	labels := make([]any, 0, len(i.labels))
	for _, l := range i.labels {
		labels = append(labels, labelJSON(l))
	}
	return map[string]any{
		"id": i.id, "node_id": gid("Issue", i.id), "number": i.number,
		"title": i.title, "body": i.body, "state": i.state, "labels": labels,
		"user":       map[string]any{"login": viewer, "id": viewerID},
		"created_at": fixedDate, "updated_at": fixedDate,
		"comments": len(i.comments), "html_url": "https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + itoa(i.number),
		"url": "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + itoa(i.number),
	}
}

func issueJSONBuf(r *Repo, i *Issue) []byte {
	if hit, ok := r.issueJSONCache[i.number]; ok &&
		hit.state == i.state && hit.title == i.title && hit.body == i.body &&
		hit.nComments == len(i.comments) && hit.nLabels == len(i.labels) {
		return hit.buf
	}
	buf := append(marshalJSON(issueJSON(r, i)), '\n')
	r.issueJSONCache[i.number] = &issueCacheEntry{buf: buf, state: i.state, title: i.title, body: i.body, nComments: len(i.comments), nLabels: len(i.labels)}
	return buf
}

func prJSON(r *Repo, p *PR) map[string]any {
	mergeable := prConflict(r, p) == ""
	var mergedAt any = nil
	if p.merged {
		mergedAt = p.mergedAt
	}
	var mergeSha any = nil
	if p.mergeSha != "" {
		mergeSha = p.mergeSha
	}
	return map[string]any{
		"id": p.id, "node_id": gid("PullRequest", p.id), "number": p.number,
		"title": p.title, "body": p.body, "state": p.state, "merged": p.merged,
		"mergeable": mergeable, "merged_at": mergedAt, "merge_commit_sha": mergeSha,
		"head":       map[string]any{"ref": p.headRef, "sha": p.headSha, "label": r.owner + ":" + p.headRef, "user": map[string]any{"login": viewer}},
		"base":       map[string]any{"ref": p.baseRef, "sha": p.baseSha, "label": r.owner + ":" + p.baseRef, "user": map[string]any{"login": viewer}},
		"user":       map[string]any{"login": viewer, "id": viewerID},
		"created_at": fixedDate, "updated_at": fixedDate,
		"html_url": "https://example.invalid/" + r.owner + "/" + r.name + "/pull/" + itoa(p.number),
	}
}

// ---------------------------------------------------------------- merge / diff

type commitRef struct {
	sha     string
	message string
	seq     int
}

func listCommits(r *Repo, start string) []commitRef {
	anc := ancestors(r, start)
	out := make([]commitRef, 0, len(anc))
	for sha := range anc {
		if cc := commitOf(r, sha); cc != nil {
			out = append(out, commitRef{sha: sha, message: cc.message, seq: r.commitSeq[sha]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

type diffFile struct {
	filename  string
	status    string
	additions int
	deletions int
	changes   int
}

func diffFiles(r *Repo, baseTree, headTree string) []diffFile {
	base := treeFiles(r, baseTree)
	head := treeFiles(r, headTree)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []diffFile
	countLines := func(lines []string) int {
		n := 0
		for _, l := range lines {
			if len(l) > 0 {
				n++
			}
		}
		return n
	}
	for _, path := range sorted {
		b, bOk := base[path]
		h, hOk := head[path]
		if bOk && hOk && b == h {
			continue
		}
		var bObj, hObj *Obj
		if bOk {
			bObj = r.objects[b]
		}
		if hOk {
			hObj = r.objects[h]
		}
		var bLines, hLines []string
		if bObj != nil {
			bLines = strings.Split(string(bObj.data), "\n")
		}
		if hObj != nil {
			hLines = strings.Split(string(hObj.data), "\n")
		}
		additions, deletions := 0, 0
		if bObj == nil {
			additions = countLines(hLines)
		} else if hObj == nil {
			deletions = countLines(bLines)
		} else {
			bSet := map[string]int{}
			for _, l := range bLines {
				bSet[l]++
			}
			hSet := map[string]int{}
			for _, l := range hLines {
				hSet[l]++
			}
			for l, c := range hSet {
				if d := c - bSet[l]; d > 0 {
					additions += d
				}
			}
			for l, c := range bSet {
				if d := c - hSet[l]; d > 0 {
					deletions += d
				}
			}
		}
		status := "modified"
		if bObj == nil {
			status = "added"
		} else if hObj == nil {
			status = "removed"
		}
		out = append(out, diffFile{filename: path, status: status, additions: additions, deletions: deletions, changes: additions + deletions})
	}
	return out
}

func diffFileJSON(f diffFile) map[string]any {
	return map[string]any{"filename": f.filename, "status": f.status, "additions": f.additions, "deletions": f.deletions, "changes": f.changes}
}

// Returns "" when mergeable, else a conflict description.
func prConflict(r *Repo, p *PR) string {
	baseTip := branchTip(r, p.baseRef)
	headTip := branchTip(r, p.headRef)
	if baseTip == "" || headTip == "" {
		return ""
	}
	mb := mergeBase(r, baseTip, headTip)
	mbTree := ""
	if mb != "" {
		mbTree = commitTree(r, mb)
	}
	baseFiles := treeFiles(r, commitTree(r, baseTip))
	headFiles := treeFiles(r, commitTree(r, headTip))
	mbFiles := treeFiles(r, mbTree)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range mbFiles {
		paths[p] = true
	}
	for path := range paths {
		m, mOk := mbFiles[path]
		h, hOk := headFiles[path]
		c, cOk := baseFiles[path]
		headChanged := !hOk || !mOk || h != m
		if !hOk && !mOk {
			headChanged = false
		}
		baseChanged := !cOk || !mOk || c != m
		if !cOk && !mOk {
			baseChanged = false
		}
		if headChanged && baseChanged && h != c {
			return "conflict in " + path
		}
	}
	return ""
}

func doMerge(r *Repo, p *PR) (sha string, conflict string) {
	if c := prConflict(r, p); c != "" {
		return "", c
	}
	baseTip := branchTip(r, p.baseRef)
	headTip := branchTip(r, p.headRef)
	mb := ""
	if baseTip != "" && headTip != "" {
		mb = mergeBase(r, baseTip, headTip)
	}
	mbTree := ""
	if mb != "" {
		mbTree = commitTree(r, mb)
	}
	baseFiles := treeFiles(r, commitTree(r, baseTip))
	headFiles := treeFiles(r, commitTree(r, headTip))
	mbFiles := treeFiles(r, mbTree)
	merged := map[string]string{}
	for k, v := range baseFiles {
		merged[k] = v
	}
	for path, h := range headFiles {
		m, mOk := mbFiles[path]
		if !mOk || h != m {
			merged[path] = h
		}
	}
	for path := range mbFiles {
		if _, inHead := headFiles[path]; !inHead {
			if b, inBase := baseFiles[path]; inBase && b == mbFiles[path] {
				delete(merged, path)
			}
		}
	}
	tree := buildTree(r, merged)
	parents := []string{}
	if baseTip != "" {
		parents = []string{baseTip}
	}
	sha = makeCommit(r, tree, parents, "Merge pull request #"+itoa(p.number)+" from "+p.headRef+"\n\n"+p.title)
	setRef(r, "refs/heads/"+p.baseRef, sha)
	p.state = "closed"
	p.merged = true
	p.mergeSha = sha
	p.mergedAt = fixedDate
	return sha, ""
}

func paginate(items []any, req *request) (page []any, link string) {
	perPage := 30
	if v := req.q("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	pageNum := 1
	if v := req.q("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			pageNum = n
		}
	}
	if pageNum < 1 {
		pageNum = 1
	}
	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	if start+perPage < len(items) {
		u, _ := url.Parse("http://" + hostOf(req) + req.target)
		q := u.Query()
		q.Set("per_page", itoa(perPage))
		q.Set("page", itoa(pageNum+1))
		u.RawQuery = q.Encode()
		link = "<" + u.String() + ">; rel=\"next\""
	}
	return page, link
}

func commitFile(r *Repo, branch, path string, content []byte, message string) (blobSha, commitSha string) {
	tip := branchTip(r, branch)
	if tip == "" {
		def := branchTip(r, r.defaultBranch)
		tip = def
		if branch != r.defaultBranch && def != "" {
			setRef(r, "refs/heads/"+branch, def)
		}
	}
	blobSha = storeObj(r, "blob", content)
	files := map[string]string{}
	if tip != "" {
		for k, v := range treeFiles(r, commitTree(r, tip)) {
			files[k] = v
		}
	}
	files[path] = blobSha
	tree := buildTree(r, files)
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	commitSha = makeCommit(r, tree, parents, message)
	setRef(r, "refs/heads/"+branch, commitSha)
	return blobSha, commitSha
}

func deleteFile(r *Repo, branch, path, message string) string {
	tip := branchTip(r, branch)
	if tip == "" {
		return ""
	}
	files := map[string]string{}
	for k, v := range treeFiles(r, commitTree(r, tip)) {
		files[k] = v
	}
	if _, ok := files[path]; !ok {
		return ""
	}
	delete(files, path)
	tree := buildTree(r, files)
	commitSha := makeCommit(r, tree, []string{tip}, message)
	setRef(r, "refs/heads/"+branch, commitSha)
	return commitSha
}

// ---------------------------------------------------------------- REST handler

func repoFromPath(owner, name string) (*Repo, *response) {
	if owner != viewer {
		return nil, jsonErr(404, "Not Found")
	}
	r := getRepo(owner, name)
	if r == nil {
		return nil, jsonErr(404, "Not Found")
	}
	return r, nil
}

var reIssueFast = regexp.MustCompile(`^/api/v3/repos/([^/]+)/([^/]+)/issues(?:/(\d+))?$`)
var reGit = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git(/.*)?$`)

func dispatch(req *request) (resp *response) {
	defer func() {
		if e := recover(); e != nil {
			resp = jsonErr(500, "Internal Server Error")
		}
	}()
	path := req.path
	method := req.method

	if method == "GET" && path == "/healthz" {
		return jsonResp(200, mustJSON(map[string]any{"ok": true}))
	}

	// git smart HTTP: /{owner}/{repo}.git/...
	if m := reGit.FindStringSubmatch(path); m != nil {
		owner, _ := url.PathUnescape(m[1])
		repoName, _ := url.PathUnescape(m[2])
		sub := m[3]
		if !authOk(req) {
			return jsonErr(401, "Requires authentication")
		}
		var repo *Repo
		if owner == viewer {
			repo = getRepo(owner, repoName)
		}
		if repo == nil {
			return jsonErr(404, "Not Found")
		}
		if method == "GET" && sub == "/info/refs" {
			service := req.q("service")
			if service == "" {
				service = "git-upload-pack"
			}
			v2 := req.header("git-protocol") == "version=2"
			if v2 && service == "git-upload-pack" {
				return rawResp(200, "application/x-git-upload-pack-advertisement", v2Advertisement)
			}
			return rawResp(200, "application/x-"+service+"-advertisement", v0Advertisement(repo, service))
		}
		if method == "POST" && sub == "/git-upload-pack" {
			v2 := req.header("git-protocol") == "version=2"
			if v2 {
				packets, _ := parsePackets(req.body, false)
				var lines []string
				command := ""
				afterDelim := false
				for _, p := range packets {
					if p.kind == "delim" {
						afterDelim = true
						continue
					}
					if p.kind != "data" {
						continue
					}
					text := strings.TrimSuffix(string(p.payload), "\n")
					if !afterDelim && strings.HasPrefix(text, "command=") {
						command = text[8:]
					} else if afterDelim {
						lines = append(lines, text)
					} else if strings.HasPrefix(text, "command=") {
						command = text[8:]
					}
				}
				switch command {
				case "ls-refs":
					return rawResp(200, "application/x-git-upload-pack-result", handleLsRefs(repo, lines))
				case "fetch":
					return rawResp(200, "application/x-git-upload-pack-result", handleFetchV2(repo, lines))
				default:
					return rawResp(200, "application/x-git-upload-pack-result", pktFlush)
				}
			}
			return rawResp(200, "application/x-git-upload-pack-result", handleFetchV0(repo, req.body))
		}
		if method == "POST" && sub == "/git-receive-pack" {
			return rawResp(200, "application/x-git-receive-pack-result", handleReceivePack(repo, req.body))
		}
		return jsonErr(404, "Not Found")
	}

	// GraphQL
	if path == "/api/graphql" {
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		if !authOk(req) {
			return jsonErr(401, "Requires authentication")
		}
		var raw any
		dec := json.NewDecoder(bytes.NewReader(req.body))
		dec.UseNumber()
		if len(req.body) > 0 {
			if err := dec.Decode(&raw); err != nil {
				return jsonResp(400, mustJSON(map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}}))
			}
		} else {
			raw = map[string]any{}
		}
		var ops []any
		isArray := false
		if arr, ok := raw.([]any); ok {
			ops = arr
			isArray = true
		} else {
			ops = []any{raw}
		}
		results := make([]any, 0, len(ops))
		for _, op := range ops {
			o, _ := op.(map[string]any)
			vars, _ := o["variables"].(map[string]any)
			if vars == nil {
				vars = map[string]any{}
			}
			results = append(results, executeGQLOp(asStr(o, "query"), vars))
		}
		if isArray {
			return jsonResp(200, mustJSON(results))
		}
		return jsonResp(200, mustJSON(results[0]))
	}

	// MCP
	if path == "/mcp" {
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		return handleMCP(req)
	}

	// REST v3
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		// Hot fast path: single-issue GET and issue POST.
		if method == "GET" || method == "POST" {
			if m := reIssueFast.FindStringSubmatch(path); m != nil {
				if !authOk(req) {
					return jsonErr(401, "Requires authentication")
				}
				owner, _ := url.PathUnescape(m[1])
				name, _ := url.PathUnescape(m[2])
				r, errResp := repoFromPath(owner, name)
				if errResp != nil {
					return errResp
				}
				if method == "GET" && m[3] != "" {
					num, _ := strconv.Atoi(m[3])
					issue := r.issues[num]
					if issue == nil {
						return jsonErr(404, "Not Found")
					}
					return jsonResp(200, issueJSONBuf(r, issue))
				}
				if method == "POST" && m[3] == "" {
					obj, ok := parseJSONBody(req.body)
					if !ok {
						return jsonErr(400, "Problems parsing JSON")
					}
					title := asStr(obj, "title")
					if title == "" {
						return jsonErr(422, "Validation Failed: title is required")
					}
					labelNames := labelNamesOf(obj)
					for _, ln := range labelNames {
						if _, ok := r.labels[ln]; !ok {
							return jsonErr(422, "Validation Failed: label \""+ln+"\" does not exist")
						}
					}
					num := r.nextNumber
					r.nextNumber++
					issueID := nextGlobalID
					nextGlobalID++
					issueSeq := nextGlobalID
					nextGlobalID++
					issue := &Issue{id: issueID, number: num, title: title, body: asStr(obj, "body"), state: "open", seq: issueSeq, comments: []Comment{}}
					for _, n := range labelNames {
						issue.labels = append(issue.labels, r.labels[n])
					}
					r.issues[num] = issue
					return jsonResp(201, issueJSONBuf(r, issue))
				}
			}
		}
		return handleRest(req)
	}

	return jsonErr(404, "Not Found")
}

func labelNamesOf(obj map[string]any) []string {
	var out []string
	if arr, ok := obj["labels"].([]any); ok {
		for _, l := range arr {
			if s, ok := l.(string); ok {
				out = append(out, s)
			} else if m, ok := l.(map[string]any); ok {
				out = append(out, asStr(m, "name"))
			} else {
				out = append(out, "")
			}
		}
	}
	return out
}

func handleRest(req *request) *response {
	method := req.method
	path := req.path
	segs := []string{}
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	rest := []string{}
	if len(segs) >= 2 {
		rest = segs[2:]
	}
	isWrite := method == "POST" || method == "PATCH" || method == "PUT" || method == "DELETE"
	var obj map[string]any = map[string]any{}
	if isWrite && (len(rest) > 0 || path == "/api/v3/user/repos") {
		parsed, ok := parseJSONBody(req.body)
		if !ok {
			return jsonErr(400, "Problems parsing JSON")
		}
		obj = parsed
	}

	if method == "GET" && len(rest) == 0 {
		return &response{status: 200, ctype: jsonType, body: mustJSON(map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}), headers: map[string]string{"x-oauth-scopes": "repo, read:org"}}
	}
	if method == "GET" && len(rest) == 1 && rest[0] == "meta" {
		return jsonResp(200, mustJSON(map[string]any{"installed_version": "1.0.0"}))
	}
	if !authOk(req) {
		return jsonErr(401, "Requires authentication")
	}
	if method == "GET" && len(rest) == 1 && rest[0] == "user" {
		return jsonResp(200, mustJSON(map[string]any{"login": viewer, "id": viewerID, "name": "Ada Iversen", "type": "User", "email": "adaiversen@example.invalid"}))
	}
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "GET" {
			var all []*Repo
			for _, r := range repos {
				if r.owner == viewer {
					all = append(all, r)
				}
			}
			sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
			items := make([]any, 0, len(all))
			for _, r := range all {
				items = append(items, repoJSON(r))
			}
			page, link := paginate(items, req)
			resp := jsonResp(200, mustJSON(page))
			if link != "" {
				resp.headers = map[string]string{"link": link}
			}
			return resp
		}
		if method == "POST" {
			name := strings.TrimSpace(asStr(obj, "name"))
			if name == "" {
				return jsonErr(422, "Validation Failed: name is required")
			}
			if getRepo(viewer, name) != nil {
				return jsonErr(422, "Validation Failed: name already exists on this account")
			}
			r := createRepo(name, asBool(obj, "private"), asStr(obj, "description"))
			return jsonResp(201, mustJSON(repoJSON(r)))
		}
	}
	if len(rest) >= 3 && rest[0] == "repos" {
		owner, _ := url.PathUnescape(rest[1])
		name, _ := url.PathUnescape(rest[2])
		tail := rest[3:]
		if len(tail) == 0 {
			if method == "GET" {
				r, errResp := repoFromPath(owner, name)
				if errResp != nil {
					return errResp
				}
				return jsonResp(200, mustJSON(repoJSON(r)))
			}
			if method == "PATCH" {
				r, errResp := repoFromPath(owner, name)
				if errResp != nil {
					return errResp
				}
				if v, ok := obj["description"]; ok {
					if s, ok := v.(string); ok {
						r.description = s
					}
				}
				return jsonResp(200, mustJSON(repoJSON(r)))
			}
			if method == "DELETE" {
				r, errResp := repoFromPath(owner, name)
				if errResp != nil {
					return errResp
				}
				_ = r
				delete(repos, repoKey(owner, name))
				return &response{status: 204, body: []byte{}}
			}
		}
		r, errResp := repoFromPath(owner, name)
		if errResp != nil {
			return errResp
		}
		// branches
		if method == "GET" && len(tail) == 1 && tail[0] == "branches" {
			var list []any
			for _, ref := range sortedRefs(r) {
				if strings.HasPrefix(ref[0], "refs/heads/") {
					list = append(list, map[string]any{"name": ref[0][11:], "commit": map[string]any{"sha": ref[1]}, "protected": false})
				}
			}
			if list == nil {
				list = []any{}
			}
			return jsonResp(200, mustJSON(list))
		}
		// commits
		if method == "GET" && len(tail) == 1 && tail[0] == "commits" {
			shaParam := req.q("sha")
			start := ""
			if shaParam != "" {
				if v := r.refs["refs/heads/"+shaParam]; v != "" {
					start = v
				} else if r.objects[shaParam] != nil {
					start = shaParam
				}
			} else {
				start = branchTip(r, r.defaultBranch)
			}
			if start == "" {
				return jsonResp(200, mustJSON([]any{}))
			}
			var list []any
			for _, c := range listCommits(r, start) {
				list = append(list, map[string]any{
					"sha": c.sha,
					"commit": map[string]any{
						"message":   c.message,
						"author":    map[string]any{"name": "Ada Iversen", "email": "adaiversen@example.invalid", "date": fixedDate},
						"committer": map[string]any{"name": "Ada Iversen", "email": "adaiversen@example.invalid", "date": fixedDate},
					},
					"author":    map[string]any{"login": viewer},
					"committer": map[string]any{"login": viewer},
				})
			}
			if list == nil {
				list = []any{}
			}
			return jsonResp(200, mustJSON(list))
		}
		// git/refs
		if len(tail) >= 2 && tail[0] == "git" && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				ref := asStr(obj, "ref")
				sha := asStr(obj, "sha")
				if ref == "" || !reSHA.MatchString(sha) {
					return jsonErr(422, "Validation Failed")
				}
				setRef(r, ref, sha)
				id := nextGlobalID
				nextGlobalID++
				return jsonResp(201, mustJSON(map[string]any{"ref": ref, "object": map[string]any{"sha": sha}, "node_id": gid("Ref", id)}))
			}
			if method == "GET" {
				refPath := strings.Join(tail[2:], "/")
				if refPath == "" {
					var list []any
					for _, ref := range sortedRefs(r) {
						list = append(list, map[string]any{"ref": ref[0], "object": map[string]any{"sha": ref[1]}})
					}
					if list == nil {
						list = []any{}
					}
					return jsonResp(200, mustJSON(list))
				}
				oid := r.refs[refPath]
				if oid == "" {
					oid = r.refs["refs/"+refPath]
				}
				if oid == "" {
					oid = r.refs["refs/heads/"+refPath]
				}
				if oid == "" {
					return jsonErr(404, "Not Found")
				}
				outRef := refPath
				if !strings.HasPrefix(refPath, "refs/") {
					outRef = "refs/heads/" + refPath
				}
				return jsonResp(200, mustJSON(map[string]any{"ref": outRef, "object": map[string]any{"sha": oid}}))
			}
		}
		// contents
		if len(tail) >= 1 && tail[0] == "contents" {
			parts := []string{}
			for _, s := range tail[1:] {
				u, _ := url.PathUnescape(s)
				parts = append(parts, u)
			}
			contentPath := strings.Join(parts, "/")
			refParam := req.q("ref")
			if refParam == "" {
				refParam = asStr(obj, "branch")
			}
			branch := refParam
			if branch == "" {
				branch = r.defaultBranch
			}
			if method == "PUT" {
				message := asStr(obj, "message")
				if message == "" {
					message = "Update"
				}
				contentB64 := strings.ReplaceAll(asStr(obj, "content"), "\n", "")
				content, _ := base64.StdEncoding.DecodeString(contentB64)
				tip := branchTip(r, branch)
				if shaStr, ok := obj["sha"].(string); ok && tip != "" {
					files := treeFiles(r, commitTree(r, tip))
					cur, exists := files[contentPath]
					if exists && cur != shaStr {
						return jsonErr(409, "Conflict: sha does not match")
					}
					if !exists {
						return jsonErr(409, "Conflict: file does not exist")
					}
				}
				isUpdate := false
				if tip != "" {
					_, isUpdate = treeFiles(r, commitTree(r, tip))[contentPath]
				}
				blobSha, commitSha := commitFile(r, branch, contentPath, content, message)
				status := 201
				if isUpdate {
					status = 200
				}
				base := contentPath
				if i := strings.LastIndexByte(contentPath, '/'); i >= 0 {
					base = contentPath[i+1:]
				}
				return jsonResp(status, mustJSON(map[string]any{
					"content": map[string]any{"name": base, "path": contentPath, "sha": blobSha, "size": len(content), "encoding": "base64", "type": "file"},
					"commit":  map[string]any{"sha": commitSha, "message": message},
				}))
			}
			if method == "GET" {
				tip := branchTip(r, branch)
				if tip == "" {
					return jsonErr(404, "Not Found")
				}
				files := treeFiles(r, commitTree(r, tip))
				if contentPath == "" {
					seen := map[string]struct {
						typ string
						sha string
					}{}
					for p := range files {
						first := strings.SplitN(p, "/", 2)[0]
						if _, ok := seen[first]; !ok {
							if strings.Contains(p, "/") {
								seen[first] = struct {
									typ string
									sha string
								}{"dir", ""}
							} else {
								seen[first] = struct {
									typ string
									sha string
								}{"file", files[p]}
							}
						}
					}
					names := make([]string, 0, len(seen))
					for n := range seen {
						names = append(names, n)
					}
					sort.Strings(names)
					var list []any
					for _, n := range names {
						sha := seen[n].sha
						if sha == "" {
							sha = oidOf("tree", []byte{})
						}
						list = append(list, map[string]any{"type": seen[n].typ, "name": n, "path": n, "sha": sha})
					}
					if list == nil {
						list = []any{}
					}
					return jsonResp(200, mustJSON(list))
				}
				if blobOid, ok := files[contentPath]; ok {
					blob := r.objects[blobOid]
					base := contentPath
					if i := strings.LastIndexByte(contentPath, '/'); i >= 0 {
						base = contentPath[i+1:]
					}
					return jsonResp(200, mustJSON(map[string]any{
						"type": "file", "encoding": "base64", "size": len(blob.data), "name": base,
						"path": contentPath, "content": base64.StdEncoding.EncodeToString(blob.data), "sha": blobOid,
					}))
				}
				prefix := contentPath + "/"
				entries := map[string]struct {
					typ string
					sha string
				}{}
				for p, oid := range files {
					if !strings.HasPrefix(p, prefix) {
						continue
					}
					restPath := p[len(prefix):]
					first := strings.SplitN(restPath, "/", 2)[0]
					if _, ok := entries[first]; !ok {
						if strings.Contains(restPath, "/") {
							entries[first] = struct {
								typ string
								sha string
							}{"dir", ""}
						} else {
							entries[first] = struct {
								typ string
								sha string
							}{"file", oid}
						}
					}
				}
				if len(entries) == 0 {
					return jsonErr(404, "Not Found")
				}
				names := make([]string, 0, len(entries))
				for n := range entries {
					names = append(names, n)
				}
				sort.Strings(names)
				var list []any
				for _, n := range names {
					sha := entries[n].sha
					if sha == "" {
						sha = oidOf("tree", []byte{})
					}
					list = append(list, map[string]any{"type": entries[n].typ, "name": n, "path": contentPath + "/" + n, "sha": sha})
				}
				return jsonResp(200, mustJSON(list))
			}
			if method == "DELETE" {
				message := asStr(obj, "message")
				if message == "" {
					message = "Delete"
				}
				sha := deleteFile(r, branch, contentPath, message)
				if sha == "" {
					return jsonErr(404, "Not Found")
				}
				return jsonResp(200, mustJSON(map[string]any{"commit": map[string]any{"sha": sha, "message": message}}))
			}
		}
		// labels
		if len(tail) == 1 && tail[0] == "labels" {
			if method == "POST" {
				labelName := asStr(obj, "name")
				if labelName == "" {
					return jsonErr(422, "Validation Failed")
				}
				color := asStr(obj, "color")
				if color == "" {
					color = "ededed"
				}
				label := Label{name: labelName, color: color}
				r.labels[labelName] = label
				return jsonResp(201, mustJSON(labelJSON(label)))
			}
			if method == "GET" {
				var list []any
				for _, l := range r.labels {
					list = append(list, labelJSON(l))
				}
				if list == nil {
					list = []any{}
				}
				return jsonResp(200, mustJSON(list))
			}
		}
		// issues
		if len(tail) >= 1 && tail[0] == "issues" {
			if len(tail) == 1 {
				if method == "POST" {
					title := asStr(obj, "title")
					if title == "" {
						return jsonErr(422, "Validation Failed: title is required")
					}
					labelNames := labelNamesOf(obj)
					for _, ln := range labelNames {
						if _, ok := r.labels[ln]; !ok {
							return jsonErr(422, "Validation Failed: label \""+ln+"\" does not exist")
						}
					}
					num := r.nextNumber
					r.nextNumber++
					issueID := nextGlobalID
					nextGlobalID++
					issueSeq := nextGlobalID
					nextGlobalID++
					issue := &Issue{id: issueID, number: num, title: title, body: asStr(obj, "body"), state: "open", seq: issueSeq, comments: []Comment{}}
					for _, n := range labelNames {
						issue.labels = append(issue.labels, r.labels[n])
					}
					r.issues[num] = issue
					return jsonResp(201, issueJSONBuf(r, issue))
				}
				if method == "GET" {
					var list []*Issue
					for _, i := range r.issues {
						list = append(list, i)
					}
					sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
					state := req.q("state")
					if state == "" {
						state = "open"
					}
					if state != "all" {
						var filtered []*Issue
						for _, i := range list {
							if i.state == state {
								filtered = append(filtered, i)
							}
						}
						list = filtered
					}
					sortParam := req.q("sort")
					if sortParam == "" {
						sortParam = "created"
					}
					dir := req.q("direction")
					if dir == "" {
						dir = "desc"
					}
					if sortParam == "created" {
						if dir == "asc" {
							sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
						} else {
							sort.Slice(list, func(a, b int) bool { return list[a].seq > list[b].seq })
						}
					}
					items := make([]any, 0, len(list))
					for _, i := range list {
						items = append(items, issueJSON(r, i))
					}
					page, link := paginate(items, req)
					resp := jsonResp(200, mustJSON(page))
					if link != "" {
						resp.headers = map[string]string{"link": link}
					}
					return resp
				}
			}
			num, err := strconv.Atoi(tail[1])
			var issue *Issue
			if err == nil {
				issue = r.issues[num]
			}
			if len(tail) == 2 {
				if issue == nil {
					return jsonErr(404, "Not Found")
				}
				if method == "GET" {
					return jsonResp(200, issueJSONBuf(r, issue))
				}
				if method == "PATCH" {
					if v, ok := obj["state"].(string); ok {
						issue.state = v
					}
					if v, ok := obj["title"].(string); ok {
						issue.title = v
					}
					if v, ok := obj["body"].(string); ok {
						issue.body = v
					}
					return jsonResp(200, issueJSONBuf(r, issue))
				}
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if issue == nil {
					return jsonErr(404, "Not Found")
				}
				if method == "POST" {
					comment := Comment{id: nextGlobalID, body: asStr(obj, "body")}
					nextGlobalID++
					issue.comments = append(issue.comments, comment)
					return jsonResp(201, mustJSON(map[string]any{"id": comment.id, "body": comment.body}))
				}
				if method == "GET" {
					var list []any
					for _, c := range issue.comments {
						list = append(list, map[string]any{"id": c.id, "body": c.body})
					}
					if list == nil {
						list = []any{}
					}
					return jsonResp(200, mustJSON(list))
				}
			}
		}
		// pulls
		if len(tail) >= 1 && tail[0] == "pulls" {
			if len(tail) == 1 {
				if method == "POST" {
					headRaw := asStr(obj, "head")
					baseRaw := asStr(obj, "base")
					head := headRaw
					if i := strings.LastIndexByte(headRaw, ':'); i >= 0 {
						head = headRaw[i+1:]
					}
					base := baseRaw
					if i := strings.LastIndexByte(baseRaw, ':'); i >= 0 {
						base = baseRaw[i+1:]
					}
					num := r.nextNumber
					r.nextNumber++
					prID := nextGlobalID
					nextGlobalID++
					prSeq := nextGlobalID
					nextGlobalID++
					pr := &PR{id: prID, number: num, title: asStr(obj, "title"), body: asStr(obj, "body"), state: "open", merged: false, headRef: head, baseRef: base, headSha: branchTip(r, head), baseSha: branchTip(r, base), seq: prSeq, mergeSha: "", mergedAt: ""}
					r.prs[num] = pr
					return jsonResp(201, mustJSON(prJSON(r, pr)))
				}
				if method == "GET" {
					var list []*PR
					for _, p := range r.prs {
						list = append(list, p)
					}
					sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
					state := req.q("state")
					if state == "" {
						state = "open"
					}
					if state != "all" {
						var filtered []*PR
						for _, p := range list {
							if p.state == state {
								filtered = append(filtered, p)
							}
						}
						list = filtered
					}
					items := make([]any, 0, len(list))
					for _, p := range list {
						items = append(items, prJSON(r, p))
					}
					page, link := paginate(items, req)
					resp := jsonResp(200, mustJSON(page))
					if link != "" {
						resp.headers = map[string]string{"link": link}
					}
					return resp
				}
			}
			num, err := strconv.Atoi(tail[1])
			var pr *PR
			if err == nil {
				pr = r.prs[num]
			}
			if len(tail) == 2 {
				if pr == nil {
					return jsonErr(404, "Not Found")
				}
				if method == "GET" {
					return jsonResp(200, mustJSON(prJSON(r, pr)))
				}
				if method == "PATCH" {
					if v, ok := obj["state"].(string); ok {
						pr.state = v
					}
					if v, ok := obj["title"].(string); ok {
						pr.title = v
					}
					return jsonResp(200, mustJSON(prJSON(r, pr)))
				}
			}
			if len(tail) == 3 && tail[2] == "files" {
				if pr == nil {
					return jsonErr(404, "Not Found")
				}
				if method == "GET" {
					files := diffFiles(r, commitTree(r, pr.baseSha), commitTree(r, pr.headSha))
					var list []any
					for _, f := range files {
						list = append(list, diffFileJSON(f))
					}
					if list == nil {
						list = []any{}
					}
					return jsonResp(200, mustJSON(list))
				}
			}
			if len(tail) == 3 && tail[2] == "merge" {
				if pr == nil {
					return jsonErr(404, "Not Found")
				}
				if method == "PUT" {
					if pr.merged {
						return jsonResp(200, mustJSON(map[string]any{"sha": pr.mergeSha, "merged": true, "message": "Pull Request successfully merged"}))
					}
					sha, conflict := doMerge(r, pr)
					if conflict != "" {
						return jsonErr(405, conflict)
					}
					return jsonResp(200, mustJSON(map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"}))
				}
			}
		}
	}
	return jsonErr(404, "Not Found")
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
