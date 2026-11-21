package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/index"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type fakeUsers struct{}

func (fakeUsers) Organizations(_ context.Context, user string) ([]string, error) {
	return []string{"org-of-" + user}, nil
}
func (fakeUsers) GitHubLogin(_ context.Context, user string) (string, error) {
	return "gh-" + user, nil
}

// clock is a settable time for the reconcilers.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// start runs the reconcilers against an envtest API server with the CRDs,
// and returns a client that reads straight from the API server.
func start(t *testing.T, now *clock, files ...*attachments.Store) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run with task test")
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Tests start a manager per run; names repeat with -count.
		Controller: config.Controller{SkipNameValidation: ptr(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := index.Add(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	o := Options{Users: fakeUsers{}, Now: now.Now}
	if len(files) > 0 {
		o.Files = files[0]
	}
	if err := Setup(ctx, mgr, o); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// eventually retries check until it returns nil or time runs out.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type errorf string

func (e errorf) Error() string { return string(e) }

func newVote(post, user string) *v1.Vote {
	return &v1.Vote{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post, user)},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: user}, Source: v1.ClientWebsite},
	}
}

func TestReconcilers(t *testing.T) {
	now := &clock{now: time.Now()}
	c := start(t, now)
	ctx := context.Background()

	post := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.1"}}
	if err := c.Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	// feedback-sync's GitHub fields: two 👍, one from u1's linked account.
	post.Status.Votes = v1.PostVotes{GitHub: 2, GitHubReactions: []v1.GitHubReaction{{Login: "gh-u1"}, {Login: "someone"}}}
	if err := c.Status().Update(ctx, post); err != nil {
		t.Fatal(err)
	}

	t.Run("a vote follows the post and records the voter", func(t *testing.T) {
		if err := c.Create(ctx, newVote(post.Name, "u1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "subscription", func() error {
			var s v1.Subscription
			if err := c.Get(ctx, types.NamespacedName{Name: v1.OwnedName(post.Name, "u1")}, &s); err != nil {
				return err
			}
			if s.Spec.Reason != v1.SubscriptionReasonVoted {
				return errorf("reason " + string(s.Spec.Reason))
			}
			return nil
		})
		eventually(t, "vote status", func() error {
			var v v1.Vote
			if err := c.Get(ctx, types.NamespacedName{Name: v1.OwnedName(post.Name, "u1")}, &v); err != nil {
				return err
			}
			if !v.Status.Subscribed || v.Status.GitHubLogin != "gh-u1" || len(v.Status.Organizations) != 1 {
				return errorf("status not filled in")
			}
			return nil
		})
	})

	t.Run("a deleted follow isn't recreated", func(t *testing.T) {
		s := &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post.Name, "u1")}}
		if err := c.Delete(ctx, s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := c.Get(ctx, client.ObjectKeyFromObject(s), s); !apierrors.IsNotFound(err) {
			t.Errorf("follow came back: %v", err)
		}
	})

	t.Run("totals count linked people once", func(t *testing.T) {
		if err := c.Create(ctx, newVote(post.Name, "u2")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "totals", func() error {
			var p v1.Post
			if err := c.Get(ctx, client.ObjectKeyFromObject(post), &p); err != nil {
				return err
			}
			// 2 Datum votes + 2 👍 - 1 person who did both.
			v := p.Status.Votes
			if v.Datum != 2 || v.Linked != 1 || v.Total != 3 || v.TrendingScore == 0 {
				return errorf("votes not tallied yet")
			}
			// Sync's fields are untouched.
			if v.GitHub != 2 || len(v.GitHubReactions) != 2 {
				return errorf("sync's fields changed")
			}
			return nil
		})
	})

	t.Run("votes and follows move off a merged post", func(t *testing.T) {
		target := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.2"}}
		if err := c.Create(ctx, target); err != nil {
			t.Fatal(err)
		}
		// u2 already voted on the target, so keeps that vote.
		if err := c.Create(ctx, newVote(target.Name, "u2")); err != nil {
			t.Fatal(err)
		}
		var p v1.Post
		if err := c.Get(ctx, client.ObjectKeyFromObject(post), &p); err != nil {
			t.Fatal(err)
		}
		p.Status.MergedInto = target.Name
		if err := c.Status().Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
		eventually(t, "moved", func() error {
			var left v1.VoteList
			if err := c.List(ctx, &left); err != nil {
				return err
			}
			names := map[string]bool{}
			for _, v := range left.Items {
				names[v.Name] = true
			}
			if names[v1.OwnedName(post.Name, "u1")] || names[v1.OwnedName(post.Name, "u2")] {
				return errorf("votes still on the merged post")
			}
			if !names[v1.OwnedName(target.Name, "u1")] || !names[v1.OwnedName(target.Name, "u2")] {
				return errorf("votes not on the target")
			}
			return nil
		})
		eventually(t, "target totals", func() error {
			var tp v1.Post
			if err := c.Get(ctx, client.ObjectKeyFromObject(target), &tp); err != nil {
				return err
			}
			if tp.Status.Votes.Datum != 2 {
				return errorf("target not tallied")
			}
			return nil
		})
	})

	t.Run("votes on a deleted post go after the retention period", func(t *testing.T) {
		gone := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.3"}}
		if err := c.Create(ctx, gone); err != nil {
			t.Fatal(err)
		}
		v := newVote(gone.Name, "u3")
		if err := c.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, gone); err != nil {
			t.Fatal(err)
		}
		eventually(t, "orphan marked", func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(v), v); err != nil {
				return err
			}
			if v.Annotations[OrphanedAtAnnotation] == "" {
				return errorf("not marked")
			}
			return nil
		})
		// Past the retention period, a touch re-queues it and it goes.
		now.Set(time.Now().Add(OrphanRetention + time.Hour))
		patch := client.MergeFrom(v.DeepCopy())
		v.Labels = map[string]string{"touch": "1"}
		if err := c.Patch(ctx, v, patch); err != nil {
			t.Fatal(err)
		}
		eventually(t, "orphan deleted", func() error {
			err := c.Get(ctx, client.ObjectKeyFromObject(v), &v1.Vote{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return errorf("still there")
		})
	})
}

func TestSubmissionReviews(t *testing.T) {
	c := start(t, &clock{now: time.Now()})
	ctx := context.Background()

	submit := func(name, user string) *v1.Submission {
		s := &v1.Submission{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: user}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b",
				Context: v1.SubmissionContext{Client: v1.ClientPortal}},
		}
		if err := c.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	decide := func(name string, spec v1.SubmissionReviewSpec) {
		eventually(t, "review for "+name, func() error {
			var r v1.SubmissionReview
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &r); err != nil {
				return err
			}
			r.Spec = spec
			return c.Update(ctx, &r)
		})
	}
	phase := func(name string, want v1.SubmissionPhase, post string) {
		eventually(t, name+" "+string(want), func() error {
			var s v1.Submission
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &s); err != nil {
				return err
			}
			got := ""
			if s.Status.PostRef != nil {
				got = s.Status.PostRef.Name
			}
			if s.Status.Phase != want || got != post {
				return errorf(string(s.Status.Phase) + " " + got)
			}
			return nil
		})
	}
	has := func(obj client.Object) error { return c.Get(ctx, client.ObjectKeyFromObject(obj), obj) }

	t.Run("new submissions wait for review", func(t *testing.T) {
		submit("sub-new", "u1")
		phase("sub-new", v1.SubmissionPhasePendingReview, "")
	})

	t.Run("rejected", func(t *testing.T) {
		submit("sub-rej", "u1")
		decide("sub-rej", v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "Out of scope"})
		phase("sub-rej", v1.SubmissionPhaseRejected, "")
	})

	t.Run("merged into a post", func(t *testing.T) {
		if err := c.Create(ctx, &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "a.b.10"}}); err != nil {
			t.Fatal(err)
		}
		submit("sub-merge", "u5")
		decide("sub-merge", v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionMerged, MergedInto: "a.b.10"})
		phase("sub-merge", v1.SubmissionPhaseMerged, "a.b.10")
		eventually(t, "follow and vote", func() error {
			s := &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName("a.b.10", "u5")}}
			if err := has(s); err != nil {
				return err
			}
			if s.Spec.Reason != v1.SubscriptionReasonMerged {
				return errorf("reason " + string(s.Spec.Reason))
			}
			v := &v1.Vote{ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName("a.b.10", "u5")}}
			if err := has(v); err != nil {
				return err
			}
			if v.Spec.Source != v1.ClientPortal {
				return errorf("source " + string(v.Spec.Source))
			}
			return nil
		})
	})

	t.Run("approved, then linked when its post syncs", func(t *testing.T) {
		submit("sub-ok", "u6")
		decide("sub-ok", v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionApproved, PublicTitle: "T", PublicBody: "B"})
		phase("sub-ok", v1.SubmissionPhaseApproved, "")
		eventually(t, "waiting condition", func() error {
			var r v1.SubmissionReview
			if err := c.Get(ctx, types.NamespacedName{Name: "sub-ok"}, &r); err != nil {
				return err
			}
			if r.Status.Applied || len(r.Status.Conditions) == 0 || r.Status.Conditions[0].Reason != "WaitingForPost" {
				return errorf("not waiting")
			}
			return nil
		})
		// feedback-sync creates the issue and syncs its post.
		post := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.99",
			Annotations: map[string]string{v1.FromSubmissionAnnotation: "sub-ok"}}}
		if err := c.Create(ctx, post); err != nil {
			t.Fatal(err)
		}
		phase("sub-ok", v1.SubmissionPhaseApproved, post.Name)
		eventually(t, "applied", func() error {
			var r v1.SubmissionReview
			if err := c.Get(ctx, types.NamespacedName{Name: "sub-ok"}, &r); err != nil {
				return err
			}
			if !r.Status.Applied {
				return errorf("not applied")
			}
			return has(&v1.Vote{ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post.Name, "u6")}})
		})
	})

	t.Run("the review goes with its submission", func(t *testing.T) {
		var r v1.SubmissionReview
		if err := c.Get(ctx, types.NamespacedName{Name: "sub-new"}, &r); err != nil {
			t.Fatal(err)
		}
		if len(r.OwnerReferences) != 1 || r.OwnerReferences[0].Name != "sub-new" {
			t.Errorf("owner references %v", r.OwnerReferences)
		}
	})
}

func TestSubmissionAttachments(t *testing.T) {
	ctx := context.Background()
	files, err := attachments.Open(ctx, "mem://")
	if err != nil {
		t.Fatal(err)
	}
	c := start(t, &clock{now: time.Now()}, files)
	ref, _, err := files.Upload(ctx, attachments.Submission, "u1", "notes.txt", "text/plain", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	sub := &v1.Submission{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-files"},
		Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindBug, Title: "t", Body: "b",
			Attachments: []v1.AttachmentReference{{UploadRef: ref}}},
	}
	if err := c.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	eventually(t, "copied", func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(sub), sub); err != nil {
			return err
		}
		if len(sub.Finalizers) != 1 {
			return errorf("no finalizer")
		}
		// Read without the owner, so only the copy counts.
		_, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref)
		return err
	})
	if err := c.Delete(ctx, sub); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted", func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(sub), &v1.Submission{}); !apierrors.IsNotFound(err) {
			return errorf("submission still there")
		}
		if _, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref); err == nil {
			return errorf("file still there")
		}
		return nil
	})
}

func TestSecurityReports(t *testing.T) {
	ctx := context.Background()
	files, err := attachments.Open(ctx, "mem://")
	if err != nil {
		t.Fatal(err)
	}
	c := start(t, &clock{now: time.Now()}, files)
	get := func(obj client.Object) error { return c.Get(ctx, client.ObjectKeyFromObject(obj), obj) }

	t.Run("a new report is received, with private triage", func(t *testing.T) {
		rep := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: "sec-1"}, Spec: v1.SecurityReportSpec{
			UserRef: &v1.UserReference{Name: "u1"}, Title: "t", Description: "d",
			Credit: &v1.SecurityCredit{Public: true, Name: "Ada L.", URL: "https://example.com"}}}
		if err := c.Create(ctx, rep); err != nil {
			t.Fatal(err)
		}
		eventually(t, "received", func() error {
			if err := get(rep); err != nil {
				return err
			}
			if rep.Status.Phase != v1.SecurityReportPhaseReceived || len(rep.Status.Timeline) != 1 {
				return errorf("status " + string(rep.Status.Phase))
			}
			return get(&v1.SecurityReportTriage{ObjectMeta: metav1.ObjectMeta{Name: "sec-1"}})
		})

		// Fixed, with public credit: acknowledged.
		rep.Status.Phase, rep.Status.Severity = v1.SecurityReportPhaseFixed, v1.SeverityHigh
		rep.Status.Timeline = append(rep.Status.Timeline, v1.SecurityTimelineEntry{
			Phase: v1.SecurityReportPhaseFixed, At: metav1.NewTime(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))})
		if err := c.Status().Update(ctx, rep); err != nil {
			t.Fatal(err)
		}
		ack := &v1.SecurityAcknowledgement{ObjectMeta: metav1.ObjectMeta{Name: "sec-1"}}
		eventually(t, "acknowledged", func() error {
			if err := get(ack); err != nil {
				return err
			}
			if ack.Status.Name != "Ada L." || ack.Status.Month != "2026-10" || ack.Status.Severity != v1.SeverityHigh {
				return errorf("ack " + ack.Status.Month)
			}
			return nil
		})
	})

	t.Run("no public credit, no acknowledgement", func(t *testing.T) {
		rep := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: "sec-2"}, Spec: v1.SecurityReportSpec{
			Reporter: &v1.ExternalReporter{Email: "r@example.com"}, Title: "t", Description: "d"}}
		if err := c.Create(ctx, rep); err != nil {
			t.Fatal(err)
		}
		eventually(t, "received", func() error {
			if err := get(rep); err != nil {
				return err
			}
			if rep.Status.Phase == "" {
				return errorf("no phase")
			}
			return nil
		})
		rep.Status.Phase = v1.SecurityReportPhaseDisclosed
		if err := c.Status().Update(ctx, rep); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := get(&v1.SecurityAcknowledgement{ObjectMeta: metav1.ObjectMeta{Name: "sec-2"}}); !apierrors.IsNotFound(err) {
			t.Errorf("acknowledgement exists: %v", err)
		}
	})

	// submit creates a submission by u7 with an attachment and waits for the
	// operator to copy the file under it.
	submit := func(t *testing.T, name string) (*v1.Submission, string) {
		t.Helper()
		ref, _, err := files.Upload(ctx, attachments.Submission, "u7", "poc.txt", "text/plain", []byte("poc"))
		if err != nil {
			t.Fatal(err)
		}
		sub := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.SubmissionSpec{
			UserRef: v1.UserReference{Name: "u7"}, Kind: v1.SubmissionKindBug, Title: "Viewer reads secrets", Body: "steps",
			Attachments: []v1.AttachmentReference{{UploadRef: ref}}}}
		if err := c.Create(ctx, sub); err != nil {
			t.Fatal(err)
		}
		eventually(t, "submission's files copied", func() error {
			if err := get(sub); err != nil {
				return err
			}
			if len(sub.Finalizers) == 0 {
				return errorf("no finalizer")
			}
			return nil
		})
		return sub, ref
	}
	decide := func(t *testing.T, sub *v1.Submission) *v1.SubmissionReview {
		t.Helper()
		r := &v1.SubmissionReview{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}
		eventually(t, "decide", func() error {
			if err := get(r); err != nil {
				return err
			}
			r.Spec = v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionMoveToSecurity, Affected: "secrets"}
			return c.Update(ctx, r)
		})
		return r
	}
	// moved waits for sub to become the report rep, with its file.
	moved := func(t *testing.T, sub *v1.Submission, ref string, rep *v1.SecurityReport) {
		t.Helper()
		eventually(t, "moved", func() error {
			if err := get(rep); err != nil {
				return err
			}
			if rep.Spec.UserRef == nil || rep.Spec.UserRef.Name != "u7" || rep.Spec.Affected != "secrets" {
				return errorf("report spec")
			}
			if !movedFrom(rep, sub) {
				return errorf("report doesn't name the submission")
			}
			if err := get(&v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}); !apierrors.IsNotFound(err) {
				return errorf("submission still there")
			}
			if _, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref); err == nil {
				return errorf("submission's copy not deleted")
			}
			_, err := files.Get(ctx, attachments.SecurityReport, rep.Name, "", ref)
			return err
		})
	}

	t.Run("a misfiled submission moves to the security track", func(t *testing.T) {
		sub, ref := submit(t, "sub-sec")
		// A report filed under the submission's own name is someone else's
		// and is left alone.
		other := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}, Spec: v1.SecurityReportSpec{
			UserRef: &v1.UserReference{Name: "u8"}, Title: "other", Description: "d"}}
		if err := c.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		decide(t, sub)
		moved(t, sub, ref, &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: v1.MovedReportName(sub.Name, sub.UID)}})
		if err := get(other); err != nil || other.Spec.Title != "other" {
			t.Errorf("the other report changed: %v", err)
		}
	})

	t.Run("a move finishes when its report already exists", func(t *testing.T) {
		// As if the operator stopped after creating the report.
		sub, ref := submit(t, "sub-sec-retry")
		rep := movedReport(sub, &v1.SubmissionReview{Spec: v1.SubmissionReviewSpec{Affected: "secrets"}}, v1.MovedReportName(sub.Name, sub.UID))
		if err := c.Create(ctx, rep); err != nil {
			t.Fatal(err)
		}
		decide(t, sub)
		moved(t, sub, ref, rep)
	})

	t.Run("a report deleted while its files are copied keeps none of them", func(t *testing.T) {
		sub, ref := submit(t, "sub-sec-deleted")
		name := v1.MovedReportName(sub.Name, sub.UID)
		copying, resume := make(chan struct{}, 4), make(chan struct{})
		hook := func(report string) {
			if report == name {
				copying <- struct{}{}
				<-resume
			}
		}
		beforeMoveCopy.Store(&hook)
		t.Cleanup(func() { beforeMoveCopy.Store(nil); close(resume) })
		waitForCopy := func(what string) {
			t.Helper()
			select {
			case <-copying:
			case <-time.After(15 * time.Second):
				t.Fatalf("the move never started %s", what)
			}
		}
		noReportFile := func(when string) {
			t.Helper()
			if _, err := files.Get(ctx, attachments.SecurityReport, name, "", ref); !errors.Is(err, attachments.ErrNotFound) {
				t.Fatalf("%s: the report has a copy of the file: %v", when, err)
			}
		}

		decide(t, sub)
		waitForCopy("copying")
		first := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := get(first); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, first); err != nil {
			t.Fatal(err)
		}
		// The report waits for the copy instead of cleaning up under it.
		time.Sleep(time.Second)
		if err := get(first); err != nil || first.DeletionTimestamp == nil {
			t.Fatalf("the report didn't wait for the copy: %v", err)
		}

		resume <- struct{}{}
		// The report goes, taking every copy with it; the submission stays
		// and is moved again, into a new report.
		waitForCopy("moving again")
		noReportFile("once the deleted report has gone")
		if err := get(&v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}); err != nil {
			t.Fatalf("submission: %v", err)
		}
		if _, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref); err != nil {
			t.Fatalf("submission's file: %v", err)
		}
		resume <- struct{}{}
		rep := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: name}}
		moved(t, sub, ref, rep)
		if rep.UID == first.UID {
			t.Errorf("moved into the deleted report")
		}
		if controllerutil.ContainsFinalizer(rep, MoveFinalizer) {
			t.Errorf("the move still holds the report")
		}
	})

	// pauseMoveCopy blocks moves to sub's report just before they copy, until
	// the returned resume is called. waitForCopy waits for the move to get
	// there.
	pauseMoveCopy := func(t *testing.T, sub *v1.Submission) (waitForCopy func(), resume func()) {
		t.Helper()
		name := v1.MovedReportName(sub.Name, sub.UID)
		copying, unblock := make(chan struct{}, 4), make(chan struct{})
		hook := func(report string) {
			if report == name {
				copying <- struct{}{}
				<-unblock
			}
		}
		beforeMoveCopy.Store(&hook)
		var once sync.Once
		resume = func() { once.Do(func() { close(unblock) }) }
		t.Cleanup(func() { beforeMoveCopy.Store(nil); resume() })
		return func() {
			t.Helper()
			select {
			case <-copying:
			case <-time.After(15 * time.Second):
				t.Fatal("the move never started copying")
			}
		}, resume
	}
	// terminating fails unless obj still exists and is being deleted.
	terminating := func(t *testing.T, obj client.Object, what string) {
		t.Helper()
		if err := get(obj); err != nil || obj.GetDeletionTimestamp() == nil {
			t.Fatalf("%s didn't wait for the copy: %v", what, err)
		}
	}

	t.Run("deleting the submission and its report during the copy leaves no files", func(t *testing.T) {
		sub, ref := submit(t, "sub-sec-both-deleted")
		waitForCopy, resume := pauseMoveCopy(t, sub)
		decide(t, sub)
		waitForCopy()
		rep := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: v1.MovedReportName(sub.Name, sub.UID)}}
		if err := get(rep); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, sub); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, rep); err != nil {
			t.Fatal(err)
		}
		// Neither cleanup runs while the copy is outstanding.
		time.Sleep(time.Second)
		terminating(t, &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}, "the submission")
		terminating(t, &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: rep.Name}}, "the report")

		resume()
		eventually(t, "both gone with their files", func() error {
			if err := get(&v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}); !apierrors.IsNotFound(err) {
				return errorf("submission still there")
			}
			if err := get(&v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: rep.Name}}); !apierrors.IsNotFound(err) {
				return errorf("report still there")
			}
			if _, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref); !errors.Is(err, attachments.ErrNotFound) {
				return errorf("the submission's file is still stored")
			}
			if _, err := files.Get(ctx, attachments.SecurityReport, rep.Name, "", ref); !errors.Is(err, attachments.ErrNotFound) {
				return errorf("the report's copy is still stored")
			}
			return nil
		})
	})

	t.Run("deleting the submission during the copy still finishes the move", func(t *testing.T) {
		sub, ref := submit(t, "sub-sec-source-deleted")
		waitForCopy, resume := pauseMoveCopy(t, sub)
		decide(t, sub)
		waitForCopy()
		if err := c.Delete(ctx, sub); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		terminating(t, &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}, "the submission")

		resume()
		rep := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: v1.MovedReportName(sub.Name, sub.UID)}}
		moved(t, sub, ref, rep)
		if controllerutil.ContainsFinalizer(rep, MoveFinalizer) {
			t.Errorf("the move still holds the report")
		}
	})

	t.Run("a report is let go when its submission is deleted mid-move", func(t *testing.T) {
		sub, _ := submit(t, "sub-sec-gone")
		// As if the operator stopped after creating the report, and then
		// staff deleted the submission.
		rep := movedReport(sub, &v1.SubmissionReview{}, v1.MovedReportName(sub.Name, sub.UID))
		if err := c.Create(ctx, rep); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, sub); err != nil {
			t.Fatal(err)
		}
		eventually(t, "let go", func() error {
			if err := get(rep); err != nil {
				return err
			}
			if controllerutil.ContainsFinalizer(rep, MoveFinalizer) {
				return errorf("still held")
			}
			return nil
		})
		if err := c.Delete(ctx, rep); err != nil {
			t.Fatal(err)
		}
		eventually(t, "deleted", func() error {
			if err := get(rep); !apierrors.IsNotFound(err) {
				return errorf("report still there")
			}
			return nil
		})
	})

	for _, tc := range []struct {
		name  string
		taken func(sub *v1.Submission) *v1.SecurityReport
	}{
		{"an unrelated report", func(sub *v1.Submission) *v1.SecurityReport {
			return &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: v1.MovedReportName(sub.Name, sub.UID)},
				Spec: v1.SecurityReportSpec{UserRef: &v1.UserReference{Name: "u8"}, Title: "other", Description: "d"}}
		}},
		{"a report naming the submission, for someone else", func(sub *v1.Submission) *v1.SecurityReport {
			rep := movedReport(sub, &v1.SubmissionReview{}, v1.MovedReportName(sub.Name, sub.UID))
			rep.Spec.UserRef.Name = "u8"
			rep.Finalizers = nil
			return rep
		}},
		{"a report naming another submission with the same name", func(sub *v1.Submission) *v1.SecurityReport {
			rep := movedReport(sub, &v1.SubmissionReview{}, v1.MovedReportName(sub.Name, sub.UID))
			rep.Annotations[v1.MovedFromSubmissionUIDAnnotation] = "00000000-0000-0000-0000-000000000000"
			rep.Finalizers = nil
			return rep
		}},
	} {
		t.Run("a move is refused when "+tc.name+" has the name", func(t *testing.T) {
			sub, ref := submit(t, fmt.Sprintf("sub-sec-taken-%d", time.Now().UnixNano()))
			taken := tc.taken(sub)
			if err := c.Create(ctx, taken); err != nil {
				t.Fatal(err)
			}
			before := taken.DeepCopy()
			review := decide(t, sub)
			eventually(t, "refused", func() error {
				if err := get(review); err != nil {
					return err
				}
				cond := meta.FindStatusCondition(review.Status.Conditions, ConditionApplied)
				if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonReportNameTaken {
					return errorf("no ReportNameTaken condition")
				}
				return nil
			})
			// Give a wrong move time to happen.
			time.Sleep(time.Second)
			if err := get(&v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}); err != nil {
				t.Fatalf("submission: %v", err)
			}
			if _, err := files.Get(ctx, attachments.Submission, sub.Name, "", ref); err != nil {
				t.Errorf("submission's file: %v", err)
			}
			if _, err := files.Get(ctx, attachments.SecurityReport, taken.Name, "", ref); err == nil {
				t.Errorf("file copied to the other report")
			}
			if err := get(taken); err != nil || taken.UID != before.UID || !reflect.DeepEqual(taken.Spec, before.Spec) {
				t.Errorf("the other report changed: %v", err)
			}

			// Once that report is gone, the move goes ahead.
			if err := c.Delete(ctx, taken); err != nil {
				t.Fatal(err)
			}
			moved(t, sub, ref, &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: taken.Name}})
		})
	}
}

func TestMovedFrom(t *testing.T) {
	sub := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: "s", UID: "uid-1"}, Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}}}
	for _, tc := range []struct {
		name string
		edit func(*v1.SecurityReport)
		want bool
	}{
		{"made from it", func(*v1.SecurityReport) {}, true},
		{"no annotations", func(r *v1.SecurityReport) { r.Annotations = nil }, false},
		{"another submission's name", func(r *v1.SecurityReport) { r.Annotations[v1.MovedFromSubmissionAnnotation] = "t" }, false},
		{"another submission's UID", func(r *v1.SecurityReport) { r.Annotations[v1.MovedFromSubmissionUIDAnnotation] = "uid-2" }, false},
		{"another owner", func(r *v1.SecurityReport) { r.Spec.UserRef.Name = "u2" }, false},
		{"no owner", func(r *v1.SecurityReport) { r.Spec.UserRef = nil }, false},
	} {
		rep := movedReport(sub, &v1.SubmissionReview{}, v1.MovedReportName(sub.Name, sub.UID))
		tc.edit(rep)
		if got := movedFrom(rep, sub); got != tc.want {
			t.Errorf("%s: movedFrom = %v", tc.name, got)
		}
	}
	if movedFrom(&v1.SecurityReport{Spec: v1.SecurityReportSpec{UserRef: &v1.UserReference{Name: "u1"}}},
		&v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: ""}, Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}}}) {
		t.Error("a submission without a UID matched a report without annotations")
	}
}
