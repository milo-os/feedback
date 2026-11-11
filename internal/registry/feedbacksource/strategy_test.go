package feedbacksource

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func validSource() *feedbackv1alpha1.FeedbackSource {
	return &feedbackv1alpha1.FeedbackSource{
		ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud"},
		Spec: feedbackv1alpha1.FeedbackSourceSpec{
			GitHub: feedbackv1alpha1.GitHubSource{
				Org:            "datum-cloud",
				InstallationID: 12345678,
				Project:        &feedbackv1alpha1.ProjectRef{Owner: "datum-cloud", Number: 22, StatusField: "Status", VotesField: "Votes"},
				Milestones:     &feedbackv1alpha1.MilestoneSource{Repository: "enhancements"},
				Changelog:      &feedbackv1alpha1.ChangelogSource{Repository: "datum", DiscussionCategory: "changelog"},
			},
			StatusMapping:   map[string]feedbackv1alpha1.PostState{"": "none", "In Progress": "in-progress", "Done": "shipped"},
			CategoryMapping: map[string]string{"Build": "build"},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*feedbackv1alpha1.FeedbackSource)
		wantErr int
	}{
		{name: "valid", mutate: func(*feedbackv1alpha1.FeedbackSource) {}},
		{name: "missing org", mutate: func(s *feedbackv1alpha1.FeedbackSource) { s.Spec.GitHub.Org = "" }, wantErr: 1},
		{name: "missing installation", mutate: func(s *feedbackv1alpha1.FeedbackSource) { s.Spec.GitHub.InstallationID = 0 }, wantErr: 1},
		{name: "bad project", mutate: func(s *feedbackv1alpha1.FeedbackSource) {
			s.Spec.GitHub.Project.Owner = ""
			s.Spec.GitHub.Project.Number = 0
		}, wantErr: 2},
		{name: "bad changelog", mutate: func(s *feedbackv1alpha1.FeedbackSource) { s.Spec.GitHub.Changelog.DiscussionCategory = "" }, wantErr: 1},
		{name: "unknown status", mutate: func(s *feedbackv1alpha1.FeedbackSource) { s.Spec.StatusMapping["Backlog"] = "someday" }, wantErr: 1},
		{name: "empty category", mutate: func(s *feedbackv1alpha1.FeedbackSource) { s.Spec.CategoryMapping["Deliver"] = "" }, wantErr: 1},
		{name: "no optional blocks", mutate: func(s *feedbackv1alpha1.FeedbackSource) {
			s.Spec.GitHub.Project, s.Spec.GitHub.Milestones, s.Spec.GitHub.Changelog = nil, nil, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := validSource()
			tc.mutate(src)
			if errs := Validate(src); len(errs) != tc.wantErr {
				t.Fatalf("got %d errors, want %d: %v", len(errs), tc.wantErr, errs)
			}
		})
	}
}

func TestOrgImmutable(t *testing.T) {
	s := NewStrategy(runtime.NewScheme())
	old := validSource()
	updated := validSource()
	updated.Spec.GitHub.Org = "milo-os"
	if errs := s.ValidateUpdate(context.Background(), updated, old); len(errs) != 1 {
		t.Fatalf("expected org change to be rejected, got %v", errs)
	}
}

func TestPrepareForUpdate(t *testing.T) {
	s := NewStrategy(runtime.NewScheme())
	old := validSource()
	old.Generation = 3
	old.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}

	// Spec unchanged and status edited: generation and status stay.
	same := validSource()
	same.Generation = 3
	s.PrepareForUpdate(context.Background(), same, old)
	if same.Generation != 3 || len(same.Status.Conditions) != 1 {
		t.Fatalf("unexpected result: generation=%d status=%v", same.Generation, same.Status)
	}

	// Spec changed: generation bumps.
	changed := validSource()
	changed.Generation = 3
	changed.Spec.GitHub.InstallationID = 99
	s.PrepareForUpdate(context.Background(), changed, old)
	if changed.Generation != 4 {
		t.Fatalf("generation = %d, want 4", changed.Generation)
	}

	// Status subresource ignores spec edits.
	ss := statusStrategy{s}
	statusEdit := validSource()
	statusEdit.Spec.GitHub.InstallationID = 99
	ss.PrepareForUpdate(context.Background(), statusEdit, old)
	if statusEdit.Spec.GitHub.InstallationID != old.Spec.GitHub.InstallationID {
		t.Fatal("status update changed spec")
	}
}
