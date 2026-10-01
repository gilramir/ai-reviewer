package textdiff

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, c.want, marked(after, Changed(before, after)), "Changed(%q, %q)", c.before, c.after)
		})
	}
}

func TestChangedOnEmptyAfter(t *testing.T) {
	assert.Empty(t, Changed(words("everything went away"), nil))
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
		require.Len(t, changed, len(after), "round %d: one flag per word", round)

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
			require.True(t, found, "round %d: %q was left unmarked but is not in %v in order\nbefore %v\nafter  %v",
				round, word, before, before, after)
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
		require.True(t, c, "word %d was left unmarked in a document with nothing in common", i)
	}
}

// Two words changed at opposite ends of a long document are two edits, however
// much text sits between them. Bounding the search by that distance instead of
// by the edit count reported everything between the two as new, which on screen
// was most of a README highlighted because a word near the top had been
// rewritten an hour earlier.
func TestDistantEditsDoNotMarkWhatIsBetweenThem(t *testing.T) {
	before := make([]string, 1500)
	for i := range before {
		before[i] = "word" + strconv.Itoa(i)
	}
	after := append([]string(nil), before...)
	after[10] = "near-the-top"
	after[len(after)-10] = "near-the-bottom"

	changed := Changed(before, after)
	for i, c := range changed {
		want := i == 10 || i == len(after)-10
		require.Equal(t, want, c, "word %d (%q): marked", i, after[i])
	}
}
