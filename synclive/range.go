package synclive

type SliceRanges [][2]int64

func (r SliceRanges) Valid() bool {
	for _, sr := range r {
		// always goes from start to end
		if sr[1] < sr[0] {
			return false
		}
		if sr[0] < 0 {
			return false
		}
	}
	return true
}

// Delta returns the ranges added and removed
func (r SliceRanges) Delta(next SliceRanges) (added SliceRanges, removed SliceRanges, same SliceRanges) {
	olds := make(map[[2]int64]bool)
	for _, oldStartEnd := range r {
		olds[oldStartEnd] = true
	}
	news := make(map[[2]int64]bool)
	for _, newStartEnd := range next {
		news[newStartEnd] = true
	}

	for oldStartEnd := range olds {
		if news[oldStartEnd] {
			same = append(same, oldStartEnd)
		} else {
			removed = append(removed, oldStartEnd)
		}
	}
	for newStartEnd := range news {
		if olds[newStartEnd] {
			continue
		}
		added = append(added, newStartEnd)
	}
	return
}

// Slice into this range.
func (r SliceRanges) SliceInto(slice Subslicer) []Subslicer {
	var result []Subslicer
	// TODO: ensure we don't have overlapping ranges
	for _, sr := range r {
		// apply range caps
		// the range are always index positions hence -1
		sliceLen := slice.Len()
		if sr[0] >= sliceLen {
			sr[0] = sliceLen - 1
		}
		if sr[1] >= sliceLen {
			sr[1] = sliceLen - 1
		}
		subslice := slice.Subslice(sr[0], sr[1]+1)
		result = append(result, subslice)
	}
	return result
}

type Subslicer interface {
	Len() int64
	Subslice(i, j int64) Subslicer
}
