package index

import (
	"fmt"
	"iter"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// realWords lead the vocabulary for generated documents: function words first, then topic words.
var realWords = strings.Fields(`the a of and to in is for on with that as by be this are from at or an it
not we our can which if when will use should all one has more must but have each its new any also than
service deploy cluster node pod container image registry build pipeline release rollback config secret
token auth login session cache queue worker job schedule cron retry timeout backoff metric alert log
trace span dashboard incident outage postmortem runbook oncall escalation pager database replica primary
backup restore migration schema index query table column row transaction lock deadlock latency throughput
network dns proxy gateway ingress egress firewall vpn certificate tls rotation key vault storage bucket
volume disk memory cpu limit request quota budget cost region zone failover capacity scaling autoscaler
fleet doctrine logistics mining market industry corporation alliance wormhole structure citadel jump
bridge moon refinery contract insurance skill training pilot ship hull module fitting ammunition drone
api endpoint client server version deprecated feature flag experiment rollout canary blue green review
approve merge branch commit tag changelog documentation decision reason tradeoff option alternative risk`)

// corpusVocabulary returns realWords followed by n made-up words. Picked with a Zipf distribution, a few
// words are very common and most are rare, like in real text: a topic word lands in a small share of
// documents, not in all of them.
func corpusVocabulary(n int) []string {
	r := rand.New(rand.NewPCG(7, 7))
	syllables := strings.Fields("ka ri to ne sa mu lo pe vi da ge zu ho ba ti ra ko me shi na fu ya")
	seen := make(map[string]bool, n)
	words := slices.Clone(realWords)
	for _, w := range words {
		seen[w] = true
	}
	for len(words) < len(realWords)+n {
		var b strings.Builder
		for range 2 + r.IntN(3) {
			b.WriteString(syllables[r.IntN(len(syllables))])
		}
		if w := b.String(); !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	return words
}

func generateDoc(r *rand.Rand, zipf *rand.Zipf, words []string, i int) (string, string) {
	word := func() string { return words[zipf.Uint64()] }
	sentence := func(n int) string {
		ws := make([]string, n)
		for j := range ws {
			ws[j] = word()
		}
		s := strings.Join(ws, " ")
		return strings.ToUpper(s[:1]) + s[1:] + "."
	}
	path := fmt.Sprintf("area-%02d/topic-%03d/doc-%05d.md", i%40, (i/40)%200, i)
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %s %s %s\ndescription: %s\ntags: [%s, %s]\n", word(), word(), word(), sentence(12), word(), word())
	if r.IntN(10) == 0 {
		b.WriteString("status: deprecated\n")
	}
	b.WriteString("---\n\n")
	for range 3 + r.IntN(4) { // about 5 KB per document
		fmt.Fprintf(&b, "## %s %s\n\n", word(), word())
		for range 2 + r.IntN(3) {
			b.WriteString(sentence(25 + r.IntN(30)))
			b.WriteString(" ")
			b.WriteString(sentence(20 + r.IntN(20)))
			b.WriteString("\n\n")
		}
		if r.IntN(3) == 0 {
			fmt.Fprintf(&b, "See [related](../../area-%02d/topic-%03d/doc-%05d.md).\n\n", r.IntN(40), r.IntN(200), r.IntN(i+1))
		}
	}
	return path, b.String()
}

func generatedCorpus(seed uint64, n int) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		r := rand.New(rand.NewPCG(seed, 1))
		words := corpusVocabulary(20_000)
		zipf := rand.NewZipf(r, 1.05, 2, uint64(len(words)-1))
		for i := range n {
			p, content := generateDoc(r, zipf, words, i)
			if !yield(Change{Path: p, Version: version(content), Content: []byte(content)}, nil) {
				return
			}
		}
	}
}

// TestMeasure records build and search performance on generated vaults. It takes minutes, so it only
// runs with SPEKI_MEASURE=1:
//
//	SPEKI_MEASURE=1 go test -tags sqlite_fts5 -run TestMeasure -v ./index/
func TestMeasure(t *testing.T) {
	if os.Getenv("SPEKI_MEASURE") == "" {
		t.Skip("set SPEKI_MEASURE=1 to measure")
	}
	t.Logf("GOMAXPROCS=%d", runtime.GOMAXPROCS(0))

	build := func(seed uint64, n int) *Index {
		x, err := Open(t.Context(), t.TempDir(), discard())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(x.Close)
		start := time.Now()
		err = x.Build(t.Context(), "c1", generatedCorpus(seed, n))
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		x.background.Wait()
		size := fileSize(x)
		t.Logf("build %d documents: %v (%.0f docs/s), index %.0f MB", n, took.Round(time.Millisecond),
			float64(n)/took.Seconds(), float64(size)/1e6)
		return x
	}

	resetPeakRSS()
	big := build(1, 50_000)
	t.Logf("peak RSS while building 50,000 documents: %s", memory("VmHWM"))

	start := time.Now()
	err := big.Apply(t.Context(), "c2", []Change{{Path: "area-00/topic-000/doc-00000.md", Version: "x", Content: []byte("# Changed\n\nnew text\n")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("apply one change to 50,000 documents: %v", time.Since(start).Round(time.Microsecond))

	var vaults []Source
	for i := range 10 {
		vaults = append(vaults, Source{Name: fmt.Sprint("v", i), Index: build(uint64(i+10), 10_000)})
	}

	queries := []string{"deploy", "rollback pipeline", "kubernets", "certificate rotation", "fleet doctrine",
		"replica failover", `"backup restore"`, "dashb", "outage -incident", "the", "service config secret"}
	measure := func(name string, sources []Source) {
		var times []time.Duration
		for range 20 {
			for _, q := range queries {
				start := time.Now()
				_, err := Search(t.Context(), sources, q, Options{})
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				times = append(times, time.Since(start))
			}
		}
		slices.Sort(times)
		t.Logf("search %s: p50 %v, p95 %v, max %v", name,
			times[len(times)/2].Round(time.Microsecond),
			times[len(times)*95/100].Round(time.Microsecond),
			times[len(times)-1].Round(time.Microsecond))
	}
	measure("1 vault of 50,000", []Source{{Name: "big", Index: big}})
	measure("1 vault of 10,000", vaults[:1])
	measure("10 vaults of 10,000", vaults)
	runtime.GC()
	t.Logf("RSS serving 1 vault of 50,000 and 10 of 10,000, after searching: %s", memory("VmRSS"))
}

// memory returns a line of /proc/self/status, such as VmRSS, or "unknown" where there's none.
func memory(field string) string {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// resetPeakRSS starts VmHWM over from the current RSS, where Linux allows it.
func resetPeakRSS() { _ = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) }

func fileSize(x *Index) int64 {
	g, err := x.acquire()
	if err != nil {
		return 0
	}
	defer g.release()
	fi, err := os.Stat(generationPath(g.dir, g.n))
	if err != nil {
		return 0
	}
	return fi.Size()
}
