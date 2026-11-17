// gitobj.go — git objects: SHA-1 hashing, object store, tree/commit/tag
// parse and build, tree navigation, reachability. Faithful port of v1.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"sort"
)

type gitObject struct {
	Type string // "commit" | "tree" | "blob" | "tag"
	Data []byte
}

func gitHash(otype string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(otype))
	h.Write([]byte(" "))
	h.Write([]byte(itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func storeObject(repo *Repo, otype string, data []byte) string {
	sha := gitHash(otype, data)
	if _, ok := repo.Objects[sha]; !ok {
		repo.Objects[sha] = gitObject{Type: otype, Data: data}
	}
	return sha
}

func getObject(repo *Repo, sha string) (gitObject, bool) {
	o, ok := repo.Objects[sha]
	return o, ok
}

// parseTag returns the target sha of an annotated tag object.
func parseTag(data []byte) (target string, message []byte) {
	head, msg, _ := bytes.Cut(data, []byte("\n\n"))
	for _, line := range bytes.Split(head, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			target = string(line[7:])
		}
	}
	return target, msg
}

// parseCommit returns (treeSha, parents, message, rawHeaders).
func parseCommit(data []byte) (tree string, parents []string, message []byte, head []byte) {
	head, msg, _ := bytes.Cut(data, []byte("\n\n"))
	for _, line := range bytes.Split(head, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("tree ")) {
			tree = string(line[5:])
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			parents = append(parents, string(line[7:]))
		}
	}
	return tree, parents, msg, head
}

// makeCommit builds a commit object with the fixed timestamp
// 1735689600 (2025-01-01T00:00:00Z): deterministic.
func makeCommit(repo *Repo, treeSHA string, parents []string, message string, author string) string {
	who := author
	if who == "" {
		who = viewer.Name + " <" + viewer.Email + ">"
	}
	ident := who + " 1735689600 +0000"
	var out bytes.Buffer
	out.WriteString("tree " + treeSHA + "\n")
	for _, p := range parents {
		out.WriteString("parent " + p + "\n")
	}
	out.WriteString("author " + ident + "\ncommitter " + ident + "\n\n")
	out.WriteString(message)
	return storeObject(repo, "commit", out.Bytes())
}

type treeEntry struct {
	Mode string
	Name []byte
	SHA  string
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
		mode := string(data[i:sp])
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		name := data[sp+1 : nul]
		sha := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, treeEntry{Mode: mode, Name: name, SHA: sha})
		i = nul + 21
	}
	return out
}

func treeSortKey(e treeEntry) []byte {
	if e.Mode == "40000" {
		k := make([]byte, 0, len(e.Name)+1)
		k = append(k, e.Name...)
		return append(k, '/')
	}
	return e.Name
}

func buildTree(entries []treeEntry) []byte {
	// git sorts tree entries with directories compared as if their name
	// had a trailing slash.
	sorted := append([]treeEntry(nil), entries...)
	sort.SliceStable(sorted, func(a, b int) bool {
		return bytes.Compare(treeSortKey(sorted[a]), treeSortKey(sorted[b])) < 0
	})
	var out bytes.Buffer
	for _, e := range sorted {
		out.WriteString(e.Mode)
		out.WriteByte(' ')
		out.Write(e.Name)
		out.WriteByte(0)
		raw, _ := hex.DecodeString(e.SHA)
		out.Write(raw)
	}
	return out.Bytes()
}

// treeGet resolves path parts in a tree; returns (mode, sha) or nil.
func treeGet(repo *Repo, treeSHA string, parts []string) (string, string, bool) {
	cur := treeSHA
	for idx, part := range parts {
		obj, ok := getObject(repo, cur)
		if !ok || obj.Type != "tree" {
			return "", "", false
		}
		var found *treeEntry
		for _, e := range parseTree(obj.Data) {
			if bytes.Equal(e.Name, []byte(part)) {
				ee := e
				found = &ee
				break
			}
		}
		if found == nil {
			return "", "", false
		}
		if idx == len(parts)-1 {
			return found.Mode, found.SHA, true
		}
		cur = found.SHA
	}
	return "", "", false
}

// treeSet returns a new tree sha with path set to (mode, blobSHA).
// blobSHA == "" deletes the path.
func treeSet(repo *Repo, treeSHA string, parts []string, mode, blobSHA string) string {
	if len(parts) == 0 {
		return treeSHA
	}
	var entries []treeEntry
	if treeSHA != "" {
		if obj, ok := getObject(repo, treeSHA); ok && obj.Type == "tree" {
			entries = parseTree(obj.Data)
		}
	}
	part := parts[0]
	rest := parts[1:]
	var out []treeEntry
	replaced := false
	for _, e := range entries {
		if bytes.Equal(e.Name, []byte(part)) {
			replaced = true
			if len(rest) > 0 {
				newSub := treeSet(repo, e.SHA, rest, mode, blobSHA)
				out = append(out, treeEntry{Mode: e.Mode, Name: e.Name, SHA: newSub})
			} else {
				if blobSHA == "" {
					continue // delete
				}
				out = append(out, treeEntry{Mode: mode, Name: e.Name, SHA: blobSHA})
			}
		} else {
			out = append(out, e)
		}
	}
	if !replaced && blobSHA != "" {
		if len(rest) > 0 {
			newSub := treeSet(repo, "", rest, mode, blobSHA)
			out = append(out, treeEntry{Mode: "40000", Name: []byte(part), SHA: newSub})
		} else {
			out = append(out, treeEntry{Mode: mode, Name: []byte(part), SHA: blobSHA})
		}
	}
	return storeObject(repo, "tree", buildTree(out))
}

// isAncestor reports whether commit anc is reachable from commit desc
// (or equal).
func isAncestor(repo *Repo, anc, desc string) bool {
	if anc == desc {
		return true
	}
	if anc == "" || desc == "" {
		return false
	}
	seen := map[string]bool{}
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj, ok := getObject(repo, cur)
		if !ok || obj.Type != "commit" {
			continue
		}
		_, parents, _, _ := parseCommit(obj.Data)
		stack = append(stack, parents...)
	}
	return false
}

// commitHistory returns commits reachable from tip, newest-first (BFS by
// generation — FIFO queue, like v1's list.pop(0)).
func commitHistory(repo *Repo, tip string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	qi := 0
	for qi < len(queue) && len(out) < limit {
		cur := queue[qi]
		qi++
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		obj, ok := getObject(repo, cur)
		if !ok || obj.Type != "commit" {
			continue
		}
		_, parents, _, _ := parseCommit(obj.Data)
		out = append(out, cur)
		queue = append(queue, parents...)
	}
	return out
}

// treePaths returns all (path, mode, sha) file entries under a tree.
func treePaths(repo *Repo, treeSHA, prefix string) [][3]string {
	obj, ok := getObject(repo, treeSHA)
	if !ok || obj.Type != "tree" {
		return nil
	}
	var out [][3]string
	for _, e := range parseTree(obj.Data) {
		path := prefix + string(e.Name)
		if e.Mode == "40000" {
			out = append(out, treePaths(repo, e.SHA, path+"/")...)
		} else {
			out = append(out, [3]string{path, e.Mode, e.SHA})
		}
	}
	return out
}

// diffPaths returns paths whose blob sha differs between two trees.
func diffPaths(repo *Repo, baseTree, headTree string) map[string]bool {
	b := map[string]string{}
	if baseTree != "" {
		for _, p := range treePaths(repo, baseTree, "") {
			b[p[0]] = p[2]
		}
	}
	h := map[string]string{}
	if headTree != "" {
		for _, p := range treePaths(repo, headTree, "") {
			h[p[0]] = p[2]
		}
	}
	changed := map[string]bool{}
	for p := range b {
		if b[p] != h[p] {
			changed[p] = true
		}
	}
	for p := range h {
		if b[p] != h[p] {
			changed[p] = true
		}
	}
	return changed
}

func sortStrings(s []string) { sort.Strings(s) }
