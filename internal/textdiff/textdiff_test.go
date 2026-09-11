package textdiff

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// marked renders a result the way a reader would see it, so a failing case
// reads as words rather than as booleans.
func marked(after []string, changed []bool) string {
	var b strings.Builder
	for i, word := range after {
		if changed[i] {
			b.WriteString("[" + word + "]")
		} else {
			b.WriteString(word)
		}
	}
	return b.String()
}

func words(s string) []string { return strings.Split(s, " ") }

func TestChanged(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
		want   string
	}{
		{
			name:   "nothing changed",
			before: "the cat sat on the mat",
			after:  "the cat sat on the mat",
			want:   "thecatsatonthemat",
		},
		{
			name:   "one word replaced",
			before: "the cat sat on the mat",
			after:  "the dog sat on the mat",
			want:   "the[dog]satonthemat",
		},
		{
			name:   "a word inserted",
			before: "the cat sat on the mat",
			after:  "the cat sat quietly on the mat",
			want:   "thecatsat[quietly]onthemat",
		},
		{
			name:   "a word removed leaves nothing marked",
			before: "the cat sat on the mat",
			after:  "the cat on the mat",
			want:   "thecatonthemat",
		},
		{
			name:   "a whole new sentence",
			before: "the cat sat on the mat",
			after:  "the cat sat on the mat and then it slept",
			want:   "thecatsatonthemat[and][then][it][slept]",
		},
		{
			name:   "an empty start marks everything",
			before: "",
			after:  "a whole new paragraph",
			want:   "[a][whole][new][paragraph]",
		},
		{
			name:   "a repeated word is not confused with its twin",
			before: "one two one three",
			after:  "one two one four three",
			want:   "onetwoone[four]three",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var before []string
			if c.before != "" {
				before = words(c.before)
			}
			after := words(c.after)
			got := marked(after, Changed(before, after))
			if got != c.want {
				t.Errorf("Changed(%q, %q)\n got %s\nwant %s", c.before, c.after, got, c.want)
			}
		})
	}
}

func TestChangedOnEmptyAfter(t *testing.T) {
	if got := Changed(words("everything went away"), nil); len(got) != 0 {
		t.Errorf("an empty result was expected, got %v", got)
	}
}

// The one invariant that matters: whatever is left unmarked must really be
// words the reader had before, in the order they had them. A marking that
// violated it would be pointing at the wrong sentence.
func TestUnmarkedWordsAreASubsequenceOfTheOldOnes(t *testing.T) {
	random := rand.New(rand.NewSource(1))

	for round := 0; round < 400; round++ {
		before := randomWords(random, random.Intn(40))
		after := mutate(random, before)

		changed := Changed(before, after)
		if len(changed) != len(after) {
			t.Fatalf("round %d: got %d flags for %d words", round, len(changed), len(after))
		}

		at := 0
		for i, word := range after {
			if changed[i] {
				continue
			}
			found := false
			for ; at < len(before); at++ {
				if before[at] == word {
					at++
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("round %d: %q was left unmarked but is not in %v in order\nbefore %v\nafter  %v",
					round, word, before, before, after)
			}
		}
	}
}

func randomWords(random *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(random.Intn(6))
	}
	return out
}

// mutate makes the kind of edit a turn makes: a few words replaced, dropped or
// added, somewhere in the middle of what was there.
func mutate(random *rand.Rand, in []string) []string {
	out := append([]string(nil), in...)
	for edits := random.Intn(5); edits > 0; edits-- {
		switch random.Intn(3) {
		case 0: // insert
			at := random.Intn(len(out) + 1)
			out = append(out[:at], append([]string{strconv.Itoa(random.Intn(6))}, out[at:]...)...)
		case 1: // delete
			if len(out) == 0 {
				continue
			}
			at := random.Intn(len(out))
			out = append(out[:at], out[at+1:]...)
		case 2: // replace
			if len(out) == 0 {
				continue
			}
			out[random.Intn(len(out))] = strconv.Itoa(random.Intn(6))
		}
	}
	return out
}

// A document rewritten from end to end is past the search's limit, and has to
// come back as wholly new rather than as nothing at all.
func TestAWholesaleRewriteIsAllNew(t *testing.T) {
	before := make([]string, 2000)
	after := make([]string, 2000)
	for i := range before {
		before[i] = "old" + strconv.Itoa(i)
		after[i] = "new" + strconv.Itoa(i)
	}

	changed := Changed(before, after)
	for i, c := range changed {
		if !c {
			t.Fatalf("word %d was left unmarked in a document with nothing in common", i)
		}
	}
}
