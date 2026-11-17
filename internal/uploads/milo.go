package uploads

import (
	"context"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"go.miloapis.com/feedback/internal/identity"
)

// Milo authenticates and authorizes callers with Milo's TokenReview and
// SubjectAccessReview APIs.
type Milo struct {
	Client kubernetes.Interface
}

var (
	_ Authenticator = Milo{}
	_ Authorizer    = Milo{}
)

// Authenticate implements Authenticator.
func (m Milo) Authenticate(ctx context.Context, token string) (authenticationv1.UserInfo, bool, error) {
	tr, err := m.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.UserInfo{}, false, err
	}
	return tr.Status.User, tr.Status.Authenticated, nil
}

// Allowed implements Authorizer. In a user's context it adds the parent
// extras Milo adds to requests made there.
func (m Milo) Allowed(ctx context.Context, u authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes, inUserContext string) (bool, error) {
	extra := map[string]authorizationv1.ExtraValue{}
	for k, v := range u.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	if inUserContext != "" {
		extra[identity.ParentAPIGroupExtraKey] = authorizationv1.ExtraValue{"iam.miloapis.com"}
		extra[identity.ParentKindExtraKey] = authorizationv1.ExtraValue{identity.ParentKindUser}
		extra[identity.ParentNameExtraKey] = authorizationv1.ExtraValue{inUserContext}
	}
	sar, err := m.Client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &attrs,
			User:               u.Username,
			UID:                u.UID,
			Groups:             u.Groups,
			Extra:              extra,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}
