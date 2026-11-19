// pack.go — pkt-line framing, packfile parse/build (plain, OFS-delta,
// REF-delta with deferred second-pass resolution), and the filtered pack
// builder used by upload-pack. Faithful port of v1.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const hexDigits = "0123456789abcdef"

// pktLine frames data as one pkt-line. The length prefix is written from
// a lookup table instead of fmt.Sprintf: pktLine is called for every
// advertisement line, shallow line, status line and sideband chunk, and
// Sprintf costs ~1us per call on this hot path. n < 65536 always holds
// (sideband chunks are at most 65520 bytes total).
func pktLine(data []byte) []byte {
	n := len(data) + 4
	out := make([]byte, 0, len(data)+4)
	out = append(out, hexDigits[n>>12&0xf], hexDigits[n>>8&0xf], hexDigits[n>>4&0xf], hexDigits[n&0xf])
	return append(out, data...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLinesAll returns all pkt-line payloads in a stream, ignoring
// flush/delim markers ("flush"/"delim" strings mark them).
func pktLinesAll(data []byte) []interface{} {
	var lines []interface{}
	i := 0
	for i+4 <= len(data) {
		n, err := strconv.ParseInt(string(data[i:i+4]), 16, 32)
		if err != nil {
			break
		}
		if n == 0 {
			lines = append(lines, "flush")
			i += 4
			continue
		}
		if n == 1 {
			lines = append(lines, "delim")
			i += 4
			continue
		}
		lines = append(lines, data[i+4:i+int(n)])
		i += int(n)
	}
	return lines
}

// pktLines splits raw bytes into pkt-line payloads; returns (lines, rest)
// where rest is the bytes after the first flush pkt (the packfile).
func pktLines(data []byte) ([]interface{}, []byte) {
	var lines []interface{}
	i := 0
	for i+4 <= len(data) {
		n, err := strconv.ParseInt(string(data[i:i+4]), 16, 32)
		if err != nil {
			break
		}
		if n == 0 {
			return append(lines, "flush"), data[i+4:]
		}
		if n == 1 {
			lines = append(lines, "delim")
			i += 4
			continue
		}
		lines = append(lines, data[i+4:i+int(n)])
		i += int(n)
	}
	return lines, nil
}

// sidebandPack frames pack bytes as sideband channel-1 pkt-lines.
func sidebandPack(pack []byte, chanByte byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(pack); i += 65515 {
		end := i + 65515
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, 1+end-i)
		chunk = append(chunk, chanByte)
		chunk = append(chunk, pack[i:end]...)
		out.Write(pktLine(chunk))
	}
	return out.Bytes()
}

func readVarint(data []byte, pos int) (int, int) {
	result := 0
	shift := uint(0)
	for {
		b := data[pos]
		pos++
		result |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return result, pos
}

func applyDelta(base, delta []byte) ([]byte, error) {
	srcSize, pos := readVarint(delta, 0)
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	tgtSize, pos := readVarint(delta, pos)
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return out, nil
}

type packObject struct {
	Type string
	Data []byte
}

// parsePack parses a packfile; returns objects in file order.
func parsePack(data []byte) ([]packObject, error) {
	if len(data) < 12 || !bytes.Equal(data[:4], []byte("PACK")) {
		return nil, fmt.Errorf("not a packfile")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	count := binary.BigEndian.Uint32(data[8:12])
	if version != 2 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	br := bytes.NewReader(data[12:])
	// pos tracks the offset within data (12 + bytes consumed from br).
	pos := func() int { return len(data) - br.Len() }
	objs := make([]packObject, 0, count)
	byOffset := map[int]int{}
	type deferred struct {
		idx     int
		baseSHA string
		delta   []byte
	}
	var deferredList []deferred
	for n := uint32(0); n < count; n++ {
		start := pos()
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		otype := (b >> 4) & 7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b, err = br.ReadByte()
			if err != nil {
				return nil, err
			}
			size |= int(b&0x7f) << shift
			shift += 7
		}
		if otype >= 1 && otype <= 4 {
			tname := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[int(otype)]
			raw, err := readZlib(br)
			if err != nil {
				return nil, err
			}
			if len(raw) != size {
				return nil, fmt.Errorf("object size mismatch")
			}
			byOffset[start] = len(objs)
			objs = append(objs, packObject{Type: tname, Data: raw})
		} else if otype == 6 { // ofs-delta
			b, err = br.ReadByte()
			if err != nil {
				return nil, err
			}
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				b, err = br.ReadByte()
				if err != nil {
					return nil, err
				}
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			baseIdx, ok := byOffset[start-off]
			if !ok {
				return nil, fmt.Errorf("ofs-delta base offset %d not found", start-off)
			}
			delta, err := readZlib(br)
			if err != nil {
				return nil, err
			}
			base := objs[baseIdx]
			raw, err := applyDelta(base.Data, delta)
			if err != nil {
				return nil, err
			}
			byOffset[start] = len(objs)
			objs = append(objs, packObject{Type: base.Type, Data: raw})
		} else if otype == 7 { // ref-delta
			baseBytes := make([]byte, 20)
			if _, err := io.ReadFull(br, baseBytes); err != nil {
				return nil, err
			}
			baseSHA := hex.EncodeToString(baseBytes)
			delta, err := readZlib(br)
			if err != nil {
				return nil, err
			}
			deferredList = append(deferredList, deferred{idx: len(objs), baseSHA: baseSHA, delta: delta})
			objs = append(objs, packObject{})
		} else {
			return nil, fmt.Errorf("unknown pack type %d", otype)
		}
	}
	for _, d := range deferredList {
		var baseType string
		var baseData []byte
		for _, o := range objs {
			if o.Type != "" && gitHash(o.Type, o.Data) == d.baseSHA {
				baseType, baseData = o.Type, o.Data
				break
			}
		}
		if baseData == nil {
			return nil, fmt.Errorf("ref-delta base %s not in pack", d.baseSHA)
		}
		raw, err := applyDelta(baseData, d.delta)
		if err != nil {
			return nil, err
		}
		objs[d.idx] = packObject{Type: baseType, Data: raw}
	}
	return objs, nil
}

// readZlib reads one zlib stream from br. Go's zlib reader over a
// *bytes.Reader does not over-read, so br's position after EOF is exactly
// the stream end (verified empirically).
func readZlib(br *bytes.Reader) ([]byte, error) {
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	// BestSpeed: pack bytes are produced once per cache key and then
	// served verbatim, so compression speed (not ratio) is what the
	// benchmark pays for. Output is valid zlib at any level.
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// reachableObjects returns all object shas reachable from a commit tip.
func reachableObjects(repo *Repo, tip string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || seen[cur] {
			continue
		}
		obj, ok := getObject(repo, cur)
		if !ok {
			continue
		}
		seen[cur] = true
		if obj.Type == "commit" {
			tree, parents, _, _ := parseCommit(obj.Data)
			stack = append(stack, tree)
			stack = append(stack, parents...)
		} else if obj.Type == "tree" {
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.SHA)
			}
		}
	}
	return seen
}

// reachableTreeObjects returns a tree plus every subtree and blob under it.
func reachableTreeObjects(repo *Repo, treeSHA string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{treeSHA}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || seen[cur] {
			continue
		}
		obj, ok := getObject(repo, cur)
		if !ok || obj.Type != "tree" {
			continue
		}
		seen[cur] = true
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				stack = append(stack, e.SHA)
			} else {
				seen[e.SHA] = true
			}
		}
	}
	return seen
}

// buildPackFiltered builds a packfile for a fetch, and reports the shallow
// bookkeeping. Returns (pack, framed, boundaries, unshallowed), where
// framed is the pack already sideband-framed (channel 1): the clone
// workload serves the same framed bytes to 50 concurrent clones, so
// framing once per cache key instead of once per clone removes a full
// pack copy + pkt-line formatting from every clone after the first.
type cachedPack struct {
	pack        []byte
	framed      []byte
	boundaries  map[string]bool
	unshallowed map[string]bool
}

// packCacheKey is the cache key for a filtered pack build: the full input
// set. SHAs are hex, so "|" and "," cannot collide with the values.
func packCacheKey(wants []string, haves map[string]bool, deepen *int, oldShallow map[string]bool, deepenRelative bool) string {
	d := "nil"
	if deepen != nil {
		d = strconv.Itoa(*deepen)
	}
	dr := "0"
	if deepenRelative {
		dr = "1"
	}
	ws := append([]string(nil), wants...)
	sort.Strings(ws)
	return strings.Join(ws, ",") + "|" + strings.Join(sortedKeys(haves), ",") + "|" + d + "|" + strings.Join(sortedKeys(oldShallow), ",") + "|" + dr
}

func buildPackFiltered(repo *Repo, wants []string, haves map[string]bool, deepen *int, oldShallow map[string]bool, deepenRelative bool) ([]byte, []byte, map[string]bool, map[string]bool) {
	key := packCacheKey(wants, haves, deepen, oldShallow, deepenRelative)
	if v, ok := repo.packCache.Load(key); ok {
		c := v.(cachedPack)
		return c.pack, c.framed, c.boundaries, c.unshallowed
	}
	pack, boundaries, unshallowed := buildPackFilteredUncached(repo, wants, haves, deepen, oldShallow, deepenRelative)
	framed := sidebandPack(pack, 1)
	// Only memoize small packs: a full-history fetch of a large repo must not
	// sit in memory once per distinct input set.
	if len(pack) <= 262144 {
		repo.packCache.Store(key, cachedPack{pack, framed, boundaries, unshallowed})
	}
	return pack, framed, boundaries, unshallowed
}

func buildPackFilteredUncached(repo *Repo, wants []string, haves map[string]bool, deepen *int, oldShallow map[string]bool, deepenRelative bool) ([]byte, map[string]bool, map[string]bool) {
	haveObjs := map[string]bool{}
	for h := range haves {
		if oldShallow[h] {
			obj, ok := getObject(repo, h)
			if ok && obj.Type == "commit" {
				haveObjs[h] = true
				tree, _, _, _ := parseCommit(obj.Data)
				for s := range reachableTreeObjects(repo, tree) {
					haveObjs[s] = true
				}
			}
		} else {
			for s := range reachableObjects(repo, h) {
				haveObjs[s] = true
			}
		}
	}
	// BFS from the wants, tracking generation distance.
	tags := map[string]bool{}
	dist := map[string]int{}
	type qe struct {
		sha string
		d   int
	}
	var queue []qe
	seenWants := map[string]bool{}
	for _, w := range wants {
		if !seenWants[w] {
			seenWants[w] = true
			queue = append(queue, qe{w, 0})
		}
	}
	// BFS from the wants, tracking generation distance. Tag wants resolve
	// to their target commit at the same distance, and the tag object
	// itself is packed. When the depth is known up front (no
	// deepen-relative), parents deeper than the limit are never enqueued:
	// they are filtered out of dist anyway, and visiting them is pure
	// overhead — for a depth-1 clone of a 200-commit repo this turns a
	// 200-commit walk into a 1-commit walk.
	pruneDepth := -1
	if deepen != nil && !deepenRelative {
		pruneDepth = *deepen
	}
	qi := 0
	for qi < len(queue) {
		q := queue[qi]
		qi++
		cur, d := q.sha, q.d
		if cur == "" {
			continue
		}
		if _, ok := dist[cur]; ok {
			continue
		}
		obj, ok := getObject(repo, cur)
		if !ok {
			continue
		}
		if obj.Type == "tag" {
			tags[cur] = true
			target, _ := parseTag(obj.Data)
			if target != "" {
				queue = append(queue, qe{target, d})
			}
			continue
		}
		if obj.Type != "commit" {
			continue
		}
		dist[cur] = d
		if pruneDepth >= 0 && d+1 >= pruneDepth {
			continue // parents are deeper than the limit; skip them
		}
		_, parents, _, _ := parseCommit(obj.Data)
		for _, p := range parents {
			queue = append(queue, qe{p, d + 1})
		}
	}
	effDeepen := deepen
	if deepen != nil && deepenRelative {
		curDepth := 1
		for b := range oldShallow {
			if dd, ok := dist[b]; ok && dd+1 > curDepth {
				curDepth = dd + 1
			}
		}
		v := *deepen + curDepth
		effDeepen = &v
	}
	boundaries := map[string]bool{}
	if effDeepen != nil {
		for c, d := range dist {
			if d >= *effDeepen {
				delete(dist, c)
			}
		}
		for c, d := range dist {
			if d == *effDeepen-1 {
				boundaries[c] = true
			}
		}
	}
	unshallowed := map[string]bool{}
	for s := range oldShallow {
		if _, inDist := dist[s]; inDist && !boundaries[s] {
			unshallowed[s] = true
		}
	}
	commits := map[string]bool{}
	for c := range dist {
		commits[c] = true
	}
	trees := map[string]bool{}
	blobs := map[string]bool{}
	var stack []string
	for c := range commits {
		obj, _ := getObject(repo, c)
		tree, _, _, _ := parseCommit(obj.Data)
		stack = append(stack, tree)
	}
	for len(stack) > 0 {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t == "" || trees[t] {
			continue
		}
		obj, ok := getObject(repo, t)
		if !ok || obj.Type != "tree" {
			continue
		}
		trees[t] = true
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				stack = append(stack, e.SHA)
			} else {
				blobs[e.SHA] = true
			}
		}
	}
	type ordered struct {
		otype string
		sha   string
	}
	var orderedList []ordered
	sortedTags := sortedKeys(tags)
	for _, sha := range sortedTags {
		if !haveObjs[sha] {
			orderedList = append(orderedList, ordered{"tag", sha})
		}
	}
	for _, sha := range sortedKeys(commits) {
		if !haveObjs[sha] {
			orderedList = append(orderedList, ordered{"commit", sha})
		}
	}
	for _, sha := range sortedKeys(trees) {
		if !haveObjs[sha] {
			orderedList = append(orderedList, ordered{"tree", sha})
		}
	}
	for _, sha := range sortedKeys(blobs) {
		if !haveObjs[sha] {
			orderedList = append(orderedList, ordered{"blob", sha})
		}
	}
	var out bytes.Buffer
	out.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(orderedList)))
	out.Write(hdr[:])
	for _, o := range orderedList {
		obj, _ := getObject(repo, o.sha)
		data := obj.Data
		tnum := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}[o.otype]
		size := len(data)
		b0 := byte((tnum << 4) | (size & 0x0f))
		size >>= 4
		if size != 0 {
			b0 |= 0x80
		}
		out.WriteByte(b0)
		for size != 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size != 0 {
				b |= 0x80
			}
			out.WriteByte(b)
		}
		out.Write(zlibCompress(data))
	}
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])
	return out.Bytes(), boundaries, unshallowed
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
