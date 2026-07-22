package util

import (
	"math"
	"regexp"
	"sort"
)

// Fingerprint prompt and sampling bounds used by model fingerprint detection.
const (
	FingerprintPromptVersion   = "v1"
	FingerprintPrompt          = "请从1到355之间随机选择一个数字，只输出这个数字，不要有任何其他内容。"
	FingerprintRange           = 355
	FingerprintMinValidSamples = 40
	FingerprintDefaultIters    = 100
	FingerprintDefaultConc     = 5
	FingerprintMaxIters        = 500
	FingerprintMinIters        = 50
	FingerprintMaxConc         = 20
	FingerprintMinConc         = 1
)

var fingerprintDigitRe = regexp.MustCompile(`\d+`)

// ParseFingerprintNumber extracts the first integer in [1, FingerprintRange] from text.
func ParseFingerprintNumber(text string) (int, bool) {
	m := fingerprintDigitRe.FindString(text)
	if m == "" {
		return 0, false
	}
	n := 0
	for _, ch := range m {
		n = n*10 + int(ch-'0')
		if n > FingerprintRange {
			return 0, false
		}
	}
	if n < 1 || n > FingerprintRange {
		return 0, false
	}
	return n, true
}

// FingerprintDistribution returns a length-FingerprintRange probability mass over 1..355.
func FingerprintDistribution(numbers []int) []float64 {
	counts := make([]int, FingerprintRange)
	total := 0
	for _, n := range numbers {
		if n >= 1 && n <= FingerprintRange {
			counts[n-1]++
			total++
		}
	}
	out := make([]float64, FingerprintRange)
	if total == 0 {
		return out
	}
	for i, c := range counts {
		out[i] = float64(c) / float64(total)
	}
	return out
}

// FingerprintSampleStats summarizes a set of fingerprint sample numbers.
type FingerprintSampleStats struct {
	Mean      float64 `json:"mean"`
	Median    float64 `json:"median"`
	StdDev    float64 `json:"std_dev"`
	Min       int     `json:"min"`
	Max       int     `json:"max"`
	Unique    int     `json:"unique"`
	Mode      int     `json:"mode"`
	ModeCount int     `json:"mode_count"`
}

// CalculateFingerprintStats computes mean/median/std/mode and range stats for numbers.
func CalculateFingerprintStats(numbers []int) FingerprintSampleStats {
	if len(numbers) == 0 {
		return FingerprintSampleStats{}
	}
	sorted := append([]int(nil), numbers...)
	sort.Ints(sorted)
	sum := 0.0
	freq := map[int]int{}
	for _, n := range numbers {
		sum += float64(n)
		freq[n]++
	}
	mean := sum / float64(len(numbers))
	var variance float64
	for _, n := range numbers {
		d := float64(n) - mean
		variance += d * d
	}
	variance /= float64(len(numbers))
	mode, modeCount := sorted[0], 0
	for k, v := range freq {
		if v > modeCount || (v == modeCount && k < mode) {
			mode, modeCount = k, v
		}
	}
	return FingerprintSampleStats{
		Mean:      mean,
		Median:    float64(sorted[len(sorted)/2]),
		StdDev:    math.Sqrt(variance),
		Min:       sorted[0],
		Max:       sorted[len(sorted)-1],
		Unique:    len(freq),
		Mode:      mode,
		ModeCount: modeCount,
	}
}

// FingerprintSimilarity holds distribution similarity and mode diagnostics.
// OverallScore is a relative distribution ranking signal, not a calibrated
// probability or authenticity verdict. ModeScore is diagnostic only because a
// single most-frequent bucket is too unstable to carry scoring weight.
type FingerprintSimilarity struct {
	CosineSimilarity float64 `json:"cosine_similarity"`
	JSDivergence     float64 `json:"js_divergence"`
	ModeScore        float64 `json:"mode_score"`
	OverallScore     float64 `json:"overall_score"`
}

// CalculateFingerprintSimilarity compares two distributions and their sample stats.
func CalculateFingerprintSimilarity(dist1, dist2 []float64, stats1, stats2 FingerprintSampleStats) FingerprintSimilarity {
	n := FingerprintRange
	if len(dist1) < n {
		n = len(dist1)
	}
	if len(dist2) < n {
		n = len(dist2)
	}
	var dot, norm1, norm2 float64
	const eps = 1e-10
	var js float64
	for i := 0; i < n; i++ {
		p, q := dist1[i], dist2[i]
		dot += p * q
		norm1 += p * p
		norm2 += q * q
		pp, qq := p+eps, q+eps
		m := (pp + qq) / 2
		js += 0.5 * (pp*math.Log(pp/m) + qq*math.Log(qq/m))
	}
	cosine := 0.0
	if norm1 > 0 && norm2 > 0 {
		cosine = dot / (math.Sqrt(norm1) * math.Sqrt(norm2))
	}
	distribScore := FingerprintDistributionScore(cosine, js)
	modeScore := 0.0
	if stats1.Mode == stats2.Mode {
		modeScore = 1.0
	} else {
		diff := math.Abs(float64(stats1.Mode - stats2.Mode))
		modeScore = math.Max(0, 1-diff/50)
	}
	return FingerprintSimilarity{
		CosineSimilarity: cosine,
		JSDivergence:     js,
		ModeScore:        modeScore,
		OverallScore:     distribScore,
	}
}

// FingerprintDistributionScore combines whole-distribution similarity metrics.
func FingerprintDistributionScore(cosineSimilarity, jsDivergence float64) float64 {
	return cosineSimilarity * math.Exp(-jsDivergence)
}

// ClampFingerprintParams normalizes user input; non-empty err string means caller should 400.
func ClampFingerprintParams(iterations, concurrency int) (int, int, string) {
	if iterations == 0 {
		iterations = FingerprintDefaultIters
	}
	if concurrency == 0 {
		concurrency = FingerprintDefaultConc
	}
	if iterations < FingerprintMinIters || iterations > FingerprintMaxIters {
		return 0, 0, "iterations must be between 50 and 500"
	}
	if concurrency < FingerprintMinConc || concurrency > FingerprintMaxConc {
		return 0, 0, "concurrency must be between 1 and 20"
	}
	return iterations, concurrency, ""
}
