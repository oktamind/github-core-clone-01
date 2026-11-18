// GraphQL v4 at /api/graphql: hand-written tokenizer/parser (variables,
// enums, input objects, fragments, aliases, batching), introspection and
// the five mutations. Ported from the Node forge with identical behavior.
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- tokenizer

func tokenizeGQL(src string) []string {
	var tokens []string
	i := 0
	isAlpha := func(c byte) bool { return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.ContainsRune("{}():[]!=$@", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					sb.WriteByte(src[j+1])
					j += 2
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, strconv.Quote(sb.String()))
			i = j + 1
			continue
		}
		if isAlpha(c) {
			j := i
			for j < len(src) && (isAlpha(src[j]) || isDigit(src[j])) {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		if isDigit(c) || c == '-' {
			j := i
			for j < len(src) && (isDigit(src[j]) || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		i++
	}
	return tokens
}

// ---------------------------------------------------------------- parser

type astNode struct {
	kind          string // "Operation" | "Field" | "FragmentSpread" | "InlineFragment"
	name          string
	alias         string
	args          map[string]any
	selections    []*astNode
	typeCondition string
	operation     string
}

type gqlParser struct {
	toks      []string
	pos       int
	fragments map[string][]*astNode
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *gqlParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *gqlParser) expect(t string) {
	if p.peek() == t {
		p.pos++
	}
}

func (p *gqlParser) parseDocument() []*astNode {
	var ops []*astNode
	for p.pos < len(p.toks) {
		t := p.peek()
		if t == "fragment" {
			p.next()
			name := p.next()
			p.next() // 'on'
			typeCond := p.next()
			_ = typeCond
			sels := p.parseSelectionSet()
			p.fragments[name] = sels
		} else if t == "query" || t == "mutation" || t == "subscription" {
			op := p.next()
			var name string
			if p.peek() != "(" && p.peek() != "{" {
				name = p.next()
			}
			if p.peek() == "(" {
				p.parseVarDefs()
			}
			sels := p.parseSelectionSet()
			ops = append(ops, &astNode{kind: "Operation", operation: op, name: name, selections: sels})
		} else if t == "{" {
			sels := p.parseSelectionSet()
			ops = append(ops, &astNode{kind: "Operation", operation: "query", selections: sels})
		} else {
			p.next()
		}
	}
	return ops
}

func (p *gqlParser) parseVarDefs() {
	p.expect("(")
	depth := 1
	for p.pos < len(p.toks) && depth > 0 {
		t := p.next()
		if t == "(" || t == "[" || t == "{" {
			depth++
		} else if t == ")" || t == "]" || t == "}" {
			depth--
		}
	}
}

func (p *gqlParser) parseSelectionSet() []*astNode {
	p.expect("{")
	var sels []*astNode
	for p.pos < len(p.toks) && p.peek() != "}" {
		sels = append(sels, p.parseField())
	}
	p.expect("}")
	return sels
}

func (p *gqlParser) parseField() *astNode {
	if p.peek() == "..." {
		p.next()
		if p.peek() == "on" {
			p.next()
			tc := p.next()
			return &astNode{kind: "InlineFragment", typeCondition: tc, selections: p.parseSelectionSet()}
		}
		name := p.next()
		return &astNode{kind: "FragmentSpread", name: name}
	}
	name := p.next()
	alias := ""
	if p.peek() == ":" {
		p.next()
		alias = name
		name = p.next()
	}
	var args map[string]any
	if p.peek() == "(" {
		args = p.parseArgs()
	}
	for p.peek() == "@" {
		p.next()
		p.next()
		if p.peek() == "(" {
			p.parseArgs()
		}
	}
	var sels []*astNode
	if p.peek() == "{" {
		sels = p.parseSelectionSet()
	}
	return &astNode{kind: "Field", name: name, alias: alias, args: args, selections: sels}
}

func (p *gqlParser) parseArgs() map[string]any {
	p.expect("(")
	args := map[string]any{}
	for p.pos < len(p.toks) && p.peek() != ")" {
		name := p.next()
		p.expect(":")
		args[name] = p.parseValue()
	}
	p.expect(")")
	return args
}

func (p *gqlParser) parseValue() any {
	t := p.next()
	if t == "$" {
		return map[string]any{"$var": p.next()}
	}
	if t == "[" {
		var arr []any
		for p.peek() != "]" {
			arr = append(arr, p.parseValue())
		}
		p.expect("]")
		return arr
	}
	if t == "{" {
		obj := map[string]any{}
		for p.peek() != "}" {
			k := p.next()
			p.expect(":")
			obj[k] = p.parseValue()
		}
		p.expect("}")
		return obj
	}
	if strings.HasPrefix(t, "\"") {
		s, err := strconv.Unquote(t)
		if err != nil {
			return t
		}
		return s
	}
	if t == "true" {
		return true
	}
	if t == "false" {
		return false
	}
	if t == "null" {
		return nil
	}
	if isNumberTok(t) {
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return map[string]any{"$enum": t}
}

func isNumberTok(t string) bool {
	if t == "" {
		return false
	}
	i := 0
	if t[0] == '-' {
		i = 1
	}
	digits := 0
	for ; i < len(t); i++ {
		c := t[i]
		if c >= '0' && c <= '9' {
			digits++
		} else if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			// allowed inside a number token
		} else {
			return false
		}
	}
	return digits > 0
}

func resolveVars(v any, vars map[string]any) any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		if name, ok := m["$var"].(string); ok {
			return vars[name]
		}
		if e, ok := m["$enum"].(string); ok {
			return e
		}
		out := map[string]any{}
		for k, val := range m {
			out[k] = resolveVars(val, vars)
		}
		return out
	}
	if arr, ok := v.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, x := range arr {
			out = append(out, resolveVars(x, vars))
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------- schema

type schemaType struct {
	fields []string
	enums  []string
}

var schemaTypes = map[string]schemaType{
	"Issue":                    {fields: []string{"id", "number", "title", "state", "body", "createdAt", "author", "url", "comments"}},
	"PullRequest":              {fields: []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "createdAt", "author", "mergedAt"}},
	"Repository":               {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"Viewer":                   {fields: []string{"login", "id", "name"}},
	"User":                     {fields: []string{"login", "id", "name"}},
	"SearchType":               {enums: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"}},
	"IssueState":               {enums: []string{"OPEN", "CLOSED"}},
	"MergeableState":           {enums: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"PullRequestState":         {enums: []string{"OPEN", "CLOSED", "MERGED"}},
	"PullRequestMergeMethod":   {enums: []string{"MERGE", "SQUASH", "REBASE"}},
	"RepositoryPermission":     {enums: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"IssueOrderField":          {enums: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"OrderDirection":           {enums: []string{"ASC", "DESC"}},
	"CreateIssuePayload":       {fields: []string{"issue", "clientMutationId"}},
	"CloseIssuePayload":        {fields: []string{"issue", "clientMutationId"}},
	"CreatePullRequestPayload": {fields: []string{"pullRequest", "clientMutationId"}},
	"MergePullRequestPayload":  {fields: []string{"pullRequest", "clientMutationId"}},
	"AddCommentPayload":        {fields: []string{"commentEdge", "subject", "clientMutationId"}},
}

var queryFields = map[string]bool{"viewer": true, "repository": true, "node": true, "__type": true, "__schema": true}

func repoGQL(r *Repo) map[string]any {
	ownerID := 1001
	if r.owner == viewer {
		ownerID = viewerID
	}
	return map[string]any{
		"__type": "Repository", "id": gid("Repository", r.id), "databaseId": r.id, "name": r.name,
		"nameWithOwner":    r.owner + "/" + r.name,
		"owner":            map[string]any{"__type": "User", "login": r.owner, "id": ownerID, "name": "Ada Iversen"},
		"defaultBranchRef": map[string]any{"__type": "Ref", "name": r.defaultBranch},
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true, "_repo": r,
	}
}

func issueGQL(r *Repo, i *Issue) map[string]any {
	state := "OPEN"
	if i.state != "open" {
		state = "CLOSED"
	}
	return map[string]any{
		"__type": "Issue", "id": gid("Issue", i.id), "number": i.number, "title": i.title, "body": i.body,
		"state": state, "createdAt": fixedDate,
		"author": map[string]any{"__type": "User", "login": viewer, "id": viewerID},
		"url":    "https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + itoa(i.number),
		"_repo":  r, "_issue": i,
	}
}

func prGQL(r *Repo, p *PR) map[string]any {
	state := "OPEN"
	if p.state != "open" {
		if p.merged {
			state = "MERGED"
		} else {
			state = "CLOSED"
		}
	}
	mergeable := "UNKNOWN"
	if branchTip(r, p.headRef) != "" && branchTip(r, p.baseRef) != "" {
		if prConflict(r, p) == "" {
			mergeable = "MERGEABLE"
		} else {
			mergeable = "CONFLICTING"
		}
	}
	var mergedAt any = nil
	if p.merged {
		mergedAt = p.mergedAt
	}
	return map[string]any{
		"__type": "PullRequest", "id": gid("PullRequest", p.id), "number": p.number,
		"title": p.title, "body": p.body, "state": state, "merged": p.merged,
		"mergeable": mergeable, "headRefName": p.headRef, "baseRefName": p.baseRef,
		"headRefOid": p.headSha, "baseRefOid": p.baseSha,
		"createdAt": fixedDate, "mergedAt": mergedAt,
		"author": map[string]any{"__type": "User", "login": viewer, "id": viewerID},
		"_repo":  r, "_pr": p,
	}
}

func findRepoByDbID(id int) *Repo {
	for _, r := range repos {
		if r.id == id {
			return r
		}
	}
	return nil
}

func findIssueByID(id int) (*Repo, *Issue) {
	for _, r := range repos {
		for _, i := range r.issues {
			if i.id == id {
				return r, i
			}
		}
	}
	return nil, nil
}

func findPRByID(id int) (*Repo, *PR) {
	for _, r := range repos {
		for _, p := range r.prs {
			if p.id == id {
				return r, p
			}
		}
	}
	return nil, nil
}

func connectionGQL(typ string, items []map[string]any, args map[string]any) map[string]any {
	list := items
	base := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if decoded, err := base64.StdEncoding.DecodeString(after); err == nil {
			if idx, err := strconv.Atoi(string(decoded)[2:]); err == nil {
				if idx+1 < len(items) {
					list = items[idx+1:]
					base = idx + 1
				} else {
					list = []map[string]any{}
					base = len(items)
				}
			}
		}
	}
	var first *int
	if f, ok := args["first"]; ok {
		if n := toInt(f); n != nil {
			first = n
		}
	}
	sliced := list
	if first != nil && *first < len(list) {
		sliced = list[:*first]
	}
	hasNext := first != nil && len(list) > *first
	nodes := make([]any, 0, len(sliced))
	edges := make([]any, 0, len(sliced))
	for j, node := range sliced {
		nodes = append(nodes, node)
		edges = append(edges, map[string]any{"__type": strings.Replace(typ, "Connection", "Edge", 1), "node": node, "cursor": b64("i:" + itoa(base+j))})
	}
	var startCursor any = nil
	var endCursor any = nil
	if len(sliced) > 0 {
		startCursor = b64("i:" + itoa(base))
		endCursor = b64("i:" + itoa(base+len(sliced)-1))
	}
	return map[string]any{
		"__type": typ, "_items": sliced,
		"nodes": nodes, "edges": edges,
		"pageInfo":   map[string]any{"__type": "PageInfo", "hasNextPage": hasNext, "hasPreviousPage": false, "startCursor": startCursor, "endCursor": endCursor},
		"totalCount": len(items),
	}
}

func toInt(v any) *int {
	switch n := v.(type) {
	case float64:
		i := int(n)
		return &i
	case int:
		return &n
	case int64:
		i := int(n)
		return &i
	case json.Number:
		if i64, err := n.Int64(); err == nil {
			i := int(i64)
			return &i
		}
	}
	return nil
}

func getGQLField(typ string, obj map[string]any, field string, args map[string]any) any {
	r, _ := obj["_repo"].(*Repo)
	switch typ {
	case "Query":
		if field == "viewer" {
			return map[string]any{"__type": "Viewer", "login": viewer, "id": viewerID, "name": "Ada Iversen"}
		}
		if field == "repository" {
			owner := asStr(args, "owner")
			name := asStr(args, "name")
			if owner != viewer {
				return nil
			}
			repo := getRepo(owner, name)
			if repo == nil {
				return nil
			}
			return repoGQL(repo)
		}
		if field == "node" {
			kind, n, ok := decodeGid(asStr(args, "id"))
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				repo := findRepoByDbID(n)
				if repo == nil {
					return nil
				}
				return repoGQL(repo)
			case "Issue":
				fr, fi := findIssueByID(n)
				if fi == nil {
					return nil
				}
				return issueGQL(fr, fi)
			case "PullRequest":
				fr, fp := findPRByID(n)
				if fp == nil {
					return nil
				}
				return prGQL(fr, fp)
			}
			return nil
		}
		if field == "__type" {
			name := asStr(args, "name")
			t, ok := schemaTypes[name]
			if !ok {
				return nil
			}
			kind := "OBJECT"
			if len(t.enums) > 0 {
				kind = "ENUM"
			}
			fields := make([]any, 0, len(t.fields))
			for _, n := range t.fields {
				fields = append(fields, map[string]any{"__type": "__Field", "name": n})
			}
			enumValues := make([]any, 0, len(t.enums))
			for _, n := range t.enums {
				enumValues = append(enumValues, map[string]any{"__type": "__EnumValue", "name": n})
			}
			return map[string]any{"__type": "__Type", "name": name, "kind": kind, "fields": fields, "enumValues": enumValues}
		}
		if field == "__schema" {
			names := make([]string, 0, len(schemaTypes))
			for n := range schemaTypes {
				names = append(names, n)
			}
			sort.Strings(names)
			types := make([]any, 0, len(names))
			for _, n := range names {
				kind := "OBJECT"
				if len(schemaTypes[n].enums) > 0 {
					kind = "ENUM"
				}
				types = append(types, map[string]any{"__type": "__Type", "name": n, "kind": kind, "fields": []any{}, "enumValues": []any{}})
			}
			return map[string]any{"__type": "__Schema", "types": types}
		}
		return undefined
	case "Repository":
		if r == nil {
			return undefined
		}
		if field == "issue" {
			num := toIntOrZero(args["number"])
			if i, ok := r.issues[num]; ok {
				return issueGQL(r, i)
			}
			return nil
		}
		if field == "issues" {
			var list []*Issue
			for _, i := range r.issues {
				list = append(list, i)
			}
			sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
			if states, ok := args["states"].([]any); ok {
				want := map[string]bool{}
				for _, s := range states {
					if str, ok := s.(string); ok {
						want[strings.ToLower(str)] = true
					}
				}
				var filtered []*Issue
				for _, i := range list {
					if want[i.state] {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
			if ob, ok := args["orderBy"].(map[string]any); ok {
				dir := asStr(ob, "direction")
				if dir == "" {
					dir = "ASC"
				}
				if dir == "DESC" {
					sort.Slice(list, func(a, b int) bool { return list[a].seq > list[b].seq })
				} else {
					sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
				}
			}
			items := make([]map[string]any, 0, len(list))
			for _, i := range list {
				items = append(items, issueGQL(r, i))
			}
			return connectionGQL("IssueConnection", items, args)
		}
		if field == "pullRequest" {
			num := toIntOrZero(args["number"])
			if p, ok := r.prs[num]; ok {
				return prGQL(r, p)
			}
			return nil
		}
		if field == "pullRequests" {
			var list []*PR
			for _, p := range r.prs {
				list = append(list, p)
			}
			sort.Slice(list, func(a, b int) bool { return list[a].seq < list[b].seq })
			items := make([]map[string]any, 0, len(list))
			for _, p := range list {
				items = append(items, prGQL(r, p))
			}
			return connectionGQL("PullRequestConnection", items, args)
		}
		return obj[field]
	case "PullRequest":
		if field == "files" {
			p, _ := obj["_pr"].(*PR)
			if p == nil || r == nil {
				return connectionGQL("FileConnection", []map[string]any{}, args)
			}
			files := diffFiles(r, commitTree(r, p.baseSha), commitTree(r, p.headSha))
			nodes := make([]map[string]any, 0, len(files))
			for _, f := range files {
				ct := "MODIFIED"
				if f.status == "added" {
					ct = "ADDED"
				} else if f.status == "removed" {
					ct = "DELETED"
				}
				nodes = append(nodes, map[string]any{"__type": "ChangedFile", "path": f.filename, "additions": f.additions, "deletions": f.deletions, "changeType": ct})
			}
			return connectionGQL("FileConnection", nodes, args)
		}
		return obj[field]
	case "Issue":
		if field == "comments" {
			i, _ := obj["_issue"].(*Issue)
			nodes := make([]map[string]any, 0)
			if i != nil {
				for _, c := range i.comments {
					nodes = append(nodes, map[string]any{"__type": "Comment", "id": gid("Comment", c.id), "body": c.body})
				}
			}
			return connectionGQL("CommentConnection", nodes, args)
		}
		return obj[field]
	case "Mutation":
		return undefined
	default:
		return obj[field]
	}
}

func toIntOrZero(v any) int {
	if n := toInt(v); n != nil {
		return *n
	}
	return 0
}

type undefinedT struct{}

var undefined = undefinedT{}

func fieldAllowed(typ, field string) bool {
	if typ == "Query" {
		return queryFields[field]
	}
	if st, ok := schemaTypes[typ]; ok {
		for _, f := range st.fields {
			if f == field {
				return true
			}
		}
	}
	switch {
	case typ == "__Type" && (field == "name" || field == "kind" || field == "fields" || field == "enumValues"):
		return true
	case typ == "__Schema" && field == "types":
		return true
	case typ == "__Field" && field == "name":
		return true
	case typ == "__EnumValue" && field == "name":
		return true
	case typ == "PageInfo" && (field == "hasNextPage" || field == "hasPreviousPage" || field == "startCursor" || field == "endCursor"):
		return true
	case strings.HasSuffix(typ, "Connection") && (field == "nodes" || field == "edges" || field == "pageInfo" || field == "totalCount"):
		return true
	case strings.HasSuffix(typ, "Edge") && (field == "node" || field == "cursor"):
		return true
	case typ == "Ref" && field == "name":
		return true
	case typ == "ChangedFile" && (field == "path" || field == "additions" || field == "deletions" || field == "changeType"):
		return true
	case typ == "Comment" && (field == "id" || field == "body"):
		return true
	}
	return false
}

func applySelection(typ string, obj map[string]any, sels []*astNode, vars map[string]any, errors *[]string, parser *gqlParser) map[string]any {
	if obj == nil {
		return nil
	}
	out := map[string]any{}
	for _, sel := range sels {
		if sel.kind == "FragmentSpread" {
			if frag, ok := parser.fragments[sel.name]; ok {
				if sub := applySelection(typ, obj, frag, vars, errors, parser); sub != nil {
					for k, v := range sub {
						out[k] = v
					}
				}
			}
			continue
		}
		if sel.kind == "InlineFragment" {
			if sel.typeCondition != "" && sel.typeCondition != typ && sel.typeCondition != obj["__type"] {
				continue
			}
			if sub := applySelection(typ, obj, sel.selections, vars, errors, parser); sub != nil {
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		field := sel.name
		outName := field
		if sel.alias != "" {
			outName = sel.alias
		}
		if field == "__typename" {
			out[outName] = obj["__type"]
			continue
		}
		if typ != "Query" && !fieldAllowed(typ, field) {
			*errors = append(*errors, "Cannot query field \""+field+"\" on type \""+typ+"\".")
			continue
		}
		rawArgs := map[string]any{}
		if sel.args != nil {
			for k, v := range sel.args {
				rawArgs[k] = resolveVars(v, vars)
			}
		}
		val := getGQLField(typ, obj, field, rawArgs)
		if _, isUndef := val.(undefinedT); isUndef {
			*errors = append(*errors, "Cannot query field \""+field+"\" on type \""+typ+"\".")
			continue
		}
		out[outName] = applyValue(val, sel.selections, vars, errors, parser)
	}
	return out
}

func applyValue(val any, sels []*astNode, vars map[string]any, errors *[]string, parser *gqlParser) any {
	if val == nil {
		return nil
	}
	if arr, ok := val.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, v := range arr {
			out = append(out, applyValue(v, sels, vars, errors, parser))
		}
		return out
	}
	if arr, ok := val.([]map[string]any); ok {
		out := make([]any, 0, len(arr))
		for _, v := range arr {
			out = append(out, applyValue(v, sels, vars, errors, parser))
		}
		return out
	}
	if o, ok := val.(map[string]any); ok {
		if sels == nil {
			return o
		}
		t, _ := o["__type"].(string)
		if t == "" {
			t = "Object"
		}
		return applySelection(t, o, sels, vars, errors, parser)
	}
	return val
}

func executeGQLOp(query string, variables map[string]any) map[string]any {
	parser := &gqlParser{fragments: map[string][]*astNode{}}
	parser.toks = tokenizeGQL(query)
	ops := parser.parseDocument()
	if len(ops) == 0 {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation found"}}}
	}
	op := ops[0]
	var errors []string
	if op.operation == "mutation" {
		data := map[string]any{}
		for _, sel := range op.selections {
			field := sel.name
			rawArgs := map[string]any{}
			if sel.args != nil {
				for k, v := range sel.args {
					rawArgs[k] = resolveVars(v, variables)
				}
			}
			input, _ := rawArgs["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			outName := field
			if sel.alias != "" {
				outName = sel.alias
			}
			switch field {
			case "createIssue":
				kind, n, ok := decodeGid(asStr(input, "repositoryId"))
				_ = kind
				var repo *Repo
				if ok {
					repo = findRepoByDbID(n)
				}
				if repo == nil {
					errors = append(errors, "Repository not found")
					continue
				}
				num := repo.nextNumber
				repo.nextNumber++
				id := nextGlobalID
				nextGlobalID++
				seq := nextGlobalID
				nextGlobalID++
				issue := &Issue{id: id, number: num, title: asStr(input, "title"), body: asStr(input, "body"), state: "open", labels: []Label{}, seq: seq, comments: []Comment{}}
				repo.issues[num] = issue
				payload := map[string]any{"__type": "CreateIssuePayload", "issue": issueGQL(repo, issue), "clientMutationId": nil}
				data[outName] = applySelection("CreateIssuePayload", payload, sel.selections, variables, &errors, parser)
			case "closeIssue":
				_, n, ok := decodeGid(asStr(input, "issueId"))
				var fr *Repo
				var fi *Issue
				if ok {
					fr, fi = findIssueByID(n)
				}
				if fi == nil {
					errors = append(errors, "Issue not found")
					continue
				}
				fi.state = "closed"
				payload := map[string]any{"__type": "CloseIssuePayload", "issue": issueGQL(fr, fi), "clientMutationId": nil}
				data[outName] = applySelection("CloseIssuePayload", payload, sel.selections, variables, &errors, parser)
			case "createPullRequest":
				_, n, ok := decodeGid(asStr(input, "repositoryId"))
				var repo *Repo
				if ok {
					repo = findRepoByDbID(n)
				}
				if repo == nil {
					errors = append(errors, "Repository not found")
					continue
				}
				num := repo.nextNumber
				repo.nextNumber++
				id := nextGlobalID
				nextGlobalID++
				seq := nextGlobalID
				nextGlobalID++
				head := asStr(input, "headRefName")
				base := asStr(input, "baseRefName")
				pr := &PR{id: id, number: num, title: asStr(input, "title"), body: asStr(input, "body"), state: "open", merged: false, headRef: head, baseRef: base, headSha: branchTip(repo, head), baseSha: branchTip(repo, base), seq: seq, mergeSha: "", mergedAt: ""}
				repo.prs[num] = pr
				payload := map[string]any{"__type": "CreatePullRequestPayload", "pullRequest": prGQL(repo, pr), "clientMutationId": nil}
				data[outName] = applySelection("CreatePullRequestPayload", payload, sel.selections, variables, &errors, parser)
			case "mergePullRequest":
				_, n, ok := decodeGid(asStr(input, "pullRequestId"))
				var fr *Repo
				var fp *PR
				if ok {
					fr, fp = findPRByID(n)
				}
				if fp == nil {
					errors = append(errors, "Pull request not found")
					continue
				}
				if !fp.merged {
					doMerge(fr, fp)
				}
				payload := map[string]any{"__type": "MergePullRequestPayload", "pullRequest": prGQL(fr, fp), "clientMutationId": nil}
				data[outName] = applySelection("MergePullRequestPayload", payload, sel.selections, variables, &errors, parser)
			case "addComment":
				_, n, ok := decodeGid(asStr(input, "subjectId"))
				var fr *Repo
				var fi *Issue
				if ok {
					fr, fi = findIssueByID(n)
				}
				if fi == nil {
					errors = append(errors, "Subject not found")
					continue
				}
				comment := Comment{id: nextGlobalID, body: asStr(input, "body")}
				nextGlobalID++
				fi.comments = append(fi.comments, comment)
				payload := map[string]any{
					"__type": "AddCommentPayload", "clientMutationId": nil,
					"commentEdge": map[string]any{"__type": "CommentEdge", "node": map[string]any{"__type": "Comment", "id": gid("Comment", comment.id), "body": comment.body}, "cursor": b64("i:" + itoa(len(fi.comments)-1))},
					"subject":     issueGQL(fr, fi),
				}
				data[outName] = applySelection("AddCommentPayload", payload, sel.selections, variables, &errors, parser)
			default:
				errors = append(errors, "Cannot query field \""+field+"\" on type \"Mutation\".")
			}
		}
		if len(errors) > 0 {
			return map[string]any{"data": nil, "errors": errList(errors)}
		}
		return map[string]any{"data": data}
	}
	root := map[string]any{"__type": "Query"}
	data := applySelection("Query", root, op.selections, variables, &errors, parser)
	if len(errors) > 0 {
		return map[string]any{"data": nil, "errors": errList(errors)}
	}
	return map[string]any{"data": data}
}

func errList(errors []string) []any {
	out := make([]any, 0, len(errors))
	for _, m := range errors {
		out = append(out, map[string]any{"message": m})
	}
	return out
}
