package utils

import "strings"

func SplitCommaTrimSpace(s string) []string {
	sls := strings.Split(s, ",")
	for i, sl := range sls {
		sls[i] = strings.TrimSpace(sl)
	}

	return sls
}

// func BitsetExists(num *int, bitPos uint) bool {
//     return *num&(1<<bitPos) != 0
// }

// func BitsetSet(num *int, bitPos uint) {
//     mask := 1 << bitPos
//     *num |= mask
// }

func PtrTool[T any](param T) *T {
	return &param
}
