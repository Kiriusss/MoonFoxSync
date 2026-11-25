package engine

import "context"

type MergeChoice struct {
	Path   string  `json:"path"`
	Choice string  `json:"choice"`
	Text   *string `json:"text,omitempty"`
}
type MergeConflict struct {
	Path        string `json:"path"`
	MainKind    string `json:"mainKind"`
	BranchKind  string `json:"branchKind"`
	BaseKind    string `json:"baseKind"`
	BothPath    string `json:"bothPath"`
	TextAllowed bool   `json:"textAllowed"`
}
type MergePlan struct {
	BaseHidden       bool            `json:"baseHidden,omitempty"`
	BaseAmbiguous    bool            `json:"baseAmbiguous,omitempty"`
	UnitID           string          `json:"unitId"`
	MainHead         string          `json:"mainHead"`
	BranchSnapshotID string          `json:"branchSnapshotId"`
	BaseSnapshotID   string          `json:"baseSnapshotId"`
	AlreadyMerged    bool            `json:"alreadyMerged"`
	Conflicts        []MergeConflict `json:"conflicts"`
	AutoPaths        []string        `json:"autoPaths"`
}

func (r *Remote) MergePlan(ctx context.Context, unit, branch string) (MergePlan, error) {
	var out MergePlan
	for _, id := range []string{unit, branch} {
		if err := validID(id); err != nil {
			return out, err
		}
	}
	err := r.call(ctx, "POST", "/units/"+unit+"/merge/plan", nil, map[string]string{"branchSnapshotId": branch}, &out)
	return out, err
}
func (r *Remote) MergeCommit(ctx context.Context, unit, main, branch, requestID, label string, choices []MergeChoice) (Receipt, error) {
	var out Receipt
	for _, id := range []string{unit, main, branch, requestID} {
		if err := validID(id); err != nil {
			return out, err
		}
	}
	if choices == nil {
		choices = []MergeChoice{}
	}
	err := r.commitCall(ctx, "POST", "/units/"+unit+"/merge/commit", map[string]any{"mainHead": main, "branchSnapshotId": branch, "requestId": requestID, "label": label, "choices": choices}, &out)
	return out, err
}
