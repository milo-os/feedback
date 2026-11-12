// Package post implements storage for Post.
//
// Field ownership:
//   - feedback-sync creates and deletes posts and owns everything in status
//     except the fields below.
//   - feedback-controller owns status.delivery's stage, highestNotified,
//     history and work.partShipped. feedback-sync keeps work items, totals
//     and pull requests, which it refreshes from GitHub.
//   - The server computes status.votes.datum, total, linked and
//     trendingScore on every write, from the votes table and the 👍
//     reactions feedback-sync reports.
//   - When feedback-sync sets status.mergedInto, votes and subscriptions move
//     to that post.
//   - Everyone else (staff) can change spec.hidden and metadata only.
package post

import (
	"context"
	"regexp"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	ids identity.Identities
}

func newStrategy(typer runtime.ObjectTyper, ids identity.Identities) strategy {
	return strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, ids: ids}
}

func (strategy) NamespaceScoped() bool { return false }

// PrepareForCreate drops fields that feedback-sync doesn't own. The
// controller fills the delivery tracker and the server computes votes.
func (strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	p := obj.(*v1.Post)
	keepServerVotes(&p.Status, v1.PostVotes{})
	keepControllerDelivery(&p.Status, nil)
}

// PrepareForUpdate keeps status: it changes through the status subresource.
func (strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	obj.(*v1.Post).Status = old.(*v1.Post).Status
}

func (strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	p := obj.(*v1.Post)
	errs := validate(p)
	if p.Status.GitHub == nil {
		errs = append(errs, field.Required(field.NewPath("status", "github"), "every post is backed by a GitHub issue"))
	}
	return errs
}

func (strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	return validate(obj.(*v1.Post))
}

func (strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (strategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (strategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (strategy) Canonicalize(runtime.Object)                   {}

func (strategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		fieldpath.APIVersion(v1.SchemeGroupVersion.String()): fieldpath.NewSet(fieldpath.MakePathOrDie("status")),
	}
}

// statusStrategy merges a status update field by field according to who is
// calling. Fields the caller doesn't own keep their stored value, like spec
// on any status update.
type statusStrategy struct {
	strategy
}

func (s statusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	p, o := obj.(*v1.Post), old.(*v1.Post)
	p.Spec = o.Spec
	p.Labels, p.Annotations = o.Labels, o.Annotations

	switch s.ids.Of(ctx) {
	case identity.Sync:
		keepServerVotes(&p.Status, o.Status.Votes)
		keepControllerDelivery(&p.Status, o.Status.Delivery)
	case identity.Controller:
		incoming := p.Status.Delivery
		p.Status = *o.Status.DeepCopy()
		keepControllerDelivery(&p.Status, incoming)
	default:
		p.Status = o.Status
	}
}

func (s statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	if s.ids.Of(ctx) == identity.Other {
		return field.ErrorList{field.Forbidden(field.NewPath("status"),
			"post status is written only by feedback-sync and feedback-controller")}
	}
	return validate(obj.(*v1.Post))
}

func (statusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		fieldpath.APIVersion(v1.SchemeGroupVersion.String()): fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}

// keepServerVotes resets the server-computed vote fields to from's.
func keepServerVotes(st *v1.PostStatus, from v1.PostVotes) {
	st.Votes.Datum = from.Datum
	st.Votes.Total = from.Total
	st.Votes.Linked = from.Linked
	st.Votes.TrendingScore = from.TrendingScore
}

// keepControllerDelivery sets the controller-owned delivery fields in st to
// from's, leaving the sync-owned work items alone.
func keepControllerDelivery(st *v1.PostStatus, from *v1.DeliveryStatus) {
	if from == nil {
		from = &v1.DeliveryStatus{}
	}
	if st.Delivery == nil {
		st.Delivery = &v1.DeliveryStatus{}
	}
	d := st.Delivery
	d.Stage = from.Stage
	d.HighestNotified = from.HighestNotified
	d.History = slices.Clone(from.History)
	var partShipped []v1.PartShipped
	if from.Work != nil {
		partShipped = slices.Clone(from.Work.PartShipped)
	}
	if d.Work == nil && len(partShipped) > 0 {
		d.Work = &v1.DeliveryWork{}
	}
	if d.Work != nil {
		d.Work.PartShipped = partShipped
	}
	if d.Stage == "" && d.HighestNotified == "" && len(d.History) == 0 && d.Work == nil {
		st.Delivery = nil
	}
}

// postNameRE matches PostName's output.
var postNameRE = regexp.MustCompile(`^[a-z0-9-]+\.[a-z0-9._-]+\.[1-9][0-9]*$`)

func validate(p *v1.Post) field.ErrorList {
	var errs field.ErrorList
	st := field.NewPath("status")
	if gh := p.Status.GitHub; gh != nil {
		ghp := st.Child("github")
		if gh.Org == "" {
			errs = append(errs, field.Required(ghp.Child("org"), ""))
		}
		if gh.Repository == "" {
			errs = append(errs, field.Required(ghp.Child("repository"), ""))
		}
		if gh.Number <= 0 {
			errs = append(errs, field.Invalid(ghp.Child("number"), gh.Number, "must be positive"))
		}
		if gh.State != "open" && gh.State != "closed" {
			errs = append(errs, field.NotSupported(ghp.Child("state"), gh.State, []string{"open", "closed"}))
		}
		if want := v1.PostName(gh.Org, gh.Repository, gh.Number); len(errs) == 0 && p.Name != want {
			errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), p.Name,
				"must be "+want+", from status.github"))
		}
	}
	if s := p.Status.State; s != "" && !slices.Contains(v1.ValidPostStates, s) {
		errs = append(errs, field.NotSupported(st.Child("state"), s, v1.ValidPostStates))
	}
	if p.Status.Votes.GitHub < 0 {
		errs = append(errs, field.Invalid(st.Child("votes", "github"), p.Status.Votes.GitHub, "must not be negative"))
	}
	if d := p.Status.Delivery; d != nil {
		dp := st.Child("delivery")
		for _, f := range []struct {
			path  *field.Path
			stage v1.DeliveryStage
		}{{dp.Child("stage"), d.Stage}, {dp.Child("highestNotified"), d.HighestNotified}} {
			if f.stage != "" && !slices.Contains(v1.ValidDeliveryStages, f.stage) {
				errs = append(errs, field.NotSupported(f.path, f.stage, v1.ValidDeliveryStages))
			}
		}
		for i, h := range d.History {
			if !slices.Contains(v1.ValidDeliveryStages, h.Stage) {
				errs = append(errs, field.NotSupported(dp.Child("history").Index(i).Child("stage"), h.Stage, v1.ValidDeliveryStages))
			}
		}
	}
	for i, r := range p.Status.Votes.GitHubReactions {
		if r.Login == "" {
			errs = append(errs, field.Required(st.Child("votes", "githubReactions").Index(i).Child("login"), ""))
		}
	}
	if m := p.Status.MergedInto; m != "" {
		mp := st.Child("mergedInto")
		switch {
		case m == p.Name:
			errs = append(errs, field.Invalid(mp, m, "a post can't be merged into itself"))
		case !postNameRE.MatchString(m):
			errs = append(errs, field.Invalid(mp, m, "must be a post name, org.repository.number"))
		}
	}
	if len(p.Status.PublicVoters) > 10 {
		errs = append(errs, field.TooMany(st.Child("publicVoters"), len(p.Status.PublicVoters), 10))
	}
	return errs
}
