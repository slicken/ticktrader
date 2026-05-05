package model

// insertWithLimitInPlace is a highly optimized function that inserts an item at the front
// of a slice while maintaining a maximum size limit. It achieves maximum performance by:
// - Zero allocations for non-empty slices (modifies in-place)
// - Single copy operation for shifting elements (O(n) time complexity)
// - Pre-allocates capacity only when slice is empty
// - Uses direct pointer manipulation for minimal overhead
func insertWithLimitInPlace[T any](slice *[]T, item T, limit int) {
	s := *slice
	n := len(s)

	switch {
	case n == 0:
		*slice = make([]T, 1, limit)
		(*slice)[0] = item

	case n < limit:
		s = append(s, item) // Temporarily append to end
		copy(s[1:], s[:n])  // Shift right
		s[0] = item         // Insert at front
		*slice = s

	default:
		copy(s[1:], s[:limit-1]) // Shift right
		s[0] = item              // Replace front
	}
}

func mapToSlice[T any](m *map[string]T) *[]T {
	slice := make([]T, 0, len(*m))
	for _, v := range *m {
		slice = append(slice, v)
	}
	return &slice
}
