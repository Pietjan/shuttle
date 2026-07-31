package live

import "testing"

// TestPageWindowElidesTheMiddle. Numbered pages are only usable while there
// are few enough to read: the window keeps the ends, the current page and a
// neighbour either side, and stands for everything else with one gap per
// run rather than one per page.
func TestPageWindowElidesTheMiddle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current int
		pages   int
		want    []int
	}{
		{"all of them fit", 1, 3, []int{1, 2, 3}},
		{"start", 1, 9, []int{1, 2, 0, 9}},
		{"middle", 5, 9, []int{1, 0, 4, 5, 6, 0, 9}},
		{"end", 9, 9, []int{1, 0, 8, 9}},
		{"a gap of one page is still a gap", 4, 6, []int{1, 0, 3, 4, 5, 6}},
		{"single page", 1, 1, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pageWindow(tc.current, tc.pages)
			if len(got) != len(tc.want) {
				t.Fatalf("pageWindow(%d, %d) = %v, want %v", tc.current, tc.pages, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("pageWindow(%d, %d) = %v, want %v", tc.current, tc.pages, got, tc.want)
				}
			}
		})
	}
}
