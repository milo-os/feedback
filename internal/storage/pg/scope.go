package pg

import (
	"context"
	"fmt"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// Scope types. A Scope with an empty Type means "every scope", used when
// staff list user-owned resources from the platform context.
const (
	ScopePlatform = "Platform"
	ScopeUser     = "User"
)

// User extra keys Milo sets on requests it proxies to aggregated APIs.
const (
	ParentKindExtraKey = "iam.miloapis.com/parent-type"
	ParentNameExtraKey = "iam.miloapis.com/parent-name"
)

// Scope is who owns an object: the platform, or one user (by UID).
type Scope struct {
	Type string
	Name string
}

// All reports whether the scope spans every owner.
func (s Scope) All() bool { return s.Type == "" }

// AllScopes is the scope that matches every owner.
var AllScopes = Scope{}

// ScopeFunc resolves the scope for a request.
type ScopeFunc func(ctx context.Context) (Scope, error)

// RequestContext returns the context Milo resolved for the request: the
// parent kind ("User", "Organization", "Project") and name from the user
// extras, or ("Platform", "") when there are none.
func RequestContext(ctx context.Context) (kind, name string) {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return ScopePlatform, ""
	}
	return contextFromUser(u)
}

func contextFromUser(u user.Info) (kind, name string) {
	extra := u.GetExtra()
	kinds, names := extra[ParentKindExtraKey], extra[ParentNameExtraKey]
	if len(kinds) == 0 || len(names) == 0 || kinds[0] == "" {
		return ScopePlatform, ""
	}
	return kinds[0], names[0]
}

// PlatformScoped stores every object in the platform scope, whatever context
// the request came from. Used for posts, releases and changelog entries;
// authorization decides who may write them.
func PlatformScoped(context.Context) (Scope, error) {
	return Scope{Type: ScopePlatform}, nil
}

// UserOwned scopes objects to the calling user in the user context, and to
// every user in the platform context (staff views). Other contexts are
// rejected; the store reports that as Forbidden.
func UserOwned(ctx context.Context) (Scope, error) {
	kind, name := RequestContext(ctx)
	switch kind {
	case ScopeUser:
		return Scope{Type: ScopeUser, Name: name}, nil
	case ScopePlatform:
		return AllScopes, nil
	default:
		return Scope{}, fmt.Errorf("only available in the user or platform context, not %s", kind)
	}
}
