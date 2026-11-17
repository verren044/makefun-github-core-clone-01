// rest.go — REST v3: repos, issues, pulls, contents, labels, branches,
// commits, git/refs. Faithful port of v1.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func err404() *apiError  { return &apiError{404, "Not Found"} }
func err422(m string) *apiError { return &apiError{422, m} }

// jsonBytes marshals v as compact JSON with a trailing newline (matching
// v1's json.dumps(payload) + "\n"). HTML escaping is off so UTF-8 passes
// through, as in Python's ensure_ascii=False equivalent for the wire.
func jsonBytes(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}

// ---- REST representations (struct field order matches v1's dict order)

type userRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type repoJSON struct {
	ID            int     `json:"id"`
	NodeID        string  `json:"node_id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         userRef `json:"owner"`
	Private       bool    `json:"private"`
	Description   string  `json:"description"`
	DefaultBranch string  `json:"default_branch"`
	HTMLURL       string  `json:"html_url"`
	URL           string  `json:"url"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	HasIssues     bool    `json:"has_issues"`
	HasProjects   bool    `json:"has_projects"`
	HasWiki       bool    `json:"has_wiki"`
	Fork          bool    `json:"fork"`
	Archived      bool    `json:"archived"`
	Disabled      bool    `json:"disabled"`
	Visibility    string  `json:"visibility"`
}

func repoRestJSON(repo *Repo) []byte {
	vis := "public"
	if repo.Private {
		vis = "private"
	}
	return jsonBytes(repoJSON{
		ID: repo.DBID, NodeID: repo.gid(), Name: repo.Name,
		FullName: repo.fullName(), Owner: userRef{repo.Owner, viewer.ID},
		Private: repo.Private, Description: repo.Description,
		DefaultBranch: repo.DefaultBranch, HTMLURL: "", URL: "",
		CreatedAt: nowISO(), UpdatedAt: nowISO(),
		HasIssues: true, HasProjects: true, HasWiki: true,
		Fork: false, Archived: false, Disabled: false, Visibility: vis,
	})
}

type labelRef struct {
	Name string `json:"name"`
}

type issueJSON struct {
	ID        int        `json:"id"`
	NodeID    string     `json:"node_id"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Labels    []labelRef `json:"labels"`
	User      userRef    `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	HTMLURL   string     `json:"html_url"`
	URL       string     `json:"url"`
}

// issueRestJSON returns the cached serialized issue (the read workload
// GETs one issue 2,000 times; this turns it into a map lookup + write).
func issueRestJSON(issue *Issue) []byte {
	if issue.cachedRest != nil {
		return issue.cachedRest
	}
	labels := make([]labelRef, 0, len(issue.Labels))
	for _, n := range issue.Labels {
		labels = append(labels, labelRef{n})
	}
	b := jsonBytes(issueJSON{
		ID: issue.Number*100000 + issue.Repo.DBID, NodeID: issue.GID,
		Number: issue.Number, Title: issue.Title, Body: issue.Body,
		State: issue.State, Labels: labels,
		User: userRef{viewerLogin, viewer.ID},
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		HTMLURL: "", URL: "",
	})
	// jsonBytes appends "\n"; strip it for the cache (the sender adds it).
	issue.cachedRest = bytes.TrimSuffix(b, []byte("\n"))
	return issue.cachedRest
}

type pullRef struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type pullJSON struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Head           pullRef `json:"head"`
	Base           pullRef `json:"base"`
	Merged         bool    `json:"merged"`
	Mergeable      bool    `json:"mergeable"`
	User           userRef `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	HTMLURL        string  `json:"html_url"`
	URL            string  `json:"url"`
	MergedAt       string  `json:"merged_at,omitempty"`
	MergeCommitSHA string  `json:"merge_commit_sha,omitempty"`
}

func pullRestJSON(pr *Pull) []byte {
	headSHA := pr.HeadSHA
	if headSHA == "" {
		headSHA = zeroSHA
	}
	baseSHA := pr.BaseSHA
	if baseSHA == "" {
		baseSHA = zeroSHA
	}
	return jsonBytes(pullJSON{
		ID: pr.Number*100000 + pr.Repo.DBID, NodeID: pr.GID,
		Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State,
		Head: pullRef{pr.Head, headSHA, pr.Repo.Owner + ":" + pr.Head},
		Base: pullRef{pr.Base, baseSHA, pr.Repo.Owner + ":" + pr.Base},
		Merged: pr.Merged, Mergeable: true,
		User: userRef{viewerLogin, viewer.ID},
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt,
		HTMLURL: "", URL: "",
		MergedAt: pr.MergedAt, MergeCommitSHA: pr.MergeCommitSHA,
	})
}

func findRepo(owner, name string) (*Repo, error) {
	repo, ok := store.repos[[2]string{owner, name}]
	if !ok {
		return nil, err404()
	}
	return repo, nil
}

func requireOwner(owner string) error {
	if owner != viewerLogin {
		return err404()
	}
	return nil
}

func createRepo(name string, description any, private any) (*Repo, error) {
	if name == "" {
		return nil, err422("name is required")
	}
	if _, ok := store.repos[[2]string{viewerLogin, name}]; ok {
		return nil, err422("Repository already exists")
	}
	dbid := store.nextRepoDBID
	store.nextRepoDBID++
	desc, _ := description.(string)
	priv, _ := private.(bool)
	repo := newRepo(name, viewerLogin, desc, priv, dbid)
	store.repos[[2]string{viewerLogin, name}] = repo
	store.repoOrder = append(store.repoOrder, [2]string{viewerLogin, name})
	return repo, nil
}

// contentsCommit creates a commit on branch setting path to content
// (or deleting when content is nil). Returns (commitSHA, blobSHA).
func contentsCommit(repo *Repo, branch string, parts []string, content []byte, message string, author string) (string, string) {
	ref := "refs/heads/" + branch
	tip := repo.Refs[ref]
	var baseTree string
	var parents []string
	if tip != "" {
		if obj, ok := getObject(repo, tip); ok && obj.Type == "commit" {
			baseTree, parents, _, _ = parseCommit(obj.Data)
			parents = []string{tip}
		}
	}
	var blobSHA string
	if content != nil {
		blobSHA = storeObject(repo, "blob", content)
	}
	newTree := treeSet(repo, baseTree, parts, "100644", blobSHA)
	msg := message
	if msg == "" {
		msg = "Update " + strings.Join(parts, "/")
	}
	commitSHA := makeCommit(repo, newTree, parents, msg, author)
	repo.Refs[ref] = commitSHA
	return commitSHA, blobSHA
}

// ---- dispatch

type restResult struct {
	status int
	body   []byte // serialized JSON (with trailing "\n") or nil for empty
	extra  map[string]string
}

func okJSON(status int, v any) restResult {
	return restResult{status: status, body: jsonBytes(v)}
}

func okRaw(status int, body []byte) restResult {
	return restResult{status: status, body: body}
}

// bodyMap parses a request body into a map; non-object bodies become an
// empty map (v1 would 500 on .get of a list; the suite never sends one).
func bodyMap(body any) map[string]any {
	if m, ok := body.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func strVal(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func boolVal(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// restDispatch routes a REST request. body is the parsed JSON (nil when
// the request had no body); rawBody is the raw bytes (unused here).
func restDispatch(method, path string, query url.Values, body any, headers map[string]string) (restResult, error) {
	seg := []string{}
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			if u, err := url.PathUnescape(s); err == nil {
				s = u
			}
			seg = append(seg, s)
		}
	}
	if len(seg) < 2 || seg[0] != "api" || seg[1] != "v3" {
		return restResult{}, err404()
	}
	rest := seg[2:]

	needJSON := func() (map[string]any, error) {
		if body == nil {
			return nil, &apiError{400, "Invalid JSON"}
		}
		return bodyMap(body), nil
	}

	if method == "GET" && len(rest) == 0 {
		return restResult{status: 200, body: jsonBytes(map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}), extra: map[string]string{"X-OAuth-Scopes": "repo, read:org"}}, nil
	}
	if method == "GET" && len(rest) == 1 && rest[0] == "meta" {
		return okJSON(200, map[string]any{"installed_version": version}), nil
	}
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "GET" {
			page, err := queryInt(query, "page", 1)
			if err != nil {
				return restResult{}, err
			}
			perPage, err := queryInt(query, "per_page", 30)
			if err != nil {
				return restResult{}, err
			}
			if perPage < 1 {
				perPage = 1
			}
			if perPage > 100 {
				perPage = 100
			}
			names := append([][2]string(nil), store.repoOrder...)
			total := len(names)
			start := (page - 1) * perPage
			if start < 0 {
				start = 0
			}
			end := start + perPage
			if end > total {
				end = total
			}
			var chunk [][2]string
			if start < total {
				chunk = names[start:end]
			}
			var buf bytes.Buffer
			buf.WriteByte('[')
			for i, k := range chunk {
				if i > 0 {
					buf.WriteByte(',')
				}
				buf.Write(bytes.TrimSuffix(repoRestJSON(store.repos[k]), []byte("\n")))
			}
			buf.WriteByte(']')
			buf.WriteByte('\n')
			res := restResult{status: 200, body: buf.Bytes()}
			if start+perPage < total {
				nxt := "/api/v3/user/repos?page=" + itoa(page+1) + "&per_page=" + itoa(perPage)
				if host := headers["Host"]; host != "" {
					nxt = "http://" + host + nxt
				}
				res.extra = map[string]string{"Link": "<" + nxt + ">; rel=\"next\""}
			}
			return res, nil
		}
		if method == "POST" {
			data, err := needJSON()
			if err != nil {
				return restResult{}, err
			}
			name, _ := data["name"].(string)
			repo, err := createRepo(name, data["description"], data["private"])
			if err != nil {
				return restResult{}, err
			}
			return restResult{status: 201, body: repoRestJSON(repo)}, nil
		}
		return restResult{}, err404()
	}
	if method == "GET" && len(rest) == 1 && rest[0] == "user" {
		return okJSON(200, map[string]any{
			"login": viewerLogin, "id": viewer.ID,
			"name": viewer.Name, "email": viewer.Email,
		}), nil
	}
	if len(rest) >= 3 && rest[0] == "repos" {
		owner, name := rest[1], rest[2]
		tail := rest[3:]
		if err := requireOwner(owner); err != nil {
			return restResult{}, err
		}
		if len(tail) == 0 {
			if method == "GET" {
				repo, err := findRepo(owner, name)
				if err != nil {
					return restResult{}, err
				}
				return restResult{status: 200, body: repoRestJSON(repo)}, nil
			}
			if method == "PATCH" {
				data, err := needJSON()
				if err != nil {
					return restResult{}, err
				}
				repo, err := findRepo(owner, name)
				if err != nil {
					return restResult{}, err
				}
				if d, ok := data["description"]; ok {
					if s, ok := d.(string); ok {
						repo.Description = s
					}
				}
				return restResult{status: 200, body: repoRestJSON(repo)}, nil
			}
			if method == "DELETE" {
				if _, ok := store.repos[[2]string{owner, name}]; !ok {
					return restResult{}, err404()
				}
				delete(store.repos, [2]string{owner, name})
				for i, k := range store.repoOrder {
					if k == [2]string{owner, name} {
						store.repoOrder = append(store.repoOrder[:i], store.repoOrder[i+1:]...)
						break
					}
				}
				return restResult{status: 204, body: nil}, nil
			}
			return restResult{}, err404()
		}
		return restRepoSub(method, tail, query, body, owner, name)
	}
	return restResult{}, err404()
}

// queryInt reads an int query param (Python int() semantics: "" -> default;
// non-numeric -> 500 via the dispatcher's recovery, matching v1's
// ValueError -> 500).
func queryInt(query url.Values, key string, def int) (int, error) {
	v := query.Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, &apiError{500, "Internal Server Error"}
	}
	return n, nil
}

func restRepoSub(method string, tail []string, query url.Values, body any, owner, name string) (restResult, error) {
	repo, err := findRepo(owner, name)
	if err != nil {
		return restResult{}, err
	}
	// ---- git/refs
	if len(tail) == 2 && tail[0] == "git" && tail[1] == "refs" && method == "POST" {
		data := bodyMap(body)
		ref, _ := data["ref"].(string)
		sha, _ := data["sha"].(string)
		if !strings.HasPrefix(ref, "refs/heads/") || sha == "" {
			return restResult{}, err422("Invalid ref")
		}
		if existing, ok := repo.Refs[ref]; ok && existing != "" {
			return restResult{}, err422("Reference already exists")
		}
		repo.Refs[ref] = sha
		return okJSON(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}), nil
	}
	if len(tail) >= 4 && tail[0] == "git" && tail[1] == "refs" && method == "GET" {
		ref := "refs/" + strings.Join(tail[2:], "/")
		sha := repo.Refs[ref]
		if sha == "" {
			return restResult{}, err404()
		}
		return okJSON(200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}), nil
	}
	// ---- branches
	if len(tail) == 1 && tail[0] == "branches" && method == "GET" {
		var out []any
		for _, ref := range repo.branches() {
			sha := repo.Refs[ref]
			out = append(out, map[string]any{
				"name": ref[len("refs/heads/"):],
				"commit": map[string]any{"sha": sha},
				"protected": false,
			})
		}
		if out == nil {
			out = []any{}
		}
		return okJSON(200, out), nil
	}
	if len(tail) >= 2 && tail[0] == "branches" && method == "GET" {
		branch := strings.Join(tail[1:], "/")
		sha := repo.branchTip(branch)
		if sha == "" {
			return restResult{}, err404()
		}
		return okJSON(200, map[string]any{
			"name": branch, "commit": map[string]any{"sha": sha}, "protected": false,
		}), nil
	}
	// ---- commits
	if len(tail) == 1 && tail[0] == "commits" && method == "GET" {
		branch := query.Get("sha")
		tip := ""
		if branch != "" {
			tip = repo.branchTip(branch)
		} else {
			tip = repo.branchTip(repo.DefaultBranch)
		}
		var out []any
		if tip != "" {
			for _, sha := range commitHistory(repo, tip, 1000) {
				obj, _ := getObject(repo, sha)
				_, _, message, _ := parseCommit(obj.Data)
				out = append(out, map[string]any{
					"sha": sha,
					"commit": map[string]any{"message": string(message)},
					"author": map[string]any{"login": viewerLogin},
				})
			}
		}
		if out == nil {
			out = []any{}
		}
		return okJSON(200, out), nil
	}
	if len(tail) == 2 && tail[0] == "commits" && method == "GET" {
		obj, ok := getObject(repo, tail[1])
		if !ok || obj.Type != "commit" {
			return restResult{}, err404()
		}
		_, _, message, _ := parseCommit(obj.Data)
		return okJSON(200, map[string]any{
			"sha": tail[1],
			"commit": map[string]any{"message": string(message)},
		}), nil
	}
	// ---- contents
	if len(tail) > 0 && tail[0] == "contents" {
		return restContents(method, tail[1:], query, body, repo)
	}
	// ---- labels
	if len(tail) == 1 && tail[0] == "labels" {
		if method == "GET" {
			var out []any
			for _, n := range repo.LabelOrder {
				l := repo.Labels[n]
				out = append(out, map[string]any{"name": l.Name, "color": l.Color})
			}
			if out == nil {
				out = []any{}
			}
			return okJSON(200, out), nil
		}
		if method == "POST" {
			data := bodyMap(body)
			lname, _ := data["name"].(string)
			if lname == "" {
				return restResult{}, err422("name is required")
			}
			color, _ := data["color"].(string)
			if color == "" {
				color = "ededed"
			}
			desc, _ := data["description"].(string)
			if _, ok := repo.Labels[lname]; !ok {
				repo.LabelOrder = append(repo.LabelOrder, lname)
			}
			repo.Labels[lname] = label{Name: lname, Color: color}
			_ = desc
			return okJSON(201, map[string]any{"name": lname, "color": color, "description": desc}), nil
		}
		return restResult{}, err404()
	}
	// ---- issues
	if len(tail) == 1 && tail[0] == "issues" {
		if method == "GET" {
			return restResult{status: 200, body: listIssuesJSON(repo, query)}, nil
		}
		if method == "POST" {
			data := bodyMap(body)
			var labelsRaw []any
			if l, ok := data["labels"]; ok {
				if arr, ok := l.([]any); ok {
					labelsRaw = arr
				}
			}
			var labelNames []string
			for _, lb := range labelsRaw {
				var lname string
				switch v := lb.(type) {
				case string:
					lname = v
				case map[string]any:
					lname, _ = v["name"].(string)
				}
				if _, ok := repo.Labels[lname]; !ok {
					return restResult{}, err422("Label does not exist")
				}
				labelNames = append(labelNames, lname)
			}
			num := repo.NextIssue
			repo.NextIssue++
			issue := newIssue(repo, num, strVal(data, "title", ""), strVal(data, "body", ""), labelNames)
			repo.Issues[num] = issue
			repo.IssueOrder = append(repo.IssueOrder, num)
			return restResult{status: 201, body: append(issueRestJSON(issue), '\n')}, nil
		}
		return restResult{}, err404()
	}
	if len(tail) >= 2 && tail[0] == "issues" {
		num, err := strconv.Atoi(tail[1])
		if err != nil {
			return restResult{}, err404()
		}
		issue, ok := repo.Issues[num]
		if !ok {
			return restResult{}, err404()
		}
		if len(tail) == 2 {
			if method == "GET" {
				return restResult{status: 200, body: append(issueRestJSON(issue), '\n')}, nil
			}
			if method == "PATCH" {
				data := bodyMap(body)
				if v, ok := data["state"]; ok {
					if s, ok := v.(string); ok {
						issue.State = s
					}
				}
				if v, ok := data["title"]; ok {
					if s, ok := v.(string); ok {
						issue.Title = s
					}
				}
				if v, ok := data["body"]; ok {
					if s, ok := v.(string); ok {
						issue.Body = s
					}
				}
				issue.UpdatedAt = nowISO()
				issue.invalidate()
				return restResult{status: 200, body: append(issueRestJSON(issue), '\n')}, nil
			}
			return restResult{}, err404()
		}
		if len(tail) == 3 && tail[2] == "comments" {
			if method == "GET" {
				var out []any
				for _, c := range issue.Comments {
					out = append(out, map[string]any{
						"id": c.ID, "body": c.Body,
						"user": map[string]any{"login": viewerLogin},
					})
				}
				if out == nil {
					out = []any{}
				}
				return okJSON(200, out), nil
			}
			if method == "POST" {
				data := bodyMap(body)
				cid := issue.NextComment
				issue.NextComment++
				comment := map[string]any{
					"id": cid, "body": strVal(data, "body", ""),
					"user": map[string]any{"login": viewerLogin},
				}
				issue.Comments = append(issue.Comments, issueComment{ID: cid, Body: strVal(data, "body", "")})
				return okJSON(201, comment), nil
			}
			return restResult{}, err404()
		}
		return restResult{}, err404()
	}
	// ---- pulls
	if len(tail) == 1 && tail[0] == "pulls" {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var buf bytes.Buffer
			buf.WriteByte('[')
			first := true
			for _, num := range repo.PullOrder {
				pr := repo.Pulls[num]
				if state == "all" || pr.State == state {
					if !first {
						buf.WriteByte(',')
					}
					first = false
					buf.Write(bytes.TrimSuffix(pullRestJSON(pr), []byte("\n")))
				}
			}
			buf.WriteByte(']')
			buf.WriteByte('\n')
			return restResult{status: 200, body: buf.Bytes()}, nil
		}
		if method == "POST" {
			data := bodyMap(body)
			head := strVal(data, "head", "")
			base := strVal(data, "base", repo.DefaultBranch)
			headSHA := repo.branchTip(head)
			if headSHA == "" {
				return restResult{}, err422("Head branch does not exist")
			}
			baseSHA := repo.branchTip(base)
			num := repo.NextPull
			repo.NextPull++
			pr := newPull(repo, num, strVal(data, "title", ""), strVal(data, "body", ""), head, base, headSHA, baseSHA)
			repo.Pulls[num] = pr
			repo.PullOrder = append(repo.PullOrder, num)
			return restResult{status: 201, body: pullRestJSON(pr)}, nil
		}
		return restResult{}, err404()
	}
	if len(tail) >= 2 && tail[0] == "pulls" {
		num, err := strconv.Atoi(tail[1])
		if err != nil {
			return restResult{}, err404()
		}
		pr := repo.Pulls[num]
		if len(tail) == 2 {
			if pr == nil {
				return restResult{}, err404()
			}
			if method == "GET" {
				return restResult{status: 200, body: pullRestJSON(pr)}, nil
			}
			return restResult{}, err404()
		}
		if len(tail) == 3 && tail[2] == "files" && method == "GET" {
			if pr == nil {
				return restResult{}, err404()
			}
			return okJSON(200, pullFiles(repo, pr)), nil
		}
		if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
			if pr == nil {
				return restResult{}, err404()
			}
			return mergePull(repo, pr, bodyMap(body))
		}
		return restResult{}, err404()
	}
	return restResult{}, err404()
}

func listIssuesJSON(repo *Repo, query url.Values) []byte {
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	direction := query.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	items := make([]*Issue, 0, len(repo.IssueOrder))
	for _, n := range repo.IssueOrder {
		items = append(items, repo.Issues[n])
	}
	if state == "open" || state == "closed" {
		filtered := items[:0]
		for _, i := range items {
			if i.State == state {
				filtered = append(filtered, i)
			}
		}
		items = filtered
	}
	reverse := direction != "asc"
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
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(issueRestJSON(it))
	}
	buf.WriteByte(']')
	buf.WriteByte('\n')
	return buf.Bytes()
}

func pullFiles(repo *Repo, pr *Pull) []any {
	var baseTree, headTree string
	if pr.BaseSHA != "" {
		if obj, ok := getObject(repo, pr.BaseSHA); ok {
			baseTree, _, _, _ = parseCommit(obj.Data)
		}
	}
	if pr.HeadSHA != "" {
		if obj, ok := getObject(repo, pr.HeadSHA); ok {
			headTree, _, _, _ = parseCommit(obj.Data)
		}
	}
	changed := diffPaths(repo, baseTree, headTree)
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []any
	for _, path := range paths {
		var b, h bool
		if baseTree != "" {
			_, _, b = treeGet(repo, baseTree, strings.Split(path, "/"))
		}
		if headTree != "" {
			_, _, h = treeGet(repo, headTree, strings.Split(path, "/"))
		}
		var status string
		var additions int
		if !b && h {
			status, additions = "added", 1
		} else if b && !h {
			status, additions = "removed", 0
		} else {
			status, additions = "modified", 1
		}
		out = append(out, map[string]any{
			"filename": path, "status": status,
			"additions": additions, "deletions": 0, "changes": additions,
		})
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func mergePull(repo *Repo, pr *Pull, data map[string]any) (restResult, error) {
	if pr.State != "open" {
		return restResult{}, &apiError{405, "Pull request is not open"}
	}
	baseTip := repo.branchTip(pr.Base)
	headTip := repo.branchTip(pr.Head)
	if headTip == "" || baseTip == "" {
		return restResult{}, &apiError{405, "Cannot merge"}
	}
	var baseTree, headTree, prBaseTree string
	if obj, ok := getObject(repo, baseTip); ok {
		baseTree, _, _, _ = parseCommit(obj.Data)
	}
	if obj, ok := getObject(repo, headTip); ok {
		headTree, _, _, _ = parseCommit(obj.Data)
	}
	if pr.BaseSHA != "" {
		if obj, ok := getObject(repo, pr.BaseSHA); ok {
			prBaseTree, _, _, _ = parseCommit(obj.Data)
		}
	}
	headChanged := diffPaths(repo, prBaseTree, headTree)
	baseChanged := diffPaths(repo, prBaseTree, baseTree)
	for p := range headChanged {
		if baseChanged[p] {
			return restResult{}, &apiError{405, "Merge conflict"}
		}
	}
	newTree := baseTree
	// deterministic order: sort the changed paths (v1 iterates a set;
	// tree_set is order-independent for the final tree, so any order
	// gives the same resulting tree sha).
	paths := make([]string, 0, len(headChanged))
	for p := range headChanged {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		mode, sha, ok := treeGet(repo, headTree, strings.Split(path, "/"))
		if !ok {
			newTree = treeSet(repo, newTree, strings.Split(path, "/"), "100644", "")
		} else {
			newTree = treeSet(repo, newTree, strings.Split(path, "/"), mode, sha)
		}
	}
	msg := "Squash merge #" + itoa(pr.Number) + ": " + pr.Title
	squashSHA := makeCommit(repo, newTree, []string{baseTip}, msg, "")
	repo.Refs["refs/heads/"+pr.Base] = squashSHA
	pr.State = "closed"
	pr.Merged = true
	pr.MergeCommitSHA = squashSHA
	pr.MergedAt = nowISO()
	pr.UpdatedAt = nowISO()
	return okJSON(200, map[string]any{
		"sha": squashSHA, "merged": true,
		"message": "Pull Request successfully merged",
	}), nil
}

func restContents(method string, parts []string, query url.Values, body any, repo *Repo) (restResult, error) {
	if len(parts) == 0 {
		return restResult{}, err404()
	}
	path := strings.Join(parts, "/")
	ref := query.Get("ref")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	if method == "GET" {
		tip := repo.branchTip(ref)
		if tip == "" {
			return restResult{}, err404()
		}
		obj, _ := getObject(repo, tip)
		tree, _, _, _ := parseCommit(obj.Data)
		mode, sha, ok := treeGet(repo, tree, parts)
		if !ok {
			return restResult{}, err404()
		}
		if mode == "40000" {
			tobj, _ := getObject(repo, sha)
			var out []any
			for _, e := range parseTree(tobj.Data) {
				name := string(e.Name)
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				out = append(out, map[string]any{
					"type": typ, "name": name, "path": path + "/" + name,
					"sha": e.SHA, "size": 0,
				})
			}
			if out == nil {
				out = []any{}
			}
			return okJSON(200, out), nil
		}
		bobj, _ := getObject(repo, sha)
		var content []byte
		if bobj.Type != "" {
			content = bobj.Data
		}
		return okJSON(200, map[string]any{
			"type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(content),
			"sha": sha, "size": len(content),
			"name": parts[len(parts)-1], "path": path,
		}), nil
	}
	if method == "PUT" {
		data := bodyMap(body)
		branch := strVal(data, "branch", repo.DefaultBranch)
		rawContent, _ := data["content"].(string)
		rawContent = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, rawContent)
		content, err := base64.StdEncoding.DecodeString(rawContent)
		if err != nil {
			return restResult{}, err422("Invalid base64 content")
		}
		tip := repo.branchTip(branch)
		var curSHA string
		var curExists bool
		if tip != "" {
			if obj, ok := getObject(repo, tip); ok {
				tree, _, _, _ := parseCommit(obj.Data)
				_, curSHA, curExists = treeGet(repo, tree, parts)
			}
		}
		existed := curExists
		if existed {
			if sha, ok := data["sha"]; ok {
				if s, ok := sha.(string); ok && s != "" && !strings.EqualFold(s, curSHA) {
					return restResult{}, &apiError{409, "sha does not match"}
				}
			}
		} else {
			if sha, ok := data["sha"]; ok {
				if s, ok := sha.(string); ok && s != "" {
					return restResult{}, &apiError{409, "sha does not match"}
				}
			}
		}
		commitSHA, blobSHA := contentsCommit(repo, branch, parts, content, strVal(data, "message", ""), "")
		return restResult{status: map[bool]int{true: 200, false: 201}[existed], body: jsonBytes(map[string]any{
			"content": map[string]any{
				"sha": blobSHA, "encoding": "base64",
				"name": parts[len(parts)-1], "path": path,
			},
			"commit": map[string]any{"sha": commitSHA},
		})}, nil
	}
	if method == "DELETE" {
		data := bodyMap(body)
		branch := strVal(data, "branch", repo.DefaultBranch)
		tip := repo.branchTip(branch)
		if tip == "" {
			return restResult{}, err404()
		}
		obj, _ := getObject(repo, tip)
		tree, _, _, _ := parseCommit(obj.Data)
		_, curSHA, ok := treeGet(repo, tree, parts)
		if !ok {
			return restResult{}, err404()
		}
		if sha, ok := data["sha"]; ok {
			if s, ok := sha.(string); ok && s != "" && !strings.EqualFold(s, curSHA) {
				return restResult{}, &apiError{409, "sha does not match"}
			}
		}
		commitSHA, _ := contentsCommit(repo, branch, parts, nil, strVal(data, "message", ""), "")
		return okJSON(200, map[string]any{"commit": map[string]any{"sha": commitSHA}}), nil
	}
	return restResult{}, err404()
}
