package util

// Batch splits a slice of strings into consecutive chunks of at most size
// elements. The reference plugins cap BookOrbit match-check requests at 500
// hashes per call; this helper produces those chunks without copying data.
//
// A size <= 0 yields a single batch containing the whole input. A nil or empty
// input returns nil.
func Batch(items []string, size int) [][]string {
	if len(items) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(items)
	}
	var out [][]string
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[start:end])
	}
	return out
}

// BatchFunc is the generic counterpart to Batch: it splits any slice into
// chunks of at most size elements, invoking fn for each chunk. Returning false
// from fn stops iteration early. Used by the bulk-progress uploader, which is
// capped at 100 items per request.
func BatchFunc[T any](items []T, size int, fn func(chunk []T) bool) {
	if len(items) == 0 {
		return
	}
	if size <= 0 {
		size = len(items)
	}
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		if !fn(items[start:end]) {
			return
		}
	}
}
