package post

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var ids = identity.Identities{Sync: []string{"sync"}, Controller: []string{"controller"}}

func as(name string) context.Context {
	return request.WithUser(context.Background(), &user.DefaultInfo{Name: name})
}

func stored() *v1.Post {
	return &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.1", Labels: map[string]string{"a": "1"}},
		Spec:       v1.PostSpec{Hidden: false},
		Status: v1.PostStatus{
			Title: "old",
			State: v1.PostStateBacklog,
			Votes: v1.PostVotes{Datum: 3, GitHub: 2, Total: 5, TrendingScore: 1.5},
			Delivery: &v1.DeliveryStatus{
				Stage: v1.DeliveryStagePlanned, HighestNotified: v1.DeliveryStageRequested,
				History: []v1.DeliveryEvent{{Stage: v1.DeliveryStagePlanned}},
				Work: &v1.DeliveryWork{Total: 2, Done: 1, Items: []v1.WorkItem{{Kind: "issue", Ref: "o/r#2", State: "open"}},
					PartShipped: []v1.PartShipped{{Changelog: "c1"}}},
			},
		},
	}
}

// incoming is a status update that touches every owned area.
func incoming() *v1.Post {
	p := stored()
	p.Spec.Hidden = true
	p.Labels = map[string]string{"b": "2"}
	p.Status.Title = "new"
	p.Status.Votes = v1.PostVotes{Datum: 99, GitHub: 7, Total: 99, TrendingScore: 99}
	p.Status.Delivery = &v1.DeliveryStatus{
		Stage: v1.DeliveryStagePickedUp, HighestNotified: v1.DeliveryStagePickedUp,
		History: []v1.DeliveryEvent{{Stage: v1.DeliveryStagePickedUp}},
		Work: &v1.DeliveryWork{Total: 3, Done: 3, Items: []v1.WorkItem{{Kind: "issue", Ref: "o/r#3", State: "closed"}},
			PartShipped: []v1.PartShipped{{Changelog: "c2"}}},
	}
	return p
}

func TestStatusMerge(t *testing.T) {
	s := statusStrategy{newStrategy(runtime.NewScheme(), ids)}

	t.Run("sync", func(t *testing.T) {
		p := incoming()
		s.PrepareForUpdate(as("sync"), p, stored())
		st := p.Status
		if st.Title != "new" || st.Votes.GitHub != 7 {
			t.Errorf("sync-owned fields not applied: %+v", st)
		}
		if st.Votes.Datum != 3 || st.Votes.Total != 5 || st.Votes.TrendingScore != 1.5 {
			t.Errorf("server-owned votes changed: %+v", st.Votes)
		}
		d := st.Delivery
		if d.Stage != v1.DeliveryStagePlanned || d.HighestNotified != v1.DeliveryStageRequested ||
			d.History[0].Stage != v1.DeliveryStagePlanned || d.Work.PartShipped[0].Changelog != "c1" {
			t.Errorf("controller-owned delivery changed: %+v", d)
		}
		if d.Work.Total != 3 || d.Work.Items[0].Ref != "o/r#3" {
			t.Errorf("sync-owned work items not applied: %+v", d.Work)
		}
		if p.Spec.Hidden || p.Labels["a"] != "1" {
			t.Error("status update changed spec or labels")
		}
		if errs := s.ValidateUpdate(as("sync"), p, stored()); len(errs) > 0 {
			t.Errorf("validate: %v", errs)
		}
	})

	t.Run("controller", func(t *testing.T) {
		p := incoming()
		s.PrepareForUpdate(as("controller"), p, stored())
		st := p.Status
		if st.Title != "old" || st.Votes.GitHub != 2 {
			t.Errorf("controller changed sync-owned fields: %+v", st)
		}
		d := st.Delivery
		if d.Stage != v1.DeliveryStagePickedUp || d.HighestNotified != v1.DeliveryStagePickedUp ||
			d.History[0].Stage != v1.DeliveryStagePickedUp || d.Work.PartShipped[0].Changelog != "c2" {
			t.Errorf("controller-owned delivery not applied: %+v", d)
		}
		if d.Work.Total != 2 || d.Work.Items[0].Ref != "o/r#2" {
			t.Errorf("controller changed work items: %+v", d.Work)
		}
	})

	t.Run("staff", func(t *testing.T) {
		p := incoming()
		s.PrepareForUpdate(as("staff@datum.net"), p, stored())
		if p.Status.Title != "old" {
			t.Error("staff changed status")
		}
		if errs := s.ValidateUpdate(as("staff@datum.net"), p, stored()); len(errs) != 1 {
			t.Errorf("staff status update should be forbidden, got %v", errs)
		}
	})
}

func TestMainUpdateKeepsStatus(t *testing.T) {
	s := newStrategy(runtime.NewScheme(), ids)
	p := incoming()
	s.PrepareForUpdate(as("sync"), p, stored())
	if p.Status.Title != "old" || !p.Spec.Hidden {
		t.Fatalf("main update: title=%q hidden=%v", p.Status.Title, p.Spec.Hidden)
	}
}

func TestCreateDropsUnownedFields(t *testing.T) {
	s := newStrategy(runtime.NewScheme(), ids)
	p := incoming()
	s.PrepareForCreate(as("sync"), p)
	if p.Status.Votes.Datum != 0 || p.Status.Votes.Total != 0 || p.Status.Votes.GitHub != 7 {
		t.Errorf("votes: %+v", p.Status.Votes)
	}
	d := p.Status.Delivery
	if d == nil || d.Stage != "" || d.HighestNotified != "" || len(d.History) != 0 || len(d.Work.PartShipped) != 0 || d.Work.Total != 3 {
		t.Errorf("delivery: %+v", d)
	}
}

func TestValidate(t *testing.T) {
	ok := stored()
	ok.Status.GitHub = &v1.GitHubIssue{Org: "Datum-Cloud", Repository: "Enhancements", Number: 1, State: "open"}
	if errs := validate(ok); len(errs) > 0 {
		t.Fatalf("valid post rejected: %v", errs)
	}
	bad := ok.DeepCopy()
	bad.Name = "datum-cloud.enhancements.2"
	bad.Status.State = "someday"
	bad.Status.Delivery.Stage = "nearly"
	if errs := validate(bad); len(errs) != 3 {
		t.Fatalf("want 3 errors (name, state, stage), got %v", errs)
	}
}
