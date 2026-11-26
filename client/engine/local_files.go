package engine

// LocalFileDeleteResult reports paths removed from the selected binding only.
// Details contains a bounded sample while Skipped counts all retained items.
type LocalFileDeleteResult struct {
	Removed int      `json:"removed"`
	Skipped int      `json:"skipped"`
	Details []string `json:"details"`
	Paused  bool     `json:"paused"`
}

func (r *LocalFileDeleteResult) skip(path string, err error) {
	r.Skipped++
	if len(r.Details) < 50 {
		r.Details = append(r.Details, path+" · "+err.Error())
	}
}
