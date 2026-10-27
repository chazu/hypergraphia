// Package simhash computes 64-bit SimHash fingerprints of short texts, so
// near-duplicate memories (paraphrases, small edits) can be found by Hamming
// distance.
package simhash

import (
	"hash/fnv"
	"math/bits"
	"strings"
)

// NearDistance is the Hamming distance at or below which two fingerprints are
// treated as near-duplicates.
const NearDistance = 3

// Of fingerprints a sequence of normalized words using word 3-shingles (or the
// words themselves when there are fewer than three).
func Of(words []string) uint64 {
	var shingles []string
	if len(words) < 3 {
		shingles = words
	} else {
		for i := 0; i+3 <= len(words); i++ {
			shingles = append(shingles, strings.Join(words[i:i+3], " "))
		}
	}
	if len(shingles) == 0 {
		return 0
	}
	var v [64]int
	for _, s := range shingles {
		h := fnv.New64a()
		h.Write([]byte(s))
		x := h.Sum64()
		for i := 0; i < 64; i++ {
			if x&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// Distance is the Hamming distance between two fingerprints.
func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }
