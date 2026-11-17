// graphql.go — GraphQL v4: tokenizer, parser, executor. Faithful port of
// v1, with two deliberate fixes for checks v1 fails:
//   - ChangedFile.path falls back to the pull-files "filename" (v1 returns
//     null; the suite's PullRequestFiles check requires the path string).
//   - float literals tokenize as floats (v1's int-first alternation errors
//     on "1.5"); a superset of v1's accepted queries.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

// ---- tokenizer

type gqlToken struct {
	kind string // "name" | "int" | "float" | "string" | "punct" | "eof"
	val  any
}

func gqlTokenize(text string) ([]gqlToken, error) {
	var toks []gqlToken
	i := 0
	for i < len(text) {
		c := text[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}
		// name
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i + 1
			for j < len(text) && ((text[j] >= 'A' && text[j] <= 'Z') || (text[j] >= 'a' && text[j] <= 'z') || (text[j] >= '0' && text[j] <= '9') || text[j] == '_') {
				j++
			}
			toks = append(toks, gqlToken{"name", text[i:j]})
			i = j
			continue
		}
		// number: int or float (float when digits are followed by "." or
		// an exponent — v1's int-first alternation errored on "1.5").
		if (c >= '0' && c <= '9') || (c == '-' && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9') {
			j := i
			if text[j] == '-' {
				j++
			}
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			isFloat := false
			if j < len(text) && text[j] == '.' {
				isFloat = true
				j++
				for j < len(text) && text[j] >= '0' && text[j] <= '9' {
					j++
				}
			}
			if j < len(text) && (text[j] == 'e' || text[j] == 'E') {
				k := j + 1
				if k < len(text) && (text[k] == '+' || text[k] == '-') {
					k++
				}
				if k < len(text) && text[k] >= '0' && text[k] <= '9' {
					isFloat = true
					j = k
					for j < len(text) && text[j] >= '0' && text[j] <= '9' {
						j++
					}
				}
			}
			num := text[i:j]
			if isFloat {
				f, err := strconv.ParseFloat(num, 64)
				if err != nil {
					return nil, &gqlError{fmt.Sprintf("Bad float %s", num)}
				}
				toks = append(toks, gqlToken{"float", f})
			} else {
				n, err := strconv.ParseInt(num, 10, 64)
				if err != nil {
					return nil, &gqlError{fmt.Sprintf("Bad int %s", num)}
				}
				toks = append(toks, gqlToken{"int", n})
			}
			i = j
			continue
		}
		// string (JSON escapes, like Python's json.loads)
		if c == '"' {
			j := i + 1
			for j < len(text) {
				if text[j] == '\\' {
					j += 2
					continue
				}
				if text[j] == '"' {
					break
				}
				j++
			}
			if j >= len(text) {
				return nil, &gqlError{"Unterminated string"}
			}
			raw := text[i : j+1]
			var s string
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				return nil, &gqlError{fmt.Sprintf("Bad string %s", raw)}
			}
			toks = append(toks, gqlToken{"string", s})
			i = j + 1
			continue
		}
		// punct: "..." or one of {}():$![],=@
		if strings.HasPrefix(text[i:], "...") {
			toks = append(toks, gqlToken{"punct", "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}():$![],=@", rune(c)) {
			toks = append(toks, gqlToken{"punct", string(c)})
			i++
			continue
		}
		return nil, &gqlError{fmt.Sprintf("Unexpected character %q", string(c))}
	}
	toks = append(toks, gqlToken{"eof", nil})
	return toks, nil
}

// ---- parser

type gqlSelection struct {
	fragment bool
	on       string // fragment type condition ("" = none)
	name     string
	alias    string
	args     map[string]gqlValue
	sels     []*gqlSelection
}

type gqlValue struct {
	kind string // "var" | "list" | "object" | "lit" | "enum"
	v    any
}

type gqlOperation struct {
	typ  string // "query" | "mutation" | "subscription"
	name string
	sels []*gqlSelection
}

type gqlParser struct {
	toks []gqlToken
	i    int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.i] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.i]; p.i++; return t }
func (p *gqlParser) expect(kind string, val any) (gqlToken, error) {
	t := p.next()
	if t.kind != kind || (val != nil && t.val != val) {
		return t, &gqlError{fmt.Sprintf("Expected %s %v, got %v", kind, val, t)}
	}
	return t, nil
}

func (p *gqlParser) parse() ([]*gqlOperation, error) {
	var ops []*gqlOperation
	for p.peek().kind != "eof" {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	opType := "query"
	var name string
	k, v := p.peek().kind, p.peek().val
	if k == "name" && (v == "query" || v == "mutation" || v == "subscription") {
		p.next()
		opType = v.(string)
		if p.peek().kind == "name" {
			name = p.next().val.(string)
		}
	} else if k == "punct" && v == "{" {
		// shorthand query
	} else {
		return nil, &gqlError{fmt.Sprintf("Expected operation, got %v", p.peek())}
	}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		if err := p.parseVarDefs(); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{typ: opType, name: name, sels: sels}, nil
}

func (p *gqlParser) parseVarDefs() error {
	if _, err := p.expect("punct", "("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := p.next()
		if t.kind == "punct" && t.val == "(" {
			depth++
		} else if t.kind == "punct" && t.val == ")" {
			depth--
		} else if t.kind == "eof" {
			return &gqlError{"Unterminated variable definitions"}
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if _, err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for {
		k, v := p.peek().kind, p.peek().val
		if k == "punct" && v == "}" {
			p.next()
			return sels, nil
		}
		if k == "punct" && v == "..." {
			p.next()
			var fragType string
			hasOn := false
			if p.peek().kind == "name" && p.peek().val == "on" {
				p.next()
				t, err := p.expect("name", nil)
				if err != nil {
					return nil, err
				}
				fragType = t.val.(string)
				hasOn = true
			} else {
				if _, err := p.expect("name", nil); err != nil {
					return nil, err
				}
			}
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			_ = hasOn
			sels = append(sels, &gqlSelection{fragment: true, on: fragType, sels: sub})
			continue
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		sels = append(sels, f)
	}
}

func (p *gqlParser) parseField() (*gqlSelection, error) {
	t, err := p.expect("name", nil)
	if err != nil {
		return nil, err
	}
	name := t.val.(string)
	alias := ""
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		alias = name
		t2, err := p.expect("name", nil)
		if err != nil {
			return nil, err
		}
		name = t2.val.(string)
	}
	if alias == "" {
		alias = name
	}
	args := map[string]gqlValue{}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		args, err = p.parseArgs()
		if err != nil {
			return nil, err
		}
	}
	var sels []*gqlSelection
	if p.peek().kind == "punct" && p.peek().val == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{name: name, alias: alias, args: args, sels: sels}, nil
}

func (p *gqlParser) parseArgs() (map[string]gqlValue, error) {
	if _, err := p.expect("punct", "("); err != nil {
		return nil, err
	}
	args := map[string]gqlValue{}
	for {
		if p.peek().kind == "punct" && p.peek().val == ")" {
			p.next()
			return args, nil
		}
		t, err := p.expect("name", nil)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[t.val.(string)] = v
	}
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.next()
	switch {
	case t.kind == "punct" && t.val == "$":
		n, err := p.expect("name", nil)
		if err != nil {
			return gqlValue{}, err
		}
		return gqlValue{"var", n.val.(string)}, nil
	case t.kind == "punct" && t.val == "[":
		var out []gqlValue
		for !(p.peek().kind == "punct" && p.peek().val == "]") {
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			out = append(out, v)
		}
		p.next()
		return gqlValue{"list", out}, nil
	case t.kind == "punct" && t.val == "{":
		out := map[string]gqlValue{}
		for !(p.peek().kind == "punct" && p.peek().val == "}") {
			n, err := p.expect("name", nil)
			if err != nil {
				return gqlValue{}, err
			}
			if _, err := p.expect("punct", ":"); err != nil {
				return gqlValue{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			out[n.val.(string)] = v
		}
		p.next()
		return gqlValue{"object", out}, nil
	case t.kind == "string" || t.kind == "int" || t.kind == "float":
		return gqlValue{"lit", t.val}, nil
	case t.kind == "name":
		switch t.val.(string) {
		case "true":
			return gqlValue{"lit", true}, nil
		case "false":
			return gqlValue{"lit", false}, nil
		case "null":
			return gqlValue{"lit", nil}, nil
		}
		return gqlValue{"enum", t.val.(string)}, nil
	}
	return gqlValue{}, &gqlError{fmt.Sprintf("Bad value %v", t.val)}
}

func resolveValue(val gqlValue, variables map[string]any) any {
	switch val.kind {
	case "var":
		return variables[val.v.(string)]
	case "list":
		out := make([]any, 0, len(val.v.([]gqlValue)))
		for _, x := range val.v.([]gqlValue) {
			out = append(out, resolveValue(x, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, x := range val.v.(map[string]gqlValue) {
			out[k] = resolveValue(x, variables)
		}
		return out
	}
	return val.v
}

// ---- schema field sets

var gqlFields = map[string]map[string]bool{
	"Query":    {"viewer": true, "repository": true, "node": true, "__type": true},
	"Mutation": {"createIssue": true, "closeIssue": true, "createPullRequest": true, "mergePullRequest": true, "addComment": true},
	"Repository": {"id": true, "databaseId": true, "name": true, "nameWithOwner": true, "owner": true,
		"defaultBranchRef": true, "viewerPermission": true, "hasIssuesEnabled": true, "issue": true, "issues": true, "pullRequests": true, "pullRequest": true},
	"Issue":       {"id": true, "number": true, "title": true, "state": true, "body": true},
	"PullRequest": {"id": true, "number": true, "headRefName": true, "baseRefName": true, "mergeable": true, "state": true, "merged": true, "files": true},
	"User":        {"login": true, "id": true, "name": true},
	"BranchRef":   {"name": true},
	"IssueConnection":       {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"FileConnection":        {"nodes": true, "pageInfo": true},
	"PageInfo":              {"hasNextPage": true, "endCursor": true, "hasPreviousPage": true, "startCursor": true},
	"ChangedFile":           {"path": true},
	"Comment":               {"id": true, "body": true},
	"IssueEdge":             {"node": true},
	"CommentEdge":           {"node": true},
	"CreateIssuePayload":      {"issue": true},
	"CloseIssuePayload":       {"issue": true},
	"CreatePullRequestPayload": {"pullRequest": true},
	"MergePullRequestPayload":  {"pullRequest": true},
	"AddCommentPayload":        {"commentEdge": true},
	"__Type":  {"fields": true, "enumValues": true, "name": true},
	"__Field": {"name": true},
	"__EnumValue": {"name": true},
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"SearchType":  nil,
}

var gqlEnumValues = map[string][]string{
	"SearchType": {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "COMMIT"},
}

func gqlCursor(number int) string {
	return base64.URLEncoding.EncodeToString([]byte("cursor:" + itoa(number)))
}

func gqlCursorNum(cursor string) (int, bool) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	s := string(raw)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func gqlIssueState(issue *Issue) string { return strings.ToUpper(issue.State) }

func gqlPRState(pr *Pull) string {
	if pr.Merged {
		return "MERGED"
	}
	return strings.ToUpper(pr.State)
}

func gqlPRMergeable(repo *Repo, pr *Pull) string {
	if pr.Merged || pr.State != "open" {
		return "UNKNOWN"
	}
	baseTip := repo.branchTip(pr.Base)
	headTip := repo.branchTip(pr.Head)
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	var baseTree, headTree, prBaseTree string
	for _, pair := range [][2]string{{baseTip, "base"}, {headTip, "head"}, {pr.BaseSHA, "prbase"}} {
		sha, slot := pair[0], pair[1]
		if sha == "" {
			continue
		}
		obj, ok := getObject(repo, sha)
		if !ok {
			continue
		}
		tree, _, _, _ := parseCommit(obj.Data)
		switch slot {
		case "base":
			baseTree = tree
		case "head":
			headTree = tree
		default:
			prBaseTree = tree
		}
	}
	headChanged := diffPaths(repo, prBaseTree, headTree)
	baseChanged := diffPaths(repo, prBaseTree, baseTree)
	for p := range headChanged {
		if baseChanged[p] {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

// ---- executor

type gqlExecutor struct {
	vars   map[string]any
	errors []any
}

func (ex *gqlExecutor) args(sel *gqlSelection) map[string]any {
	out := map[string]any{}
	for k, v := range sel.args {
		out[k] = resolveValue(v, ex.vars)
	}
	return out
}

func (ex *gqlExecutor) execute(op *gqlOperation) (map[string]any, error) {
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	data, err := ex.execSelections(rootType, nil, op.sels)
	if err != nil {
		// ApiError (e.g. merge of a non-open PR) is an HTTP error, not a
		// GraphQL error — v1's dispatch returns its status (405).
		if _, ok := err.(*apiError); ok {
			return nil, err
		}
		ex.errors = append(ex.errors, map[string]any{"message": err.Error()})
		return map[string]any{"data": nil, "errors": ex.errors}, nil
	}
	out := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		out["errors"] = ex.errors
	}
	return out, nil
}

func (ex *gqlExecutor) execSelections(typeName string, obj any, sels []*gqlSelection) (map[string]any, error) {
	allowed := gqlFields[typeName]
	result := map[string]any{}
	for _, sel := range sels {
		if sel.fragment {
			on := sel.on
			if on != "" && on != typeName && !(typeName == "Query" && on == "Repository") {
				// inline fragment on a different type: skip unless the
				// object actually is that type (node() results)
				typename := ""
				if m, ok := obj.(map[string]any); ok {
					if t, ok := m["__typename"].(string); ok {
						typename = t
					}
				}
				if obj == nil || typename != on {
					continue
				}
			}
			subType := on
			if subType == "" {
				subType = typeName
			}
			sub, err := ex.execSelections(subType, obj, sel.sels)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				result[k] = v
			}
			continue
		}
		if !allowed[sel.name] {
			return nil, &gqlError{fmt.Sprintf("Cannot query field %q on type %q", sel.name, typeName)}
		}
		v, err := ex.resolve(typeName, obj, sel)
		if err != nil {
			return nil, err
		}
		result[sel.alias] = v
	}
	return result, nil
}

func (ex *gqlExecutor) resolve(typeName string, obj any, sel *gqlSelection) (any, error) {
	name := sel.name
	args := ex.args(sel)
	subs := sel.sels
	switch typeName {
	case "Query":
		switch name {
		case "viewer":
			return ex.execSelections("User", map[string]any{"login": viewerLogin}, subs)
		case "repository":
			owner, _ := args["owner"].(string)
			rname, _ := args["name"].(string)
			repo, ok := store.repos[[2]string{owner, rname}]
			if !ok {
				return nil, nil
			}
			return ex.execSelections("Repository", repo, subs)
		case "node":
			gid, _ := args["id"].(string)
			if strings.HasPrefix(gid, "R") {
				if dbid, err := strconv.Atoi(gid[1:]); err == nil {
					for _, k := range store.repoOrder {
						repo := store.repos[k]
						if repo.DBID == dbid {
							return ex.execSelections("Repository", repo, subs)
						}
					}
				}
			}
			return nil, nil
		case "__type":
			tname, _ := args["name"].(string)
			return ex.introspect(tname, subs)
		}
	case "Mutation":
		return ex.mutation(name, args, subs)
	case "Repository":
		return ex.repoField(obj.(*Repo), name, args, subs)
	case "Issue":
		issue := obj.(*Issue)
		switch name {
		case "state":
			return gqlIssueState(issue), nil
		case "id":
			return issue.GID, nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "body":
			return issue.Body, nil
		}
	case "PullRequest":
		return ex.prField(obj.(*Pull), name, args, subs)
	case "User":
		if name == "login" {
			return viewerLogin, nil
		}
		return nil, nil
	case "BranchRef":
		if m, ok := obj.(map[string]any); ok && name == "name" {
			return m["name"], nil
		}
		return nil, nil
	case "IssueConnection", "PullRequestConnection", "FileConnection":
		m := obj.(map[string]any)
		switch name {
		case "nodes":
			nodes, _ := m["nodes"].([]any)
			tname := map[string]string{
				"IssueConnection": "Issue", "PullRequestConnection": "PullRequest",
				"FileConnection": "ChangedFile",
			}[typeName]
			out := make([]any, 0, len(nodes))
			for _, n := range nodes {
				v, err := ex.execSelections(tname, n, subs)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		case "pageInfo":
			pi, _ := m["pageInfo"].(map[string]any)
			return ex.execSelections("PageInfo", pi, subs)
		case "totalCount":
			nodes, _ := m["nodes"].([]any)
			return len(nodes), nil
		}
	case "PageInfo":
		if m, ok := obj.(map[string]any); ok {
			return m[name], nil
		}
		return nil, nil
	case "ChangedFile":
		if m, ok := obj.(map[string]any); ok && name == "path" {
			// v1 reads obj["path"] only; pull_files emits "filename",
			// so v1 answers null here and the suite's PullRequestFiles
			// check fails. Fall back to "filename".
			if p, ok := m["path"]; ok {
				return p, nil
			}
			return m["filename"], nil
		}
		return nil, nil
	case "Comment":
		if m, ok := obj.(map[string]any); ok {
			return m[name], nil
		}
		return nil, nil
	case "CreateIssuePayload", "CloseIssuePayload":
		m := obj.(map[string]any)
		return ex.execSelections("Issue", m["issue"], subs)
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		m := obj.(map[string]any)
		return ex.execSelections("PullRequest", m["pullRequest"], subs)
	case "AddCommentPayload":
		if name == "commentEdge" {
			m := obj.(map[string]any)
			return ex.execSelections("CommentEdge", map[string]any{"node": m["comment"]}, subs)
		}
	case "CommentEdge":
		if name == "node" {
			m := obj.(map[string]any)
			return ex.execSelections("Comment", m["node"], subs)
		}
	case "__Type":
		m := obj.(map[string]any)
		switch name {
		case "fields":
			var out []any
			if f, ok := m["fields"].([]string); ok {
				for _, s := range f {
					out = append(out, map[string]any{"name": s})
				}
			}
			if out == nil {
				out = []any{}
			}
			return out, nil
		case "enumValues":
			var out []any
			if f, ok := m["enumValues"].([]string); ok {
				for _, s := range f {
					out = append(out, map[string]any{"name": s})
				}
			}
			if out == nil {
				out = []any{}
			}
			return out, nil
		case "name":
			return m["name"], nil
		}
	}
	return nil, nil
}

func (ex *gqlExecutor) introspect(typeName string, subs []*gqlSelection) (any, error) {
	obj := map[string]any{
		"name": typeName,
		"fields": gqlTypeFields[typeName],
		"enumValues": gqlEnumValues[typeName],
	}
	return ex.execSelections("__Type", obj, subs)
}

func (ex *gqlExecutor) repoField(repo *Repo, name string, args map[string]any, subs []*gqlSelection) (any, error) {
	switch name {
	case "id":
		return repo.gid(), nil
	case "databaseId":
		return repo.DBID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.fullName(), nil
	case "owner":
		return ex.execSelections("User", map[string]any{"login": repo.Owner}, subs)
	case "defaultBranchRef":
		return ex.execSelections("BranchRef", map[string]any{"name": repo.DefaultBranch}, subs)
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		num := toInt(args["number"])
		issue, ok := repo.Issues[num]
		if !ok {
			return nil, nil
		}
		return ex.execSelections("Issue", issue, subs)
	case "issues":
		return ex.issuesConn(repo, args, subs)
	case "pullRequests":
		return ex.prsConn(repo, args, subs)
	case "pullRequest":
		num := toInt(args["number"])
		pr, ok := repo.Pulls[num]
		if !ok {
			return nil, nil
		}
		return ex.execSelections("PullRequest", pr, subs)
	}
	return nil, nil
}

func (ex *gqlExecutor) issuesConn(repo *Repo, args map[string]any, subs []*gqlSelection) (any, error) {
	items := make([]*Issue, 0, len(repo.IssueOrder))
	for _, n := range repo.IssueOrder {
		items = append(items, repo.Issues[n])
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		want := map[string]bool{}
		for _, s := range states {
			if str, ok := s.(string); ok {
				want[strings.ToUpper(str)] = true
			}
		}
		filtered := items[:0]
		for _, i := range items {
			if want[gqlIssueState(i)] {
				filtered = append(filtered, i)
			}
		}
		items = filtered
	}
	order, _ := args["orderBy"].(map[string]any)
	field := "CREATED_AT"
	if order != nil {
		if f, ok := order["field"].(string); ok && f != "" {
			field = strings.ToUpper(f)
		}
	}
	_ = field // v1 computes it but sorts by (created_seq, number) only
	direction := "DESC"
	if order != nil {
		if d, ok := order["direction"].(string); ok && d != "" {
			direction = strings.ToUpper(d)
		}
	}
	reverse := direction != "ASC"
	sortIssues(items, reverse)
	var first *int
	if f, ok := args["first"]; ok && f != nil {
		n := toInt(f)
		first = &n
	}
	after, _ := args["after"].(string)
	if after != "" {
		if num, ok := gqlCursorNum(after); ok {
			filtered := items[:0]
			for _, i := range items {
				if (reverse && i.Number < num) || (!reverse && i.Number > num) {
					filtered = append(filtered, i)
				}
			}
			items = filtered
		}
	}
	total := len(items)
	_ = total
	page := items
	if first != nil && *first < len(items) {
		page = items[:*first]
	}
	hasNext := len(items) > len(page)
	var endCursor any
	if len(page) > 0 {
		endCursor = gqlCursor(page[len(page)-1].Number)
	}
	nodes := make([]any, 0, len(page))
	for _, i := range page {
		nodes = append(nodes, i)
	}
	return ex.execSelections("IssueConnection", map[string]any{
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": hasNext, "endCursor": endCursor,
			"hasPreviousPage": false, "startCursor": nil,
		},
	}, subs)
}

func (ex *gqlExecutor) prsConn(repo *Repo, args map[string]any, subs []*gqlSelection) (any, error) {
	items := make([]*Pull, 0, len(repo.PullOrder))
	for _, n := range repo.PullOrder {
		items = append(items, repo.Pulls[n])
	}
	var first *int
	if f, ok := args["first"]; ok && f != nil {
		n := toInt(f)
		first = &n
	}
	page := items
	if first != nil && *first < len(items) {
		page = items[:*first]
	}
	nodes := make([]any, 0, len(page))
	for _, p := range page {
		nodes = append(nodes, p)
	}
	return ex.execSelections("PullRequestConnection", map[string]any{
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": len(items) > len(page),
			"endCursor": nil, "hasPreviousPage": false, "startCursor": nil,
		},
	}, subs)
}

func (ex *gqlExecutor) prField(pr *Pull, name string, args map[string]any, subs []*gqlSelection) (any, error) {
	switch name {
	case "id":
		return pr.GID, nil
	case "number":
		return pr.Number, nil
	case "headRefName":
		return pr.Head, nil
	case "baseRefName":
		return pr.Base, nil
	case "mergeable":
		return gqlPRMergeable(pr.Repo, pr), nil
	case "state":
		return gqlPRState(pr), nil
	case "merged":
		return pr.Merged, nil
	case "files":
		files := pullFiles(pr.Repo, pr)
		if f, ok := args["first"]; ok && f != nil {
			n := toInt(f)
			if n < len(files) {
				files = files[:n]
			}
		}
		return ex.execSelections("FileConnection", map[string]any{
			"nodes": files,
			"pageInfo": map[string]any{
				"hasNextPage": false, "endCursor": nil,
				"hasPreviousPage": false, "startCursor": nil,
			},
		}, subs)
	}
	return nil, nil
}

func (ex *gqlExecutor) mutation(name string, args map[string]any, subs []*gqlSelection) (any, error) {
	inp, _ := args["input"].(map[string]any)
	if inp == nil {
		inp = map[string]any{}
	}
	switch name {
	case "createIssue":
		repo := ex.repoByGID(strVal2(inp, "repositoryId"))
		if repo == nil {
			return nil, &gqlError{"Repository not found"}
		}
		num := repo.NextIssue
		repo.NextIssue++
		issue := newIssue(repo, num, strVal2(inp, "title"), strVal2(inp, "body"), []string{})
		repo.Issues[num] = issue
		repo.IssueOrder = append(repo.IssueOrder, num)
		return ex.execSelections("CreateIssuePayload", map[string]any{"issue": issue}, subs)
	case "closeIssue":
		issue := ex.issueByGID(strVal2(inp, "issueId"))
		if issue == nil {
			return nil, &gqlError{"Issue not found"}
		}
		issue.State = "closed"
		issue.UpdatedAt = nowISO()
		issue.invalidate()
		return ex.execSelections("CloseIssuePayload", map[string]any{"issue": issue}, subs)
	case "createPullRequest":
		repo := ex.repoByGID(strVal2(inp, "repositoryId"))
		if repo == nil {
			return nil, &gqlError{"Repository not found"}
		}
		head := strVal2(inp, "headRefName")
		base := strVal2(inp, "baseRefName")
		if base == "" {
			base = repo.DefaultBranch
		}
		headSHA := repo.branchTip(head)
		if headSHA == "" {
			return nil, &gqlError{"Head branch does not exist"}
		}
		num := repo.NextPull
		repo.NextPull++
		pr := newPull(repo, num, strVal2(inp, "title"), strVal2(inp, "body"), head, base, headSHA, repo.branchTip(base))
		repo.Pulls[num] = pr
		repo.PullOrder = append(repo.PullOrder, num)
		return ex.execSelections("CreatePullRequestPayload", map[string]any{"pullRequest": pr}, subs)
	case "mergePullRequest":
		pr := ex.prByGID(strVal2(inp, "pullRequestId"))
		if pr == nil {
			return nil, &gqlError{"Pull request not found"}
		}
		method := strVal2(inp, "mergeMethod")
		if method == "" {
			method = "SQUASH"
		}
		if _, err := mergePull(pr.Repo, pr, map[string]any{"merge_method": strings.ToLower(method)}); err != nil {
			return nil, err
		}
		return ex.execSelections("MergePullRequestPayload", map[string]any{"pullRequest": pr}, subs)
	case "addComment":
		issue := ex.issueByGID(strVal2(inp, "subjectId"))
		if issue == nil {
			return nil, &gqlError{"Issue not found"}
		}
		cid := issue.NextComment
		issue.NextComment++
		body := strVal2(inp, "body")
		// NOTE: v1 stores the comment with the int id in issue.comments
		// but returns the "C<n>" gid in the mutation payload.
		issue.Comments = append(issue.Comments, issueComment{ID: cid, Body: body})
		gid := store.gid("C")
		comment := map[string]any{"id": gid, "body": body, "user": map[string]any{"login": viewerLogin}}
		return ex.execSelections("AddCommentPayload", map[string]any{"comment": comment}, subs)
	}
	return nil, &gqlError{fmt.Sprintf("Cannot query field %q on type %q", name, "Mutation")}
}

func (ex *gqlExecutor) repoByGID(gid string) *Repo {
	if !strings.HasPrefix(gid, "R") {
		return nil
	}
	dbid, err := strconv.Atoi(gid[1:])
	if err != nil {
		return nil
	}
	for _, k := range store.repoOrder {
		if store.repos[k].DBID == dbid {
			return store.repos[k]
		}
	}
	return nil
}

func (ex *gqlExecutor) issueByGID(gid string) *Issue {
	if !strings.HasPrefix(gid, "I") {
		return nil
	}
	for _, k := range store.repoOrder {
		for _, issue := range store.repos[k].Issues {
			if issue.GID == gid {
				return issue
			}
		}
	}
	return nil
}

func (ex *gqlExecutor) prByGID(gid string) *Pull {
	if !strings.HasPrefix(gid, "P") {
		return nil
	}
	for _, k := range store.repoOrder {
		for _, pr := range store.repos[k].Pulls {
			if pr.GID == gid {
				return pr
			}
		}
	}
	return nil
}

// handleGraphQL runs a parsed GraphQL document (dict or list). The error
// return is non-nil only for an ApiError (HTTP error, e.g. merge of a
// non-open PR), which the dispatcher turns into its status code.
func handleGraphQL(doc any) (int, any, error) {
	if list, ok := doc.([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			v, err := runGQL(item)
			if err != nil {
				return 0, nil, err
			}
			out = append(out, v)
		}
		return 200, out, nil
	}
	v, err := runGQL(doc)
	if err != nil {
		return 0, nil, err
	}
	return 200, v, nil
}

func runGQL(item any) (any, error) {
	m, _ := item.(map[string]any)
	var query string
	var variables map[string]any
	if m != nil {
		query, _ = m["query"].(string)
		variables, _ = m["variables"].(map[string]any)
	}
	if variables == nil {
		variables = map[string]any{}
	}
	toks, err := gqlTokenize(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}, nil
	}
	p := &gqlParser{toks: toks}
	ops, err := p.parse()
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}, nil
	}
	if len(ops) == 0 {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Empty query"}}}, nil
	}
	if len(ops) == 1 {
		return (&gqlExecutor{vars: variables}).execute(ops[0])
	}
	var results []any
	for _, op := range ops {
		v, err := (&gqlExecutor{vars: variables}).execute(op)
		if err != nil {
			return nil, err
		}
		results = append(results, v)
	}
	return results, nil
}

// toInt converts a JSON number (float64 from encoding/json, or int64 from
// the GraphQL tokenizer) to int.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func strVal2(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func sortIssues(items []*Issue, reverse bool) {
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].CreatedSeq != items[b].CreatedSeq {
			if reverse {
				return items[a].CreatedSeq > items[b].CreatedSeq
			}
			return items[a].CreatedSeq < items[b].CreatedSeq
		}
		if reverse {
			return items[a].Number > items[b].Number
		}
		return items[a].Number < items[b].Number
	})
}
