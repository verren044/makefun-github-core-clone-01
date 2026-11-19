// smart.go — git smart HTTP: v0/v2 advertisements, upload-pack v0/v2,
// receive-pack v0, report-status. Faithful port of v1.
package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const uploadV0Caps = "multi_ack thin-pack side-band side-band-64k ofs-delta " +
	"shallow deepen-since deepen-not deepen-relative no-progress " +
	"include-tag multi_ack_detailed no-done object-format=sha1 " +
	"agent=forge/1.0"
const receiveV0Caps = "report-status delete-refs side-band-64k quiet atomic " +
	"ofs-delta agent=forge/1.0"

var v2Caps = []string{"ls-refs=unborn", "fetch=shallow wait-for-done",
	"server-option", "object-format=sha1", "agent=forge/1.0"}

func gitAdvertiseV0(repo *Repo, service string) []byte {
	caps := uploadV0Caps
	if service != "git-upload-pack" {
		caps = receiveV0Caps
	}
	var out bytes.Buffer
	out.Write(pktLine([]byte("# service=" + service + "\n")))
	out.Write(pktFlush)
	headSHA := repo.Refs[repo.HeadRef]
	caps = "symref=HEAD:" + repo.HeadRef + " " + caps
	// sorted refs
	refNames := make([]string, 0, len(repo.Refs))
	for r := range repo.Refs {
		refNames = append(refNames, r)
	}
	sort.Strings(refNames)
	type line struct{ sha, ref string }
	var lines []line
	if headSHA != "" {
		lines = append(lines, line{headSHA, "HEAD"})
	}
	for _, ref := range refNames {
		if sha := repo.Refs[ref]; sha != "" {
			lines = append(lines, line{sha, ref})
		}
	}
	if len(lines) == 0 {
		out.Write(pktLine([]byte(zeroSHA + " capabilities^{}\x00" + caps + "\n")))
		out.Write(pktFlush)
		return out.Bytes()
	}
	first := true
	for _, l := range lines {
		if first {
			out.Write(pktLine([]byte(l.sha + " " + l.ref + "\x00" + caps + "\n")))
			first = false
		} else {
			out.Write(pktLine([]byte(l.sha + " " + l.ref + "\n")))
		}
		if strings.HasPrefix(l.ref, "refs/tags/") {
			if obj, ok := getObject(repo, l.sha); ok && obj.Type == "tag" {
				if target, _ := parseTag(obj.Data); target != "" {
					out.Write(pktLine([]byte(target + " " + l.ref + "^{}\n")))
				}
			}
		}
	}
	out.Write(pktFlush)
	return out.Bytes()
}

func gitAdvertiseV2() []byte {
	return v2AdvertiseBytes
}

// v2AdvertiseBytes is the whole v2 capability advertisement, computed
// once: it never varies, and every clone requests it.
var v2AdvertiseBytes = func() []byte {
	var out bytes.Buffer
	out.Write(pktLine([]byte("version 2\n")))
	for _, cap := range v2Caps {
		out.Write(pktLine([]byte(cap + "\n")))
	}
	out.Write(pktFlush)
	return out.Bytes()
}()

func handleUploadPackV0(repo *Repo, body []byte) []byte {
	lines := pktLinesAll(body)
	var wants []string
	haves := map[string]bool{}
	oldShallow := map[string]bool{}
	var deepen *int
	done := false
	for _, ln := range lines {
		if ln == "flush" || ln == "delim" {
			continue
		}
		text := strings.TrimSpace(string(ln.([]byte)))
		if i := strings.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		switch {
		case strings.HasPrefix(text, "want "):
			wants = append(wants, strings.Split(text[5:], " ")[0])
		case strings.HasPrefix(text, "have "):
			haves[strings.Split(text[5:], " ")[0]] = true
		case strings.HasPrefix(text, "shallow "):
			oldShallow[strings.Split(text[8:], " ")[0]] = true
		case strings.HasPrefix(text, "deepen "):
			if v, err := strconv.Atoi(text[7:]); err == nil {
				deepen = &v
			} else {
				deepen = nil
			}
		case text == "done":
			done = true
		}
	}
	if len(wants) > 0 {
		// Remember the negotiation: over stateless HTTP the client sends
		// "done" in a follow-up request that carries no wants.
		dedup := []string{}
		seen := map[string]bool{}
		for _, w := range wants {
			if !seen[w] {
				seen[w] = true
				dedup = append(dedup, w)
			}
		}
		repo.LastUpload = &lastUploadState{Wants: dedup, Deepen: deepen, OldShallow: oldShallow}
	}
	if !done {
		if deepen != nil && len(wants) > 0 {
			_, _, boundaries, _ := buildPackFiltered(repo, wants, haves, deepen, oldShallow, false)
			var out bytes.Buffer
			for _, w := range sortedKeys(boundaries) {
				out.Write(pktLine([]byte("shallow " + w + "\n")))
			}
			out.Write(pktFlush)
			return out.Bytes()
		}
		return pktLine([]byte("NAK\n"))
	}
	if len(wants) == 0 {
		if repo.LastUpload != nil {
			wants, deepen, oldShallow = repo.LastUpload.Wants, repo.LastUpload.Deepen, repo.LastUpload.OldShallow
		}
	}
	_, framed, boundaries, _ := buildPackFiltered(repo, wants, haves, deepen, oldShallow, false)
	var out bytes.Buffer
	if len(boundaries) > 0 {
		for _, w := range sortedKeys(boundaries) {
			out.Write(pktLine([]byte("shallow " + w + "\n")))
		}
		out.Write(pktFlush)
	}
	out.Write(pktLine([]byte("NAK\n")))
	out.Write(framed)
	out.Write(pktFlush)
	return out.Bytes()
}

func handleReceivePackV0(repo *Repo, body []byte) []byte {
	lines, rest := pktLines(body)
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	caps := ""
	for _, ln := range lines {
		if ln == "flush" || ln == "delim" {
			break
		}
		text := strings.TrimSpace(string(ln.([]byte)))
		if i := strings.IndexByte(text, 0); i >= 0 {
			caps = text[i+1:]
			text = text[:i]
		}
		parts := strings.Split(text, " ")
		if len(parts) >= 3 {
			cmds = append(cmds, cmd{parts[0], parts[1], parts[2]})
		}
	}
	useSideband := strings.Contains(caps, "side-band-64k") || strings.Contains(caps, "side-band")
	var objs []packObject
	if len(rest) > 0 {
		var err error
		objs, err = parsePack(rest)
		if err != nil {
			return reportStatusTyped(nil, [][2]string{{"unpack", err.Error()}}, useSideband, false)
		}
	}
	for _, o := range objs {
		storeObject(repo, o.Type, o.Data)
	}
	var results []statusResult
	for _, c := range cmds {
		if c.new == zeroSHA {
			delete(repo.Refs, c.ref)
			results = append(results, statusResult{c.ref, true, ""})
			continue
		}
		obj, ok := getObject(repo, c.new)
		if !ok {
			results = append(results, statusResult{c.ref, false, "object not found"})
			continue
		}
		if strings.HasPrefix(c.ref, "refs/tags/") {
			if obj.Type != "tag" && obj.Type != "commit" {
				results = append(results, statusResult{c.ref, false, "object not found"})
				continue
			}
			repo.Refs[c.ref] = c.new
			results = append(results, statusResult{c.ref, true, ""})
			continue
		}
		if obj.Type != "commit" {
			results = append(results, statusResult{c.ref, false, "object not found"})
			continue
		}
		if c.old == zeroSHA {
			repo.Refs[c.ref] = c.new
			results = append(results, statusResult{c.ref, true, ""})
		} else if isAncestor(repo, c.old, c.new) {
			repo.Refs[c.ref] = c.new
			results = append(results, statusResult{c.ref, true, ""})
		} else {
			results = append(results, statusResult{c.ref, false, "non-fast-forward"})
		}
	}
	return reportStatusTyped(results, nil, useSideband, true)
}

func reportStatusTyped(results []statusResult, errors [][2]string, useSideband bool, ok bool) []byte {
	if !ok {
		msg := "error"
		if len(errors) > 0 {
			msg = errors[0][1]
		}
		body := pktLine([]byte("unpack " + msg + "\n"))
		body = append(body, pktFlush...)
		if useSideband {
			return sidebandWrap(body)
		}
		return body
	}
	var body bytes.Buffer
	body.Write(pktLine([]byte("unpack ok\n")))
	for _, r := range results {
		if r.ok {
			body.Write(pktLine([]byte("ok " + r.ref + "\n")))
		} else {
			body.Write(pktLine([]byte("ng " + r.ref + " " + r.reason + "\n")))
		}
	}
	body.Write(pktFlush)
	if useSideband {
		return sidebandWrap(body.Bytes())
	}
	return body.Bytes()
}

type statusResult struct {
	ref    string
	ok     bool
	reason string
}

func sidebandWrap(body []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(body); i += 65515 {
		end := i + 65515
		if end > len(body) {
			end = len(body)
		}
		chunk := make([]byte, 0, 1+end-i)
		chunk = append(chunk, 1)
		chunk = append(chunk, body[i:end]...)
		out.Write(pktLine(chunk))
	}
	out.Write(pktFlush)
	return out.Bytes()
}

func handleUploadPackV2(repo *Repo, body []byte) []byte {
	lines, _ := pktLines(body)
	var cmd string
	var args []string
	inArgs := false
	for _, ln := range lines {
		if ln == "flush" {
			break
		}
		if ln == "delim" {
			inArgs = true
			continue
		}
		text := strings.TrimSpace(string(ln.([]byte)))
		if !inArgs && strings.HasPrefix(text, "command=") {
			cmd = text[len("command="):]
		} else if inArgs {
			args = append(args, text)
		}
	}
	if cmd == "ls-refs" {
		var out bytes.Buffer
		unborn := false
		peel := false
		symrefs := false
		var prefixes []string
		for _, a := range args {
			switch {
			case a == "unborn":
				unborn = true
			case a == "peel":
				peel = true
			case a == "symrefs":
				symrefs = true
			case strings.HasPrefix(a, "ref-prefix "):
				prefixes = append(prefixes, a[len("ref-prefix "):])
			}
		}
		headSHA := repo.Refs[repo.HeadRef]
		prefixOK := func(refname string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(refname, p) {
					return true
				}
			}
			return false
		}
		if headSHA != "" && prefixOK("HEAD") {
			line := headSHA + " HEAD"
			if symrefs {
				line += " symref-target:" + repo.HeadRef
			}
			out.Write(pktLine([]byte(line + "\n")))
		}
		refNames := make([]string, 0, len(repo.Refs))
		for r := range repo.Refs {
			refNames = append(refNames, r)
		}
		sort.Strings(refNames)
		for _, ref := range refNames {
			sha := repo.Refs[ref]
			if sha == "" || !prefixOK(ref) {
				continue
			}
			line := sha + " " + ref
			if peel && strings.HasPrefix(ref, "refs/tags/") {
				if obj, ok := getObject(repo, sha); ok && obj.Type == "tag" {
					if target, _ := parseTag(obj.Data); target != "" {
						line += " peeled:" + target
					}
				}
			}
			out.Write(pktLine([]byte(line + "\n")))
		}
		if unborn && headSHA == "" && (prefixOK("HEAD") || prefixOK(repo.HeadRef)) {
			out.Write(pktLine([]byte("unborn HEAD symref-target:" + repo.HeadRef + "\n")))
		}
		out.Write(pktFlush)
		return out.Bytes()
	}
	if cmd == "fetch" {
		var wants []string
		haves := map[string]bool{}
		var deepen *int
		oldShallow := map[string]bool{}
		deepenRelative := false
		done := false
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "want "):
				wants = append(wants, strings.Split(a[5:], " ")[0])
			case strings.HasPrefix(a, "have "):
				haves[strings.Split(a[5:], " ")[0]] = true
			case strings.HasPrefix(a, "deepen "):
				if v, err := strconv.Atoi(a[7:]); err == nil {
					deepen = &v
				} else {
					deepen = nil
				}
			case a == "deepen-relative":
				deepenRelative = true
			case a == "done":
				done = true
			case strings.HasPrefix(a, "shallow "):
				oldShallow[strings.Split(a[8:], " ")[0]] = true
			}
		}
		_, framed, boundaries, unshallowed := buildPackFiltered(repo, wants, haves, deepen, oldShallow, deepenRelative)
		var out bytes.Buffer
		if len(haves) > 0 && !done {
			wantCommits := map[string]bool{}
			for _, w := range wants {
				cur := w
				for i := 0; i < 8; i++ {
					obj, ok := getObject(repo, cur)
					if ok && obj.Type == "tag" {
						target, _ := parseTag(obj.Data)
						if target == "" {
							break
						}
						cur = target
					} else {
						break
					}
				}
				wantCommits[cur] = true
			}
			var common []string
			for _, h := range sortedKeys(haves) {
				if _, ok := getObject(repo, h); !ok {
					continue
				}
				for w := range wantCommits {
					if h == w || isAncestor(repo, h, w) {
						common = append(common, h)
						break
					}
				}
			}
			out.Write(pktLine([]byte("acknowledgments\n")))
			if len(common) > 0 {
				for _, h := range common {
					out.Write(pktLine([]byte("ACK " + h + "\n")))
				}
				out.Write(pktLine([]byte("ready\n")))
			} else {
				out.Write(pktLine([]byte("NAK\n")))
			}
			out.Write(pktDelim)
			if len(common) == 0 {
				return out.Bytes()
			}
		}
		if len(boundaries) > 0 || len(unshallowed) > 0 {
			out.Write(pktLine([]byte("shallow-info\n")))
			for _, w := range sortedKeys(boundaries) {
				out.Write(pktLine([]byte("shallow " + w + "\n")))
			}
			for _, w := range sortedKeys(unshallowed) {
				out.Write(pktLine([]byte("unshallow " + w + "\n")))
			}
			out.Write(pktDelim)
		}
		out.Write(pktLine([]byte("packfile\n")))
		out.Write(framed)
		out.Write(pktFlush)
		return out.Bytes()
	}
	return append(pktLine([]byte("version 2\n")), pktFlush...)
}
