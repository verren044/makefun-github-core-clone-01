// store.go — forge state: the Store, Repo, Issue, Pull types and the seeded
// scenario users. Faithful port of v1 main.py (Python stdlib) to Go stdlib.
// All state is guarded by one RWMutex held by the dispatcher: read-only
// routes take RLock (clone/read stay concurrent), mutating routes take Lock.
package main

import (
	"os"
	"strings"
	"sync"
)

const (
	// Default trial clock (competition.yaml entries_close). Overridden by
	// MAKEFUN_TRIAL_NOW. Never a wall clock: every timestamp the forge
	// emits is this string (or a fixed commit timestamp), so two runs are
	// byte-identical.
	defaultTrialNow = "2026-11-14T06:30:00Z"
	zeroSHA         = "0000000000000000000000000000000000000000"
	version         = "1.0.0-forge"
)

var trialNow = func() string {
	if v := os.Getenv("MAKEFUN_TRIAL_NOW"); v != "" {
		return v
	}
	return defaultTrialNow
}()

func nowISO() string { return trialNow }

// ------------------------------------------------------------------ RNG
// The kit's seeded RNG (wire.ts SCENARIO_SEED = 0x6a09e667f3bcc909):
// fnv1a64, splitmix-style mix64, xorshift64* — ported exactly.

const u64mask = uint64(0xFFFFFFFFFFFFFFFF)

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, ch := range text {
		h ^= uint64(ch)
		h = (h * 0x100000001b3) & u64mask
	}
	return h
}

func mix64(v uint64) uint64 {
	z := (v + 0x9e3779b97f4a7c15) & u64mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64mask
	return (z ^ (z >> 31)) & u64mask
}

type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	r := &rng{state: seed & u64mask}
	if r.state == 0 {
		r.state = 0x6a09e667f3bcc909
	}
	return r
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64mask
	x = (x ^ (x << 25)) & u64mask
	x = (x ^ (x >> 27)) & u64mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64mask
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *rng) nextInt(n int) int {
	if n == 1 {
		return 0
	}
	// rejection sampling against 2^32 (Python's arbitrary precision;
	// limit can be 2^32 itself, so compare in uint64).
	limit := (uint64(1) << 32) - ((uint64(1) << 32) % uint64(n))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(n))
		}
	}
}

func (r *rng) pick(items []string) string { return items[r.nextInt(len(items))] }

// ---------------------------------------------------------- scenario users

var firstNames = []string{"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo",
	"iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel",
	"quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane"}
var lastNames = []string{"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia",
	"holm", "iversen", "jansen", "keller", "lind", "moreau", "novak",
	"olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich",
	"vega", "weber", "xu", "yilmaz", "zimmer"}

type scenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func cleanLogin(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func scenarioUsers() []scenarioUser {
	r := newRng(mix64(0x6a09e667f3bcc909 ^ fnv1a64("users")))
	used := map[string]bool{}
	users := make([]scenarioUser, 0, 12)
	for i := 0; i < 12; i++ {
		first, last := r.pick(firstNames), r.pick(lastNames)
		login := cleanLogin(first + last)
		suffix := 2
		for used[login] {
			login = first + last + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{
			ID:    1000 + i,
			Login: login,
			Name:  first + " " + last,
			Email: login + "@example.invalid",
		})
	}
	return users
}

var users = scenarioUsers()

// VIEWER is users[0] (adaiversen, id 1000); STRANGER is users[2] (olgabauer).
var viewer = func() scenarioUser {
	if len(users) > 0 {
		return users[0]
	}
	return scenarioUser{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}
}()

var stranger = func() scenarioUser {
	if len(users) > 2 {
		return users[2]
	}
	return scenarioUser{ID: 1002, Login: "olgabauer", Name: "olga bauer", Email: "olgabauer@example.invalid"}
}()

var viewerLogin = viewer.Login

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

// ------------------------------------------------------------------- store

type Store struct {
	mu           sync.RWMutex
	repos        map[[2]string]*Repo
	repoOrder    [][2]string
	nextRepoDBID int
	nextGID      int
	nextSeq      int
}

func newStore() *Store {
	return &Store{
		repos:        map[[2]string]*Repo{},
		nextRepoDBID: 1,
		nextGID:      1,
		nextSeq:      1,
	}
}

// seq returns a monotonic creation sequence number (ordering).
func (s *Store) seq() int {
	v := s.nextSeq
	s.nextSeq++
	return v
}

// gid returns an opaque GraphQL id ("I12", "P3", ...).
func (s *Store) gid(kind string) string {
	n := s.nextGID
	s.nextGID++
	return kind + itoa(n)
}

func pad6(n int) string {
	s := itoa(n)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

var store = newStore()

type Repo struct {
	Name         string
	Owner        string
	Description  string
	Private      bool
	DBID         int
	DefaultBranch string
	CreatedSeq   int
	// git state
	Objects map[string]gitObject // sha -> object
	Refs    map[string]string    // "refs/heads/<branch>" -> sha ("" = unborn)
	HeadRef string
	// lastUpload remembers the v0 negotiation (wants, deepen, old shallow)
	// so the follow-up "done" request can build the pack.
	LastUpload *lastUploadState
	// packCache memoizes buildPackFiltered results. A pack is a pure function
	// of (wants, haves, deepen, oldShallow, deepenRelative) over immutable,
	// content-addressed objects, so a cached pack never goes stale: the same
	// inputs always build the same bytes. This is what makes 50 concurrent
	// clones of one tip cheap — the depth-1 pack is built once, not 50 times.
	// sync.Map because v2 upload-pack runs under RLock (many concurrent
	// readers); the cached pack bytes and maps are never mutated by callers.
	packCache sync.Map // string -> cachedPack
	// forge state
	Labels     map[string]label // name -> label
	LabelOrder []string         // creation order (v1: dict insertion order)
	Issues     map[int]*Issue
	IssueOrder []int
	NextIssue  int
	Pulls      map[int]*Pull
	PullOrder  []int
	NextPull   int
}

type label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// lastUploadState is the remembered v0 upload-pack negotiation.
type lastUploadState struct {
	Wants      []string
	Deepen     *int
	OldShallow map[string]bool
}

func newRepo(name, owner, description string, private bool, dbid int) *Repo {
	return &Repo{
		Name:          name,
		Owner:         owner,
		Description:   description,
		Private:       private,
		DBID:          dbid,
		DefaultBranch: "main",
		CreatedSeq:    store.seq(),
		Objects:       map[string]gitObject{},
		Refs:          map[string]string{},
		HeadRef:       "refs/heads/main",
		Labels:        map[string]label{},
		Issues:        map[int]*Issue{},
		NextIssue:     1,
		Pulls:         map[int]*Pull{},
		NextPull:      1,
	}
}

func (r *Repo) fullName() string { return r.Owner + "/" + r.Name }
func (r *Repo) gid() string      { return "R" + itoa(r.DBID) }
func (r *Repo) branchTip(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

// branches returns sorted "refs/heads/<branch>" names.
func (r *Repo) branches() []string {
	out := []string{}
	for b := range r.Refs {
		if strings.HasPrefix(b, "refs/heads/") {
			out = append(out, b)
		}
	}
	sortStrings(out)
	return out
}

type Issue struct {
	Repo        *Repo
	Number      int
	Title       string
	Body        string
	State       string // "open" | "closed"
	Labels      []string
	Comments    []issueComment
	NextComment int
	CreatedSeq  int
	GID         string
	CreatedAt   string
	UpdatedAt   string
	// cachedRest is the serialized REST representation, invalidated on any
	// mutation of the issue. The read workload GETs one issue 2,000 times,
	// so this cache turns the hot path into a map lookup + write.
	cachedRest []byte
}

type issueComment struct {
	ID   int
	Body string
}

func newIssue(repo *Repo, number int, title, body string, labels []string) *Issue {
	if body == "" {
		// keep explicit empty vs nil distinction out; v1: body or ""
	}
	if labels == nil {
		labels = []string{}
	}
	return &Issue{
		Repo:        repo,
		Number:      number,
		Title:       title,
		Body:        body,
		State:       "open",
		Labels:      append([]string{}, labels...),
		Comments:    []issueComment{},
		NextComment: 1,
		CreatedSeq:  store.seq(),
		GID:         store.gid("I"),
		CreatedAt:   nowISO(),
		UpdatedAt:   nowISO(),
	}
}

func (is *Issue) invalidate() { is.cachedRest = nil }

type Pull struct {
	Repo           *Repo
	Number         int
	Title          string
	Body           string
	Head           string
	Base           string
	HeadSHA        string
	BaseSHA        string
	State          string // "open" | "closed"
	Merged         bool
	MergeCommitSHA string
	MergedAt       string
	CreatedSeq     int
	GID            string
	CreatedAt      string
	UpdatedAt      string
}

func newPull(repo *Repo, number int, title, body, head, base, headSHA, baseSHA string) *Pull {
	return &Pull{
		Repo:       repo,
		Number:     number,
		Title:      title,
		Body:       body,
		Head:       head,
		Base:       base,
		HeadSHA:    headSHA,
		BaseSHA:    baseSHA,
		State:      "open",
		CreatedSeq: store.seq(),
		GID:        store.gid("P"),
		CreatedAt:  nowISO(),
		UpdatedAt:  nowISO(),
	}
}
